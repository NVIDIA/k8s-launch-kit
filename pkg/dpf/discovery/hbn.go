// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
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
	hostNetworking := collectHostHBNNetworking(ctx, r, inv, deps.HostNetworking, deps.DeferHBNReadiness)
	readiness := evaluateHBN(inv, report.Discovery.Checks, hostNetworking)
	managementChecks, managementFacts := managementHBNChecks(provisioning, inv, r.DPFNamespace)
	inv.HBN.Management = managementFacts
	additionalChecks := append(managementChecks, hbnEnrollmentAccessCheck(ctx, r, inv, deps.HBNEnrollment))
	provisioningInputs := r.Input.Site.Provisioning
	provisioningReady := true
	if deps.RetainedProvisioning != nil {
		var err error
		provisioningInputs, err = deps.RetainedProvisioning.ResolveProvisioning(r.Input.Site)
		if err != nil {
			provisioningReady = false
			additionalChecks = append(additionalChecks, inventory.Check{ID: "dpf.hbn.retained-provisioning", Subject: "retained deployment recipe", Vantage: "executor", RequiredPhase: "deploy", State: "failed", ReasonCode: "RETAINED_PROVISIONING_INVALID", Message: err.Error()})
		} else {
			additionalChecks = append(additionalChecks, hbnSoftwareChecks(provisioningInputs, inv)...)
			additionalChecks = append(additionalChecks, hbnGenerationInputChecks(provisioningInputs)...)
		}
	} else if r.Input.External {
		var catalogChecks []inventory.Check
		provisioningInputs, catalogChecks = hbnCatalogInputs(r.Input.Site, inv)
		additionalChecks = append(additionalChecks, catalogChecks...)
		for _, check := range catalogChecks {
			if check.State != "passed" {
				provisioningReady = false
			}
		}
	} else {
		additionalChecks = append(additionalChecks, hbnSoftwareChecks(provisioningInputs, inv)...)
		additionalChecks = append(additionalChecks, hbnGenerationInputChecks(provisioningInputs)...)
	}
	additionalChecks = append(additionalChecks, hbnProvisioningAPIChecks(inv)...)
	additionalChecks = append(additionalChecks, hbnResetSupportChecks(inv)...)
	if provisioningReady {
		additionalChecks = append(additionalChecks, hbnArtifactSourceChecks(ctx, provisioningInputs, inv, deps.Artifacts, deps.ArtifactSourceMode)...)
	} else {
		additionalChecks = append(additionalChecks, inventory.Check{ID: "dpf.hbn.artifact-source", Subject: "release artifacts", Vantage: "executor", RequiredPhase: "generate", State: "unknown", ReasonCode: "PROVISIONING_PREREQUISITE_UNAVAILABLE", Message: "Artifact downloads were not checked because fixed provisioning inputs could not be resolved. Resolve the reported provisioning blocker first."})
	}
	additionalChecks = append(additionalChecks, hbnHostAddressPlanCheck(r.HBN, r.HBNGeneration))
	additionalChecks = append(additionalChecks, hbnRuntimeBudgetCheck(inv))
	controllerServiceCIDR := ""
	if r.Software != nil {
		controllerServiceCIDR = r.Software.TenantServiceCIDR
	}
	bootstrapChecks, tenantFacts := hbnBootstrapChecks(provisioningInputs, r.DPFNamespace, inv, provisioning, controllerServiceCIDR)
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
		ui.FromContext(ctx).Info("[RUNNING] HBN observations collected; owned deployment-phase eligibility will be assessed from current operation proofs.")
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
	result := inventory.Readiness{UseCase: "hbn-bgp", State: "ready"}
	if inv == nil || inv.HBN == nil {
		result.Checks = HBNFabricConfigurationChecks(inv)
		result.State = "blocked"
		return result
	}
	physical := len(inv.DPUs) > 0 && len(inv.Hosts) > 0 && len(inv.Associations) == len(inv.DPUs)
	for _, check := range discoveryChecks {
		if check.RequiredPhase == "discover" && check.State != "passed" {
			physical = false
		}
	}
	result.Checks = append(result.Checks, hbnReadinessCheck("physical-inventory", "hosts and DPUs", physical, "PHYSICAL_INVENTORY_REQUIRED", "Every selected DPU must have verified identity, BMC administrator access and a unique host association."))
	result.Checks = append(result.Checks, HBNFabricConfigurationChecks(inv)...)
	for _, sw := range inv.HBN.Switches {
		result.Checks = append(result.Checks, hbnFabricAddressCheck(sw, inv.HBN.Profile))
		for _, issue := range sw.Snapshot.Issues {
			if issue.Input == "asic" {
				result.Checks = append(result.Checks, hbnReadinessCheck("switch-source", sw.Name+"/"+issue.Input, false, issue.Code, issue.Message))
			}
		}
		for _, binding := range sw.Ports {
			result.Checks = append(result.Checks, hbnASICCheck(sw, binding, inv.HBN.Profile))
		}
	}
	result.Checks = append(result.Checks, HostHBNChecks(inv, hostNetworking)...)
	for _, check := range result.Checks {
		if check.State != "passed" {
			result.State = "blocked"
			break
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
