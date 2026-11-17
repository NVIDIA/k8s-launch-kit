// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const cleanFRRConfig = `frr defaults datacenter
router bgp 65000 vrf hbn
 neighbor fabric peer-group
 neighbor fabric remote-as external
 neighbor swp1 interface peer-group fabric
 address-family ipv4 unicast
  neighbor fabric activate
 exit-address-family
!
router bgp 65001 vrf unrelated
 neighbor other peer-group
 neighbor swp99 interface peer-group other
 address-family ipv4 unicast
  neighbor other activate
 exit-address-family
!
`

func parseRunningConfig(text string) Snapshot {
	snapshot := Snapshot{}
	parseFRRRunningBGP([]byte(text), &snapshot)
	return snapshot
}

func policyVRF(t *testing.T, snapshot Snapshot, name string) FRRPolicyVRF {
	t.Helper()
	if snapshot.Configured.FRRPolicy == nil {
		t.Fatalf("FRR policy was not parsed: %#v", snapshot.Issues)
	}
	for _, vrf := range snapshot.Configured.FRRPolicy.VRFs {
		if vrf.Name == name {
			return vrf
		}
	}
	t.Fatalf("VRF %q not found in parsed policy: %#v", name, snapshot.Configured.FRRPolicy.VRFs)
	return FRRPolicyVRF{}
}

func TestParseFRRPolicyPreservesKnownUnfilteredPeerAndDatacenterMode(t *testing.T) {
	snapshot := parseRunningConfig(cleanFRRConfig)
	if len(snapshot.Issues) != 0 {
		t.Fatalf("unexpected parser issues: %#v", snapshot.Issues)
	}
	policy := snapshot.Configured.FRRPolicy
	if policy == nil || !policy.Complete || policy.Defaults == nil || *policy.Defaults != "datacenter" || policy.EBGPRequiresPolicy == nil || *policy.EBGPRequiresPolicy {
		t.Fatalf("datacenter default was not established: %#v", policy)
	}
	vrf := policyVRF(t, snapshot, "hbn")
	if !vrf.Complete || vrf.LocalASN == nil || *vrf.LocalASN != 65000 || len(vrf.Neighbors) != 1 || len(vrf.PeerGroups) != 1 {
		t.Fatalf("selected neighbor and group were not represented: %#v", vrf)
	}
	peer := vrf.Neighbors[0]
	if peer.Name != "swp1" || peer.PeerGroup == nil || *peer.PeerGroup != "fabric" || !peer.Interface || peer.InboundFilter || peer.OutboundFilter || peer.UnsupportedPolicy {
		t.Fatalf("unfiltered selected peer facts are incorrect: %#v", peer)
	}
	if vrf.PeerGroups[0].Name != "fabric" || vrf.PeerGroups[0].InboundFilter || vrf.PeerGroups[0].OutboundFilter {
		t.Fatalf("unfiltered peer group facts are incorrect: %#v", vrf.PeerGroups)
	}
	if vrf.PeerGroups[0].RemoteAS == nil || *vrf.PeerGroups[0].RemoteAS != "external" {
		t.Fatalf("peer-group remote ASN was not retained: %#v", vrf.PeerGroups[0])
	}
}

func TestParseFRRPolicyAllowsBoundedBaselineSettingsAndTracksUnnumberedRemoteAS(t *testing.T) {
	text := strings.Replace(cleanFRRConfig, " neighbor fabric remote-as external", " neighbor fabric remote-as external\n neighbor fabric description fabric-peer\n neighbor swp1 advertisement-interval 0\n neighbor swp1 timers 3 9\n neighbor swp1 timers connect 10", 1)
	text = strings.Replace(text, " neighbor swp1 interface peer-group fabric", " neighbor swp1 interface peer-group fabric\n neighbor swp1 remote-as 65101", 1)
	vrf := policyVRF(t, parseRunningConfig(text), "hbn")
	if !vrf.Complete || len(vrf.Neighbors) != 1 || vrf.Neighbors[0].RemoteAS == nil || *vrf.Neighbors[0].RemoteAS != "65101" || vrf.Neighbors[0].UnsupportedPolicy || len(vrf.Neighbors[0].UnsupportedSettings) != 0 {
		t.Fatalf("bounded baseline settings or direct remote ASN were not parsed: %#v", vrf)
	}

	interfaceRemoteAS := strings.Replace(cleanFRRConfig, " neighbor swp1 interface peer-group fabric", " neighbor swp1 interface remote-as 65101", 1)
	vrf = policyVRF(t, parseRunningConfig(interfaceRemoteAS), "hbn")
	if !vrf.Complete || len(vrf.Neighbors) != 1 || !vrf.Neighbors[0].Interface || vrf.Neighbors[0].PeerGroup != nil || vrf.Neighbors[0].RemoteAS == nil || *vrf.Neighbors[0].RemoteAS != "65101" {
		t.Fatalf("direct unnumbered remote-AS syntax was not represented: %#v", vrf)
	}
}

func TestParseFRRPolicyScopesUnknownPeerSettingsWithoutRetainingArguments(t *testing.T) {
	text := strings.Replace(cleanFRRConfig, " neighbor swp1 interface peer-group fabric", " neighbor swp1 interface peer-group fabric\n neighbor swp1 password highly-sensitive\n neighbor swp1 local-as 65055", 1)
	text = strings.Replace(text, " neighbor swp99 interface peer-group other", " neighbor swp99 interface peer-group other\n neighbor swp99 update-source loopback-sensitive", 1)
	snapshot := parseRunningConfig(text)
	if snapshot.Configured.FRRPolicy == nil || !snapshot.Configured.FRRPolicy.Complete {
		t.Fatalf("scoped unknown peer settings invalidated the full FRR source: %#v", snapshot.Issues)
	}
	hbnVRF := policyVRF(t, snapshot, "hbn")
	otherVRF := policyVRF(t, snapshot, "unrelated")
	if !hbnVRF.Complete || !hbnVRF.Neighbors[0].UnsupportedPolicy || len(hbnVRF.Neighbors[0].UnsupportedSettings) != 2 || !otherVRF.Complete || !otherVRF.Neighbors[0].UnsupportedPolicy || len(otherVRF.Neighbors[0].UnsupportedSettings) != 1 {
		t.Fatalf("unknown peer settings were not scoped to their peer: hbn=%#v unrelated=%#v", hbnVRF, otherVRF)
	}
	encoded, err := json.Marshal(snapshot.Configured.FRRPolicy)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"highly-sensitive", "65055", "loopback-sensitive"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("unsupported peer argument leaked into typed facts: %s", encoded)
		}
	}
}

func TestParseFRRPolicyResolvesDirectAndInheritedFiltersWithoutRetainingNames(t *testing.T) {
	cases := []struct {
		name string
		line string
		peer bool
		in   bool
		out  bool
	}{
		{name: "inherited inbound", line: "  neighbor fabric route-map route-map-secret in", in: true},
		{name: "inherited outbound", line: "  neighbor fabric prefix-list prefix-secret out", out: true},
		{name: "direct inbound", line: "  neighbor swp1 distribute-list filter-secret in", peer: true, in: true},
		{name: "direct outbound", line: "  neighbor swp1 filter-list filter-secret out", peer: true, out: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.Replace(cleanFRRConfig, " exit-address-family", tc.line+"\n exit-address-family", 1)
			snapshot := parseRunningConfig(input)
			vrf := policyVRF(t, snapshot, "hbn")
			if !vrf.Complete {
				t.Fatalf("valid filter directive made VRF incomplete: %#v", snapshot.Issues)
			}
			if tc.peer {
				if len(vrf.Neighbors) != 1 || vrf.Neighbors[0].InboundFilter != tc.in || vrf.Neighbors[0].OutboundFilter != tc.out {
					t.Fatalf("direct filter facts incorrect: %#v", vrf.Neighbors)
				}
			} else if len(vrf.PeerGroups) != 1 || vrf.PeerGroups[0].InboundFilter != tc.in || vrf.PeerGroups[0].OutboundFilter != tc.out {
				t.Fatalf("inherited filter facts incorrect: %#v", vrf.PeerGroups)
			}
			encoded, err := json.Marshal(snapshot.Configured.FRRPolicy)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"route-map-secret", "prefix-secret", "filter-secret"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("raw policy name leaked into typed facts: %s", encoded)
				}
			}
		})
	}
}

func TestParseFRRPolicyScopesMalformedNeighborPolicyToItsVRF(t *testing.T) {
	input := strings.Replace(cleanFRRConfig, "router bgp 65001 vrf unrelated\n neighbor other peer-group", "router bgp 65001 vrf unrelated\n neighbor other peer-group\n neighbor swp99 route-map missing-direction", 1)
	snapshot := parseRunningConfig(input)
	if snapshot.Configured.FRRPolicy == nil || !snapshot.Configured.FRRPolicy.Complete {
		t.Fatalf("well-scoped neighbor syntax issue invalidated the complete source: %#v", snapshot.Issues)
	}
	if !policyVRF(t, snapshot, "hbn").Complete {
		t.Fatalf("unrelated VRF made selected HBN policy incomplete: %#v", snapshot.Configured.FRRPolicy)
	}
	other := policyVRF(t, snapshot, "unrelated")
	if !other.Complete || len(other.Neighbors) != 1 || !other.Neighbors[0].UnsupportedPolicy {
		t.Fatalf("malformed unrelated neighbor command was not scoped as unsupported: %#v", other)
	}
}

func TestParseFRRPolicyModeOverridesAndMissingModeRemainExplicit(t *testing.T) {
	input := strings.Replace(cleanFRRConfig, " neighbor fabric peer-group", " bgp ebgp-requires-policy\n neighbor fabric peer-group", 1)
	snapshot := parseRunningConfig(input)
	vrf := policyVRF(t, snapshot, "hbn")
	if vrf.EBGPRequiresPolicy == nil || !*vrf.EBGPRequiresPolicy {
		t.Fatalf("per-VRF policy requirement override missing: %#v", vrf)
	}

	traditional := strings.Replace(cleanFRRConfig, "datacenter", "traditional", 1)
	snapshot = parseRunningConfig(traditional)
	if snapshot.Configured.FRRPolicy == nil || !snapshot.Configured.FRRPolicy.Complete || snapshot.Configured.FRRPolicy.EBGPRequiresPolicy != nil {
		t.Fatalf("unsupported implicit traditional default was fabricated: %#v", snapshot.Configured.FRRPolicy)
	}

	noDefaults := strings.TrimPrefix(cleanFRRConfig, "frr defaults datacenter\n")
	snapshot = parseRunningConfig(noDefaults)
	if snapshot.Configured.FRRPolicy == nil || snapshot.Configured.FRRPolicy.Complete || !hasIssue(snapshot.Issues, "frrRunningBGP", "$.defaults", "missing_field") {
		t.Fatalf("missing defaults mode was treated as complete: %#v", snapshot)
	}
}

func TestParseFRRPolicyIgnoresOtherAddressFamilyFiltersAndMarksUnknownFamily(t *testing.T) {
	ipv6 := strings.Replace(cleanFRRConfig, " address-family ipv4 unicast", " address-family ipv6 unicast", 1)
	ipv6 = strings.Replace(ipv6, "  neighbor fabric activate", "  neighbor fabric route-map ipv6-map in", 1)
	vrf := policyVRF(t, parseRunningConfig(ipv6), "hbn")
	if !vrf.Complete || vrf.PeerGroups[0].InboundFilter {
		t.Fatalf("IPv6-only filter was incorrectly applied to selected IPv4-unicast policy: %#v", vrf)
	}

	unsupported := strings.Replace(cleanFRRConfig, " address-family ipv4 unicast", " address-family custom family", 1)
	vrf = policyVRF(t, parseRunningConfig(unsupported), "hbn")
	if vrf.Complete {
		t.Fatalf("unknown selected address family was silently ignored: %#v", vrf)
	}
}

func TestParseFRRPolicyTracksIPv4ActivationDefaultsAndOverrides(t *testing.T) {
	defaultEnabled := parseRunningConfig(cleanFRRConfig)
	vrf := policyVRF(t, defaultEnabled, "hbn")
	if vrf.IPv4UnicastDefault == nil || !*vrf.IPv4UnicastDefault || vrf.PeerGroups[0].IPv4UnicastActivated == nil || !*vrf.PeerGroups[0].IPv4UnicastActivated {
		t.Fatalf("documented IPv4 default or explicit group activation missing: %#v", vrf)
	}

	globalDisabled := strings.Replace(cleanFRRConfig, " neighbor fabric peer-group", " no bgp default ipv4-unicast\n neighbor fabric peer-group", 1)
	vrf = policyVRF(t, parseRunningConfig(globalDisabled), "hbn")
	if vrf.IPv4UnicastDefault == nil || *vrf.IPv4UnicastDefault {
		t.Fatalf("explicitly disabled IPv4 default was not retained: %#v", vrf)
	}

	directDisable := strings.Replace(cleanFRRConfig, "  neighbor fabric activate", "  neighbor fabric activate\n  no neighbor swp1 activate", 1)
	vrf = policyVRF(t, parseRunningConfig(directDisable), "hbn")
	if vrf.Neighbors[0].IPv4UnicastActivated == nil || *vrf.Neighbors[0].IPv4UnicastActivated {
		t.Fatalf("direct peer disable was not retained: %#v", vrf.Neighbors[0])
	}

	groupDisableDirectEnable := strings.Replace(cleanFRRConfig, "  neighbor fabric activate", "  no neighbor fabric activate\n  neighbor swp1 activate", 1)
	vrf = policyVRF(t, parseRunningConfig(groupDisableDirectEnable), "hbn")
	if vrf.PeerGroups[0].IPv4UnicastActivated == nil || *vrf.PeerGroups[0].IPv4UnicastActivated || vrf.Neighbors[0].IPv4UnicastActivated == nil || !*vrf.Neighbors[0].IPv4UnicastActivated {
		t.Fatalf("direct peer activation did not override group disable: %#v", vrf)
	}
}

func TestParseFRRPolicyTracksVRFAndPeerShutdownOverrides(t *testing.T) {
	bgpShutdown := strings.Replace(cleanFRRConfig, " neighbor fabric peer-group", " bgp shutdown\n neighbor fabric peer-group", 1)
	vrf := policyVRF(t, parseRunningConfig(bgpShutdown), "hbn")
	if vrf.BGPShutdown == nil || !*vrf.BGPShutdown {
		t.Fatalf("VRF BGP shutdown was not retained: %#v", vrf)
	}

	groupShutdown := strings.Replace(cleanFRRConfig, " neighbor fabric peer-group", " neighbor fabric peer-group\n neighbor fabric shutdown", 1)
	vrf = policyVRF(t, parseRunningConfig(groupShutdown), "hbn")
	if vrf.PeerGroups[0].Shutdown == nil || !*vrf.PeerGroups[0].Shutdown {
		t.Fatalf("peer-group shutdown was not retained: %#v", vrf.PeerGroups[0])
	}

	directEnable := strings.Replace(groupShutdown, " neighbor swp1 interface peer-group fabric", " neighbor swp1 interface peer-group fabric\n no neighbor swp1 shutdown", 1)
	vrf = policyVRF(t, parseRunningConfig(directEnable), "hbn")
	if vrf.Neighbors[0].Shutdown == nil || *vrf.Neighbors[0].Shutdown {
		t.Fatalf("direct peer no-shutdown override was not retained: %#v", vrf.Neighbors[0])
	}
}

func TestParseFRRPolicyRejectsMalformedAndOversizedSource(t *testing.T) {
	malformed := strings.Replace(cleanFRRConfig, "bgp 65000", "bgp 65000", 1)
	malformed = strings.Replace(malformed, "frr defaults datacenter", "frr defaults unknown", 1)
	if snapshot := parseRunningConfig(malformed); snapshot.Configured.FRRPolicy == nil || snapshot.Configured.FRRPolicy.Complete || !hasIssue(snapshot.Issues, "frrRunningBGP", "$", "unsupported_defaults") {
		t.Fatalf("unsupported FRR mode was accepted: %#v", snapshot)
	}
	if snapshot := parseRunningConfig(strings.Repeat("!\n", maxFRRConfigLines+1)); snapshot.Configured.FRRPolicy == nil || snapshot.Configured.FRRPolicy.Complete {
		t.Fatalf("excessive FRR source line count was accepted: %#v", snapshot.Configured.FRRPolicy)
	}
	tooLarge := Snapshot{}
	parseFRRRunningBGP(bytes.Repeat([]byte("x"), maxFRRConfigBytes+1), &tooLarge)
	if tooLarge.Configured.FRRPolicy != nil || !hasIssue(tooLarge.Issues, "frrRunningBGP", "$", "output_limit") {
		t.Fatalf("excessive FRR source byte count was accepted: %#v", tooLarge)
	}
}
