// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestMergeNFDConfigDataPreservesUnrelatedSettings(t *testing.T) {
	existing := "sources:\n  pci:\n    deviceLabelFields: [device]\n    extra: keep\n  custom:\n    enabled: true\nother: keep\n"
	desired := "sources:\n  pci:\n    deviceLabelFields: [vendor]\n    deviceClassWhitelist: ['02','03']\n"
	got, err := mergeNFDConfigData(existing, desired)
	require.NoError(t, err)
	require.Contains(t, got, "extra: keep")
	require.Contains(t, got, "enabled: true")
	require.Contains(t, got, "other: keep")
	require.Contains(t, got, "vendor")
	again, err := mergeNFDConfigData(got, desired)
	require.NoError(t, err)
	require.Equal(t, got, again)
}

func TestMergeSubscriptionEnvPreservesUnrelatedFields(t *testing.T) {
	sub := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1", "kind": "Subscription",
		"metadata": map[string]interface{}{"name": "nvidia-network-operator", "namespace": "operator-ns"},
		"spec": map[string]interface{}{"name": "nvidia-network-operator", "channel": "v26.7", "config": map[string]interface{}{"env": []interface{}{
			map[string]interface{}{"name": "UNRELATED", "valueFrom": map[string]interface{}{"secretKeyRef": map[string]interface{}{"name": "x", "key": "y"}}},
			map[string]interface{}{"name": "MAINTENANCE_OPERATOR_ENABLED", "value": "false"},
		}}},
	}}
	sub.SetGroupVersionKind(schema.GroupVersionKind{Group: "operators.coreos.com", Version: "v1alpha1", Kind: "Subscription"})
	c := fake.NewClientBuilder().WithObjects(sub).Build()
	desired := map[string]string{"MAINTENANCE_OPERATOR_ENABLED": "true", "MAINTENANCE_OPERATOR_REQUESTOR_NAMESPACE": "maintenance-ns"}
	require.NoError(t, mergeSubscriptionEnv(context.Background(), c, "operator-ns", desired, false))
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(sub.GroupVersionKind())
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "operator-ns", Name: "nvidia-network-operator"}, got))
	channel, _, _ := unstructured.NestedString(got.Object, "spec", "channel")
	require.Equal(t, "v26.7", channel)
	env, _, _ := unstructured.NestedSlice(got.Object, "spec", "config", "env")
	require.Len(t, env, 3)
	require.Contains(t, env[0].(map[string]interface{}), "valueFrom")
	require.Equal(t, "true", env[1].(map[string]interface{})["value"])
	require.NoError(t, mergeSubscriptionEnv(context.Background(), c, "operator-ns", desired, false))
	again := &unstructured.Unstructured{}
	again.SetGroupVersionKind(sub.GroupVersionKind())
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "operator-ns", Name: "nvidia-network-operator"}, again))
	envAgain, _, _ := unstructured.NestedSlice(again.Object, "spec", "config", "env")
	require.Equal(t, env, envAgain)
}

func TestMergeSubscriptionEnvRequiresExistingSubscription(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	err := mergeSubscriptionEnv(context.Background(), c, "operator-ns", map[string]string{"X": "Y"}, false)
	require.ErrorContains(t, err, "existing Network Operator Subscription required")
}

func TestNetworkOperatorSubscriptionRolloutChecksEnvAndReadiness(t *testing.T) {
	deployment := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]interface{}{"name": "network-operator", "generation": int64(3)},
		"spec": map[string]interface{}{
			"replicas": int64(1),
			"template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{
				map[string]interface{}{"name": "operator", "env": []interface{}{
					map[string]interface{}{"name": "MAINTENANCE_OPERATOR_ENABLED", "value": "true"},
				}},
			}}},
		},
		"status": map[string]interface{}{"observedGeneration": int64(3), "updatedReplicas": int64(1), "availableReplicas": int64(1)},
	}}
	desired := map[string]string{"MAINTENANCE_OPERATOR_ENABLED": "true"}
	require.True(t, deploymentHasEnv(deployment, desired))
	require.True(t, deploymentRolledOut(deployment))
	require.False(t, deploymentHasEnv(deployment, map[string]string{"MAINTENANCE_OPERATOR_ENABLED": "false"}))
	require.NoError(t, unstructured.SetNestedField(deployment.Object, int64(2), "status", "observedGeneration"))
	require.False(t, deploymentRolledOut(deployment))
}
