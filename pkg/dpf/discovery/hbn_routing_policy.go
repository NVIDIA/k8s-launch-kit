// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"
	"strconv"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// hbnRoutingPolicyChecks verifies the effective IPv4-unicast enabled state,
// FRR policy mode, and filter attachments for selected HBN peers. It does not
// assert imported-prefix intent, accepted routes, or runtime reachability.
func hbnRoutingPolicyChecks(inv *inventory.Inventory) []inventory.Check {
	if inv == nil || inv.HBN == nil {
		return []inventory.Check{routingPolicyCheck("switches", "", "unknown", "ROUTING_POLICY_INPUT_MISSING", "Selected switch and HBN profile facts are required to inspect routing policy.")}
	}
	allocatedASNs, err := selectedHBNASNs(inv.HBN)
	checks := make([]inventory.Check, 0)
	for _, sw := range inv.HBN.Switches {
		for _, port := range sw.Ports {
			expectedASN, found := allocatedASNs[port.DPUSerial]
			if err != nil || !found {
				checks = append(checks, routingPolicyCheck(sw.Name, port.Name, "unknown", "ROUTING_POLICY_ASN_ASSIGNMENT_UNKNOWN", "The selected DPU ASN cannot be recomputed from the retained serial assignments."))
				continue
			}
			checks = append(checks, checkSelectedPeerPolicy(sw, inv.HBN.Profile, port.Name, expectedASN))
		}
	}
	if len(checks) == 0 {
		return []inventory.Check{routingPolicyCheck("switches", "", "unknown", "ROUTING_POLICY_PEERS_MISSING", "Selected HBN switch peers are not available for routing-policy checks.")}
	}
	return checks
}

func selectedHBNASNs(selected *inventory.HBNInventory) (map[string]uint32, error) {
	if selected == nil || len(selected.ASNs) == 0 {
		return nil, fmt.Errorf("selected DPU ASN assignments are absent")
	}
	serials := make([]string, 0, len(selected.ASNs))
	seen := make(map[string]struct{}, len(selected.ASNs))
	for _, assignment := range selected.ASNs {
		if assignment.DPUSerial == "" {
			return nil, fmt.Errorf("selected DPU ASN assignment has an empty serial")
		}
		if _, exists := seen[assignment.DPUSerial]; exists {
			return nil, fmt.Errorf("selected DPU ASN assignments contain a duplicate serial")
		}
		seen[assignment.DPUSerial] = struct{}{}
		serials = append(serials, assignment.DPUSerial)
	}
	expected, err := selected.Profile.AllocateASNs(serials)
	if err != nil {
		return nil, err
	}
	for _, assignment := range selected.ASNs {
		if expected[assignment.DPUSerial] != assignment.ASN {
			return nil, fmt.Errorf("selected DPU ASN assignment differs from the resolved profile")
		}
	}
	return expected, nil
}

