// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package controllercontract

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type WorkloadMode uint8

const (
	WorkloadNone WorkloadMode = iota
	WorkloadTemplate
	WorkloadIPAM
	WorkloadInterface
)

func (r *Rules) WorkloadReadiness(object *unstructured.Unstructured, mode WorkloadMode) Readiness {
	if !r.valid() {
		return invalidRulesReadiness()
	}
	if object == nil || object.GetDeletionTimestamp() != nil {
		return result("WORKLOAD_RESOURCE_DELETING", "The retained workload prerequisite is unavailable or deleting.")
	}
	generation, ok := metadataGeneration(object)
	if !ok {
		return result("WORKLOAD_GENERATION_INVALID", "The workload resource generation is missing or invalid.")
	}
	observed, found, err := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
	if err != nil || !found || observed < generation || observed == 0 {
		return result("WORKLOAD_STATUS_STALE", "The DPF controller has not observed the current workload generation.")
	}
	if observed > generation {
		return result("WORKLOAD_STATUS_INVALID", "The DPF controller observed a future workload generation.")
	}
	var conditions []string
	switch mode {
	case WorkloadTemplate:
		conditions = []string{"Ready", "DPUServiceTemplateReconciled"}
	case WorkloadIPAM:
		conditions = []string{"DPUIPAMObjectReconciled"}
	case WorkloadInterface:
		conditions = []string{"ServiceInterfaceSetReconciled"}
	default:
		return result("WORKLOAD_STATUS_UNSUPPORTED", "No readiness condition is defined for this workload resource.")
	}
	for _, conditionType := range conditions {
		state := conditionReadiness(object, conditionType, "WORKLOAD_CONDITION_STALE", true)
		if !state.Ready {
			if state.ReasonCode == "RESOURCE_NOT_READY" {
				condition, conditionState := findCondition(object, conditionType)
				if !conditionState.Ready || condition == nil {
					return conditionState
				}
				if condition.reason != "Pending" {
					return withControllerReason(result("RESOURCE_TERMINAL_FAILURE", fmt.Sprintf("The %s condition failed reconciliation.", conditionType)), condition.reason)
				}
				state.ReasonCode = "WORKLOAD_RECONCILIATION_PENDING"
			}
			return state
		}
	}
	return ready("The DPF controller reported the required current-generation workload reconciliation condition.")
}

func TerminalWorkloadReadiness(state Readiness) bool {
	switch state.ReasonCode {
	case "RESOURCE_TERMINAL_FAILURE", "RESOURCE_DELETING", "RESOURCE_STATUS_INVALID", "RESOURCE_STATUS_AMBIGUOUS", "WORKLOAD_GENERATION_INVALID", "WORKLOAD_STATUS_INVALID", "WORKLOAD_STATUS_UNSUPPORTED":
		return true
	default:
		return false
	}
}
