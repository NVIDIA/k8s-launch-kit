// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package nicconfigdaemon

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	yaml "sigs.k8s.io/yaml"

	"github.com/nvidia/k8s-launch-kit/pkg/nicconfigdaemon/assets"
)

const (
	testRepo    = "nvcr.io/nvidia/mellanox"
	testVersion = "v1.3.1"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, apiextv1.AddToScheme(scheme))
	return scheme
}

func newFakeClient(t *testing.T, initObjs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(initObjs...).
		Build()
}

// embeddedCRDNames returns the metadata.name of every CRD shipped under
// pkg/nicconfigdaemon/assets/crds. Used so tests stay in sync with the
// vendored set without hard-coding the count.
func embeddedCRDNames(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(assets.CRDs, "crds")
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, rErr := fs.ReadFile(assets.CRDs, "crds/"+entry.Name())
		require.NoError(t, rErr)
		crd := &apiextv1.CustomResourceDefinition{}
		require.NoError(t, yaml.Unmarshal(data, crd))
		require.NotEmpty(t, crd.Name, "embedded CRD %s is missing metadata.name", entry.Name())
		names = append(names, crd.Name)
	}
	return names
}

func TestEnsure_CreatesNamespaceAndCRDsWhenMissing(t *testing.T) {
	c := newFakeClient(t)

	require.NoError(t, Ensure(context.Background(), c, Options{
		Repository: testRepo,
		Version:    testVersion,
	}))

	// Namespace
	ns := &corev1.Namespace{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: Namespace}, ns))

	// SA
	sa := &corev1.ServiceAccount{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: Namespace, Name: SAName}, sa))

	// ClusterRole + ClusterRoleBinding
	cr := &rbacv1.ClusterRole{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: ClusterRoleName}, cr))
	crb := &rbacv1.ClusterRoleBinding{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: ClusterRoleBindingName}, crb))

	// DaemonSet
	ds := &appsv1.DaemonSet{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: Namespace, Name: DaemonSetName}, ds))

	// CRDs: every embedded YAML was created
	for _, name := range embeddedCRDNames(t) {
		crd := &apiextv1.CustomResourceDefinition{}
		require.NoErrorf(t, c.Get(context.Background(), types.NamespacedName{Name: name}, crd),
			"CRD %q missing after Ensure", name)
	}
}

func TestEnsure_SkipsCRDsWhenPresent(t *testing.T) {
	// Pre-create every embedded CRD with a non-default annotation so we can
	// confirm Ensure left them untouched.
	prePopulated := make([]client.Object, 0)
	for _, name := range embeddedCRDNames(t) {
		prePopulated = append(prePopulated, &apiextv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Annotations: map[string]string{"pre-existing": "true"},
			},
			Spec: apiextv1.CustomResourceDefinitionSpec{
				Group: "configuration.net.nvidia.com",
				Names: apiextv1.CustomResourceDefinitionNames{
					Kind:     "Placeholder",
					ListKind: "PlaceholderList",
					Plural:   "placeholders",
					Singular: "placeholder",
				},
				Scope: apiextv1.NamespaceScoped,
				Versions: []apiextv1.CustomResourceDefinitionVersion{
					{Name: "v1alpha1", Served: true, Storage: true,
						Schema: &apiextv1.CustomResourceValidation{
							OpenAPIV3Schema: &apiextv1.JSONSchemaProps{Type: "object"},
						}},
				},
			},
		})
	}
	c := newFakeClient(t, prePopulated...)

	require.NoError(t, Ensure(context.Background(), c, Options{
		Repository: testRepo,
		Version:    testVersion,
	}))

	for _, name := range embeddedCRDNames(t) {
		crd := &apiextv1.CustomResourceDefinition{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name}, crd))
		// Annotation survives → Ensure did not overwrite.
		assert.Equal(t, "true", crd.Annotations["pre-existing"], "Ensure must not touch pre-existing CRD %q", name)
		// The placeholder Kind survives → CRD spec was not replaced.
		assert.Equal(t, "Placeholder", crd.Spec.Names.Kind)
	}
}