func checkSelectedPeerPolicy(sw inventory.FabricSwitch, profile hbn.Profile, peerName string, expectedDPUASN uint32) inventory.Check {
	unknown := func(code, message string) inventory.Check {
		return routingPolicyCheck(sw.Name, peerName, "unknown", code, message)
	}
	vrfName := profile.FabricVRF
	policy := sw.Snapshot.Configured.FRRPolicy
	if policy == nil || !policy.Complete {
		return unknown("ROUTING_POLICY_SOURCE_INCOMPLETE", "A complete FRR running BGP configuration is required to evaluate the selected peer.")
	}
	var selected *fabric.FRRPolicyVRF
	for i := range policy.VRFs {
		if policy.VRFs[i].Name == vrfName {
			selected = &policy.VRFs[i]
			break
		}
	}
	if selected == nil || !selected.Complete {
		return unknown("ROUTING_POLICY_VRF_UNRESOLVED", fmt.Sprintf("FRR policy facts for selected VRF %s are incomplete.", vrfName))
	}
	var peer *fabric.FRRPolicyNeighbor
	for i := range selected.Neighbors {
		if selected.Neighbors[i].Name == peerName {
			peer = &selected.Neighbors[i]
			break
		}
	}
	if peer == nil {
		return unknown("ROUTING_POLICY_PEER_UNRESOLVED", "FRR does not contain the selected peer configuration needed to resolve inherited policy.")
	}
	if selected.BGPShutdown == nil {
		return unknown("ROUTING_POLICY_BGP_STATE_UNKNOWN", "FRR does not establish whether BGP is administratively enabled for the selected VRF.")
	}
	if selected.LocalASN == nil {
		return unknown("ROUTING_POLICY_LOCAL_AS_UNKNOWN", "FRR does not establish the local ASN for the selected VRF.")
	}
	if *selected.LocalASN != profile.FabricASN {
		return routingPolicyCheck(sw.Name, peerName, "failed", "ROUTING_POLICY_LOCAL_AS_MISMATCH", "The selected FRR VRF local ASN differs from the resolved HBN fabric ASN.")
	}
	if *selected.BGPShutdown {
		return routingPolicyCheck(sw.Name, peerName, "failed", "ROUTING_POLICY_BGP_SHUTDOWN", "BGP is administratively shut down for the selected VRF.")
	}
	var nvuePeer *fabric.BGPNeighborConfig
	for i := range sw.Snapshot.Configured.VRFs {
		configuredVRF := &sw.Snapshot.Configured.VRFs[i]
		if configuredVRF.Name != vrfName || configuredVRF.Neighbors == nil {
			continue
		}
		for j := range *configuredVRF.Neighbors {
			if (*configuredVRF.Neighbors)[j].Name == peerName {
				nvuePeer = &(*configuredVRF.Neighbors)[j]
				break
			}
		}
	}
	if nvuePeer == nil || !sameOptionalText(nvuePeer.PeerGroup, peer.PeerGroup) {
		return unknown("ROUTING_POLICY_PEER_BINDING_MISMATCH", "Applied NVUE and FRR do not identify the same selected peer-group binding.")
	}
	if !peer.Interface {
		return unknown("ROUTING_POLICY_PEER_TYPE_MISMATCH", "FRR does not establish the selected interface-based unnumbered peer.")
	}
	var group *fabric.FRRPolicyPeerGroup
	if peer.PeerGroup != nil {
		for i := range selected.PeerGroups {
			if selected.PeerGroups[i].Name == *peer.PeerGroup {
				group = &selected.PeerGroups[i]
				break
			}
		}
		if group == nil {
			return unknown("ROUTING_POLICY_PEER_GROUP_UNRESOLVED", "The selected FRR peer group could not be resolved.")
		}
	}
	if peer.InboundFilter || peer.OutboundFilter || (group != nil && (group.InboundFilter || group.OutboundFilter)) {
		return unknown("ROUTING_POLICY_UNSUPPORTED", "The selected FRR peer has a filter whose effect this check cannot evaluate.")
	}
	if code, message, found := unsupportedFRRSettingCheck(peer, group); found {
		return unknown(code, message)
	}
	if peer.UnsupportedPolicy || (group != nil && group.UnsupportedPolicy) {
		return unknown("ROUTING_POLICY_UNSUPPORTED", "The selected FRR peer or peer group has a setting this check cannot evaluate.")
	}
	remoteAS := peer.RemoteAS
	if remoteAS == nil && group != nil {
		remoteAS = group.RemoteAS
	}
	if remoteAS == nil {
		return unknown("ROUTING_POLICY_REMOTE_AS_UNKNOWN", "FRR does not establish the selected peer's effective remote ASN.")
	}
	if nvuePeer.EffectiveRemoteAS == nil {
		return unknown("ROUTING_POLICY_REMOTE_AS_UNKNOWN", "Applied NVUE does not establish the selected peer's effective remote ASN.")
	}
	if !matchesEffectiveRemoteAS(*remoteAS, *nvuePeer.EffectiveRemoteAS, expectedDPUASN) {
		return routingPolicyCheck(sw.Name, peerName, "failed", "ROUTING_POLICY_REMOTE_AS_MISMATCH", "The selected FRR peer remote ASN differs from applied NVUE or the allocated DPU ASN.")
	}
	// FRR peer-specific values override peer-group values. An absent shutdown
	// directive means enabled; an absent activation directive uses the router's
	// documented IPv4-unicast default unless the router disables that default.
	var groupShutdown, groupActivation *bool
	if group != nil {
		groupShutdown = group.Shutdown
		groupActivation = group.IPv4UnicastActivated
	}
	peerIsShutdown := false
	if groupShutdown != nil {
		peerIsShutdown = *groupShutdown
	}
	if peer.Shutdown != nil {
		peerIsShutdown = *peer.Shutdown
	}
	if peerIsShutdown {
		return routingPolicyCheck(sw.Name, peerName, "failed", "ROUTING_POLICY_PEER_SHUTDOWN", "The selected FRR peer is administratively shut down.")
	}
	if selected.IPv4UnicastDefault == nil {
		return unknown("ROUTING_POLICY_ACTIVATION_UNKNOWN", "FRR does not establish the default IPv4-unicast activation state for the selected VRF.")
	}
	ipv4Active := *selected.IPv4UnicastDefault
	if groupActivation != nil {
		ipv4Active = *groupActivation
	}
	if peer.IPv4UnicastActivated != nil {
		ipv4Active = *peer.IPv4UnicastActivated
	}
	if !ipv4Active {
		return routingPolicyCheck(sw.Name, peerName, "failed", "ROUTING_POLICY_PEER_INACTIVE", "The selected FRR peer is not active for IPv4 unicast.")
	}
	// The per-VRF setting overrides the process default. Only the documented
	// datacenter default is inferred; other modes need an explicit setting.
	requiresPolicy := selected.EBGPRequiresPolicy
	if requiresPolicy == nil {
		requiresPolicy = policy.EBGPRequiresPolicy
	}
	if requiresPolicy == nil && policy.Defaults != nil && *policy.Defaults == "datacenter" {
		v := false
		requiresPolicy = &v
	}
	if requiresPolicy == nil {
		return unknown("ROUTING_POLICY_MODE_UNKNOWN", "FRR eBGP policy mode cannot be established for the selected peer.")
	}
	if *requiresPolicy {
		return routingPolicyCheck(sw.Name, peerName, "failed", "EBGP_REQUIRES_POLICY_ENABLED", "FRR requires an explicit import and export policy for eBGP peers, but none is configured for this selected peer.")
	}
	return routingPolicyCheck(sw.Name, peerName, "passed", "ROUTING_POLICY_UNFILTERED", "The selected IPv4-unicast peer and inherited peer group have no configured route filters; the profile declares no import-prefix constraint, and runtime routes are not verified here.")
}

