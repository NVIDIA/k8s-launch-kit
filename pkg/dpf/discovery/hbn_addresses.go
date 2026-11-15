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
