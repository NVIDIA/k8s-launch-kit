// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package managementnetwork checks fresh control-plane network observations
// needed before creating a new HBN tenant.
package managementnetwork

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

const checkID = "dpf.hbn.management.tenant-network"

// CheckNewTenantNetwork checks that the exact control-plane nodes selected for
// a new tenant have a usable on-link interface for its VIP. It reads only the
// supplied observations; it does not reserve an address or prove VRID, L2, or
// fabric readiness. Existing-tenant reuse needs ownership-aware checks instead.
func CheckNewTenantNetwork(nodes []inventory.Node, tenant bootstrap.Tenant) []inventory.Check {
	if tenant.VIP == "" {
		return []inventory.Check{selectionCheck(tenant, "unknown", "TENANT_VIP_UNAVAILABLE", "The requested tenant VIP is not configured.")}
	}
	vip, err := netip.ParseAddr(tenant.VIP)
	if err != nil || !vip.Is4() || !vip.IsGlobalUnicast() {
		return []inventory.Check{selectionCheck(tenant, "failed", "TENANT_VIP_INVALID", "The requested tenant VIP must be a literal global-unicast IPv4 address.")}
	}
	if tenant.Interface == "" {
		return []inventory.Check{selectionCheck(tenant, "unknown", "TENANT_INTERFACE_UNAVAILABLE", "The requested tenant interface is not configured.")}
	}
	if !validInterfaceName(tenant.Interface) {
		return []inventory.Check{selectionCheck(tenant, "failed", "TENANT_INTERFACE_INVALID", "The requested tenant interface name is invalid.")}
	}
	if len(tenant.NodeSelector) == 0 {
		return []inventory.Check{selectionCheck(tenant, "unknown", "TENANT_NODE_SELECTOR_UNAVAILABLE", "A nonempty tenant control-plane node selector is required to identify the exact network scope.")}
	}

	candidates := make([]inventory.Node, 0, len(nodes))
	for _, node := range nodes {
		if node.ControlPlane {
			candidates = append(candidates, node)
		}
	}
	if len(candidates) == 0 {
		return []inventory.Check{selectionCheck(tenant, "unknown", "TENANT_CONTROL_PLANE_UNAVAILABLE", "No current control-plane nodes were observed for tenant network selection.")}
	}
	if !validCandidateIdentities(candidates) {
		return []inventory.Check{selectionCheck(tenant, "unknown", "TENANT_NODE_IDENTITY_INVALID", "Control-plane node identities are missing or duplicated; tenant network selection is ambiguous.")}
	}

	selected := make([]inventory.Node, 0, len(candidates))
	checks := make([]inventory.Check, 0, len(candidates))
	for _, node := range candidates {
		if node.Labels == nil {
			checks = append(checks, nodeCheck(tenant, node, "unknown", "TENANT_NODE_LABELS_UNAVAILABLE", "Fresh node labels are unavailable, so selector membership cannot be determined."))
			continue
		}
		if matchesSelector(node.Labels, tenant.NodeSelector) {
			selected = append(selected, node)
		}
	}
	if len(selected) == 0 {
		if len(checks) != 0 {
			sortChecks(checks)
			return checks
		}
		return []inventory.Check{selectionCheck(tenant, "unknown", "TENANT_NODE_SELECTION_EMPTY", "No current control-plane node matches the tenant node selector.")}
	}

	slices.SortFunc(selected, func(a, b inventory.Node) int { return strings.Compare(a.UID, b.UID) })
	occupiedOn := occupiedVIPNodes(selected, vip)
	for _, node := range selected {
		checks = append(checks, checkNode(tenant, node, vip, occupiedOn))
	}
	sortChecks(checks)
	return checks
}

func validCandidateIdentities(nodes []inventory.Node) bool {
	names, uids := map[string]struct{}{}, map[string]struct{}{}
	for _, node := range nodes {
		if node.Name == "" || node.UID == "" {
			return false
		}
		if _, ok := names[node.Name]; ok {
			return false
		}
		if _, ok := uids[node.UID]; ok {
			return false
		}
		names[node.Name] = struct{}{}
		uids[node.UID] = struct{}{}
	}
	return true
}

