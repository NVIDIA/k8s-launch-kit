// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func selectedNSIFixture() (*unstructured.Unstructured, *unstructured.Unstructured) {
	set := runtimeObject("ServiceInterfaceSet", "selected-port", "dpf", map[string]any{"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"selected": "true"}}, "spec": map[string]any{"interfaceType": "service", "service": map[string]any{"serviceID": "selected-service", "network": "bridge", "interfaceName": "p0_if"}}}})
	shard := runtimeObject("NodeServiceInterfaces", "selected-node-sfc", "dpf", map[string]any{"node": "selected-node", "type": "sfc", "interfaces": []any{map[string]any{"name": "dpf_selected-port", "labels": map[string]any{"selected": "true", "svc.dpu.nvidia.com/serviceinterfaceset-name": "selected-port", "svc.dpu.nvidia.com/serviceinterfaceset-namespace": "dpf", "svc.dpu.nvidia.com/service-id": "selected-service"}, "interfaceType": "service", "service": map[string]any{"serviceID": "selected-service", "network": "bridge", "interfaceName": "p0_if"}}}})
	shard.SetGeneration(4)
	shard.Object["status"] = map[string]any{"observedGeneration": int64(4), "conditions": []any{conditionMap("Ready", "True", "Reconciled", 4), conditionMap("NodeServiceInterfacesReconciled", "True", "Reconciled", 4)}, "interfaceStatuses": []any{map[string]any{"name": "dpf_selected-port", "conditions": []any{conditionMap("Ready", "True", "Reconciled", 4)}}}}
	return set, shard
}

func TestValidationSelectedNSIUsesExactNamedEntryAndCurrentGeneration(t *testing.T) {
	set, shard := selectedNSIFixture()
	beforeSet, beforeShard := set.DeepCopy(), shard.DeepCopy()
	if err := verifySelectedNSIEntry(testRules(t), set, shard); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(set.Object, beforeSet.Object) || !reflect.DeepEqual(shard.Object, beforeShard.Object) {
		t.Fatal("selected NSI verification mutated sources")
	}
	for _, test := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
	}{
		{name: "legacy combined key", mutate: func(shard *unstructured.Unstructured) {
			rows, _, _ := unstructured.NestedSlice(shard.Object, "spec", "interfaces")
			rows[0].(map[string]any)["name"] = "dpf/selected-port"
			_ = unstructured.SetNestedSlice(shard.Object, rows, "spec", "interfaces")
		}},
		{name: "wrong service identity", mutate: func(shard *unstructured.Unstructured) {
			rows, _, _ := unstructured.NestedSlice(shard.Object, "spec", "interfaces")
			rows[0].(map[string]any)["service"].(map[string]any)["serviceID"] = "other"
			_ = unstructured.SetNestedSlice(shard.Object, rows, "spec", "interfaces")
		}},
		{name: "terminating selected entry", mutate: func(shard *unstructured.Unstructured) {
			rows, _, _ := unstructured.NestedSlice(shard.Object, "spec", "interfaces")
			rows[0].(map[string]any)["terminating"] = true
			_ = unstructured.SetNestedSlice(shard.Object, rows, "spec", "interfaces")
		}},
		{name: "duplicate selected entry", mutate: func(shard *unstructured.Unstructured) {
			rows, _, _ := unstructured.NestedSlice(shard.Object, "spec", "interfaces")
			rows = append(rows, rows[0])
			_ = unstructured.SetNestedSlice(shard.Object, rows, "spec", "interfaces")
		}},
		{name: "status missing", mutate: func(shard *unstructured.Unstructured) {
			unstructured.RemoveNestedField(shard.Object, "status", "interfaceStatuses")
		}},
		{name: "status older", mutate: func(shard *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(shard.Object, []any{map[string]any{"name": "dpf_selected-port", "conditions": []any{conditionMap("Ready", "True", "Reconciled", 3)}}}, "status", "interfaceStatuses")
		}},
		{name: "status future", mutate: func(shard *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(shard.Object, []any{map[string]any{"name": "dpf_selected-port", "conditions": []any{conditionMap("Ready", "True", "Reconciled", 5)}}}, "status", "interfaceStatuses")
		}},
		{name: "status duplicate", mutate: func(shard *unstructured.Unstructured) {
			rows, _, _ := unstructured.NestedSlice(shard.Object, "status", "interfaceStatuses")
			_ = unstructured.SetNestedSlice(shard.Object, append(rows, rows[0]), "status", "interfaceStatuses")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := shard.DeepCopy()
			test.mutate(candidate)
			if err := verifySelectedNSIEntry(testRules(t), set, candidate); err == nil {
				t.Fatal("invalid selected entry accepted")
			}
		})
	}
	unrelated := shard.DeepCopy()
	rows, _, _ := unstructured.NestedSlice(unrelated.Object, "spec", "interfaces")
	rows = append(rows, map[string]any{"name": "another_unrelated", "interfaceType": "physical", "physical": map[string]any{"interfaceName": "other"}})
	_ = unstructured.SetNestedSlice(unrelated.Object, rows, "spec", "interfaces")
	if err := verifySelectedNSIEntry(testRules(t), set, unrelated); err != nil {
		t.Fatalf("unrelated shared shard entry rejected: %v", err)
	}
}

func TestValidationWorkloadReadinessDoesNotInventNumberReady(t *testing.T) {
	set := runtimeObject("ServiceInterfaceSet", "selected", "dpf", map[string]any{})
	set.SetGeneration(2)
	set.Object["status"] = map[string]any{"observedGeneration": int64(2), "numberApplied": int64(2), "conditions": []any{conditionMap("Ready", "True", "Reconciled", 2), conditionMap("ServiceInterfacesReconciled", "True", "Reconciled", 2), conditionMap("ServiceInterfacesReady", "True", "Reconciled", 2)}}
	if err := testRules(t).ValidationWorkloadReadiness(set); err != nil {
		t.Fatalf("valid current set without numberReady rejected: %v", err)
	}
	for _, observed := range []int64{0, 1, 3} {
		candidate := set.DeepCopy()
		_ = unstructured.SetNestedField(candidate.Object, observed, "status", "observedGeneration")
		if err := testRules(t).ValidationWorkloadReadiness(candidate); err == nil {
			t.Fatalf("generation %d accepted", observed)
		}
	}
	candidate := set.DeepCopy()
	conditions, _, _ := unstructured.NestedSlice(candidate.Object, "status", "conditions")
	_ = unstructured.SetNestedSlice(candidate.Object, append(conditions, conditions[0]), "status", "conditions")
	if err := testRules(t).ValidationWorkloadReadiness(candidate); err == nil {
		t.Fatal("ambiguous positive condition accepted")
	}
}
