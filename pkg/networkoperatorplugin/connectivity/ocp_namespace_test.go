// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package connectivity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestIsolateOpenShiftValidation(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	nadGVK := schema.GroupVersionKind{Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition"}
	scheme.AddKnownTypeWithName(nadGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(nadGVK.GroupVersion().WithKind("NetworkAttachmentDefinitionList"), &unstructured.UnstructuredList{})
	nad := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.cni.cncf.io/v1", "kind": "NetworkAttachmentDefinition",
		"metadata": map[string]interface{}{"name": "rail-0", "namespace": "workloads", "annotations": map[string]interface{}{
			"k8s.v1.cni.cncf.io/resourceName": "openshift.io/rail0", "sriovnetwork.openshift.io/owner-ref": "external-owner",
		}},
		"spec": map[string]interface{}{"config": `{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"pool"}}`},
	}}
	pullSecret := &corev1.Secret{}
	pullSecret.Name = "registry"
	pullSecret.Namespace = "workloads"
	pullSecret.Type = corev1.SecretTypeDockerConfigJson
	pullSecret.Data = map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nad, pullSecret).Build()
	ds := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1", "kind": "DaemonSet",
		"metadata": map[string]interface{}{"name": "test", "namespace": "workloads"},
		"spec": map[string]interface{}{"template": map[string]interface{}{
			"metadata": map[string]interface{}{"annotations": map[string]interface{}{networksAnnotation: "workloads/rail-0"}},
			"spec":     map[string]interface{}{"imagePullSecrets": []interface{}{map[string]interface{}{"name": "registry"}}},
		}},
	}}
	refs := []DaemonSetRef{{Name: "test", Namespace: "workloads"}}
	role := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
		"metadata": map[string]interface{}{"name": "scc-role", "namespace": "workloads"},
		"rules":    []interface{}{map[string]interface{}{"resourceNames": []interface{}{"original-scc"}}},
	}}
	scc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "security.openshift.io/v1", "kind": "SecurityContextConstraints",
		"metadata": map[string]interface{}{"name": "original-scc"},
	}}
	rb := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]interface{}{"name": "scc-role", "namespace": "workloads"},
		"subjects": []interface{}{map[string]interface{}{"kind": "ServiceAccount", "name": "test", "namespace": "workloads"}},
	}}
	var created []client.Object
	require.NoError(t, isolateOpenShiftValidation(context.Background(), c, []*unstructured.Unstructured{ds}, refs,
		[]*unstructured.Unstructured{scc, role, rb}, &created))
	target := refs[0].Namespace
	require.Regexp(t, `^l8k-validation-[0-9a-f]{12}$`, target)
	require.Equal(t, target, ds.GetNamespace())
	require.Equal(t, target, scc.GetName())
	require.Equal(t, target, role.GetNamespace())
	require.Equal(t, target, rb.GetNamespace())
	require.Equal(t, target, role.Object["rules"].([]interface{})[0].(map[string]interface{})["resourceNames"].([]interface{})[0])
	require.Equal(t, target, rb.Object["subjects"].([]interface{})[0].(map[string]interface{})["namespace"])
	annotation, found, err := unstructured.NestedString(ds.Object, "spec", "template", "metadata", "annotations", networksAnnotation)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "rail-0", annotation)
	copy := &unstructured.Unstructured{}
	copy.SetGroupVersionKind(nadGVK)
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: target, Name: "rail-0"}, copy))
	require.Equal(t, "openshift.io/rail0", copy.GetAnnotations()["k8s.v1.cni.cncf.io/resourceName"])
	require.NotContains(t, copy.GetAnnotations(), "sriovnetwork.openshift.io/owner-ref")
	secretCopy := &corev1.Secret{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: target, Name: "registry"}, secretCopy))
	require.Equal(t, pullSecret.Data, secretCopy.Data)
	require.Len(t, created, 3) // Namespace, pull secret, and NAD are tracked for cleanup.
}
