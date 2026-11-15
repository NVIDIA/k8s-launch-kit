// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func validInputs() NVUEInputs {
	return NVUEInputs{
		AppliedInterfaces:     []byte(`{"swp1":{"type":"swp","bridge":{"domain":{"br_default":{}}},"link":{"mtu":9216}}}`),
		AppliedVRFs:           []byte(`{"blue":{"router":{"bgp":{"state":"enabled","autonomous-system":65001,"address-family":{"ipv4-unicast":{"state":"enabled","redistribute":{"connected":{"state":"enabled","route-map":"connected-only"}}}},"neighbor":{"swp1":{"peer-group":"external","type":"unnumbered"}},"peer-group":{"external":{"remote-as":"external","address-family":{"ipv4-unicast":{"state":"enabled"}}}}}}}}`),
		AppliedRouter:         []byte(`{"bgp":{"state":"enabled","autonomous-system":"none"}}`),
		AppliedBridge:         []byte(`{"domain":{"br_default":{"encap":"802.1Q","port":{"swp1":{"vlan":{"100":{"tag-state":"tagged","fwd-state":"forwarding"}}}}}}}`),
		OperationalInterfaces: []byte(`{"swp1":{"type":"swp","vrf":"blue","link":{"admin-status":"up","oper-status":"up","mtu":9216,"speed":"100G"}}}`),
		OperationalBridge:     []byte(`{"domain":{"br_default":{"port":{"swp1":{"state":"forwarding","vlan":{"100":{"tag-state":"tagged","fwd-state":"forwarding"}}}}}}}`),
		OperationalAddresses:  []byte(`[]`),
		OperationalRoutesIPv4: []byte(`[]`),
		OperationalRoutesIPv6: []byte(`[]`),
		LLDP:                  []byte(`{"lldp":[{"interface":[{"name":"swp1","chassis":[{"id":[{"type":"mac","value":"02:00:00:00:00:01"}],"name":[{"value":"dpu-node-example"}]}],"port":[{"id":[{"type":"ifname","value":"p0"}],"name":[{"value":"p0"}]}]}]}]}`),
		FRRBGPSummary:         []byte(`{"blue":{"ipv4Unicast":{"routerId":"192.0.2.1","as":65001,"peers":{"swp1":{"remoteAs":65002,"state":"Established","prefixReceived":3,"prefixAccepted":2,"prefixSent":1}}}},"empty":{"ipv4Unicast":{"routerId":"192.0.2.2","as":65003,"peers":{}}}}`),
	}
}

