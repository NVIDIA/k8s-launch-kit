// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"slices"
	"strconv"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// Validation distinguishes unavailable observations from measured configuration
// failures. Discovery's shared predicates remain authoritative for values; this
// adapter supplies their finite source dependencies, not alternate defaults.
func validationFabricConfigurationChecks(inv *inventory.Inventory) []inventory.Check {
	checks := discovery.HBNFabricConfigurationChecks(inv)
	if inv == nil || inv.HBN == nil {
		return checks
	}
	issuesBySwitch := make(map[string][]fabric.Issue, len(inv.HBN.Switches))
	for _, sw := range inv.HBN.Switches {
		issuesBySwitch[sw.Name] = validationRelevantSwitchIssues(sw, inv, checks)
	}
	for i := range checks {
		check := &checks[i]
		for _, sw := range inv.HBN.Switches {
			if check.Subject != sw.Name && !strings.HasPrefix(check.Subject, sw.Name+"/") {
				continue
			}
			port := strings.TrimPrefix(check.Subject, sw.Name+"/")
			inputs := fabricCheckInputs(check.ID, port)
			if len(inputs) == 0 {
				continue
			} // Declared scope and already-typed unknown checks.
			var unavailable []fabric.Issue
			for _, issue := range issuesBySwitch[sw.Name] {
				if issue.Code == "not_configured" || issue.Code == "interface_not_listed" || issue.Code == "conflicting_observation" {
					continue
				}
				if issue.Input == "connection" {
					unavailable = append(unavailable, issue)
					break
				}
				for _, input := range inputs {
					if issue.Input == input {
						unavailable = append(unavailable, issue)
					}
				}
			}
			if len(unavailable) > 0 && !definitiveFabricMismatch(*check, sw, inv) {
				check.State, check.ReasonCode = "unknown", "SWITCH_SOURCE_UNAVAILABLE"
				check.Message = "This check could not be evaluated: " + validationSwitchProblems(unavailable)
			}
		}
	}
	return checks
}

func fabricCheckInputs(id, port string) []string {
	switch strings.TrimPrefix(id, "dpf.hbn.") {
	case "bgp-enabled":
		return []string{"appliedRouter"}
	case "vrf", "vrf-bgp", "ipv4-unicast", "neighbor":
		return []string{"appliedVRFs"}
	case "fabric-asn":
		return []string{"appliedVRFs", "appliedRouter"}
	case "port":
		return []string{"appliedInterfaces", "appliedInterface/" + port}
	case "link":
		return []string{"operationalInterfaces", "operationalInterface/" + port}
	case "port-mtu", "port-vrf":
		return []string{"appliedInterfaces", "appliedInterface/" + port, "operationalInterfaces", "operationalInterface/" + port}
	case "routed-port":
		return []string{"appliedInterfaces", "appliedInterface/" + port, "operationalInterfaces", "operationalInterface/" + port, "appliedBridge", "operationalBridge"}
	default:
		return nil
	}
}

// Explicit contradictions from an independently successful source stay failures
// even when another source is absent. Absence itself is never a contradiction.
func definitiveFabricMismatch(check inventory.Check, sw inventory.FabricSwitch, inv *inventory.Inventory) bool {
	if check.State != "failed" {
		return false
	}
	port := strings.TrimPrefix(check.Subject, sw.Name+"/")
	available := func(inputs ...string) bool {
		for _, issue := range sw.Snapshot.Issues {
			if issue.Input == "connection" {
				return false
			}
			for _, input := range inputs {
				if issue.Input == input {
					return false
				}
			}
		}
		return true
	}
	configured := available("appliedInterfaces", "appliedInterface/"+port)
	operational := available("operationalInterfaces", "operationalInterface/"+port)
	profile := inv.HBN.Profile
	mismatch := func(mtu *int, vrf *string) bool {
		return check.ID == "dpf.hbn.port-mtu" && mtu != nil && *mtu < profile.UplinkMTU || check.ID == "dpf.hbn.port-vrf" && vrf != nil && *vrf != profile.FabricVRF
	}
	for _, p := range sw.Snapshot.Configured.Interfaces {
		if p.Name == port && configured {
			if mismatch(p.MTU, p.VRF) {
				return true
			}
			if check.ID == "dpf.hbn.routed-port" && p.BridgeDomain != nil && *p.BridgeDomain != "" && *p.BridgeDomain != "none" {
				return true
			}
		}
	}
	for _, p := range sw.Snapshot.Operational.Interfaces {
		if p.Name == port && operational && mismatch(p.MTU, p.VRF) {
			return true
		}
	}
	if check.ID == "dpf.hbn.routed-port" {
		if available("appliedBridge") {
			for _, b := range sw.Snapshot.Configured.Bridges {
				for _, p := range b.Ports {
					if p.Name == port {
						return true
					}
				}
			}
		}
		if available("operationalBridge") {
			for _, b := range sw.Snapshot.Operational.Bridges {
				for _, p := range b.Ports {
					if p.Name == port {
						return true
					}
				}
			}
		}
	}
	return false
}

