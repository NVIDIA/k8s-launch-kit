// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package managementnetwork

import (
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func TestCheckNewTenantNetworkThreeNodeSelection(t *testing.T) {
	nodes := []inventory.Node{validNode("uid-a", "cp-a", "10.0.0.11"), validNode("uid-b", "cp-b", "10.0.0.12"), validNode("uid-c", "cp-c", "10.0.0.13")}
	checks := CheckNewTenantNetwork(nodes, validTenant())
	if len(checks) != 3 {
		t.Fatalf("checks=%d, want one per selected node: %#v", len(checks), checks)
	}
	for i, check := range checks {
		if check.ID != checkID || check.RequiredPhase != "deploy" || check.State != "passed" || check.ReasonCode != "TENANT_VIP_ONLINK_ROUTE_VERIFIED" {
			t.Fatalf("check[%d]=%#v, want successful deploy pre-create check", i, check)
		}
	}
	if !strings.Contains(checks[0].Subject, "cp-a") || checks[0].Vantage != "management-node/uid-a" {
		t.Fatalf("check subject/vantage not human-readable and identity-bound: %#v", checks[0])
	}
}

func TestCheckNewTenantNetworkRequiresExactSelectorMembership(t *testing.T) {
	selected := validNode("uid-a", "cp-a", "10.0.0.11")
	selected.Labels = map[string]string{"node-role.kubernetes.io/control-plane": ""}
	unselected := validNode("uid-b", "cp-b", "10.0.0.12")
	unselected.Labels = map[string]string{"other": "label"}
	unselected.Interfaces = nil
	checks := CheckNewTenantNetwork([]inventory.Node{unselected, selected}, validTenant())
	if len(checks) != 1 || checks[0].State != "passed" || !strings.Contains(checks[0].Subject, "cp-a") {
		t.Fatalf("known selector subset should cover only cp-a: %#v", checks)
	}

	missing := validNode("uid-c", "cp-c", "10.0.0.13")
	missing.Labels = nil // Saved inventory deliberately omits transient API labels.
	checks = CheckNewTenantNetwork([]inventory.Node{selected, missing}, validTenant())
	if len(checks) != 2 || checks[1].State != "unknown" || checks[1].ReasonCode != "TENANT_NODE_LABELS_UNAVAILABLE" {
		t.Fatalf("missing labels must preserve unknown selector membership: %#v", checks)
	}

	selected.Labels = map[string]string{"node-role.kubernetes.io/control-plane": "nonempty"}
	checks = CheckNewTenantNetwork([]inventory.Node{selected}, validTenant())
	if len(checks) != 1 || checks[0].State != "unknown" || checks[0].ReasonCode != "TENANT_NODE_SELECTION_EMPTY" {
		t.Fatalf("present label with wrong empty-value match should not select node: %#v", checks)
	}
}

func TestCheckNewTenantNetworkSelectionUnknown(t *testing.T) {
	tests := []struct {
		name  string
		nodes []inventory.Node
		want  string
	}{
		{name: "no candidates", want: "TENANT_CONTROL_PLANE_UNAVAILABLE"},
		{name: "incomplete identity", nodes: []inventory.Node{{ControlPlane: true, Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}}}, want: "TENANT_NODE_IDENTITY_INVALID"},
		{name: "duplicate uid", nodes: []inventory.Node{validNode("same", "cp-a", "10.0.0.11"), validNode("same", "cp-b", "10.0.0.12")}, want: "TENANT_NODE_IDENTITY_INVALID"},
		{name: "empty selector", nodes: []inventory.Node{validNode("uid-a", "cp-a", "10.0.0.11")}, want: "TENANT_NODE_SELECTOR_UNAVAILABLE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenant := validTenant()
			if tt.name == "empty selector" {
				tenant.NodeSelector = nil
			}
			checks := CheckNewTenantNetwork(tt.nodes, tenant)
			if len(checks) != 1 || checks[0].State != "unknown" || checks[0].ReasonCode != tt.want {
				t.Fatalf("checks=%#v, want one unknown %s", checks, tt.want)
			}
		})
	}
}

