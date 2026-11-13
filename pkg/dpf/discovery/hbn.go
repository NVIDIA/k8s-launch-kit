// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

type SwitchCollector interface {
	Collect(context.Context, config.SwitchTarget, time.Duration) fabric.Snapshot
}

func collectHBN(ctx context.Context, r config.Request, report *inventory.Report, collector SwitchCollector, provisioning *managementprobe.Snapshot) {
	if r.HBN == nil {
		return
	}
	inv := report.Inventory
	inv.HBN = &inventory.HBNInventory{Profile: *r.HBN, Namespace: r.DPFNamespace}
	serials := []string{}
	for _, dpu := range inv.DPUs {
		serials = append(serials, dpu.SerialNumber)
	}
	if asns, err := r.HBN.AllocateASNs(serials); err == nil {
		slices.Sort(serials)
		for _, serial := range serials {
			inv.HBN.ASNs = append(inv.HBN.ASNs, inventory.ASNAssignment{DPUSerial: serial, ASN: asns[serial]})
		}
	}
	for _, sw := range r.Switches {
		observation := fabric.Snapshot{Issues: []fabric.Issue{{Input: "switch", Code: "COLLECTOR_UNAVAILABLE", Message: "Switch inspection is unavailable."}}}
		if collector != nil {
			observation = collector.Collect(ctx, sw, r.RequestTimeout)
		}
		inv.HBN.Switches = append(inv.HBN.Switches, inventory.FabricSwitch{Name: sw.Name, Endpoint: sw.Endpoint, Platform: sw.Platform, Ports: slices.Clone(sw.Ports), Snapshot: observation})
	}
	readiness := evaluateHBN(inv, report.Discovery.Checks)
	managementChecks, managementFacts := managementHBNChecks(provisioning, inv, r.DPFNamespace)
	inv.HBN.Management = managementFacts
	additionalChecks := append(managementChecks, hbnSoftwareChecks(r.Input.Site.Provisioning, inv)...)
	additionalChecks = append(additionalChecks, hbnGenerationInputChecks(r.Input.Site.Provisioning)...)
	bootstrapChecks, tenantFacts := hbnBootstrapChecks(r.Input.Site.Provisioning, r.DPFNamespace, inv, provisioning)
	additionalChecks = append(additionalChecks, bootstrapChecks...)
	inv.HBN.Management.TenantClusters = tenantFacts
	inv.HBN.Management.Issues = append(inv.HBN.Management.Issues, projectTenantIssues(provisioning, tenantFacts)...)
	if r.HBNGeneration != nil {
		for _, sw := range inv.HBN.Switches {
			additionalChecks = append(additionalChecks, hbnFabricPoolCheck(sw, r.HBN.FabricVRF, r.HBNGeneration.LoopbackCIDR, "loopback"))
		}
	}
	readiness.Checks = append(readiness.Checks, additionalChecks...)
	for _, check := range additionalChecks {
		if check.State != "passed" {
			readiness.State = "blocked"
		}
	}
	report.Readiness = &readiness
	for _, check := range readiness.Checks {
		status := "PASS"
		if check.State != "passed" {
			status = "BLOCKED"
		}
		ui.FromContext(ctx).Info("[%s] HBN-BGP %q: %s", status, check.Subject, check.Message)
	}
	// Keep unrelated switch inventory out of the handoff. The readiness pass
	// above sees the complete snapshot to detect collisions and ambiguity.
	for i := range inv.HBN.Switches {
		pools := []string{r.HBN.WorkloadCIDR}
		if r.HBNGeneration != nil {
			pools = append(pools, r.HBNGeneration.LoopbackCIDR)
		}
		restrictFabric(&inv.HBN.Switches[i], r.HBN.FabricVRF, pools...)
	}
}

