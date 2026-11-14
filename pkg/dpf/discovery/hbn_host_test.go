// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func TestHostHBNChecksRequireExactAssociationAndDoNotMutate(t *testing.T) {
	inv := hostHBNTestInventory()
	before := rawHostHBNTestInventory(t, inv)
	checks := hostHBNChecks(&inv)
	if got := hostHBNCheck(t, checks, "host-association", "SERIAL-1 @ worker-01"); got.State != "passed" {
		t.Fatalf("exact association state=%s reason=%s", got.State, got.ReasonCode)
	}
	after := rawHostHBNTestInventory(t, inv)
	if !bytes.Equal(after, before) {
		t.Fatal("hostHBNChecks mutated its input inventory")
	}

	inv.Associations = nil
	checks = hostHBNChecks(&inv)
	if got := hostHBNCheck(t, checks, "host-association", "SERIAL-1"); got.State != "unknown" || got.ReasonCode != "HOST_ASSOCIATION_UNVERIFIED" {
		t.Fatalf("missing association = %#v", got)
	}

	inv = hostHBNTestInventory()
	inv.Associations[0].SerialNumber = "OTHER-SERIAL"
	checks = hostHBNChecks(&inv)
	if got := hostHBNCheck(t, checks, "host-association", "SERIAL-1"); got.State != "failed" || got.ReasonCode != "HOST_ASSOCIATION_CONFLICT" {
		t.Fatalf("contradictory association = %#v", got)
	}
}

func TestHostHBNChecksSelectedFunctionsAndIOMMUZeroOrUnknown(t *testing.T) {
	inv := hostHBNTestInventory()
	checks := hostHBNChecks(&inv)
	if got := hostHBNCheck(t, checks, "host-pf", "SERIAL-1 @ worker-01 / 0000:21:00.0"); got.State != "passed" {
		t.Fatalf("selected PF state=%s reason=%s", got.State, got.ReasonCode)
	}
	if got := hostHBNCheck(t, checks, "host-iommu-group", "SERIAL-1 @ worker-01 / 0000:21:00.0"); got.State != "passed" {
		t.Fatalf("IOMMU group zero should pass: %#v", got)
	}

	inv.Hosts[0].Adapters[0].PCIFunctions[1].IOMMUGroup = nil
	inv.Hosts[0].Runtime.Observations["host-pf:0000:21:00.1:iommu-group"] = inventory.Observation{State: "unknown", ReasonCode: "IOMMU_GROUP_ABSENT"}
	checks = hostHBNChecks(&inv)
	if got := hostHBNCheck(t, checks, "host-iommu-group", "SERIAL-1 @ worker-01 / 0000:21:00.1"); got.State != "unknown" || got.ReasonCode != "IOMMU_GROUP_ABSENT" {
		t.Fatalf("absent IOMMU group must preserve the collector reason: %#v", got)
	}

	inv.Hosts[0].Adapters[0].PCIFunctions[0].Driver = ""
	inv.Hosts[0].Runtime.Observations["host-pf:0000:21:00.0:driver"] = inventory.Observation{State: "unavailable", ReasonCode: "PERMISSION_DENIED"}
	checks = hostHBNChecks(&inv)
	if got := hostHBNCheck(t, checks, "host-pf", "SERIAL-1 @ worker-01 / 0000:21:00.0"); got.State != "unknown" || got.ReasonCode != "PERMISSION_DENIED" {
		t.Fatalf("driver read failure must preserve the collector reason: %#v", got)
	}

	inv = hostHBNTestInventory()
	inv.Hosts[0].Runtime.Observations["host-pf:0000:21:00.1:netdevs"] = inventory.Observation{State: "unavailable", ReasonCode: "SYSFS_READ_FAILED"}
	checks = hostHBNChecks(&inv)
	if got := hostHBNCheck(t, checks, "host-pf", "SERIAL-1 @ worker-01 / 0000:21:00.1"); got.State != "unknown" || got.ReasonCode != "SYSFS_READ_FAILED" {
		t.Fatalf("network-interface read failure must preserve the collector reason: %#v", got)
	}
}