func TestCheckNewTenantNetworkExplicitFailuresAndUnknowns(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*inventory.Node, *bootstrap.Tenant)
		state  string
		reason string
	}{
		{name: "invalid VIP", mutate: func(_ *inventory.Node, in *bootstrap.Tenant) { in.VIP = "not-an-ip" }, state: "failed", reason: "TENANT_VIP_INVALID"},
		{name: "IPv6 VIP", mutate: func(_ *inventory.Node, in *bootstrap.Tenant) { in.VIP = "2001:db8::1" }, state: "failed", reason: "TENANT_VIP_INVALID"},
		{name: "network VIP", mutate: func(n *inventory.Node, in *bootstrap.Tenant) {
			in.VIP = "10.0.0.0"
			n.TenantVIPRoute.Destination = in.VIP
		}, state: "failed", reason: "TENANT_VIP_NOT_USABLE_HOST"},
		{name: "broadcast VIP", mutate: func(n *inventory.Node, in *bootstrap.Tenant) {
			in.VIP = "10.0.0.255"
			n.TenantVIPRoute.Destination = in.VIP
		}, state: "failed", reason: "TENANT_VIP_NOT_USABLE_HOST"},
		{name: "missing VIP input", mutate: func(_ *inventory.Node, in *bootstrap.Tenant) { in.VIP = "" }, state: "unknown", reason: "TENANT_VIP_UNAVAILABLE"},
		{name: "invalid interface", mutate: func(_ *inventory.Node, in *bootstrap.Tenant) { in.Interface = "bad/name" }, state: "failed", reason: "TENANT_INTERFACE_INVALID"},
		{name: "dot interface", mutate: func(_ *inventory.Node, in *bootstrap.Tenant) { in.Interface = "." }, state: "failed", reason: "TENANT_INTERFACE_INVALID"},
		{name: "missing interface input", mutate: func(_ *inventory.Node, in *bootstrap.Tenant) { in.Interface = "" }, state: "unknown", reason: "TENANT_INTERFACE_UNAVAILABLE"},
		{name: "not Ready", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.Ready = false }, state: "failed", reason: "TENANT_NODE_NOT_READY"},
		{name: "unschedulable", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.Unschedulable = true }, state: "failed", reason: "TENANT_NODE_UNSCHEDULABLE"},
		{name: "missing OS", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.OS = "" }, state: "unknown", reason: "TENANT_NODE_OS_UNAVAILABLE"},
		{name: "non Linux", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.OS = "windows" }, state: "failed", reason: "TENANT_NODE_OS_UNSUPPORTED"},
		{name: "missing interfaces", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.Interfaces = nil }, state: "unknown", reason: "TENANT_NODE_INTERFACES_UNAVAILABLE"},
		{name: "interface absent", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.Interfaces = []inventory.Interface{} }, state: "failed", reason: "TENANT_INTERFACE_NOT_FOUND"},
		{name: "duplicate interface", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.Interfaces = append(n.Interfaces, n.Interfaces[0]) }, state: "unknown", reason: "TENANT_INTERFACE_AMBIGUOUS"},
		{name: "bad interface index", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.Interfaces[0].Index = 0 }, state: "unknown", reason: "TENANT_INTERFACE_INDEX_INVALID"},
		{name: "unknown admin state", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.Interfaces[0].AdminUp = nil }, state: "unknown", reason: "TENANT_INTERFACE_ADMIN_STATE_UNAVAILABLE"},
		{name: "admin down", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { down := false; n.Interfaces[0].AdminUp = &down }, state: "failed", reason: "TENANT_INTERFACE_ADMIN_DOWN"},
		{name: "unknown oper state", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.Interfaces[0].OperState = "UNKNOWN" }, state: "unknown", reason: "TENANT_INTERFACE_OPER_STATE_UNAVAILABLE"},
		{name: "oper down", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.Interfaces[0].OperState = "DOWN" }, state: "failed", reason: "TENANT_INTERFACE_OPER_DOWN"},
		{name: "malformed oper state", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.Interfaces[0].OperState = "up-ish" }, state: "unknown", reason: "TENANT_INTERFACE_OPER_STATE_INVALID"},
		{name: "missing addresses", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.Interfaces[0].Addresses = nil }, state: "unknown", reason: "TENANT_NODE_ADDRESSES_UNAVAILABLE"},
		{name: "no IPv4 prefix", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.Interfaces[0].Addresses = []string{"2001:db8::10/64"} }, state: "failed", reason: "TENANT_VIP_NOT_ON_INTERFACE"},
		{name: "prefix excludes VIP", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.Interfaces[0].Addresses = []string{"10.0.1.11/24"} }, state: "failed", reason: "TENANT_VIP_NOT_ON_INTERFACE"},
		{name: "malformed address", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.Interfaces[0].Addresses = []string{"10.0.0.11/999"} }, state: "unknown", reason: "TENANT_NODE_ADDRESS_INVALID"},
		{name: "IPv4 mapped address prefix", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) {
			n.Interfaces[0].Addresses = []string{"::ffff:10.0.0.11/120"}
		}, state: "unknown", reason: "TENANT_NODE_ADDRESS_INVALID"},
		{name: "VIP assigned on selected node interface", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.Interfaces[1].Addresses = []string{"10.0.0.200/32"} }, state: "failed", reason: "TENANT_VIP_ALREADY_ASSIGNED"},
		{name: "route unavailable", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.TenantVIPRoute = nil }, state: "unknown", reason: "TENANT_ROUTE_UNAVAILABLE"},
		{name: "route read failed", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) {
			n.TenantVIPRoute.ReasonCode = "ROUTE_LOOKUP_FAILED"
			n.TenantVIPRoute.Message = "private raw text"
		}, state: "unknown", reason: "ROUTE_LOOKUP_FAILED"},
		{name: "unsupported route reason", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.TenantVIPRoute.ReasonCode = "COMMAND_FAILED" }, state: "unknown", reason: "TENANT_ROUTE_UNAVAILABLE"},
		{name: "message without route reason", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.TenantVIPRoute.Message = "private raw text" }, state: "unknown", reason: "TENANT_ROUTE_INVALID"},
		{name: "unsafe route reason", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.TenantVIPRoute.ReasonCode = "bad reason\nsecret" }, state: "unknown", reason: "TENANT_ROUTE_UNAVAILABLE"},
		{name: "wrong route destination", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.TenantVIPRoute.Destination = "10.0.0.201" }, state: "failed", reason: "TENANT_ROUTE_DESTINATION_MISMATCH"},
		{name: "missing route interface", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.TenantVIPRoute.InterfaceIndex = 0 }, state: "unknown", reason: "TENANT_ROUTE_INTERFACE_UNAVAILABLE"},
		{name: "wrong route interface", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.TenantVIPRoute.InterfaceIndex = 8 }, state: "failed", reason: "TENANT_ROUTE_INTERFACE_MISMATCH"},
		{name: "gateway on on-link VIP", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.TenantVIPRoute.Gateway = "10.0.0.1" }, state: "failed", reason: "TENANT_ROUTE_NOT_ONLINK"},
		{name: "missing route source", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) { n.TenantVIPRoute.Source = "" }, state: "unknown", reason: "TENANT_ROUTE_SOURCE_UNAVAILABLE"},
		{name: "route source on other interface", mutate: func(n *inventory.Node, _ *bootstrap.Tenant) {
			n.Interfaces[1].Addresses = []string{"10.0.0.12/24"}
			n.TenantVIPRoute.Source = "10.0.0.12"
		}, state: "failed", reason: "TENANT_ROUTE_SOURCE_MISMATCH"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node, tenant := validNode("uid-a", "cp-a", "10.0.0.11"), validTenant()
			tt.mutate(&node, &tenant)
			checks := CheckNewTenantNetwork([]inventory.Node{node}, tenant)
			if len(checks) != 1 || checks[0].State != tt.state || checks[0].ReasonCode != tt.reason {
				t.Fatalf("checks=%#v, want %s/%s", checks, tt.state, tt.reason)
			}
			if tt.name == "unsafe route reason" && strings.Contains(checks[0].Message, "secret") {
				t.Fatalf("unsafe source message leaked: %#v", checks[0])
			}
		})
	}
}

