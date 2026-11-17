// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"fmt"
	"slices"
	"testing"
)

func TestNVUENeighborAddressFamilyOverrides(t *testing.T) {
	for _, test := range []struct {
		name, group, state    string
		wantFamily, wantIssue bool
	}{
		{name: "expanded no group", group: "none", state: "enabled", wantFamily: true},
		{name: "neighbor disables inherited family", group: "fabric", state: "disabled"},
		{name: "neighbor enables inherited family", group: "fabric", state: "enabled", wantFamily: true},
		{name: "unknown group stays unresolved", group: "missing", state: "enabled", wantFamily: true, wantIssue: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := validInputs()
			input.AppliedVRFs = []byte(fmt.Sprintf(`{"blue":{"router":{"bgp":{"state":"enabled","autonomous-system":65000,"address-family":{"ipv4-unicast":{"state":"enabled"}},"peer-group":{"fabric":{"remote-as":"external","address-family":{"ipv4-unicast":{"state":"enabled"}}}},"neighbor":{"swp1":{"peer-group":%q,"remote-as":65101,"type":"unnumbered","address-family":{"ipv4-unicast":{"state":%q}}}}}}}}`, test.group, test.state))
			snapshot := ParseNVUE(input)
			if len(snapshot.Configured.VRFs) != 1 || snapshot.Configured.VRFs[0].Neighbors == nil || len(*snapshot.Configured.VRFs[0].Neighbors) != 1 {
				t.Fatalf("missing parsed neighbor: %#v", snapshot)
			}
			neighbor := (*snapshot.Configured.VRFs[0].Neighbors)[0]
			if got := slices.Contains(neighbor.EffectiveAddressFamilies, "ipv4-unicast"); got != test.wantFamily {
				t.Fatalf("IPv4-unicast enabled=%v, want %v", got, test.wantFamily)
			}
			if test.wantIssue != (len(snapshot.Issues) != 0) {
				t.Fatalf("unexpected issues: %#v", snapshot.Issues)
			}
			if !test.wantIssue && (neighbor.EffectiveRemoteAS == nil || *neighbor.EffectiveRemoteAS != "65101") {
				t.Fatalf("explicit remote ASN was lost: %#v", neighbor)
			}
		})
	}
}