func fabricValidationTitle(check inventory.Check) string {
	titles := map[string]string{
		"asn-allocation": "DPU ASN allocation", "switch-access": "Declared switch access", "switch-inputs": "Switch and uplink declarations", "fabric-devices": "Selected physical DPUs", "fabric-revision": "Active and saved switch configuration", "bgp-enabled": "Switch BGP enabled", "vrf": "Fabric VRF exists", "vrf-bgp": "Fabric VRF BGP enabled", "fabric-asn": "Fabric ASN", "ipv4-unicast": "IPv4 unicast address family", "port-scope": "Uplink belongs to a selected DPU", "port": "Configured physical uplink", "port-mtu": "Uplink MTU", "port-vrf": "Uplink VRF", "routed-port": "Uplink uses routed mode", "link": "Uplink administrative and operational state", "neighbor": "Unnumbered BGP peer configuration", "cabling": "DPU uplink cabling", "uplink-bindings": "Both DPU uplinks have unique switch bindings",
	}
	id := strings.TrimPrefix(check.ID, "dpf.hbn.")
	if title := titles[id]; title != "" {
		return title
	}
	if strings.HasPrefix(id, "routing-policy.") {
		return "Effective BGP routing policy"
	}
	return "Fabric configuration"
}

func validationSwitchProblems(issues []fabric.Issue) string {
	messages := []string{}
	for _, issue := range issues {
		if issue.Input == "asic" {
			continue
		}
		message := issue.Message
		if message == "" {
			message = "A required switch observation is unavailable."
		}
		if !slices.Contains(messages, message) {
			messages = append(messages, message)
		}
	}
	return strings.Join(messages, " ")
}

// Only sources consumed by the selected configuration/cabling predicates can
// block this phase. Occupancy/route diagnostics belong to other lifecycle gates.
func validationRelevantSwitchIssues(sw inventory.FabricSwitch, inv *inventory.Inventory, checks []inventory.Check) []fabric.Issue {
	var result []fabric.Issue
	cablingComplete := true
	for _, check := range checks {
		if check.ID == "dpf.hbn.cabling" && strings.HasPrefix(check.Subject, sw.Name+"/") && check.State != "passed" {
			cablingComplete = false
		}
	}
	selectedField := func(field string, names []string) bool {
		if !strings.HasPrefix(field, "$[") {
			return true
		}
		for _, name := range names {
			prefix := "$[" + strconv.Quote(name) + "]"
			if field == prefix || strings.HasPrefix(field, prefix+".") {
				return true
			}
		}
		return false
	}
	var ports []string
	for _, port := range sw.Ports {
		ports = append(ports, port.Name)
	}
	for _, issue := range sw.Snapshot.Issues {
		switch issue.Input {
		case "asic", "operationalAddresses", "operationalRoutesIPv4", "operationalRoutesIPv6", "operationalNextHopsBefore", "operationalNextHopsAfter", "frrBGPSummary":
			continue
		case "lldp", "localLLDP":
			if cablingComplete {
				continue
			}
		case "appliedVRFs":
			if !selectedField(issue.Field, []string{inv.HBN.Profile.FabricVRF}) {
				continue
			}
		case "appliedInterfaces", "operationalInterfaces":
			if !selectedField(issue.Field, ports) {
				continue
			}
		}
		result = append(result, issue)
	}
	return result
}