func TestCheckNewTenantNetworkAllowsPointToPointPrefix(t *testing.T) {
	node := validNode("uid-a", "cp-a", "10.0.0.11")
	node.Interfaces[0].Addresses = []string{"10.0.0.11/31"}
	tenant := validTenant()
	tenant.VIP = "10.0.0.10"
	node.TenantVIPRoute.Destination = tenant.VIP
	checks := CheckNewTenantNetwork([]inventory.Node{node}, tenant)
	if len(checks) != 1 || checks[0].State != "passed" {
		t.Fatalf("/31 point-to-point prefix should allow both addresses: %#v", checks)
	}
}

func TestCheckNewTenantNetworkRejectsVIPOnAnySelectedNode(t *testing.T) {
	first := validNode("uid-a", "cp-a", "10.0.0.11")
	second := validNode("uid-b", "cp-b", "10.0.0.12")
	second.Interfaces[1].Addresses = []string{"10.0.0.200/32"}
	checks := CheckNewTenantNetwork([]inventory.Node{first, second}, validTenant())
	if len(checks) != 2 || checks[0].State != "passed" || checks[1].State != "failed" || checks[1].ReasonCode != "TENANT_VIP_ALREADY_ASSIGNED" {
		t.Fatalf("selected-node VIP occupancy not attributed: %#v", checks)
	}
}

func validTenant() bootstrap.Tenant {
	return bootstrap.Tenant{Name: "tenant-a", VIP: "10.0.0.200", Interface: "mgmt0", NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""}}
}

func validNode(uid, name, address string) inventory.Node {
	adminUp := true
	return inventory.Node{
		Name: name, UID: uid, ControlPlane: true, Ready: true, OS: "linux",
		Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
		Interfaces: []inventory.Interface{
			{Name: "mgmt0", Index: 3, AdminUp: &adminUp, OperState: "UP", Addresses: []string{address + "/24"}},
			{Name: "eth9", Index: 4, AdminUp: &adminUp, OperState: "UP", Addresses: []string{}},
		},
		TenantVIPRoute: &inventory.RouteLookup{Destination: "10.0.0.200", InterfaceIndex: 3, Source: address},
	}
}