func TestHostHBNChecksIgnoreUnselectedNICForManagementProtection(t *testing.T) {
	inv := hostHBNTestInventory()
	inv.Hosts[0].Adapters = append(inv.Hosts[0].Adapters, inventory.Adapter{
		Key: "unselected-adapter", SerialNumber: "OTHER-SERIAL", PCIFunctions: []inventory.PCIFunction{{
			Address: "0000:41:00.0", Driver: "", Netdevs: []string{"management0"}, IOMMUGroup: nil,
		}},
	})
	inv.Hosts[0].Runtime.Interfaces = append(inv.Hosts[0].Runtime.Interfaces, inventory.Interface{Name: "management0", Index: 20, MTU: 1500, Addresses: []string{"192.0.2.12/24"}})
	inv.Hosts[0].Runtime.Network.Links = append(inv.Hosts[0].Runtime.Network.Links, inventory.NetworkLink{Name: "management0"})
	inv.Hosts[0].Runtime.Routes = append(inv.Hosts[0].Runtime.Routes, inventory.Route{Destination: "0.0.0.0/0", Gateway: "192.0.2.1", InterfaceIndex: 20, Table: 254})

	checks := hostHBNChecks(&inv)
	if got := hostHBNCheck(t, checks, "host-management-separation", "SERIAL-1 @ worker-01"); got.State != "passed" {
		t.Fatalf("unselected NIC management path blocked selected adapter: %#v", got)
	}
	for _, check := range checks {
		if check.Subject == "SERIAL-1 @ worker-01 / 0000:41:00.0" {
			t.Fatalf("unselected NIC was evaluated as a selected PF: %#v", check)
		}
	}
}

func TestHostHBNChecksProtectIPv4AndIPv6DefaultRoutesOnSelectedPath(t *testing.T) {
	for _, test := range []struct {
		name, destination, reason string
	}{
		{name: "IPv4 default on selected PF", destination: "0.0.0.0/0", reason: "HOST_IPV4_DEFAULT_ROUTE_ON_SELECTED_PATH"},
		{name: "IPv6 default on selected PF", destination: "::/0", reason: "HOST_IPV6_DEFAULT_ROUTE_ON_SELECTED_PATH"},
	} {
		t.Run(test.name, func(t *testing.T) {
			inv := hostHBNTestInventory()
			inv.Hosts[0].Runtime.Routes = []inventory.Route{{Destination: test.destination, Gateway: "192.0.2.1", InterfaceIndex: 10, Table: 254}}
			checks := hostHBNChecks(&inv)
			if got := hostHBNCheck(t, checks, "host-management-separation", "SERIAL-1 @ worker-01"); got.State != "failed" || got.ReasonCode != test.reason {
				t.Fatalf("selected default route is not protected: %#v", got)
			}
		})
	}
}

func TestHostHBNChecksFollowBridgeAndBondAncestors(t *testing.T) {
	t.Run("bridge ancestor address", func(t *testing.T) {
		inv := hostHBNTestInventory()
		setHostMaster(inv.Hosts[0].Runtime, "hostpf0", "br-mgmt", "bridge", 20)
		inv.Hosts[0].Runtime.Interfaces[2].Addresses = []string{"2001:db8:1::4/64"}
		checks := hostHBNChecks(&inv)
		if got := hostHBNCheck(t, checks, "host-management-separation", "SERIAL-1 @ worker-01"); got.State != "failed" || got.ReasonCode != "HOST_ADDRESS_ON_SELECTED_PATH" {
			t.Fatalf("bridge ancestor address was not protected: %#v", got)
		}
	})

	t.Run("bond ancestor default route", func(t *testing.T) {
		inv := hostHBNTestInventory()
		setHostMaster(inv.Hosts[0].Runtime, "hostpf0", "bond0", "bond", 20)
		inv.Hosts[0].Runtime.Routes = []inventory.Route{{Destination: "::/0", Gateway: "fe80::1", InterfaceIndex: 20, Table: 254}}
		checks := hostHBNChecks(&inv)
		if got := hostHBNCheck(t, checks, "host-management-separation", "SERIAL-1 @ worker-01"); got.State != "failed" || got.ReasonCode != "HOST_IPV6_DEFAULT_ROUTE_ON_SELECTED_PATH" {
			t.Fatalf("bond ancestor route was not protected: %#v", got)
		}
	})
}

