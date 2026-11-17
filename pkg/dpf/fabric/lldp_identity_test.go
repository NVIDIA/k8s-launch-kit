// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"strings"
	"testing"
)

func TestLocalLLDPBindsTheAuthenticatedSwitchIdentity(t *testing.T) {
	valid := `{"local-chassis":[{"chassis":[{"id":[{"type":"mac","value":"02:AA:00:00:01:01"}],"name":[{"value":"leaf-a"}]}]}]}`
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"valid", valid, true},
		{"empty", `{}`, false},
		{"null", `null`, false},
		{"wrong type", `[]`, false},
		{"unsupported chassis subtype", strings.Replace(valid, `"mac"`, `"local"`, 1), false},
		{"multicast chassis", strings.Replace(valid, "02:AA", "03:AA", 1), false},
		{"empty name", strings.Replace(valid, "leaf-a", "", 1), false},
		{"ambiguous identity", strings.Replace(valid, `"name":[`, `"name":[{"value":"leaf-b"},`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := validInputs()
			in.LocalLLDP = []byte(tc.raw)
			result := ParseNVUE(in)
			if tc.valid {
				if len(result.Issues) != 0 || result.Operational.LocalLLDP == nil || result.Operational.LocalLLDP.ChassisID != "02:aa:00:00:01:01" || result.Operational.LocalLLDP.SystemName != "leaf-a" {
					t.Fatalf("identity not retained: %+v", result)
				}
			} else if result.Operational.LocalLLDP != nil || len(result.Issues) == 0 {
				t.Fatalf("invalid identity accepted: %+v", result)
			}
		})
	}
}