func sortChecks(checks []inventory.Check) {
	slices.SortFunc(checks, func(a, b inventory.Check) int {
		return strings.Compare(a.Vantage, b.Vantage)
	})
}

func matchesSelector(labels, selector map[string]string) bool {
	for key, wanted := range selector {
		actual, exists := labels[key]
		if !exists || actual != wanted {
			return false
		}
	}
	return true
}

func checkNode(tenant bootstrap.Tenant, node inventory.Node, vip netip.Addr, occupiedOn map[string]bool) inventory.Check {
	if !node.Ready {
		return nodeCheck(tenant, node, "failed", "TENANT_NODE_NOT_READY", "The selected management control-plane node is not Ready.")
	}
	if node.Unschedulable {
		return nodeCheck(tenant, node, "failed", "TENANT_NODE_UNSCHEDULABLE", "The selected management control-plane node is unschedulable.")
	}
	if node.OS == "" {
		return nodeCheck(tenant, node, "unknown", "TENANT_NODE_OS_UNAVAILABLE", "The selected management node operating system was not observed.")
	}
	if !strings.EqualFold(node.OS, "linux") {
		return nodeCheck(tenant, node, "failed", "TENANT_NODE_OS_UNSUPPORTED", "The selected management control-plane node is not Linux.")
	}
	if occupiedOn[node.UID] {
		return nodeCheck(tenant, node, "failed", "TENANT_VIP_ALREADY_ASSIGNED", "The requested tenant VIP is already assigned to an interface on a selected control-plane node.")
	}
	if node.Interfaces == nil {
		return nodeCheck(tenant, node, "unknown", "TENANT_NODE_INTERFACES_UNAVAILABLE", "Fresh management interface observations are unavailable on the selected node.")
	}

	var selectedInterface *inventory.Interface
	for i := range node.Interfaces {
		if node.Interfaces[i].Name == tenant.Interface {
			if selectedInterface != nil {
				return nodeCheck(tenant, node, "unknown", "TENANT_INTERFACE_AMBIGUOUS", "The requested interface name occurs more than once in the node observation.")
			}
			selectedInterface = &node.Interfaces[i]
		}
	}
	if selectedInterface == nil {
		return nodeCheck(tenant, node, "failed", "TENANT_INTERFACE_NOT_FOUND", "The requested tenant interface is absent from the selected node.")
	}
	if selectedInterface.Index <= 0 || duplicateInterfaceIndex(node.Interfaces, selectedInterface.Index) {
		return nodeCheck(tenant, node, "unknown", "TENANT_INTERFACE_INDEX_INVALID", "The selected interface index is missing or ambiguous.")
	}
	if selectedInterface.AdminUp == nil {
		return nodeCheck(tenant, node, "unknown", "TENANT_INTERFACE_ADMIN_STATE_UNAVAILABLE", "The selected interface administrative state was not observed.")
	}
	if !*selectedInterface.AdminUp {
		return nodeCheck(tenant, node, "failed", "TENANT_INTERFACE_ADMIN_DOWN", "The selected tenant interface is administratively down.")
	}
	switch strings.ToUpper(selectedInterface.OperState) {
	case "UP":
	case "DOWN", "LOWERLAYERDOWN", "DORMANT", "NOTPRESENT":
		return nodeCheck(tenant, node, "failed", "TENANT_INTERFACE_OPER_DOWN", "The selected tenant interface is not operationally up.")
	case "", "UNKNOWN":
		return nodeCheck(tenant, node, "unknown", "TENANT_INTERFACE_OPER_STATE_UNAVAILABLE", "The selected interface operational state was not observed as up or down.")
	default:
		return nodeCheck(tenant, node, "unknown", "TENANT_INTERFACE_OPER_STATE_INVALID", "The selected interface has an unsupported operational-state value.")
	}

	addresses, usablePrefix, unusableVIPHost, addressState := inspectInterfaceAddresses(node.Interfaces, tenant.Interface, vip)
	if addressState != "" {
		return nodeCheck(tenant, node, "unknown", addressState, "Selected-node IPv4 address observations are missing or malformed.")
	}
	if unusableVIPHost {
		return nodeCheck(tenant, node, "failed", "TENANT_VIP_NOT_USABLE_HOST", "The requested VIP is a subnet network or broadcast address on the selected interface.")
	}
	if !usablePrefix {
		return nodeCheck(tenant, node, "failed", "TENANT_VIP_NOT_ON_INTERFACE", "No usable IPv4 prefix on the selected interface contains the requested VIP.")
	}
	if routeState, reason, message := checkRoute(node.TenantVIPRoute, vip, selectedInterface.Index, addresses); routeState != "passed" {
		return nodeCheck(tenant, node, routeState, reason, message)
	}
	return nodeCheck(tenant, node, "passed", "TENANT_VIP_ONLINK_ROUTE_VERIFIED", "The selected node has the requested interface up with an on-link IPv4 prefix and route source for the tenant VIP.")
}

