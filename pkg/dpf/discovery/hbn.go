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

func collectHBN(ctx context.Context, r config.Request, report *inventory.Report, provisioning *managementprobe.Snapshot, deps Dependencies) {
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
		if deps.Switches != nil {
			observation = deps.Switches.Collect(ctx, sw, r.RequestTimeout)
		}
		inv.HBN.Switches = append(inv.HBN.Switches, inventory.FabricSwitch{Name: sw.Name, Endpoint: sw.Endpoint, Platform: sw.Platform, Ports: slices.Clone(sw.Ports), Snapshot: observation})
	}
	collectHostHBNCabling(ctx, r, inv, deps.HostLLDP)
	hostNetworking := collectHostHBNNetworking(ctx, r, inv, deps.HostNetworking)
	readiness := evaluateHBN(inv, report.Discovery.Checks, hostNetworking)
	managementChecks, managementFacts := managementHBNChecks(provisioning, inv, r.DPFNamespace)
	inv.HBN.Management = managementFacts
	additionalChecks := append(managementChecks, hbnEnrollmentAccessCheck(ctx, r, inv, deps.HBNEnrollment))
	additionalChecks = append(additionalChecks, hbnSoftwareChecks(r.Input.Site.Provisioning, inv)...)
	additionalChecks = append(additionalChecks, hbnProvisioningAPIChecks(inv)...)
	additionalChecks = append(additionalChecks, hbnResetSupportChecks(inv)...)
	additionalChecks = append(additionalChecks, hbnArtifactSourceChecks(ctx, r.Input.Site.Provisioning, inv, deps.Artifacts)...)
	additionalChecks = append(additionalChecks, hbnGenerationInputChecks(r.Input.Site.Provisioning)...)
	additionalChecks = append(additionalChecks, hbnHostAddressPlanCheck(r.HBN, r.HBNGeneration))
	additionalChecks = append(additionalChecks, hbnRuntimeBudgetCheck(inv))
	bootstrapChecks, tenantFacts := hbnBootstrapChecks(r.Input.Site.Provisioning, r.DPFNamespace, inv, provisioning)
	additionalChecks = append(additionalChecks, bootstrapChecks...)
	inv.HBN.Management.TenantClusters = tenantFacts
	inv.HBN.Management.Issues = append(inv.HBN.Management.Issues, projectTenantIssues(provisioning, tenantFacts)...)
	additionalChecks = append(additionalChecks, hbnPermissionChecks(ctx, r, inv, deps.HBNPermissions)...)
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
	if deps.DeferHBNReadiness {
		ui.FromContext(ctx).Info("[RUNNING] HBN observations collected; owned-operation resume eligibility will be assessed from current operation proofs.")
	} else {
		logHBNReadiness(ctx, readiness.Checks)
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

func logHBNReadiness(ctx context.Context, checks []inventory.Check) {
	output := ui.FromContext(ctx)
	passedCallerPermissions := 0
	for _, check := range checks {
		if strings.HasPrefix(check.ID, hbnCallerRBACPrefix) && check.State == "passed" {
			passedCallerPermissions++
			continue
		}
		status := "PASS"
		if check.State != "passed" {
			status = "BLOCKED"
		}
		output.Info("[%s] HBN-BGP %q: %s", status, check.Subject, check.Message)
	}
	if passedCallerPermissions > 0 {
		output.Info("[PASS] HBN-BGP caller authorization: %d scoped permission checks passed.", passedCallerPermissions)
	}
}

func evaluateHBN(inv *inventory.Inventory, discoveryChecks []inventory.Check, hostNetworking map[string]inventory.Check) inventory.Readiness {
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
		revisionCheck := hbnRevisionCheck(sw)
		result.Checks = append(result.Checks, revisionCheck)
		if revisionCheck.State != "passed" {
			result.State = "blocked"
		}
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
			asicCheck := hbnASICCheck(sw, binding, profile)
			result.Checks = append(result.Checks, asicCheck)
			if asicCheck.State != "passed" {
				result.State = "blocked"
			}
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
			result.Checks = append(result.Checks, cabling)
			if cabling.State != "passed" {
				result.State = "blocked"
			}
		}
	}
	for _, serial := range serials {
		add("uplink-bindings", serial, bindings[serial+"/p0"] == 1 && bindings[serial+"/p1"] == 1, "TWO_DPU_UPLINKS_REQUIRED", "Exactly one switch binding is required for each DPU uplink p0 and p1.")
	}
	for _, check := range hbnRoutingPolicyChecks(inv) {
		result.Checks = append(result.Checks, check)
		if check.State != "passed" {
			result.State = "blocked"
		}
	}
	for _, check := range hostHBNChecks(inv, hostNetworking) {
		result.Checks = append(result.Checks, check)
		if check.State != "passed" {
			result.State = "blocked"
		}
	}
	return result
}

func hbnObservedValue[T ~string | ~int | ~uint32](value *T) string {
	if value == nil {
		return "not measured"
	}
	return fmt.Sprint(*value)
}

func restrictFabric(sw *inventory.FabricSwitch, vrf string, poolCIDRs ...string) {
	selected := map[string]bool{}
	for _, port := range sw.Ports {
		selected[port.Name] = true
	}
	s := &sw.Snapshot
	s.Configured.Interfaces = slices.DeleteFunc(s.Configured.Interfaces, func(p fabric.InterfaceConfig) bool { return !selected[p.Name] })
	s.Configured.VRFs = slices.DeleteFunc(s.Configured.VRFs, func(v fabric.VRFConfig) bool { return v.Name != vrf })
	if policy := s.Configured.FRRPolicy; policy != nil {
		policy.VRFs = slices.DeleteFunc(policy.VRFs, func(v fabric.FRRPolicyVRF) bool { return v.Name != vrf })
		for i := range policy.VRFs {
			selectedVRF := &policy.VRFs[i]
			selectedVRF.Neighbors = slices.DeleteFunc(selectedVRF.Neighbors, func(p fabric.FRRPolicyNeighbor) bool { return !selected[p.Name] })
			groups := map[string]bool{}
			for _, peer := range selectedVRF.Neighbors {
				if peer.PeerGroup != nil {
					groups[*peer.PeerGroup] = true
				}
			}
			selectedVRF.PeerGroups = slices.DeleteFunc(selectedVRF.PeerGroups, func(p fabric.FRRPolicyPeerGroup) bool { return !groups[p.Name] })
		}
	}
	if s.ASIC != nil {
		s.ASIC.Ports = slices.DeleteFunc(s.ASIC.Ports, func(p fabric.ASICPort) bool { return !selected[p.Name] })
	}
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
