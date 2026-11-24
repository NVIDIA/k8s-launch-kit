// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/uuid"
	"os"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestOpenShiftCleanUsesRetainedConfigAndBundleTogether(t *testing.T) {
	root := filepath.Join(t.TempDir(), "network-operator")
	cfg := &config.LaunchKitConfig{Flavor: config.FlavorOCP, NetworkOperator: &config.NetworkOperatorConfig{Namespace: "site-operator"}}
	_, err := config.WriteEffectiveConfig(root, cfg)
	require.NoError(t, err)
	files := filepath.Join(root, "network-operator")
	require.NoError(t, os.MkdirAll(files, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(files, "10-ncp.yaml"), []byte("apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: example\n"), 0o600))
	for _, path := range []string{root, files} {
		got, artifacts, err := loadOCPCleanBundle(path)
		require.NoError(t, err)
		require.Equal(t, cfg.NetworkOperator.Namespace, got.NetworkOperator.Namespace)
		require.Len(t, artifacts.Documents(), 1)
	}
	cfg.Flavor = config.FlavorK8s
	_, err = config.WriteEffectiveConfig(root, cfg)
	require.NoError(t, err)
	_, _, err = loadOCPCleanBundle(files)
	require.ErrorContains(t, err, "OpenShift Host bundle")
	_, _, err = loadOCPCleanBundle(t.TempDir())
	require.ErrorContains(t, err, "retained OpenShift configuration")
}

func TestCleanPlatformSelectionCannotWidenTarget(t *testing.T) {
	require.NoError(t, validateCleanPlatform(true, "", ""))
	require.NoError(t, validateCleanPlatform(true, "ocp", "files"))
	require.NoError(t, validateCleanPlatform(false, "k8s", ""))
	require.Error(t, validateCleanPlatform(true, "k8s", ""))
	require.Error(t, validateCleanPlatform(false, "ocp", ""))
	require.Error(t, validateCleanPlatform(false, "", "files"))
	require.Error(t, validateCleanPlatform(true, "other", ""))
}

func TestOpenShiftCleanOverridesOnlyConfirmBundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	require.NoError(t, os.WriteFile(path, []byte("flavor: ocp\nnetworkOperator:\n  namespace: site\nunrelated: ignored\n"), 0o600))
	require.NoError(t, checkOCPCleanOverrides(path, "site", "site"))
	require.Error(t, checkOCPCleanOverrides(path, "other", ""))
	require.Error(t, checkOCPCleanOverrides(path, "site", "other"))
	require.Error(t, checkOCPCleanOverrides("", "bad/namespace", ""))
}

func TestOpenShiftCleanCommandRemovesOnlyBundleConfiguration(t *testing.T) {
	root := t.TempDir()
	cfg := &config.LaunchKitConfig{Flavor: config.FlavorOCP, NetworkOperator: &config.NetworkOperatorConfig{Namespace: "site-operator"}}
	_, err := config.WriteEffectiveConfig(root, cfg)
	require.NoError(t, err)
	dir := filepath.Join(root, "network-operator")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	const manifest = "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: original\n  annotations:\n    nvidia.kubernetes-launch-kit.version: test\nspec:\n  nvIpam:\n    enableWebhook: false\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "10-ncp.yaml"), []byte(manifest), 0o600))
	_, artifacts, err := loadOCPCleanBundle(root)
	require.NoError(t, err)
	obj := artifacts.Documents()[0].ObjectCopy()
	obj.SetUID("original")
	obj.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "l8k", Operation: metav1.ManagedFieldsOperationApply, APIVersion: obj.GetAPIVersion(), FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:nvIpam":{"f:enableWebhook":{}}}}`)}})
	sub := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "operators.coreos.com/v1alpha1", "kind": "Subscription", "metadata": map[string]any{"namespace": "site-operator", "name": "nvidia-network-operator"}, "spec": map[string]any{"name": "nvidia-network-operator"}}}
	c := fake.NewClientBuilder().WithReturnManagedFields().WithObjects(obj, sub).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			obj.SetUID(uuid.NewUUID())
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	oldFiles, oldSkip, oldTimeout := cleanDeploymentFiles, skipCleanWait, cleanTimeout
	preserveCleanFlagState(t)
	t.Cleanup(func() { cleanDeploymentFiles, skipCleanWait, cleanTimeout = oldFiles, oldSkip, oldTimeout })
	cleanDeploymentFiles, skipCleanWait, cleanTimeout = root, true, time.Second
	userConfig, networkOperatorNamespace = "", ""
	var stdout, stderr bytes.Buffer
	out := ui.NewJSON(&stdout, &stderr)
	ctx := ui.WithOutput(context.Background(), out)
	require.NoError(t, runOpenShiftClean(ctx, c, nil, out, out))
	var result ui.JSONResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	require.True(t, result.Success)
	require.Equal(t, 1, result.Cleanup.CustomResourcesDeleted)
	require.False(t, result.Cleanup.HelmReleaseRemoved)
	require.NotEmpty(t, result.Cleanup.Retained)
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(sub), sub))
}