// inspectInterfaceAddresses returns interface IPv4 addresses keyed by exact
// host address, whether a valid on-link prefix contains the VIP, and an
// observation error code when any selected-node address data is malformed.
func inspectInterfaceAddresses(interfaces []inventory.Interface, wanted string, vip netip.Addr) (map[netip.Addr]struct{}, bool, bool, string) {
	addresses := map[netip.Addr]struct{}{}
	usable := false
	unusableVIPHost := false
	for _, iface := range interfaces {
		if iface.Addresses == nil {
			return nil, false, false, "TENANT_NODE_ADDRESSES_UNAVAILABLE"
		}
		for _, raw := range iface.Addresses {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				return nil, false, false, "TENANT_NODE_ADDRESS_INVALID"
			}
			if prefix.Addr().Is4In6() {
				return nil, false, false, "TENANT_NODE_ADDRESS_INVALID"
			}
			if prefix.Addr().Is6() {
				continue
			}
			if !prefix.Addr().Is4() {
				return nil, false, false, "TENANT_NODE_ADDRESS_INVALID"
			}
			addr := prefix.Addr().Unmap()
			if iface.Name == wanted {
				addresses[addr] = struct{}{}
				if prefix.Bits() > 0 && addr.IsGlobalUnicast() && prefix.Contains(vip) {
					if !usableVIPHost(prefix, vip) {
						unusableVIPHost = true
					}
					usable = true
				}
			}
		}
	}
	return addresses, usable, unusableVIPHost, ""
}

func usableVIPHost(prefix netip.Prefix, vip netip.Addr) bool {
	if prefix.Bits() >= 31 {
		return true
	}
	network := binary.BigEndian.Uint32(prefix.Masked().Addr().AsSlice())
	mask := ^uint32(0) << uint(32-prefix.Bits())
	broadcast := network | ^mask
	address := binary.BigEndian.Uint32(vip.AsSlice())
	return address != network && address != broadcast
}

func occupiedVIPNodes(nodes []inventory.Node, vip netip.Addr) map[string]bool {
	occupied := map[string]bool{}
	for _, node := range nodes {
		for _, iface := range node.Interfaces {
			for _, raw := range iface.Addresses {
				if prefix, err := netip.ParsePrefix(raw); err == nil && prefix.Addr().Is4() && prefix.Addr().Unmap() == vip {
					occupied[node.UID] = true
				} else if addr, err := netip.ParseAddr(raw); err == nil && addr.Is4() && addr.Unmap() == vip {
					occupied[node.UID] = true
				}
			}
		}
	}
	return occupied
}