func TestParseNVUESeparatesConfiguredAndOperationalFacts(t *testing.T) {
	in := validInputs()
	in.AppliedInterfaces = []byte(strings.Replace(string(in.AppliedInterfaces), `"mtu":9216`, `"mtu":9216,"futureNvueField":{"new":true}`, 1))
	snapshot := ParseNVUE(in)
	if len(snapshot.Issues) != 0 {
		t.Fatalf("unexpected parse issues: %#v", snapshot.Issues)
	}
	if snapshot.Configured.BGP == nil || snapshot.Configured.BGP.Enabled == nil || !*snapshot.Configured.BGP.Enabled || snapshot.Configured.BGP.ASN != nil || snapshot.Configured.BGP.ASNConfigured == nil || *snapshot.Configured.BGP.ASNConfigured {
		t.Fatalf("global BGP configured state not parsed: %#v", snapshot.Configured.BGP)
	}
	if len(snapshot.Configured.VRFs) != 1 {
		t.Fatalf("expected one configured VRF, got %#v", snapshot.Configured.VRFs)
	}
	vrf := snapshot.Configured.VRFs[0]
	if vrf.Name != "blue" || vrf.BGPEnabled == nil || !*vrf.BGPEnabled || vrf.ASN == nil || *vrf.ASN != 65001 {
		t.Fatalf("configured VRF facts incorrect: %#v", vrf)
	}
	if len(vrf.AddressFamilies) != 1 || vrf.AddressFamilies[0].Enabled == nil || !*vrf.AddressFamilies[0].Enabled {
		t.Fatalf("configured address family missing: %#v", vrf.AddressFamilies)
	}
	redist := vrf.AddressFamilies[0].Redistribute
	if len(redist) != 1 || redist[0].Enabled == nil || !*redist[0].Enabled || redist[0].RouteMap == nil || *redist[0].RouteMap != "connected-only" {
		t.Fatalf("redistribution config incorrect: %#v", redist)
	}
	if len(vrf.PeerGroups) != 1 || vrf.PeerGroups[0].RemoteAS == nil || *vrf.PeerGroups[0].RemoteAS != "external" {
		t.Fatalf("peer group was not preserved: %#v", vrf.PeerGroups)
	}
	if vrf.Neighbors == nil || len(*vrf.Neighbors) != 1 {
		t.Fatalf("configured neighbor missing: %#v", vrf.Neighbors)
	}
	neighbor := (*vrf.Neighbors)[0]
	if neighbor.PeerGroup == nil || *neighbor.PeerGroup != "external" || neighbor.Type == nil || *neighbor.Type != "unnumbered" || neighbor.EffectiveRemoteAS == nil || *neighbor.EffectiveRemoteAS != "external" || len(neighbor.EffectiveAddressFamilies) != 1 || neighbor.EffectiveAddressFamilies[0] != "ipv4-unicast" {
		t.Fatalf("peer-group inheritance was not resolved: %#v", neighbor)
	}
	configuredPort := findConfiguredInterface(snapshot.Configured.Interfaces, "swp1")
	if configuredPort == nil || configuredPort.BridgeDomain == nil || *configuredPort.BridgeDomain != "br_default" || configuredPort.VRF != nil {
		t.Fatalf("bridge domain was confused with VRF membership: %#v", snapshot.Configured.Interfaces)
	}
	if len(snapshot.Configured.Bridges) != 1 || len(snapshot.Configured.Bridges[0].Ports) != 1 || snapshot.Configured.Bridges[0].Ports[0].VLANs[0].ID != 100 {
		t.Fatalf("configured bridge/VLAN facts incorrect: %#v", snapshot.Configured.Bridges)
	}
	if len(snapshot.Operational.Interfaces) != 1 {
		t.Fatalf("operational interface missing: %#v", snapshot.Operational.Interfaces)
	}
	link := snapshot.Operational.Interfaces[0]
	if link.AdminState == nil || *link.AdminState != "up" || link.OperState == nil || *link.OperState != "up" || link.MTU == nil || *link.MTU != 9216 || link.VRF == nil || *link.VRF != "blue" {
		t.Fatalf("operational link state incorrect: %#v", link)
	}
	if len(snapshot.Operational.LLDP) != 1 || snapshot.Operational.LLDP[0].ChassisName == nil || *snapshot.Operational.LLDP[0].ChassisName != "dpu-node-example" {
		t.Fatalf("LLDP hint was not parsed: %#v", snapshot.Operational.LLDP)
	}
	if len(snapshot.Operational.BGP) != 2 {
		t.Fatalf("expected two VRF summaries: %#v", snapshot.Operational.BGP)
	}
	if len(snapshot.Operational.BGP[0].Peers) != 1 {
		t.Fatalf("established peer missing: %#v", snapshot.Operational.BGP)
	}
	peer := snapshot.Operational.BGP[0].Peers[0]
	if peer.State == nil || *peer.State != "Established" || peer.RemoteAS == nil || *peer.RemoteAS != 65002 || peer.PrefixesAccepted == nil || *peer.PrefixesAccepted != 2 {
		t.Fatalf("BGP peer state incorrect: %#v", peer)
	}
	if got := snapshot.Operational.BGP[1].Peers; got == nil || len(got) != 0 {
		t.Fatalf("empty peer map must remain known-empty: %#v", got)
	}
}

func TestParseNVUENeighborOverrideKeepsInheritedAddressFamilies(t *testing.T) {
	in := validInputs()
	in.AppliedVRFs = []byte(strings.Replace(string(in.AppliedVRFs), `"peer-group":"external","type":"unnumbered"`, `"peer-group":"external","type":"unnumbered","remote-as":65002`, 1))
	snapshot := ParseNVUE(in)
	if len(snapshot.Configured.VRFs) != 1 || snapshot.Configured.VRFs[0].Neighbors == nil || len(*snapshot.Configured.VRFs[0].Neighbors) != 1 {
		t.Fatalf("configured neighbor missing: %#v", snapshot.Configured.VRFs)
	}
	neighbor := (*snapshot.Configured.VRFs[0].Neighbors)[0]
	if neighbor.EffectiveRemoteAS == nil || *neighbor.EffectiveRemoteAS != "65002" || len(neighbor.EffectiveAddressFamilies) != 1 || neighbor.EffectiveAddressFamilies[0] != "ipv4-unicast" {
		t.Fatalf("neighbor override did not preserve peer-group AF inheritance: %#v", neighbor)
	}
}

