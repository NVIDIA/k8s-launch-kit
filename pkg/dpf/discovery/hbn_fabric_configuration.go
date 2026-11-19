// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// HBNFabricConfigurationChecks evaluates selected switch configuration,
// operational uplink facts, and cabling from the current inventory. The caller
// supplies complete switch observations and the selected device and ASN scope.
// Pool occupancy, direct ASIC evidence and runtime reachability have separate
// lifecycle checks.
func HBNFabricConfigurationChecks(inv *inventory.Inventory) []inventory.Check {
	if inv == nil || inv.HBN == nil {
		return []inventory.Check{hbnReadinessCheck("inputs", "profile", false, "HBN_INPUTS_MISSING", "Resolved HBN profile and physical inventory are required.")}
	}
	var checks []inventory.Check
	add := func(id, subject string, ok bool, reason, message string) {
		checks = append(checks, hbnReadinessCheck(id, subject, ok, reason, message))
	}
	profile := inv.HBN.Profile
	serials := []string{}
	for _, dpu := range inv.DPUs {
		serials = append(serials, dpu.SerialNumber)
	}
	if len(serials) == 0 {
		add("fabric-devices", "DPUs", false, "HBN_DEVICES_MISSING", "At least one selected DPU is required to check the fabric.")
	}
	asns, err := profile.AllocateASNs(serials)
	add("asn-allocation", "profile", err == nil, "ASN_ALLOCATION_INVALID", "DPU ASNs must be unique private ASNs, distinct from the fabric ASN.")
	add("switch-access", "fabric", len(inv.HBN.Switches) > 0, "SWITCH_ACCESS_REQUIRED", "Explicit SSH switch access and DPU uplink bindings are required to inspect the fabric.")
	bindings := map[string]int{}
	for _, sw := range inv.HBN.Switches {
		if sw.Name == "" || sw.Endpoint == "" || len(sw.Ports) == 0 {
			subject := sw.Name
			if subject == "" {
				subject = "selected switch"
			}
			add("switch-inputs", subject, false, "HBN_SWITCH_INPUTS_INCOMPLETE", "Each selected switch requires an identity, endpoint and explicit uplink bindings.")
		}
		checks = append(checks, hbnRevisionCheck(sw))
		for _, issue := range sw.Snapshot.Issues {
			if issue.Input != "asic" {
				add("switch-source", sw.Name+"/"+issue.Input, false, issue.Code, issue.Message)
			}
		}
		var vrf *fabric.VRFConfig
		for i := range sw.Snapshot.Configured.VRFs {
			if sw.Snapshot.Configured.VRFs[i].Name == profile.FabricVRF {
				vrf = &sw.Snapshot.Configured.VRFs[i]
			}
		}
		bgp := sw.Snapshot.Configured.BGP
		globalEnabled := bgp != nil && bgp.Enabled != nil && *bgp.Enabled
		add("bgp-enabled", sw.Name, globalEnabled, "BGP_NOT_ENABLED", "The applied switch configuration must enable BGP.")
		asnMatches, familyEnabled := false, false
		var observedASN *uint32
		if vrf != nil {
			observedASN = vrf.ASN
			asnMatches = vrf.ASN != nil && *vrf.ASN == profile.FabricASN
			if profile.FabricVRF == "default" && vrf.ASN == nil && bgp != nil {
				observedASN = bgp.ASN
				asnMatches = bgp.ASN != nil && *bgp.ASN == profile.FabricASN
			}
			for _, af := range vrf.AddressFamilies {
				if af.Name == "ipv4-unicast" && af.Enabled != nil && *af.Enabled {
					familyEnabled = true
				}
			}
		}
		add("vrf", sw.Name, vrf != nil, "VRF_NOT_CONFIGURED", fmt.Sprintf("The applied configuration must contain fabric VRF %s.", profile.FabricVRF))
		add("vrf-bgp", sw.Name, vrf != nil && vrf.BGPEnabled != nil && *vrf.BGPEnabled, "VRF_BGP_NOT_ENABLED", "BGP must be enabled in the selected fabric VRF.")
		add("fabric-asn", sw.Name, asnMatches, "FABRIC_ASN_MISMATCH", fmt.Sprintf("Fabric VRF %s: expected ASN %d; observed %s.", profile.FabricVRF, profile.FabricASN, hbnObservedValue(observedASN)))
		add("ipv4-unicast", sw.Name, familyEnabled, "IPV4_UNICAST_NOT_ENABLED", "The fabric VRF must enable IPv4 unicast BGP.")
		for _, binding := range sw.Ports {
			bindings[binding.DPUSerial+"/"+binding.DPUInterface]++
			subject := sw.Name + "/" + binding.Name
			asn, selected := asns[binding.DPUSerial]
			add("port-scope", subject, selected, "DPU_BINDING_NOT_SELECTED", "The intended peer must be one of the physically discovered DPUs.")
			portConfigured, mtuKnown, vrfKnown, unbridged := false, false, false, false
			configuredBridge := false
			mtuMismatch, vrfMismatch := false, false
			var configuredMTU, operationalMTU *int
			var configuredVRF, operationalVRF *string
			for _, port := range sw.Snapshot.Configured.Interfaces {
				if port.Name != binding.Name {
					continue
				}
				portConfigured = true
				configuredMTU, configuredVRF = port.MTU, port.VRF
				if port.MTU != nil {
					mtuKnown = true
					mtuMismatch = *port.MTU < profile.UplinkMTU
				}
				if port.VRF != nil {
					vrfKnown = true
					vrfMismatch = *port.VRF != profile.FabricVRF
				}
				unbridged = port.BridgeDomain != nil && (*port.BridgeDomain == "" || *port.BridgeDomain == "none")
				configuredBridge = port.BridgeDomain != nil && !unbridged
			}
			// Operational NVUE reports effective values when applied config
			// inherits a platform default. Never infer them from absence alone.
			linkUp := false
			for _, port := range sw.Snapshot.Operational.Interfaces {
				if port.Name != binding.Name {
					continue
				}
				linkUp = port.OperState != nil && *port.OperState == "up" && port.AdminState != nil && *port.AdminState == "up"
				operationalMTU, operationalVRF = port.MTU, port.VRF
				if port.MTU != nil {
					mtuKnown = true
					mtuMismatch = mtuMismatch || *port.MTU < profile.UplinkMTU
				}
				if port.VRF != nil {
					vrfKnown = true
					vrfMismatch = vrfMismatch || *port.VRF != profile.FabricVRF
				}
				// A physical port directly enslaved to the selected VRF is a
				// routed port. A sparse list with no bridge field proves nothing.
				if port.Type != nil && *port.Type == "swp" && port.Parent != nil && port.VRF != nil && *port.Parent == profile.FabricVRF && *port.VRF == profile.FabricVRF {
					unbridged = true
				}
			}
			if configuredBridge {
				unbridged = false
			}
			for _, bridge := range sw.Snapshot.Configured.Bridges {
				for _, port := range bridge.Ports {
					if port.Name == binding.Name {
						unbridged = false
					}
				}
			}
			for _, bridge := range sw.Snapshot.Operational.Bridges {
				for _, port := range bridge.Ports {
					if port.Name == binding.Name {
						unbridged = false
					}
				}
			}
			add("port", subject, portConfigured, "SWITCH_PORT_NOT_CONFIGURED", "The selected physical switch port must exist in the applied configuration.")
			add("port-mtu", subject, mtuKnown && !mtuMismatch, "UPLINK_MTU_UNVERIFIED", fmt.Sprintf("Uplink MTU: expected at least %d bytes; configured %s; operational %s.", profile.UplinkMTU, hbnObservedValue(configuredMTU), hbnObservedValue(operationalMTU)))
			add("port-vrf", subject, vrfKnown && !vrfMismatch, "PORT_VRF_MISMATCH", fmt.Sprintf("Uplink VRF: expected %s; configured %s; operational %s.", profile.FabricVRF, hbnObservedValue(configuredVRF), hbnObservedValue(operationalVRF)))
			add("routed-port", subject, unbridged, "ROUTED_PORT_UNVERIFIED", "Flat BGP requires a routed uplink outside a layer-2 bridge.")
			add("link", subject, linkUp, "UPLINK_NOT_UP", "The selected uplink must be administratively and operationally up.")
			neighborOK := false
			if selected && vrf != nil && vrf.Neighbors != nil {
				for _, peer := range *vrf.Neighbors {
					if peer.Name != binding.Name || peer.Type == nil || *peer.Type != "unnumbered" || peer.EffectiveRemoteAS == nil {
						continue
					}
					remote := *peer.EffectiveRemoteAS
					neighborOK = (remote == "external" || remote == strconv.FormatUint(uint64(asn), 10)) && slices.Contains(peer.EffectiveAddressFamilies, "ipv4-unicast")
				}
			}
			add("neighbor", subject, neighborOK, "UNNUMBERED_PEER_MISMATCH", "An IPv4-unicast unnumbered BGP neighbor for the selected DPU ASN must be configured; establishment is checked after deployment.")
			cabling := hbnCablingCheck(inv, sw, binding)
			if cabling.State == "unknown" {
				cabling = combineHBNCablingCheck(cabling, hostHBNCablingCheck(inv, &sw, binding))
			}
			checks = append(checks, cabling)
		}
	}
	for _, serial := range serials {
		add("uplink-bindings", serial, bindings[serial+"/p0"] == 1 && bindings[serial+"/p1"] == 1, "TWO_DPU_UPLINKS_REQUIRED", "Exactly one switch binding is required for each DPU uplink p0 and p1.")
	}
	return append(checks, hbnRoutingPolicyChecks(inv)...)
}

func hbnReadinessCheck(id, subject string, ok bool, reason, message string) inventory.Check {
	state := "passed"
	if !ok {
		state = "failed"
	}
	return inventory.Check{ID: "dpf.hbn." + id, Subject: subject, Vantage: "executor", RequiredPhase: "deploy", State: state, ReasonCode: reason, Message: message}
}
