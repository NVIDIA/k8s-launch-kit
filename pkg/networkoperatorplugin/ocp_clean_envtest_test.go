// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Real API-server field ownership, resourceVersions and UIDs exercise behavior
// controller-runtime's fake client cannot reproduce.
func TestOpenShiftCleanAPIServerOwnership(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("requires local envtest control-plane binaries via KUBEBUILDER_ASSETS")
	}
	crd := &apiextensions.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "nicclusterpolicies.mellanox.com"},
		Spec: apiextensions.CustomResourceDefinitionSpec{
			Group: "mellanox.com", Names: apiextensions.CustomResourceDefinitionNames{Plural: "nicclusterpolicies", Kind: "NicClusterPolicy"}, Scope: apiextensions.ClusterScoped,
			Versions: []apiextensions.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true, Schema: &apiextensions.CustomResourceValidation{OpenAPIV3Schema: &apiextensions.JSONSchemaProps{Type: "object", Properties: map[string]apiextensions.JSONSchemaProps{"spec": {Type: "object", XPreserveUnknownFields: ptr.To(true), Properties: map[string]apiextensions.JSONSchemaProps{"defaulted": {Type: "boolean", Default: &apiextensions.JSON{Raw: []byte(`true`)}}}}}}}}},
		},
	}
	testEnv := &envtest.Environment{CRDs: []*apiextensions.CustomResourceDefinition{crd}}
	restConfig, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testEnv.Stop()) })
	c, err := client.New(restConfig, client.Options{})
	require.NoError(t, err)
	require.NoError(t, c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "operation-test"}}))
	lockCtx, cancelLock := context.WithCancel(context.Background())
	release, err := acquireOCPOperation(lockCtx, c, "operation-test", "test-deployment", "deploy")
	require.NoError(t, err)
	_, err = acquireOCPOperation(lockCtx, c, "operation-test", "test-deployment", "clean")
	require.ErrorContains(t, err, "is held")
	cancelLock()
	require.NoError(t, release())
	release, err = acquireOCPOperation(context.Background(), c, "operation-test", "test-deployment", "clean")
	require.NoError(t, err)
	require.NoError(t, release())

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	desired := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "mellanox.com/v1alpha1", "kind": "NicClusterPolicy", "metadata": map[string]any{"name": "recorded"}, "spec": map[string]any{"probe": "one"},
	}}
	desired.SetAnnotations(map[string]string{launchKitVersionAnnotation: "test", ocpCleanOwnerAnnotation: "test-deployment"})
	live := desired.DeepCopy()
	require.NoError(t, applyOCPUnstructured(ctx, c, live, false))
	require.NotEmpty(t, live.GetUID())
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(desired), live))
	require.NoError(t, validateOCPCleanLiveOwnership(ctx, c, desired, live), "owned fields: %v", live.GetManagedFields())
	require.NoError(t, applyOCPUnstructured(ctx, c, desired.DeepCopy(), false))
	// The actual served default is allowed, but a newer l8k-owned active
	// component cannot be explained by the older retained desired subset.
	defaulted, found, err := unstructured.NestedBool(live.Object, "spec", "defaulted")
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, defaulted)
	require.NoError(t, unstructured.SetNestedField(live.Object, map[string]interface{}{"image": "new-driver"}, "spec", "ofedDriver"))
	require.NoError(t, c.Update(ctx, live, client.FieldOwner("l8k")))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(desired), live))
	require.ErrorContains(t, validateOCPCleanLiveOwnership(ctx, c, desired, live), "live spec differs")

	external := desired.DeepCopy()
	external.SetName("external")
	external.SetAnnotations(nil)
	require.NoError(t, c.Create(ctx, external, client.FieldOwner("site")))
	adopted := desired.DeepCopy()
	adopted.SetName("external")
	require.NoError(t, applyOCPUnstructured(ctx, c, adopted, false))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(external), external))
	require.Error(t, validateOCPCleanLiveOwnership(ctx, c, adopted, external))
}
