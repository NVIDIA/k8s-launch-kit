// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"strconv"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func TestValidationFabricUnavailableSourceIsNotAConfigurationFailure(t *testing.T) {
	o, _, _ := physicalValidationFixture(t)
	inv := o.physicalInventory()
	inv.HBN.Switches = []inventory.FabricSwitch{{Name: "leaf-a", Endpoint: "ssh://leaf.example.test:22", Ports: []inventory.SwitchPort{{Name: "swp1", DPUSerial: "SERIAL-A", DPUInterface: "p0"}}, Snapshot: fabric.Snapshot{Issues: []fabric.Issue{{Input: "connection", Code: "ACCESS_UNAVAILABLE"}}}}}
	checks := validationFabricConfigurationChecks(inv)
	for _, check := range checks {
		switch check.ID {
		case "dpf.hbn.bgp-enabled", "dpf.hbn.vrf", "dpf.hbn.vrf-bgp", "dpf.hbn.fabric-asn", "dpf.hbn.ipv4-unicast", "dpf.hbn.port", "dpf.hbn.port-mtu", "dpf.hbn.port-vrf", "dpf.hbn.routed-port", "dpf.hbn.link", "dpf.hbn.neighbor":
			if check.State != "unknown" || check.ReasonCode != "SWITCH_SOURCE_UNAVAILABLE" {
				t.Fatalf("missing source fabricated failure: %+v", check)
			}
		case "dpf.hbn.uplink-bindings":
			if check.State != "failed" {
				t.Fatal("independent bad declaration was suppressed")
			}
		}
	}
}

func TestValidationFabricPartialReadsPreserveIndependentContradictions(t *testing.T) {
	o, _, _ := physicalValidationFixture(t)
	inv := o.physicalInventory()
	wrongMTU := 1280
	sw := inventory.FabricSwitch{Name: "leaf-a", Endpoint: "ssh://leaf.example.test:22", Ports: []inventory.SwitchPort{{Name: "swp1", DPUSerial: "SERIAL-A", DPUInterface: "p0"}}, Snapshot: fabric.Snapshot{Configured: fabric.ConfiguredFacts{Interfaces: []fabric.InterfaceConfig{{Name: "swp1", MTU: &wrongMTU}}}, Issues: []fabric.Issue{{Input: "operationalInterface/swp1", Code: "PERMISSION_DENIED"}, {Input: "appliedVRFs", Code: "PERMISSION_DENIED"}}}}
	inv.HBN.Switches = []inventory.FabricSwitch{sw}
	for _, check := range validationFabricConfigurationChecks(inv) {
		if check.ID == "dpf.hbn.port-mtu" && check.State != "failed" {
			t.Fatalf("known wrong applied MTU hidden: %+v", check)
		}
		if check.ID == "dpf.hbn.vrf" && check.State != "unknown" {
			t.Fatalf("missing VRF source fabricated absence: %+v", check)
		}
	}
	correctMTU := inv.HBN.Profile.UplinkMTU
	inv.HBN.Switches[0].Snapshot.Configured.Interfaces[0].MTU = &correctMTU
	for _, check := range validationFabricConfigurationChecks(inv) {
		if check.ID == "dpf.hbn.port-mtu" && check.State != "unknown" {
			t.Fatalf("partial observation passed MTU: %+v", check)
		}
	}
}

func TestValidationFabricPortFieldIssuesPreserveMeasuredContradictions(t *testing.T) {
	for _, tc := range []struct {
		name, input, field, want string
	}{
		{"unselected port", "appliedInterfaces", `$["swp99"].link.mtu`, "failed"},
		{"other selected port", "appliedInterfaces", `$["swp2"].link.mtu`, "failed"},
		{"other field on same port", "appliedInterfaces", `$["swp1"].vrf`, "failed"},
		{"other detail field", "appliedInterface/swp1", "$.bridge", "failed"},
		{"same field", "appliedInterfaces", `$["swp1"].link.mtu`, "unknown"},
		{"same parent field", "appliedInterfaces", `$["swp1"].link`, "unknown"},
		{"same port malformed", "appliedInterfaces", `$["swp1"]`, "unknown"},
		{"whole source malformed", "appliedInterfaces", "$", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, _, _ := physicalValidationFixture(t)
			inv := o.physicalInventory()
			wrongMTU := 1280
			inv.HBN.Switches = []inventory.FabricSwitch{{Name: "leaf-a", Ports: []inventory.SwitchPort{{Name: "swp1", DPUSerial: "SERIAL-A", DPUInterface: "p0"}, {Name: "swp2", DPUSerial: "SERIAL-A", DPUInterface: "p1"}}, Snapshot: fabric.Snapshot{
				Configured: fabric.ConfiguredFacts{Interfaces: []fabric.InterfaceConfig{{Name: "swp1", MTU: &wrongMTU}}},
				Issues:     []fabric.Issue{{Input: "operationalInterface/swp1", Code: "PERMISSION_DENIED"}, {Input: tc.input, Field: tc.field, Code: "invalid_type"}},
			}}}
			found := false
			for _, check := range validationFabricConfigurationChecks(inv) {
				if check.ID == "dpf.hbn.port-mtu" && check.Subject == "leaf-a/swp1" {
					found = true
					if check.State != tc.want {
						t.Fatalf("selected field availability misclassified: %+v want %s", check, tc.want)
					}
				}
			}
			if !found {
				t.Fatal("selected MTU check missing")
			}
		})
	}
}

func TestValidationFabricIgnoresUnrelatedSourcesAndKeepsMeasuredAbsence(t *testing.T) {
	o, _, _ := physicalValidationFixture(t)
	inv := o.physicalInventory()
	vrf := inv.HBN.Profile.FabricVRF
	sw := inventory.FabricSwitch{Name: "leaf-a", Endpoint: "ssh://leaf.example.test:22", Ports: []inventory.SwitchPort{{Name: "swp1", DPUSerial: "SERIAL-A", DPUInterface: "p0"}}, Snapshot: fabric.Snapshot{Configured: fabric.ConfiguredFacts{VRFs: []fabric.VRFConfig{{Name: vrf}}}, Issues: []fabric.Issue{{Input: "appliedVRFs", Field: `$["unrelated"].router`, Code: "not_configured", Message: "Unrelated VRF has no router."}, {Input: "operationalRoutesIPv6", Field: "$", Code: "PERMISSION_DENIED", Message: "Unused occupancy source unavailable."}}}}
	inv.HBN.Switches = []inventory.FabricSwitch{sw}
	checks := validationFabricConfigurationChecks(inv)
	if issues := validationRelevantSwitchIssues(sw, inv, checks); len(issues) != 0 {
		t.Fatalf("unrelated state blocked selected switch: %+v", issues)
	}
	for _, check := range checks {
		if check.ID == "dpf.hbn.vrf" && check.State != "passed" {
			t.Fatalf("unrelated VRF invalidated selected VRF: %+v", check)
		}
	}
	inv.HBN.Switches[0].Snapshot.Issues = append(inv.HBN.Switches[0].Snapshot.Issues, fabric.Issue{Input: "appliedVRFs", Field: "$[" + strconv.Quote(vrf) + "].router.bgp", Code: "not_configured", Message: "Selected VRF has no BGP configuration."})
	for _, check := range validationFabricConfigurationChecks(inv) {
		if check.ID == "dpf.hbn.vrf-bgp" && check.State != "failed" {
			t.Fatalf("measured missing BGP became unavailable: %+v", check)
		}
	}
}
