// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package controllercontract

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// ValidationConditions requires every selected condition exactly at the current
// object generation; it never substitutes a healthy unrelated condition.
func (r *Rules) ValidationConditions(object *unstructured.Unstructured, types ...string) Readiness {
	if !r.valid() {
		return invalidRulesReadiness()
	}
	if object == nil {
		return result("RESOURCE_UNAVAILABLE", "Selected resource was not observed.")
	}
	if deleting := deletionReadiness(object); deleting != nil {
		return *deleting
	}
	generation, ok := metadataGeneration(object)
	if !ok {
		return result("RESOURCE_GENERATION_MISSING", "Current generation is missing.")
	}
	observed, found, err := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
	if err != nil || !found || observed != generation {
		return result("RESOURCE_STATUS_INVALID", "Controller status does not match the current generation.")
	}
	for _, kind := range types {
		if check := conditionAtGeneration(object, kind, generation, true); !check.Ready {
			return check
		}
	}
	return ready("Every required controller condition is true at the current generation.")
}