func TestParseNVUEEmptyNeighborMapDiffersFromMissingBGPBlock(t *testing.T) {
	in := validInputs()
	in.AppliedVRFs = []byte(`{"blue":{"router":{"bgp":{"state":"disabled","autonomous-system":65001,"address-family":{},"neighbor":{}}}},"green":{"router":{}}}`)
	snapshot := ParseNVUE(in)
	if len(snapshot.Configured.VRFs) != 2 {
		t.Fatalf("expected both VRFs, got %#v", snapshot.Configured.VRFs)
	}
	blue := snapshot.Configured.VRFs[0]
	if blue.Name != "blue" || blue.BGPEnabled == nil || *blue.BGPEnabled || blue.Neighbors == nil || len(*blue.Neighbors) != 0 {
		t.Fatalf("disabled BGP with known-empty neighbors was not preserved: %#v", blue)
	}
	green := snapshot.Configured.VRFs[1]
	if green.Name != "green" || green.BGPEnabled != nil || green.Neighbors != nil {
		t.Fatalf("missing BGP block was converted to disabled/empty: %#v", green)
	}
	if !hasIssue(snapshot.Issues, "appliedVRFs", "$[\"green\"].router.bgp", "not_configured") {
		t.Fatalf("missing BGP block has no field-specific issue: %#v", snapshot.Issues)
	}
}

func TestParseNVUEDisabledDefaultVRFsMayOmitInactiveBGPSubtrees(t *testing.T) {
	in := validInputs()
	in.AppliedVRFs = []byte(`{"default":{"router":{"bgp":{"state":"disabled"}}},"mgmt":{"router":{"bgp":{"state":"disabled"}}},"blue":{"router":{"bgp":{"state":"enabled","autonomous-system":65001,"address-family":{"ipv4-unicast":{"state":"enabled"}},"neighbor":{}}}}}`)
	snapshot := ParseNVUE(in)
	if len(snapshot.Issues) != 0 {
		t.Fatalf("disabled default VRFs with omitted inactive subtrees should parse cleanly: %#v", snapshot.Issues)
	}
	for _, name := range []string{"default", "mgmt"} {
		var found *VRFConfig
		for i := range snapshot.Configured.VRFs {
			if snapshot.Configured.VRFs[i].Name == name {
				found = &snapshot.Configured.VRFs[i]
				break
			}
		}
		if found == nil || found.BGPEnabled == nil || *found.BGPEnabled || found.ASN != nil || found.AddressFamilies != nil || found.Neighbors != nil {
			t.Errorf("disabled %s VRF must remain explicitly disabled without fabricated inactive facts: %#v", name, found)
		}
	}
	blue := snapshot.Configured.VRFs[0]
	if blue.Name != "blue" || blue.BGPEnabled == nil || !*blue.BGPEnabled || blue.ASN == nil || *blue.ASN != 65001 || blue.Neighbors == nil || len(*blue.Neighbors) != 0 {
		t.Fatalf("enabled selected VRF facts must still be parsed: %#v", snapshot.Configured.VRFs)
	}
}

func TestParseNVUEKeepsOperationalBridgeMembershipSeparateFromAppliedConfig(t *testing.T) {
	in := validInputs()
	in.AppliedInterfaces = []byte(`{"swp1s0":{"type":"swp","link":{"mtu":9216}},"swp1s1":{"type":"swp","link":{"mtu":9216}}}`)
	in.AppliedBridge = []byte(`{"domain":{}}`)
	in.OperationalBridge = []byte(`{"domain":{"br_default":{"port":{"swp1s0":{"state":"forwarding","vlan":{"1":{"tag-state":"untagged","fwd-state":"forwarding"}}},"swp1s1":{"state":"forwarding","vlan":{"1":{"tag-state":"untagged","fwd-state":"forwarding"}}}}}}}`)
	snapshot := ParseNVUE(in)
	if len(snapshot.Issues) != 0 {
		t.Fatalf("valid empty applied bridge map and operational membership produced issues: %#v", snapshot.Issues)
	}
	if len(snapshot.Configured.Bridges) != 0 {
		t.Fatalf("operational bridge membership must not be promoted to applied config: %#v", snapshot.Configured.Bridges)
	}
	if len(snapshot.Operational.Bridges) != 1 || len(snapshot.Operational.Bridges[0].Ports) != 2 {
		t.Fatalf("operational bridge membership was not retained: %#v", snapshot.Operational.Bridges)
	}
}

func TestParseNVUEIncompleteAndBadTypesRemainIssues(t *testing.T) {
	in := validInputs()
	in.AppliedVRFs = []byte(`{"blue":{"router":{"bgp":{"state":true,"autonomous-system":"bad","address-family":{"ipv4-unicast":{"state":true}},"neighbor":[]}}}}`)
	in.OperationalInterfaces = []byte(`{"swp1":{"type":"swp","link":{"admin-status":true,"oper-status":"up","mtu":"9216"}}}`)
	in.FRRBGPSummary = []byte(`{"blue":{"ipv4Unicast":{"peers":{"peer":{"state":1}}}}}`)
	snapshot := ParseNVUE(in)
	for _, want := range []struct{ input, field, code string }{
		{"appliedVRFs", "$[\"blue\"].router.bgp.state", "invalid_type"},
		{"appliedVRFs", "$[\"blue\"].router.bgp.autonomous-system", "invalid_value"},
		{"appliedVRFs", "$[\"blue\"].router.bgp.address-family.ipv4-unicast.state", "invalid_type"},
		{"appliedVRFs", "$[\"blue\"].router.bgp.neighbor", "invalid_type"},
		{"operationalInterfaces", "$[\"swp1\"].link.admin-status", "invalid_type"},
		{"operationalInterfaces", "$[\"swp1\"].link.mtu", "invalid_type"},
		{"frrBGPSummary", "$[\"blue\"].ipv4Unicast.peers.peer.state", "invalid_type"},
	} {
		if !hasIssue(snapshot.Issues, want.input, want.field, want.code) {
			t.Errorf("missing issue for %s %s: %#v", want.input, want.field, snapshot.Issues)
		}
	}
}