func unsupportedFRRSettingCheck(peer *fabric.FRRPolicyNeighbor, group *fabric.FRRPolicyPeerGroup) (string, string, bool) {
	has := func(category string) bool {
		for _, setting := range peer.UnsupportedSettings {
			if setting == category {
				return true
			}
		}
		if group != nil {
			for _, setting := range group.UnsupportedSettings {
				if setting == category {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has("authentication"):
		return "ROUTING_POLICY_AUTHENTICATION_UNVERIFIED", "The selected FRR peer or peer group has authentication settings not represented by the HBN profile.", true
	case has("local-as"):
		return "ROUTING_POLICY_LOCAL_AS_UNSUPPORTED", "The selected FRR peer or peer group has a local-AS override not represented by the HBN profile.", true
	case has("asn"):
		return "ROUTING_POLICY_REMOTE_AS_UNSUPPORTED", "The selected FRR peer or peer group has remote-AS configuration that cannot be reconciled with the HBN assignment.", true
	case has("transport"):
		return "ROUTING_POLICY_TRANSPORT_UNSUPPORTED", "The selected FRR peer or peer group has transport settings not represented by the HBN profile.", true
	case has("routing-policy"):
		return "ROUTING_POLICY_UNSUPPORTED", "The selected FRR peer or peer group has a route-policy setting this check cannot evaluate.", true
	case has("other"):
		return "ROUTING_POLICY_UNSUPPORTED", "The selected FRR peer or peer group has a setting this check cannot evaluate.", true
	default:
		return "", "", false
	}
}

func matchesEffectiveRemoteAS(configured, nvue string, expectedDPUASN uint32) bool {
	if expectedDPUASN == 0 {
		return false
	}
	expected := strconv.FormatUint(uint64(expectedDPUASN), 10)
	return (configured == "external" || configured == expected) && (nvue == "external" || nvue == expected)
}

func sameOptionalText(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func routingPolicyCheck(switchName, peerName, state, reason, message string) inventory.Check {
	subject := switchName
	if peerName != "" {
		subject += "/" + peerName
	}
	return inventory.Check{
		ID:            "dpf.hbn.routing-policy." + switchName + "." + peerName,
		Subject:       subject,
		Vantage:       "switch",
		RequiredPhase: "deploy",
		State:         state,
		ReasonCode:    reason,
		Message:       message,
	}
}
