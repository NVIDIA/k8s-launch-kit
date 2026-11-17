// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package controllercontract

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Rules owns selected controller semantics and the existing immutable schema owner.
// Selection does not establish live ownership, freshness, or permission for effects.
type Rules struct{ schemas *generation.ObjectSchemas }

// Select requires validated retained identity; catalog discovery support alone is insufficient.
func Select(release, profile, renderer string, schemas *generation.ObjectSchemas) (*Rules, error) {
	if release != "26.8.0" || profile != "hbn-bgp" || renderer != "native-dpf-generation/v1" {
		return nil, fmt.Errorf("unsupported retained controller contract: release, profile or renderer is not supported")
	}
	if schemas == nil {
		return nil, fmt.Errorf("selected operator schemas are required")
	}
	return &Rules{schemas: schemas}, nil
}
func (r *Rules) valid() bool { return r != nil && r.schemas != nil }
func invalidRulesReadiness() Readiness {
	return result("CONTROLLER_RULES_UNAVAILABLE", "Selected controller rules are required.")
}

// Validate rejects missing or unselected rules before access or effects.
func (r *Rules) Validate() error {
	if !r.valid() {
		return fmt.Errorf("selected controller rules are required")
	}
	return nil
}
func (r *Rules) DefaultedCopy(object *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r.schemas.DefaultedCopy(object)
}

// NodeEffectRemoved requires the pinned current-generation removal condition.
func (r *Rules) NodeEffectRemoved(object *unstructured.Unstructured) Readiness {
	if !r.valid() {
		return invalidRulesReadiness()
	}
	if object == nil {
		return result("RESOURCE_UNAVAILABLE", "No resource observation was provided.")
	}
	return conditionReadiness(object, "NodeEffectRemoved", "DPU_STATUS_STALE", true)
}

// ValidDPUDetails preserves the live fence's absent-or-exact-False predicate.
func (r *Rules) ValidDPUDetails(object *unstructured.Unstructured) bool {
	if !r.valid() || object == nil {
		return false
	}
	condition, check := findCondition(object, "InvalidDPUDetails")
	return check.Ready && (condition == nil || condition.status == "False")
}