func evaluateHBN(inv *inventory.Inventory, discoveryChecks []inventory.Check) inventory.Readiness {
	result := inventory.Readiness{UseCase: "hbn-bgp", State: "ready", Checks: []inventory.Check{}}
	add := func(id, subject string, ok bool, reason, message string) {
		state := "passed"
		if !ok {
			state = "failed"
			result.State = "blocked"
		}
		result.Checks = append(result.Checks, inventory.Check{ID: "dpf.hbn." + id, Subject: subject, Vantage: "executor", RequiredPhase: "deploy", State: state, ReasonCode: reason, Message: message})
	}
	if inv == nil || inv.HBN == nil {
		add("inputs", "profile", false, "HBN_INPUTS_MISSING", "Resolved HBN profile and physical inventory are required.")
		return result
	}
	profile := inv.HBN.Profile
	physical := len(inv.DPUs) > 0 && len(inv.Hosts) > 0 && len(inv.Associations) == len(inv.DPUs)
	for _, check := range discoveryChecks {
		if check.RequiredPhase == "discover" && check.State != "passed" {
			physical = false
		}
	}
	add("physical-inventory", "hosts and DPUs", physical, "PHYSICAL_INVENTORY_REQUIRED", "Every selected DPU must have verified identity, BMC administrator access and a unique host association.")
	serials := []string{}
	for _, dpu := range inv.DPUs {
		serials = append(serials, dpu.SerialNumber)
	}
	asns, err := profile.AllocateASNs(serials)
	add("asn-allocation", "profile", err == nil, "ASN_ALLOCATION_INVALID", "DPU ASNs must be unique private ASNs, distinct from the fabric ASN.")
	add("switch-access", "fabric", len(inv.HBN.Switches) > 0, "SWITCH_ACCESS_REQUIRED", "Explicit SSH switch access and DPU uplink bindings are required to inspect the fabric.")
	bindings := map[string]int{}
	for _, sw := range inv.HBN.Switches {
		addressCheck := hbnFabricAddressCheck(sw, profile)
		result.Checks = append(result.Checks, addressCheck)
		if addressCheck.State != "passed" {
			result.State = "blocked"
		}
		for _, issue := range sw.Snapshot.Issues {
			add("switch-source", sw.Name+"/"+issue.Input, false, issue.Code, issue.Message)
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
		if vrf != nil {
			asnMatches = vrf.ASN != nil && *vrf.ASN == profile.FabricASN
			if profile.FabricVRF == "default" && vrf.ASN == nil && bgp != nil {
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
		add("fabric-asn", sw.Name, asnMatches, "FABRIC_ASN_MISMATCH", fmt.Sprintf("Fabric VRF %s must use ASN %d.", profile.FabricVRF, profile.FabricASN))
		add("ipv4-unicast", sw.Name, familyEnabled, "IPV4_UNICAST_NOT_ENABLED", "The fabric VRF must enable IPv4 unicast BGP.")
		for _, binding := range sw.Ports {
			bindings[binding.DPUSerial+"/"+binding.DPUInterface]++
			subject := sw.Name + "/" + binding.Name
			asn, selected := asns[binding.DPUSerial]
			add("port-scope", subject, selected, "DPU_BINDING_NOT_SELECTED", "The intended peer must be one of the physically discovered DPUs.")
			portConfigured, mtuKnown, vrfKnown, unbridged := false, false, false, false
			mtuMismatch, vrfMismatch := false, false
			for _, port := range sw.Snapshot.Configured.Interfaces {
				if port.Name != binding.Name {
					continue
				}
				portConfigured = true
				if port.MTU != nil {
					mtuKnown = true
					mtuMismatch = *port.MTU < profile.UplinkMTU
				}
				if port.VRF != nil {
					vrfKnown = true
					vrfMismatch = *port.VRF != profile.FabricVRF
				}
				unbridged = port.BridgeDomain != nil && (*port.BridgeDomain == "" || *port.BridgeDomain == "none")
			}
			// Operational NVUE reports effective values when applied config
			// inherits a platform default. Never infer them from absence alone.
			linkUp := false
			for _, port := range sw.Snapshot.Operational.Interfaces {
				if port.Name != binding.Name {
					continue
				}
				linkUp = port.OperState != nil && *port.OperState == "up" && port.AdminState != nil && *port.AdminState == "up"
				if port.MTU != nil {
					mtuKnown = true
					mtuMismatch = mtuMismatch || *port.MTU < profile.UplinkMTU
				}
				if port.VRF != nil {
					vrfKnown = true
					vrfMismatch = vrfMismatch || *port.VRF != profile.FabricVRF
				}
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
			add("port-mtu", subject, mtuKnown && !mtuMismatch, "UPLINK_MTU_UNVERIFIED", fmt.Sprintf("The uplink MTU must be at least %d bytes.", profile.UplinkMTU))
			add("port-vrf", subject, vrfKnown && !vrfMismatch, "PORT_VRF_MISMATCH", fmt.Sprintf("The uplink must belong to fabric VRF %s.", profile.FabricVRF))
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
			result.Checks = append(result.Checks, cabling)
			if cabling.State != "passed" {
				result.State = "blocked"
			}
		}
	}
	for _, serial := range serials {
		add("uplink-bindings", serial, bindings[serial+"/p0"] == 1 && bindings[serial+"/p1"] == 1, "TWO_DPU_UPLINKS_REQUIRED", "Exactly one switch binding is required for each DPU uplink p0 and p1.")
	}
	// These prerequisites are intentionally explicit while their collectors are
	// being integrated. A partial fabric preflight must never claim readiness.
	for _, pending := range []struct{ id, subject, code, message string }{
		{"software-artifacts", "software", "HBN_ARTIFACTS_NOT_VERIFIED", "The selected hardware-specific DPF/HBN software and immutable installation artifacts have not been verified."},
		{"routing-policy", "fabric", "ROUTING_POLICY_NOT_MEASURED", "Effective import/export policy for workload routes has not been verified."},
	} {
		add(pending.id, pending.subject, false, pending.code, pending.message)
	}
	for _, check := range hostHBNChecks(inv) {
		result.Checks = append(result.Checks, check)
		if check.State != "passed" {
			result.State = "blocked"
		}
	}
	return result
}

func restrictFabric(sw *inventory.FabricSwitch, vrf string, poolCIDRs ...string) {
	selected := map[string]bool{}
	for _, port := range sw.Ports {
		selected[port.Name] = true
	}
	s := &sw.Snapshot
	s.Configured.Interfaces = slices.DeleteFunc(s.Configured.Interfaces, func(p fabric.InterfaceConfig) bool { return !selected[p.Name] })
	s.Configured.VRFs = slices.DeleteFunc(s.Configured.VRFs, func(v fabric.VRFConfig) bool { return v.Name != vrf })
	s.Operational.Interfaces = slices.DeleteFunc(s.Operational.Interfaces, func(p fabric.InterfaceState) bool { return !selected[p.Name] })
	s.Operational.LLDP = slices.DeleteFunc(s.Operational.LLDP, func(p fabric.LLDPPeerHint) bool { return !selected[p.LocalInterface] })
	s.Operational.BGP = slices.DeleteFunc(s.Operational.BGP, func(v fabric.BGPSession) bool {
		return v.VRF != vrf || v.AddressFamily != "" && v.AddressFamily != "ipv4Unicast" && v.AddressFamily != "ipv4-unicast"
	})
	// The handoff contains the selected port/peer view, not complete source
	// inventories. All source-completeness and collision checks run above, before
	// this projection. Retain only addresses/routes relevant to the desired pool.
	pools := make([]netip.Prefix, 0, len(poolCIDRs))
	for _, raw := range poolCIDRs {
		if pool, err := netip.ParsePrefix(raw); err == nil {
			pools = append(pools, pool)
		}
	}
	overlaps := func(raw string) bool {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() == 0 {
			return false
		}
		for _, pool := range pools {
			if prefix.Overlaps(pool) {
				return true
			}
		}
		return false
	}
	if s.Operational.Addresses != nil {
		values := slices.Clone(*s.Operational.Addresses)
		values = slices.DeleteFunc(values, func(a fabric.InterfaceAddress) bool { return !selected[a.Interface] && !overlaps(a.Prefix) })
		if values == nil {
			values = []fabric.InterfaceAddress{}
		}
		s.Operational.Addresses = &values
	}
	for _, routes := range []**[]fabric.Route{&s.Operational.IPv4Routes, &s.Operational.IPv6Routes} {
		if *routes == nil {
			continue
		}
		values := slices.Clone(**routes)
		values = slices.DeleteFunc(values, func(r fabric.Route) bool { return !overlaps(r.Prefix) })
		if values == nil {
			values = []fabric.Route{}
		}
		*routes = &values
	}
	for i := range s.Configured.VRFs {
		v := &s.Configured.VRFs[i]
		v.AddressFamilies = slices.DeleteFunc(slices.Clone(v.AddressFamilies), func(a fabric.AddressFamilyConfig) bool { return a.Name != "ipv4-unicast" })
		groups := map[string]bool{}
		if v.Neighbors != nil {
			peers := slices.Clone(*v.Neighbors)
			peers = slices.DeleteFunc(peers, func(p fabric.BGPNeighborConfig) bool { return !selected[p.Name] })
			if peers == nil {
				peers = []fabric.BGPNeighborConfig{}
			}
			for _, peer := range peers {
				if peer.PeerGroup != nil {
					groups[*peer.PeerGroup] = true
				}
			}
			v.Neighbors = &peers
		}
		v.PeerGroups = slices.DeleteFunc(slices.Clone(v.PeerGroups), func(g fabric.PeerGroupConfig) bool { return !groups[g.Name] })
	}
	for i := range s.Operational.BGP {
		s.Operational.BGP[i].Peers = slices.DeleteFunc(slices.Clone(s.Operational.BGP[i].Peers), func(p fabric.BGPPeerState) bool { return !selected[p.Name] })
	}

	for i := range s.Configured.Bridges {
		s.Configured.Bridges[i].Ports = slices.DeleteFunc(s.Configured.Bridges[i].Ports, func(p fabric.BridgePortConfig) bool { return !selected[p.Name] })
	}
	s.Configured.Bridges = slices.DeleteFunc(s.Configured.Bridges, func(b fabric.BridgeConfig) bool { return len(b.Ports) == 0 })
	for i := range s.Operational.Bridges {
		s.Operational.Bridges[i].Ports = slices.DeleteFunc(s.Operational.Bridges[i].Ports, func(p fabric.BridgePortState) bool { return !selected[p.Name] })
	}
	s.Operational.Bridges = slices.DeleteFunc(s.Operational.Bridges, func(b fabric.BridgeState) bool { return len(b.Ports) == 0 })
}

// hbnScope excludes transient observations and includes the desired profile and
// exact selected port bindings; SSH trust is already part of RequestedScope.
func hbnScope(h *inventory.HBNInventory) string {
	if h == nil {
		return ""
	}
	type binding struct {
		Endpoint, Platform string
		Ports              []inventory.SwitchPort
	}
	bindings := []binding{}
	for _, sw := range h.Switches {
		bindings = append(bindings, binding{sw.Endpoint, sw.Platform, sw.Ports})
	}
	slices.SortFunc(bindings, func(a, b binding) int { return strings.Compare(a.Endpoint, b.Endpoint) })
	return inventory.Digest("hbn-intent-v1", struct {
		Namespace string
		Profile   hbn.Profile
		Bindings  []binding
	}{h.Namespace, h.Profile, bindings})
}
