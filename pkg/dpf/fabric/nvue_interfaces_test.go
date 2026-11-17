// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"strings"
	"testing"
)

func selectedInterfaceInputs() NVUEInputs {
	in := validInputs()
	in.AppliedInterfaces = []byte(`{"swp1":{"type":"swp","link":{"mtu":9216}}}`)
	in.OperationalInterfaces = []byte(`{"swp1":{"type":"swp","link":{"mtu":9216,"admin-status":"up","oper-status":"up"}}}`)
	in.SelectedAppliedInterfaces = map[string][]byte{"swp1": []byte(`{"type":"swp","vrf":"blue","link":{"mtu":9216}}`)}
	in.SelectedOperationalInterfaces = map[string][]byte{"swp1": []byte(`{"type":"swp","vrf":"blue","parent":"blue","link":{"mtu":9216,"admin-status":"up","oper-status":"up"}}`)}
	return in
}

func TestSelectedInterfaceDetailsEnrichSparseLists(t *testing.T) {
	snapshot := ParseNVUE(selectedInterfaceInputs())
	if len(snapshot.Issues) != 0 {
		t.Fatalf("unexpected issues: %+v", snapshot.Issues)
	}
	configured, operational := snapshot.Configured.Interfaces[0], snapshot.Operational.Interfaces[0]
	if configured.VRF == nil || *configured.VRF != "blue" || operational.VRF == nil || *operational.VRF != "blue" || operational.Parent == nil || *operational.Parent != "blue" {
		t.Fatalf("selected details were lost: %+v %+v", configured, operational)
	}
	if configured.BridgeDomain != nil {
		t.Fatal("missing bridge field must not become an explicit unbridged assertion")
	}
}

func TestSelectedInterfaceDetailsNeverHideConflictsOrMissingSources(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*NVUEInputs)
		code   string
	}{
		{"changed applied MTU", func(in *NVUEInputs) {
			in.SelectedAppliedInterfaces["swp1"] = []byte(`{"type":"swp","link":{"mtu":9000}}`)
		}, "conflicting_observation"},
		{"changed operational MTU", func(in *NVUEInputs) {
			in.SelectedOperationalInterfaces["swp1"] = []byte(strings.ReplaceAll(string(in.SelectedOperationalInterfaces["swp1"]), "9216", "9000"))
		}, "conflicting_observation"},
		{"absent list entry", func(in *NVUEInputs) { in.AppliedInterfaces = []byte(`{}`) }, "interface_not_listed"},
		{"failed command", func(in *NVUEInputs) { in.SelectedAppliedInterfaces["swp1"] = nil }, "missing_output"},
		{"malformed detail", func(in *NVUEInputs) { in.SelectedOperationalInterfaces["swp1"] = []byte(`[]`) }, "malformed_json"},
		{"malformed parent", func(in *NVUEInputs) { in.SelectedOperationalInterfaces["swp1"] = []byte(`{"parent":false}`) }, "invalid_type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := selectedInterfaceInputs()
			tc.change(&in)
			found := false
			for _, issue := range ParseNVUE(in).Issues {
				if issue.Code == tc.code && (strings.HasPrefix(issue.Input, "appliedInterface/") || strings.HasPrefix(issue.Input, "operationalInterface/")) {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing selected-source issue %s", tc.code)
			}
		})
	}
}