func TestEnsure_AppliesDaemonSetWithExpectedImage(t *testing.T) {
	c := newFakeClient(t)

	pullSecrets := []string{"my-registry-creds"}
	require.NoError(t, Ensure(context.Background(), c, Options{
		Repository:       testRepo,
		Version:          testVersion,
		ImagePullSecrets: pullSecrets,
		LogLevel:         "debug",
	}))

	ds := &appsv1.DaemonSet{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: Namespace, Name: DaemonSetName}, ds))

	require.Len(t, ds.Spec.Template.Spec.Containers, 1)
	container := ds.Spec.Template.Spec.Containers[0]

	expectedImage := testRepo + "/" + DaemonImageName + ":" + testVersion
	assert.Equal(t, expectedImage, container.Image, "daemon image mismatch")

	// LOG_LEVEL flowed through from Options
	var logLevelEnv string
	for _, e := range container.Env {
		if e.Name == "LOG_LEVEL" {
			logLevelEnv = e.Value
		}
	}
	assert.Equal(t, "debug", logLevelEnv)

	// ImagePullSecrets surfaced on the pod spec
	require.Len(t, ds.Spec.Template.Spec.ImagePullSecrets, 1)
	assert.Equal(t, "my-registry-creds", ds.Spec.Template.Spec.ImagePullSecrets[0].Name)

	// nodeSelector pins to Mellanox-NIC nodes
	assert.Equal(t, "true",
		ds.Spec.Template.Spec.NodeSelector["feature.node.kubernetes.io/pci-15b3.present"])

	// Privileged + hostPID required for the daemon to read /sys
	assert.True(t, ds.Spec.Template.Spec.HostPID)
	require.NotNil(t, container.SecurityContext)
	require.NotNil(t, container.SecurityContext.Privileged)
	assert.True(t, *container.SecurityContext.Privileged)
}

func TestEnsure_Idempotent(t *testing.T) {
	c := newFakeClient(t)
	opts := Options{Repository: testRepo, Version: testVersion}

	require.NoError(t, Ensure(context.Background(), c, opts))
	// Second call must not error or leave duplicates behind.
	require.NoError(t, Ensure(context.Background(), c, opts))

	// Single namespace exists.
	ns := &corev1.Namespace{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: Namespace}, ns))
}

func TestEnsure_RejectsEmptyRepositoryOrVersion(t *testing.T) {
	c := newFakeClient(t)

	err := Ensure(context.Background(), c, Options{Version: testVersion})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Repository")

	err = Ensure(context.Background(), c, Options{Repository: testRepo})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Version")
}

func TestOptions_Image_TrimsTrailingSlashOnRepository(t *testing.T) {
	opts := Options{Repository: "nvcr.io/nvidia/mellanox/", Version: "v1.3.1"}
	assert.Equal(t, "nvcr.io/nvidia/mellanox/nic-configuration-operator-daemon:v1.3.1", opts.Image())
}

func TestCleanup_DeletesNamespaceAndClusterRBAC(t *testing.T) {
	c := newFakeClient(t)
	require.NoError(t, Ensure(context.Background(), c, Options{
		Repository: testRepo,
		Version:    testVersion,
	}))

	require.NoError(t, Cleanup(context.Background(), c))

	// Namespace removed
	err := c.Get(context.Background(), types.NamespacedName{Name: Namespace}, &corev1.Namespace{})
	assert.True(t, apierrors.IsNotFound(err), "namespace should be gone, got: %v", err)

	// ClusterRole/ClusterRoleBinding removed
	err = c.Get(context.Background(), types.NamespacedName{Name: ClusterRoleName}, &rbacv1.ClusterRole{})
	assert.True(t, apierrors.IsNotFound(err))
	err = c.Get(context.Background(), types.NamespacedName{Name: ClusterRoleBindingName}, &rbacv1.ClusterRoleBinding{})
	assert.True(t, apierrors.IsNotFound(err))

	// CRDs intentionally remain in place (so external NicDevice CRs survive).
	for _, name := range embeddedCRDNames(t) {
		err := c.Get(context.Background(), types.NamespacedName{Name: name}, &apiextv1.CustomResourceDefinition{})
		assert.NoErrorf(t, err, "CRD %q should remain after Cleanup", name)
	}
}

func TestCleanup_NamespaceMissingIsNoOp(t *testing.T) {
	c := newFakeClient(t)

	// Nothing was ever created; Cleanup must succeed.
	require.NoError(t, Cleanup(context.Background(), c))
}

func TestRenderDaemonManifests_AllExpectedKindsPresent(t *testing.T) {
	objs, err := renderDaemonManifests(Options{
		Repository: testRepo,
		Version:    testVersion,
		LogLevel:   "info",
	})
	require.NoError(t, err)

	kinds := map[string]bool{}
	for _, o := range objs {
		kinds[strings.ToLower(o.GetKind())] = true
	}
	for _, expected := range []string{"namespace", "serviceaccount", "clusterrole", "clusterrolebinding", "daemonset"} {
		assert.True(t, kinds[expected], "expected manifest kind %q to be rendered", expected)
	}
}
