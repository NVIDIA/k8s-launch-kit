// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestMergeNFDConfigDataPreservesUnrelatedSettings(t *testing.T) {
	existing := "sources:\n  pci:\n    deviceClassWhitelist: ['01', '02']\n    deviceLabelFields: [device]\n    extra: keep\n  custom:\n    enabled: true\nother: keep\n"
	desired := "sources:\n  pci:\n    deviceLabelFields: [vendor]\n    deviceClassWhitelist: ['02','03']\n"
	got, err := mergeNFDConfigData(existing, desired)
	require.NoError(t, err)
	require.Contains(t, got, "extra: keep")
	require.Contains(t, got, "enabled: true")
	require.Contains(t, got, "other: keep")
	require.Contains(t, got, "vendor")
	require.Contains(t, got, "- \"01\"")
	require.NotContains(t, got, "- device")
	require.Contains(t, got, "- \"03\"")
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
		"status": map[string]interface{}{"observedGeneration": int64(3), "replicas": int64(1), "updatedReplicas": int64(1), "availableReplicas": int64(1)},
	}}
	desired := map[string]string{"MAINTENANCE_OPERATOR_ENABLED": "true"}
	require.True(t, deploymentHasEnv(deployment, desired))
	require.True(t, deploymentRolledOut(deployment))
	require.NoError(t, unstructured.SetNestedField(deployment.Object, int64(2), "status", "replicas"))
	require.False(t, deploymentRolledOut(deployment), "an old available replica must not satisfy a new rollout")
	require.NoError(t, unstructured.SetNestedField(deployment.Object, int64(1), "status", "replicas"))
	require.False(t, deploymentHasEnv(deployment, map[string]string{"MAINTENANCE_OPERATOR_ENABLED": "false"}))
	require.NoError(t, unstructured.SetNestedField(deployment.Object, int64(2), "status", "observedGeneration"))
	require.False(t, deploymentRolledOut(deployment))
}

func TestMergeSubscriptionEnvFindsPackageWithCustomSubscriptionName(t *testing.T) {
	sub := ocpTestObject("operators.coreos.com/v1alpha1", "Subscription", "operator-ns", "custom-network-sub", map[string]interface{}{
		"spec":   map[string]interface{}{"name": "nvidia-network-operator"},
		"status": map[string]interface{}{"installedCSV": "nvidia-network-operator.v26.7.0-1"},
	})
	c := fake.NewClientBuilder().WithObjects(sub).Build()
	require.NoError(t, mergeSubscriptionEnv(context.Background(), c, "operator-ns", map[string]string{
		"MAINTENANCE_OPERATOR_ENABLED": "true",
	}, false))
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(sub.GroupVersionKind())
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "operator-ns", Name: "custom-network-sub"}, got))
	env, found, err := unstructured.NestedSlice(got.Object, "spec", "config", "env")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "true", env[0].(map[string]interface{})["value"])
}

