// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package controllercontract

import "testing"

func TestConditionsCollectionDistinguishesMissingFromMalformed(t *testing.T) {
	for _, test := range []struct {
		name, requiredCode, optionalCode string
		present                          bool
		conditions                       any
	}{
		{name: "absent", requiredCode: "RESOURCE_CONDITIONS_MISSING"},
		{name: "empty list", present: true, conditions: []any{}},
		{name: "condition map", present: true, conditions: map[string]any{"Ready": "invalid"}, requiredCode: "RESOURCE_STATUS_INVALID", optionalCode: "RESOURCE_STATUS_INVALID"},
		{name: "scalar", present: true, conditions: "invalid", requiredCode: "RESOURCE_STATUS_INVALID", optionalCode: "RESOURCE_STATUS_INVALID"},
		{name: "scalar list entry", present: true, conditions: []any{"invalid"}, requiredCode: "RESOURCE_STATUS_INVALID", optionalCode: "RESOURCE_STATUS_INVALID"},
		{name: "invalid condition field", present: true, conditions: []any{map[string]any{"type": "Ready", "status": int64(1)}}, requiredCode: "RESOURCE_STATUS_INVALID", optionalCode: "RESOURCE_STATUS_INVALID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := map[string]any{}
			if test.present {
				status["conditions"] = test.conditions
			}
			object := dpfObject("DPUDevice", status)
			_, required := listConditions(object)
			_, optional := optionalConditions(object)
			if required.ReasonCode != test.requiredCode || optional.ReasonCode != test.optionalCode {
				t.Fatalf("required=%+v optional=%+v", required, optional)
			}
			if required.Ready != (test.requiredCode == "") || optional.Ready != (test.optionalCode == "") {
				t.Fatalf("invalid condition collection accepted: required=%+v optional=%+v", required, optional)
			}
		})
	}
}
