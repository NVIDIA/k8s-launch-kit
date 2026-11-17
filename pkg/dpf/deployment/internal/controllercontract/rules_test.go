// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package controllercontract

import (
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
)

func testRules(t *testing.T) *Rules {
	t.Helper()
	r, err := Select("26.8.0", "hbn-bgp", "native-dpf-generation/v1", &generation.ObjectSchemas{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSelectionRejectsUnsupportedIdentityAndMissingSchemas(t *testing.T) {
	for _, identity := range []struct{ release, profile, renderer string }{
		{"26.4.0", "hbn-bgp", "native-dpf-generation/v1"},
		{"26.8.1", "hbn-bgp", "native-dpf-generation/v1"},
		{"26.8.0", "other", "native-dpf-generation/v1"},
		{"26.8.0", "hbn-bgp", "native-dpf-generation/v2"},
		{"", "hbn-bgp", "native-dpf-generation/v1"},
	} {
		if r, err := Select(identity.release, identity.profile, identity.renderer, &generation.ObjectSchemas{}); err == nil || r != nil {
			t.Fatalf("unsupported identity selected: %+v", identity)
		}
	}
	if r, err := Select("26.8.0", "hbn-bgp", "native-dpf-generation/v1", nil); err == nil || r != nil {
		t.Fatal("missing schemas selected")
	}
	for _, r := range []*Rules{nil, {}} {
		if r.Validate() == nil || r.ResourceReadiness(nil).Ready || r.EnrolledDeviceReadiness(nil).Ready || r.EnrolledNodeReadiness(nil).Ready || r.NodeEffectRemoved(nil).Ready || r.ValidDPUDetails(nil) || r.WorkloadReadiness(nil, WorkloadTemplate).Ready {
			t.Fatal("unselected rules accepted status")
		}
		if _, err := r.DefaultedCopy(nil); err == nil {
			t.Fatal("unselected rules defaulted a resource")
		}
		if _, _, err := r.ComparisonCopies(nil, nil); err == nil {
			t.Fatal("unselected rules normalized resources")
		}
		if _, err := r.HBNServiceSpec(nil, nil, "", "", "", "", nil, nil, nil); err == nil {
			t.Fatal("unselected rules projected Service")
		}
		if _, err := r.HBNServiceInterface("", "", "", "", nil, nil); err == nil {
			t.Fatal("unselected rules projected ServiceInterface")
		}
		if _, err := r.HBNApplicationValues(nil, nil); err == nil {
			t.Fatal("unselected rules projected App values")
		}
		if _, err := r.HBNNetworkAnnotation("", nil); err == nil {
			t.Fatal("unselected rules projected annotation")
		}
	}
}

func TestLiveConditionProjectionsPreserveExactPredicates(t *testing.T) {
	rules := testRules(t)
	for _, tc := range []struct {
		name, status string
		generation   int64
		ready        bool
	}{
		{"current", "True", 2, true}, {"stale", "True", 1, false}, {"future", "True", 3, false}, {"false", "False", 2, false}, {"unknown", "Unknown", 2, false},
	} {
		t.Run("removal-"+tc.name, func(t *testing.T) {
			object := dpfObject("DPU", map[string]any{"conditions": []any{conditionMap("NodeEffectRemoved", tc.status, "Success", tc.generation)}})
			if rules.NodeEffectRemoved(object).Ready != tc.ready {
				t.Fatal("removal generation/status predicate changed")
			}
		})
	}
	for _, tc := range []struct {
		name       string
		conditions any
		valid      bool
	}{
		{"absent", []any{}, true},
		{"false", []any{map[string]any{"type": "InvalidDPUDetails", "status": "False"}}, true},
		{"true", []any{map[string]any{"type": "InvalidDPUDetails", "status": "True"}}, false},
		{"unknown", []any{map[string]any{"type": "InvalidDPUDetails", "status": "Unknown"}}, false},
		{"malformed", "invalid", false},
		{"duplicate", []any{map[string]any{"type": "InvalidDPUDetails", "status": "False"}, map[string]any{"type": "InvalidDPUDetails", "status": "False"}}, false},
	} {
		t.Run("details-"+tc.name, func(t *testing.T) {
			object := dpfObject("DPUNode", map[string]any{"conditions": tc.conditions})
			if rules.ValidDPUDetails(object) != tc.valid {
				t.Fatal("details absent-or-False predicate changed")
			}
		})
	}
}
