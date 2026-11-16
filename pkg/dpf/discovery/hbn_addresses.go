// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func hbnFabricAddressCheck(sw inventory.FabricSwitch, profile hbn.Profile) inventory.Check {
	return hbnFabricPoolCheck(sw, profile.FabricVRF, profile.WorkloadCIDR, "workload")
}

func hbnFabricPoolCheck(sw inventory.FabricSwitch, vrfName, poolCIDR, poolName string) inventory.Check {
	return fabricPoolCheck(sw, vrfName, poolCIDR, poolName, nil)
}

// ResumeRouteAllocation is a current allocation bound by deployment to its
// original IPAM/tenant/DPU ownership. It never authorizes an entire pool.
type ResumeRouteAllocation struct{ DPUSerial, Pool, Prefix string }

// ResumeFabricPoolCheck retains ordinary address/VRF/vacancy validation and
// permits only exact owned prefixes whose every current next hop is a freshly
// corroborated physical uplink to that allocation's particular DPU.
func ResumeFabricPoolCheck(inv *inventory.Inventory, sw inventory.FabricSwitch, vrfName, poolCIDR, poolName string, allocations []ResumeRouteAllocation) inventory.Check {
	if inv == nil || inv.HBN == nil {
		return hbnFabricPoolCheck(sw, vrfName, poolCIDR, poolName)
	}
	allowed := map[string]map[string]bool{}
	for _, a := range allocations {
		if a.Pool != poolName {
			continue
		}
		if _, found := allowed[a.Prefix]; found {
			return inventory.Check{ID: "dpf.hbn.fabric-address-conflicts", Subject: sw.Name + "/" + poolName, State: "failed", ReasonCode: "OWNED_RESUME_ALLOCATION_AMBIGUOUS", Message: "A route prefix has multiple selected allocation owners.", Vantage: "executor", RequiredPhase: "deploy"}
		}
		ports := map[string]bool{}
		for _, port := range sw.Ports {
			if port.DPUSerial == a.DPUSerial && hbnCablingCheck(inv, sw, port).State == "passed" {
				ports[port.Name] = true
			}
		}
		allowed[a.Prefix] = ports
	}
	return fabricPoolCheck(sw, vrfName, poolCIDR, poolName, allowed)
}

func fabricPoolCheck(sw inventory.FabricSwitch, vrfName, poolCIDR, poolName string, owned map[string]map[string]bool) inventory.Check {
	check := inventory.Check{ID: "dpf.hbn.fabric-address-conflicts", Subject: sw.Name + "/" + poolName, State: "unknown", ReasonCode: "FABRIC_ADDRESSES_UNAVAILABLE", Message: "Complete switch address and IPv4/IPv6 all-table route observations are required to check the proposed " + poolName + " pool.", Vantage: "executor", RequiredPhase: "deploy"}
	facts := sw.Snapshot.Operational
	for _, issue := range sw.Snapshot.Issues {
		switch issue.Input {
		case "operationalAddresses", "operationalRoutesIPv4", "operationalRoutesIPv6":
			check.ReasonCode = issue.Code
			check.Message = issue.Message
			return check
		}
	}
	if facts.Addresses == nil || facts.IPv4Routes == nil || facts.IPv6Routes == nil {
		return check
	}
	pool, err := netip.ParsePrefix(poolCIDR)
	if err != nil {
		check.State = "failed"
		check.ReasonCode = strings.ToUpper(poolName) + "_POOL_INVALID"
		check.Message = "The resolved " + poolName + " address pool is invalid."
		return check
	}
	for _, address := range *facts.Addresses {
		prefix, err := netip.ParsePrefix(address.Prefix)
		if err != nil {
			check.ReasonCode = "FABRIC_ADDRESS_INVALID"
			return check
		}
		if !prefix.Overlaps(pool) {
			continue
		}
		vrf, known := addressVRF(sw.Snapshot, address.Interface)
		if known && vrf != vrfName {
			continue
		}
		check.Message = fmt.Sprintf("%s pool %s overlaps switch interface %s address %s; verify its VRF and choose a nonconflicting pool.", poolName, pool, address.Interface, address.Prefix)
		check.ReasonCode = "FABRIC_ADDRESS_VRF_UNVERIFIED"
		if known {
			check.State = "failed"
			check.ReasonCode = strings.ToUpper(poolName) + "_ADDRESS_OVERLAP"
		}
		return check
	}
	for _, routes := range []*[]fabric.Route{facts.IPv4Routes, facts.IPv6Routes} {
		for _, route := range *routes {
			prefix, err := netip.ParsePrefix(route.Prefix)
			if err != nil {
				check.ReasonCode = "FABRIC_ROUTE_INVALID"
				return check
			}
			// An aggregate default route is normal fabric egress, not allocation of
			// every address it covers. Specific overlapping routes need review.
			if prefix.Bits() == 0 || !prefix.Overlaps(pool) {
				continue
			}
			wanted := route.Table == vrfName || vrfName == "default" && (route.Table == "main" || route.Table == "254")
			knownOther := false
			for _, vrf := range sw.Snapshot.Configured.VRFs {
				if route.Table == vrf.Name && vrf.Name != vrfName {
					knownOther = true
				}
			}
			if !wanted && knownOther {
				continue
			}

			if wanted && ownedResumeRoute(route, owned[route.Prefix]) {
				continue
			}
			check.Message = fmt.Sprintf("%s pool %s overlaps route %s in switch table %s; verify ownership and VRF before assigning this pool.", poolName, pool, route.Prefix, route.Table)
			check.ReasonCode = "FABRIC_ROUTE_VRF_UNVERIFIED"
			if wanted {
				check.State = "failed"
				check.ReasonCode = strings.ToUpper(poolName) + "_ROUTE_OVERLAP"
			}
			return check
		}
	}
	check.State = "passed"
	check.ReasonCode = "OBSERVED_FABRIC_POOL_NONCONFLICTING"
	if owned != nil {
		check.ReasonCode = "OWNED_RESUME_FABRIC_ALLOCATIONS_VERIFIED"
	}
	check.Message = "No conflicting allocation was found in the observed switch addresses and route tables for the " + poolName + " pool; site and cluster IPAM allocation are separate checks."
	return check
}

func addressVRF(snapshot fabric.Snapshot, name string) (string, bool) {
	result := ""
	for _, port := range snapshot.Configured.Interfaces {
		if port.Name == name && port.VRF != nil {
			result = *port.VRF
		}
	}
	for _, port := range snapshot.Operational.Interfaces {
		if port.Name == name && port.VRF != nil {
			if result != "" && result != *port.VRF {
				return "", false
			}
			result = *port.VRF
		}
	}
	return result, result != ""
}

func ownedResumeRoute(route fabric.Route, ports map[string]bool) bool {
	if len(ports) == 0 || len(route.NextHops) == 0 || route.Protocol != "bgp" && route.Protocol != "186" || route.Type != "" && route.Type != "unicast" {
		return false
	}
	for _, hop := range route.NextHops {
		gateway, err := netip.ParseAddr(hop.Gateway)
		if !ports[hop.Interface] || err != nil || gateway.IsUnspecified() || gateway.IsMulticast() || gateway.IsLoopback() {
			return false
		}
	}
	return true
}
