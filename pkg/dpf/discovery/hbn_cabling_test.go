// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"encoding/json"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"testing"
)

func TestHBNCablingRequiresSameRunUniqueMACAndPortClaim(t *testing.T) {
	for _, tc := range []struct {
		name, state, code string
		mutate            func(*inventory.Inventory)
	}{
		{"corroborated", "passed", "LLDP_CABLING_CORROBORATED", func(*inventory.Inventory) {}},
		{"name without card MAC", "unknown", "DPU_MAC_OWNERSHIP_UNAVAILABLE", func(i *inventory.Inventory) { i.DPUs[0].NetworkFunctionMACs = nil }},
		{"identity invalidated", "unknown", "DPU_MAC_OWNERSHIP_UNAVAILABLE", func(i *inventory.Inventory) { i.DPUs[0].ObservationState = "unavailable" }},
		{"duplicate selected serial", "failed", "DPU_MAC_IDENTITY_AMBIGUOUS", func(i *inventory.Inventory) { i.DPUs = append(i.DPUs, i.DPUs[0]) }},
		{"wrong card", "failed", "LLDP_DPU_MAC_MISMATCH", func(i *inventory.Inventory) { i.DPUs[0].NetworkFunctionMACs = []string{"02:00:00:00:00:02"} }},
		{"MAC owned by two cards", "failed", "DPU_MAC_OWNERSHIP_CONFLICT", func(i *inventory.Inventory) {
			i.DPUs = append(i.DPUs, inventory.Device{ID: "other", SerialNumber: "other", NetworkFunctionMACs: i.DPUs[0].NetworkFunctionMACs})
		}},
		{"peer absent", "unknown", "LLDP_PEER_NOT_UNIQUE", func(i *inventory.Inventory) { i.HBN.Switches[0].Snapshot.Operational.LLDP = nil }},
		{"two peers on port", "failed", "LLDP_PEER_NOT_UNIQUE", func(i *inventory.Inventory) {
			p := i.HBN.Switches[0].Snapshot.Operational.LLDP[0]
			i.HBN.Switches[0].Snapshot.Operational.LLDP = append(i.HBN.Switches[0].Snapshot.Operational.LLDP, p)
		}},
		{"same MAC on another port", "failed", "LLDP_PORT_MAC_AMBIGUOUS", func(i *inventory.Inventory) {
			p := i.HBN.Switches[0].Snapshot.Operational.LLDP[0]
			p.LocalInterface = "swp2"
			i.HBN.Switches[0].Snapshot.Operational.LLDP = append(i.HBN.Switches[0].Snapshot.Operational.LLDP, p)
		}},
		{"port claim missing", "unknown", "LLDP_DPU_PORT_UNAVAILABLE", func(i *inventory.Inventory) { i.HBN.Switches[0].Snapshot.Operational.LLDP[0].PortDescription = nil }},
		{"wrong uplink", "failed", "LLDP_DPU_PORT_MISMATCH", func(i *inventory.Inventory) {
			i.HBN.Switches[0].Snapshot.Operational.LLDP[0].PortDescription = stringPointer("p1")
		}},
		{"conflicting labels", "failed", "LLDP_DPU_PORT_MISMATCH", func(i *inventory.Inventory) {
			i.HBN.Switches[0].Snapshot.Operational.LLDP[0].PortName = stringPointer("p1")
		}},
		{"partial LLDP", "unknown", "SOURCE_UNAVAILABLE", func(i *inventory.Inventory) {
			i.HBN.Switches[0].Snapshot.Issues = []fabric.Issue{{Input: "lldp", Code: "SOURCE_UNAVAILABLE", Message: "read failed"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := inventory.Inventory{DPUs: []inventory.Device{{ID: "dpu", SerialNumber: "SERIAL-A", ObservationState: "observed", NetworkFunctionMACs: []string{"02:00:00:00:00:01"}}}, HBN: &inventory.HBNInventory{Switches: []inventory.FabricSwitch{{Name: "leaf", Ports: []inventory.SwitchPort{{Name: "swp1", DPUSerial: "SERIAL-A", DPUInterface: "p0"}}, Snapshot: fabric.Snapshot{Operational: fabric.OperationalFacts{LLDP: []fabric.LLDPPeerHint{{LocalInterface: "swp1", PortID: stringPointer("02:00:00:00:00:01"), PortDescription: stringPointer("p0")}}}}}}}}
			tc.mutate(&inv)
			before, _ := json.Marshal(inv)
			sw := inv.HBN.Switches[0]
			got := hbnCablingCheck(&inv, sw, sw.Ports[0])
			if got.State != tc.state || got.ReasonCode != tc.code {
				t.Fatalf("got %#v, want %s/%s", got, tc.state, tc.code)
			}
			after, _ := json.Marshal(inv)
			if string(before) != string(after) {
				t.Fatal("cabling evaluation mutated inventory")
			}
		})
	}
}

func TestHBNMACOwnershipChangesInventoryFingerprint(t *testing.T) {
	inv := inventory.Inventory{FormatVersion: inventory.FormatVersion, DPUs: []inventory.Device{{RecordKey: "dpu-a", ID: inventory.PhysicalID("dpu", []string{"nvidia", "SERIAL-A"}), IdentityKey: []string{"nvidia", "SERIAL-A"}, SerialNumber: "SERIAL-A", Model: "B4240", ReaderAdapter: "bluefield-card-v1", ObservationState: "observed", BMCEndpoint: "https://bmc.example", NetworkFunctionMACs: []string{"02:00:00:00:00:01"}}}}
	inv.DPUs[0].Observations = map[string]inventory.Observation{"networkFunctionMACs": {State: "observed", Tool: "redfish", Values: inv.DPUs[0].NetworkFunctionMACs}}
	if _, err := inventory.DeviceToOutput(inv.DPUs[0], "dpus"); err != nil {
		t.Fatal(err)
	}
	Canonicalize(&inv)
	before := inv.Fingerprint
	inv.DPUs[0].NetworkFunctionMACs = []string{"02:00:00:00:00:02"}
	inv.DPUs[0].Observations["networkFunctionMACs"] = inventory.Observation{State: "observed", Tool: "redfish", Values: inv.DPUs[0].NetworkFunctionMACs}
	Canonicalize(&inv)
	if inv.Fingerprint == before {
		t.Fatal("changed card MAC ownership was omitted from material inventory")
	}
}