func TestParseNVUEMissingOutputAndMalformedJSONAreNotSuccess(t *testing.T) {
	snapshot := ParseNVUE(NVUEInputs{AppliedInterfaces: []byte("{"), FRRBGPSummary: []byte(`{"blue":[]}`)})
	if !hasIssue(snapshot.Issues, "appliedInterfaces", "$", "malformed_json") {
		t.Fatalf("malformed JSON was not reported: %#v", snapshot.Issues)
	}
	if !hasIssue(snapshot.Issues, "appliedVRFs", "$", "missing_output") {
		t.Fatalf("missing applied VRFs were not reported: %#v", snapshot.Issues)
	}
	if !hasIssue(snapshot.Issues, "frrBGPSummary", "$[\"blue\"]", "invalid_type") {
		t.Fatalf("invalid BGP summary block was not reported: %#v", snapshot.Issues)
	}
}

func TestParseNVUESnapshotIsStableAndHasYAMLTags(t *testing.T) {
	first, err := json.Marshal(ParseNVUE(validInputs()))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(ParseNVUE(validInputs()))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("snapshot JSON is nondeterministic:\n%s\n%s", first, second)
	}
	var rendered map[string]any
	if err := yaml.Unmarshal(first, &rendered); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(ParseNVUE(validInputs()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "configured:") || !strings.Contains(string(encoded), "operational:") {
		t.Fatalf("YAML tags not honored: %s", encoded)
	}
}

func hasIssue(issues []Issue, input, field, code string) bool {
	for _, issue := range issues {
		if issue.Input == input && issue.Field == field && issue.Code == code {
			return true
		}
	}
	return false
}

func findConfiguredInterface(interfaces []InterfaceConfig, name string) *InterfaceConfig {
	for i := range interfaces {
		if interfaces[i].Name == name {
			return &interfaces[i]
		}
	}
	return nil
}

func TestLLDPDescriptionAndAmbiguousPortIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, ports string
		valid       bool
	}{
		{"description", `[{"id":[{"value":"02:00:00:00:00:01"}],"descr":[{"value":"p0"}]}]`, true},
		{"multiple port ids", `[{"id":[{"value":"02:00:00:00:00:01"},{"value":"02:00:00:00:00:02"}],"descr":[{"value":"p0"}]}]`, false},
		{"multiple port descriptions", `[{"id":[{"value":"02:00:00:00:00:01"}],"descr":[{"value":"p0"},{"value":"p1"}]}]`, false},
		{"multiple port entries", `[{"id":[{"value":"02:00:00:00:00:01"}]},{"id":[{"value":"02:00:00:00:00:02"}]}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var snapshot Snapshot
			parseLLDP([]byte(`{"lldp":[{"interface":[{"name":"swp1","port":`+tc.ports+`}]}]}`), &snapshot)
			if tc.valid {
				if len(snapshot.Issues) != 0 || len(snapshot.Operational.LLDP) != 1 || snapshot.Operational.LLDP[0].PortDescription == nil || *snapshot.Operational.LLDP[0].PortDescription != "p0" {
					t.Fatalf("description lost: %#v", snapshot)
				}
			} else if len(snapshot.Issues) == 0 || len(snapshot.Operational.LLDP) != 0 {
				t.Fatalf("ambiguous peer survived: %#v", snapshot)
			}
		})
	}
}

func TestLLDPRequiresInterfaceCollection(t *testing.T) {
	for _, raw := range []string{`{"lldp":[{}]}`, `{"lldp":[{"interface":null}]}`} {
		var snapshot Snapshot
		parseLLDP([]byte(raw), &snapshot)
		if len(snapshot.Issues) != 1 || snapshot.Issues[0].Code != "missing_field" {
			t.Fatalf("missing LLDP source accepted: %#v", snapshot)
		}
	}
	var empty Snapshot
	parseLLDP([]byte(`{"lldp":[{"interface":[]}]}`), &empty)
	if len(empty.Issues) != 0 {
		t.Fatalf("known empty LLDP list rejected: %#v", empty)
	}
}
