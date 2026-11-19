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
	if err != nil || (found && (observed < 0 || observed > generation)) {
		return result("RESOURCE_STATUS_INVALID", "Controller observed generation is malformed, negative or ahead of the current generation.")
	}
	if !found || observed < generation {
		return result("RESOURCE_STATUS_STALE", "Controller has not observed the current generation.")
	}
	for _, kind := range types {
		if check := conditionAtGeneration(object, kind, generation, true); !check.Ready {
			return check
		}
	}
	return ready("Every required controller condition is true at the current generation.")
}

// SystemCredentialReadiness uses the credential API's per-condition generation
// contract; that API has no top-level observedGeneration status member.
func (r *Rules) SystemCredentialReadiness(object *unstructured.Unstructured) Readiness {
	if !r.valid() {
		return invalidRulesReadiness()
	}
	if object == nil {
		return result("RESOURCE_UNAVAILABLE", "Credential request is unavailable.")
	}
	generation, ok := metadataGeneration(object)
	if !ok {
		return result("RESOURCE_GENERATION_MISSING", "Credential request generation is invalid.")
	}
	if deleting := deletionReadiness(object); deleting != nil {
		return *deleting
	}
	for _, kind := range []string{"Ready", "ServiceAccountReconciled", "SecretReconciled"} {
		if check := conditionAtGeneration(object, kind, generation, true); !check.Ready {
			return check
		}
	}
	return ready("Credential request conditions are current and ready.")
}
