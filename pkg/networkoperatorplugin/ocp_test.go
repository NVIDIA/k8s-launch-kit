// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func ocpTestObject(apiVersion, kind, namespace, name string, extra map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
	}}
	for key, value := range extra {
		obj.Object[key] = value
	}
	return obj
}

func TestCheckOCPOperatorsRequiresInstalledCSVAndServedAPI(t *testing.T) {
	ctx := context.Background()
	cfg := &config.LaunchKitConfig{Flavor: config.FlavorOCP, NetworkOperator: &config.NetworkOperatorConfig{
		Namespace: "operator-ns", SelectedRelease: "26.7",
	}}
	sub := ocpTestObject("operators.coreos.com/v1alpha1", "Subscription", "operator-ns", "nvidia-network-operator", map[string]interface{}{
		"spec":   map[string]interface{}{"name": "nvidia-network-operator"},
		"status": map[string]interface{}{"installedCSV": "nvidia-network-operator.v26.7.0-1"},
	})
	csv := ocpTestObject("operators.coreos.com/v1alpha1", "ClusterServiceVersion", "operator-ns", "nvidia-network-operator.v26.7.0-1", map[string]interface{}{
		"spec":   map[string]interface{}{"version": "26.7.0-1"},
		"status": map[string]interface{}{"phase": "Succeeded"},
	})
	crd := ocpTestObject("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "nicclusterpolicies.mellanox.com", map[string]interface{}{
		"spec":   map[string]interface{}{"versions": []interface{}{map[string]interface{}{"name": "v1alpha1", "served": true}}},
		"status": map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Established", "status": "True"}}},
	})
	newClient := func(objects ...client.Object) client.Client {
		return fake.NewClientBuilder().WithObjects(objects...).Build()
	}
	require.NoError(t, CheckOCPOperators(ctx, newClient(sub, csv, crd), cfg, false))
	customSub := sub.DeepCopy()
	customSub.SetName("custom-network-sub")
	require.NoError(t, CheckOCPOperators(ctx, newClient(customSub, csv, crd), cfg, false))
	require.ErrorContains(t, CheckOCPOperators(ctx, newClient(crd), cfg, false), "not installed")
	require.ErrorContains(t, CheckOCPOperators(ctx, newClient(sub, csv), cfg, false), "required API")

	wrongSub := sub.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(wrongSub.Object, "wrong-operator", "spec", "name"))
	require.ErrorContains(t, CheckOCPOperators(ctx, newClient(wrongSub, csv, crd), cfg, false), "expected")

	failedCSV := csv.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(failedCSV.Object, "Failed", "status", "phase"))
	require.ErrorContains(t, CheckOCPOperators(ctx, newClient(sub, failedCSV, crd), cfg, false), "expected Succeeded")

	unserved := crd.DeepCopy()
	require.NoError(t, unstructured.SetNestedSlice(unserved.Object, []interface{}{map[string]interface{}{"name": "v1alpha1", "served": false}}, "spec", "versions"))
	require.ErrorContains(t, CheckOCPOperators(ctx, newClient(sub, csv, unserved), cfg, false), "not served")

	wrongVersion := *cfg
	operatorCfg := *cfg.NetworkOperator
	operatorCfg.SelectedRelease = "26.4"
	wrongVersion.NetworkOperator = &operatorCfg
	require.ErrorContains(t, CheckOCPOperators(ctx, newClient(sub, csv, crd), &wrongVersion, false), "does not match")
}
