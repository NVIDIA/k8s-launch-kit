// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package controllercontract

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ValidationWorkloadReadiness is the final, generation-current contract for
// pinned v26.8 management and tenant SFC objects. The early deployment copy
// milestone remains intentionally weaker. Stable chart/capture evidence is
// documented in the implementation component contract; no invented numberReady
// counter or legacy ServiceInterface instance is required for the NSI path.
func (r *Rules) ValidationWorkloadReadiness(object *unstructured.Unstructured) error {
	if !r.valid() || object == nil || object.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" || object.GetDeletionTimestamp() != nil {
		return fmt.Errorf("current workload readiness input invalid")
	}
	conditions := map[string][]string{
		"DPUService":            {"Ready", "ApplicationsReconciled", "ApplicationsReady"},
		"DPUServiceChain":       {"Ready", "ServiceChainSetReady", "ServiceChainSetReconciled"},
		"DPUServiceInterface":   {"Ready", "ServiceInterfacePreReqsReady", "ServiceInterfaceSetReady", "ServiceInterfaceSetReconciled"},
		"ServiceChainSet":       {"Ready", "ServiceChainsReconciled", "ServiceChainsReady"},
		"ServiceChain":          {"Ready", "ServiceChainReconciled"},
		"ServiceInterfaceSet":   {"Ready", "ServiceInterfacesReconciled", "ServiceInterfacesReady"},
		"NodeServiceInterfaces": {"Ready", "NodeServiceInterfacesReconciled"},
	}
	required, ok := conditions[object.GetKind()]
	if !ok {
		return fmt.Errorf("current workload kind unsupported")
	}
	generation, valid := metadataGeneration(object)
	observed, found, err := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
	if !valid || err != nil || !found || observed != generation {
		return fmt.Errorf("current workload generation unavailable")
	}
	for _, condition := range required {
		if !conditionAtGeneration(object, condition, generation, true).Ready {
			return fmt.Errorf("current workload condition unavailable")
		}
	}
	return nil
}

// ValidationNSIEntryReady binds exactly one selected status entry to the shard
// generation. Shared, unrelated entries do not contribute selected evidence.
func (r *Rules) ValidationNSIEntryReady(shard *unstructured.Unstructured, name string) error {
	if err := r.ValidationWorkloadReadiness(shard); err != nil {
		return err
	}
	rows, found, err := unstructured.NestedSlice(shard.Object, "status", "interfaceStatuses")
	if err != nil || !found {
		return fmt.Errorf("selected NSI entry status unavailable")
	}
	matched := 0
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("NSI entry status malformed")
		}
		if row["name"] != name {
			continue
		}
		matched++
		entry := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"generation": shard.GetGeneration()}, "status": map[string]any{"conditions": row["conditions"]}}}
		if !conditionAtGeneration(entry, "Ready", shard.GetGeneration(), true).Ready {
			return fmt.Errorf("selected NSI entry not ready")
		}
	}
	if matched != 1 {
		return fmt.Errorf("selected NSI entry status missing or ambiguous")
	}
	return nil
}
