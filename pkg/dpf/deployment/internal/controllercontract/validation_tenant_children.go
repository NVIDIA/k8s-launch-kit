// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package controllercontract

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ValidationWorkloadReadiness is the final, generation-current contract for
// pinned v26.8 management and tenant SFC objects. The early deployment copy
// milestone remains intentionally weaker. Stable chart/capture evidence is
// documented in the implementation component contract; no invented numberReady
// counter or legacy ServiceInterface instance is required for the NSI path.
func (r *Rules) ValidationWorkloadReadiness(object *unstructured.Unstructured) Readiness {
	if !r.valid() || object == nil || object.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" || object.GetDeletionTimestamp() != nil {
		return result("RESOURCE_STATUS_INVALID", "Current workload readiness input is invalid.")
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
		return result("RESOURCE_KIND_UNSUPPORTED", "Current workload kind is unsupported.")
	}
	base := r.ValidationConditions(object)
	if !base.Ready && base.ReasonCode != "RESOURCE_STATUS_STALE" {
		return base
	}
	generation, _ := metadataGeneration(object)
	pending := base
	for _, condition := range required {
		check := validationWorkloadCondition(object, condition, generation)
		if check.Ready {
			continue
		}
		if !validationWorkloadProgressing(check) {
			return check
		}
		pending = check
	}
	if !pending.Ready {
		return pending
	}
	return ready("Every selected workload condition is true at the current generation.")
}

// ValidationNSIEntryReady binds exactly one selected status entry to the shard
// generation. Shared, unrelated entries do not contribute selected evidence.
func (r *Rules) ValidationNSIEntryReady(shard *unstructured.Unstructured, name string) Readiness {
	base := r.ValidationWorkloadReadiness(shard)
	if !base.Ready && !validationWorkloadProgressing(base) {
		return base
	}
	rows, found, err := unstructured.NestedSlice(shard.Object, "status", "interfaceStatuses")
	if err != nil {
		return result("RESOURCE_STATUS_INVALID", "NSI entry status collection is malformed.")
	}
	if !found {
		return result("RESOURCE_STATUS_STALE", "Selected NSI entry status has not been reported.")
	}
	matched := 0
	var selected map[string]any
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			return result("RESOURCE_STATUS_INVALID", "NSI entry status is malformed.")
		}
		entryName, validName := row["name"].(string)
		if !validName || entryName == "" {
			return result("RESOURCE_STATUS_INVALID", "NSI entry status name is malformed.")
		}
		if entryName != name {
			continue
		}
		matched++
		selected = row
	}
	if matched > 1 {
		return result("RESOURCE_STATUS_AMBIGUOUS", "Selected NSI entry status is duplicated.")
	}
	if matched == 0 {
		return result("RESOURCE_STATUS_STALE", "Selected NSI entry status has not been reported.")
	}
	entry := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"generation": shard.GetGeneration()}, "status": map[string]any{"conditions": selected["conditions"]}}}
	if check := validationWorkloadCondition(entry, "Ready", shard.GetGeneration()); !check.Ready {
		return check
	}
	if !base.Ready {
		return base
	}
	return ready("Selected NSI entry is current and ready.")
}

// Pending is the pinned controllers' explicit progressing reason. Other False
// reasons and Unknown status cannot authorize wait retries. Validate all required
// conditions so a progressing sibling never hides malformed or terminal status.
func validationWorkloadCondition(object *unstructured.Unstructured, kind string, generation int64) Readiness {
	condition, state := findCondition(object, kind)
	if !state.Ready {
		return state
	}
	if condition != nil {
		if condition.status != "True" && condition.status != "False" || condition.observedGeneration != nil && (*condition.observedGeneration < 0 || *condition.observedGeneration > generation) {
			return result("RESOURCE_STATUS_INVALID", "Selected workload condition status or observed generation is invalid.")
		}
		if condition.status == "False" && condition.reason != "Pending" {
			return result("RESOURCE_TERMINAL_FAILURE", "Selected workload condition is False without a supported progressing reason.")
		}
	}
	check := conditionAtGeneration(object, kind, generation, true)
	if check.ReasonCode == "RESOURCE_NOT_READY" {
		return result("RESOURCE_PROGRESSING", "Selected workload condition reports Pending.")
	}
	return check
}

func validationWorkloadProgressing(check Readiness) bool {
	switch check.ReasonCode {
	case "RESOURCE_STATUS_STALE", "RESOURCE_PROGRESSING", "CONDITION_STATUS_STALE", "CONDITION_GENERATION_MISSING", "READINESS_CONDITION_MISSING":
		return true
	default:
		return false
	}
}