func TestOpenShiftRequiresSubscriptionBeforeConfigurationWrites(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().Build()
	cfg := &config.LaunchKitConfig{Flavor: config.FlavorOCP, NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-ns"}}
	doc := []byte("apiVersion: nfd.openshift.io/v1\nkind: NodeFeatureDiscovery\nmetadata:\n  name: nfd-instance\n  namespace: nfd-ns\nspec:\n  workerConfig:\n    configData: 'sources: {}'")
	artifacts, decodeErr := bundle.FromFiles([]bundle.File{{Name: "30-nfd.yaml", Content: string(doc)}})
	require.NoError(t, decodeErr)
	obj := artifacts.Documents()[0].ObjectCopy()
	err := applyOCPOperatorConfiguration(ctx, c, cfg, []*unstructured.Unstructured{obj}, false)
	require.ErrorContains(t, err, "Subscription is required")
	nfd := ocpTestObject("nfd.openshift.io/v1", "NodeFeatureDiscovery", "nfd-ns", "nfd-instance", nil)
	require.True(t, apierrors.IsNotFound(c.Get(ctx, types.NamespacedName{Namespace: "nfd-ns", Name: "nfd-instance"}, nfd)))
}

func TestOpenShiftRetryWaitsForSubscriptionAdoption(t *testing.T) {
	sub := ocpTestObject("operators.coreos.com/v1alpha1", "Subscription", "operator-ns", "nvidia-network-operator", map[string]interface{}{
		"spec": map[string]interface{}{"name": "nvidia-network-operator", "config": map[string]interface{}{"env": []interface{}{
			map[string]interface{}{"name": "MAINTENANCE_OPERATOR_ENABLED", "value": "true"},
			map[string]interface{}{"name": "MAINTENANCE_OPERATOR_REQUESTOR_NAMESPACE", "value": "nvidia-maintenance-operator"},
		}}},
		"status": map[string]interface{}{"installedCSV": "nvidia-network-operator.v26.7.0-1"},
	})
	csv := ocpTestObject("operators.coreos.com/v1alpha1", "ClusterServiceVersion", "operator-ns", "nvidia-network-operator.v26.7.0-1", map[string]interface{}{
		"spec": map[string]interface{}{"install": map[string]interface{}{"spec": map[string]interface{}{"deployments": []interface{}{
			map[string]interface{}{"name": "network-operator"},
		}}}},
	})
	c := fake.NewClientBuilder().WithObjects(sub, csv).Build()
	cfg := &config.LaunchKitConfig{Flavor: config.FlavorOCP, NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-ns"}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := applyOCPOperatorConfiguration(ctx, c, cfg, nil, false)
	require.ErrorContains(t, err, "did not adopt maintenance settings")
}

func TestOCPDisabledPluginsPreservedAndValidated(t *testing.T) {
	desired := ocpTestObject("sriovnetwork.openshift.io/v1", "SriovOperatorConfig", "sriov-ns", "default", map[string]interface{}{
		"spec": map[string]interface{}{"disablePlugins": []interface{}{"mellanox"}},
	})
	current := desired.DeepCopy()
	require.NoError(t, unstructured.SetNestedStringSlice(current.Object, []string{"virtual"}, "spec", "disablePlugins"))
	require.NoError(t, unstructured.SetNestedField(current.Object, true, "spec", "enableInjector"))
	require.NoError(t, mergeOCPSpec(current, desired))
	plugins, _, _ := unstructured.NestedStringSlice(current.Object, "spec", "disablePlugins")
	require.Equal(t, []string{"virtual", "mellanox"}, plugins)
	injector, _, _ := unstructured.NestedBool(current.Object, "spec", "enableInjector")
	require.True(t, injector)
	before := current.DeepCopy()
	require.NoError(t, mergeOCPSpec(current, desired))
	require.Equal(t, before.Object, current.Object)
	data, err := current.MarshalJSON()
	require.NoError(t, err)
	// Validate with the generated minimum, allowing the site's extra plugin.
	desiredData, err := desired.MarshalJSON()
	require.NoError(t, err)
	artifacts, err := bundle.FromFiles([]bundle.File{{Name: "01-config.yaml", Content: string(desiredData)}})
	require.NoError(t, err)
	results, err := ValidateBundle(context.Background(), fake.NewClientBuilder().WithObjects(current.DeepCopy()).Build(), artifacts, config.FlavorOCP)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, crstate.StateSuccess, results[0].State, string(data))
	unstructured.RemoveNestedField(desired.Object, "spec", "disablePlugins")
	require.NoError(t, mergeOCPSpec(current, desired))
	require.Equal(t, before.Object, current.Object, "opt-out preserves existing plugin ownership")
	require.NoError(t, unstructured.SetNestedField(desired.Object, "malformed", "spec", "disablePlugins"))
	require.ErrorContains(t, mergeOCPSpec(current, desired), "merge SR-IOV disablePlugins")
}