func TestHostHBNChecksIncompleteFactsCannotPassAndKeepSourceReason(t *testing.T) {
	inv := hostHBNTestInventory()
	inv.Hosts[0].Runtime.Observations["routes"] = inventory.Observation{State: "unavailable", ReasonCode: "API_ACCESS_DENIED"}
	checks := hostHBNChecks(&inv)
	if got := hostHBNCheck(t, checks, "host-management-separation", "SERIAL-1 @ worker-01"); got.State != "unknown" || got.ReasonCode != "API_ACCESS_DENIED" {
		t.Fatalf("incomplete combined routes cannot pass and must retain source reason: %#v", got)
	}
	if got := hostHBNCheck(t, checks, "host-configuration", "SERIAL-1 @ worker-01"); got.State != "unknown" || got.ReasonCode != "HOST_CONFIGURATION_NOT_VERIFIED" {
		t.Fatalf("deployment-specific host address assignment should remain explicit: %#v", got)
	}

	inv = hostHBNTestInventory()
	inv.Hosts[0].Runtime.Observations["network-link-details"] = inventory.Observation{State: "unavailable", ReasonCode: "SSH_RUNTIME_INVALID"}
	checks = hostHBNChecks(&inv)
	if got := hostHBNCheck(t, checks, "host-management-separation", "SERIAL-1 @ worker-01"); got.State != "unknown" || got.ReasonCode != "SSH_RUNTIME_INVALID" {
		t.Fatalf("incomplete master graph cannot pass: %#v", got)
	}
}

func hostHBNTestInventory() inventory.Inventory {
	group0, group1 := 0, 12
	device := inventory.Device{ID: "dpu-id", SerialNumber: "SERIAL-1", ObservationState: "observed"}
	host := inventory.Device{
		ID: "host-id", RecordKey: "host-record", SSHEndpoint: "ssh://192.0.2.20:22", ObservationState: "observed",
		Adapters: []inventory.Adapter{{
			Key: "selected-adapter", SerialNumber: "SERIAL-1", PCIAddress: "0000:21:00.0",
			PCIFunctions: []inventory.PCIFunction{
				{Address: "0000:21:00.0", Driver: "mlx5_core", IOMMUGroup: &group0, Netdevs: []string{"hostpf0"}},
				{Address: "0000:21:00.1", Driver: "mlx5_core", IOMMUGroup: &group1, Netdevs: []string{"hostpf1"}},
			},
		}},
		Runtime: &inventory.RuntimeObservation{
			Hostname: "worker-01",
			Interfaces: []inventory.Interface{
				{Name: "hostpf0", Index: 10, MTU: 9000, Addresses: []string{}},
				{Name: "hostpf1", Index: 11, MTU: 9000, Addresses: []string{}},
			},
			Routes:  []inventory.Route{},
			Network: &inventory.NetworkObservation{Links: []inventory.NetworkLink{{Name: "hostpf0"}, {Name: "hostpf1"}}},
			Observations: map[string]inventory.Observation{
				"interfaces":                       {State: "observed"},
				"network-links":                    {State: "observed"},
				"network-link-details":             {State: "observed"},
				"routes":                           {State: "observed"},
				"host-pf:0000:21:00.0:driver":      {State: "observed"},
				"host-pf:0000:21:00.0:netdevs":     {State: "observed"},
				"host-pf:0000:21:00.0:iommu-group": {State: "observed", Values: []string{"group=0"}},
				"host-pf:0000:21:00.1:driver":      {State: "observed"},
				"host-pf:0000:21:00.1:netdevs":     {State: "observed"},
				"host-pf:0000:21:00.1:iommu-group": {State: "observed", Values: []string{"group=12"}},
			},
		},
	}
	return inventory.Inventory{
		DPUs: []inventory.Device{device}, Hosts: []inventory.Device{host},
		Associations: []inventory.Association{{DPUID: "dpu-id", HostID: "host-id", HostAdapterKey: "selected-adapter", SerialNumber: "SERIAL-1"}},
	}
}

func rawHostHBNTestInventory(t *testing.T, inv inventory.Inventory) []byte {
	t.Helper()
	// Alias the custom public inventory codec so the snapshot covers collector
	// fields rather than only the public handoff projection.
	type rawInventory inventory.Inventory
	data, err := json.Marshal(rawInventory(inv))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func setHostMaster(runtime *inventory.RuntimeObservation, pfName, masterName, kind string, masterIndex int) {
	for i := range runtime.Network.Links {
		if runtime.Network.Links[i].Name == pfName {
			runtime.Network.Links[i].Master = masterName
		}
	}
	runtime.Network.Links = append(runtime.Network.Links, inventory.NetworkLink{Name: masterName, Kind: kind})
	runtime.Interfaces = append(runtime.Interfaces, inventory.Interface{Name: masterName, Index: masterIndex, MTU: 9000, Addresses: []string{}})
}

func hostHBNCheck(t *testing.T, checks []inventory.Check, suffix, subject string) inventory.Check {
	t.Helper()
	for _, check := range checks {
		if check.ID == "dpf.hbn."+suffix && check.Subject == subject {
			return check
		}
	}
	t.Fatalf("missing HBN host check %s for %q: %#v", suffix, subject, checks)
	return inventory.Check{}
}