func checkRoute(route *inventory.RouteLookup, vip netip.Addr, interfaceIndex int, addresses map[netip.Addr]struct{}) (string, string, string) {
	if route == nil {
		return "unknown", "TENANT_ROUTE_UNAVAILABLE", "The node route lookup for the requested tenant VIP was not observed."
	}
	if route.ReasonCode != "" {
		code := route.ReasonCode
		if !safeReasonCode(code) {
			code = "TENANT_ROUTE_UNAVAILABLE"
		}
		return "unknown", code, "The node kernel could not resolve a route to the requested tenant VIP; inspect node routing."
	}
	if route.Message != "" {
		return "unknown", "TENANT_ROUTE_INVALID", "The node route observation contains an error message without a classified reason."
	}
	destination, err := netip.ParseAddr(route.Destination)
	if err != nil || !destination.Is4() || destination.Is4In6() {
		return "unknown", "TENANT_ROUTE_INVALID", "The node route lookup returned a malformed destination."
	}
	if destination.Unmap() != vip {
		return "failed", "TENANT_ROUTE_DESTINATION_MISMATCH", "The observed route lookup destination does not match the requested tenant VIP."
	}
	if route.InterfaceIndex <= 0 {
		return "unknown", "TENANT_ROUTE_INTERFACE_UNAVAILABLE", "The node route lookup did not identify an output interface."
	}
	if route.InterfaceIndex != interfaceIndex {
		return "failed", "TENANT_ROUTE_INTERFACE_MISMATCH", "The route to the tenant VIP uses a different interface than the requested tenant endpoint."
	}
	if route.Gateway != "" {
		gateway, err := netip.ParseAddr(route.Gateway)
		if err != nil || !gateway.Is4() {
			return "unknown", "TENANT_ROUTE_GATEWAY_INVALID", "The node route lookup returned a malformed next hop."
		}
		return "failed", "TENANT_ROUTE_NOT_ONLINK", "The requested tenant VIP is expected to be on-link but the route uses a gateway."
	}
	source, err := netip.ParseAddr(route.Source)
	if err != nil || !source.Is4() {
		return "unknown", "TENANT_ROUTE_SOURCE_UNAVAILABLE", "The node route lookup did not return a valid IPv4 source address."
	}
	if _, exists := addresses[source.Unmap()]; !exists {
		return "failed", "TENANT_ROUTE_SOURCE_MISMATCH", "The route source is not assigned to an observed interface on the selected node."
	}
	return "passed", "TENANT_ROUTE_VERIFIED", ""
}

func duplicateInterfaceIndex(interfaces []inventory.Interface, index int) bool {
	count := 0
	for _, iface := range interfaces {
		if iface.Index == index {
			count++
		}
	}
	return count != 1
}

func nodeCheck(tenant bootstrap.Tenant, node inventory.Node, state, reason, message string) inventory.Check {
	return inventory.Check{ID: checkID, Subject: fmt.Sprintf("tenant %s on control-plane node %s", tenant.Name, node.Name), Vantage: "management-node/" + node.UID, RequiredPhase: "deploy", State: state, ReasonCode: reason, Message: message, EvidenceRefs: []string{}}
}

func selectionCheck(tenant bootstrap.Tenant, state, reason, message string) inventory.Check {
	subject := "tenant network selection"
	if tenant.Name != "" {
		subject = "tenant " + tenant.Name + " network selection"
	}
	return inventory.Check{ID: checkID, Subject: subject, Vantage: "management-cluster", RequiredPhase: "deploy", State: state, ReasonCode: reason, Message: message, EvidenceRefs: []string{}}
}

func validInterfaceName(name string) bool {
	if len(name) == 0 || len(name) > 15 || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.:-", r) {
			return false
		}
	}
	return true
}

func safeReasonCode(code string) bool {
	switch code {
	case "ROUTE_LOOKUP_TIMEOUT", "ROUTE_LOOKUP_FAILED", "ROUTE_INTERFACE_UNAVAILABLE":
		return true
	default:
		return false
	}
}
