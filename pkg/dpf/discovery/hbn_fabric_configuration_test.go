// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"encoding/json"
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HBN fabric configuration checks", func() {
	var inv inventory.Inventory
	BeforeEach(func() {
		profile, err := hbn.Resolve(hbn.Options{})
		Expect(err).NotTo(HaveOccurred())
		asns, err := profile.AllocateASNs([]string{"DPU-A"})
		Expect(err).NotTo(HaveOccurred())
		frr := fmt.Sprintf("frr defaults datacenter\nrouter bgp %d vrf %s\n neighbor swp1 interface remote-as external\n neighbor swp2 interface remote-as external\n address-family ipv4 unicast\n exit-address-family\n", profile.FabricASN, profile.FabricVRF)
		policy := fabric.ParseNVUE(fabric.NVUEInputs{FRRRunningBGP: []byte(frr)}).Configured.FRRPolicy
		vrf := fabric.VRFConfig{
			Name: profile.FabricVRF, ASN: &profile.FabricASN, BGPEnabled: boolPointer(true),
			AddressFamilies: []fabric.AddressFamilyConfig{{Name: "ipv4-unicast", Enabled: boolPointer(true)}},
		}
		sw := inventory.FabricSwitch{
			Name: "leaf-a", Endpoint: "ssh://leaf-a.example.test:22", Platform: "cumulus-nvue",
			Snapshot: fabric.Snapshot{
				Revision:   &fabric.NVUERevisionState{AppliedApplyID: "a", SavedApplyID: "a"},
				Configured: fabric.ConfiguredFacts{BGP: &fabric.BGPConfig{Enabled: boolPointer(true)}, FRRPolicy: policy},
			},
		}
		var peers []fabric.BGPNeighborConfig
		macs := []string{"02:00:00:00:00:01", "02:00:00:00:00:02"}
		for i, mac := range macs {
			port, uplink := fmt.Sprintf("swp%d", i+1), fmt.Sprintf("p%d", i)
			sw.Ports = append(sw.Ports, inventory.SwitchPort{Name: port, DPUSerial: "DPU-A", DPUInterface: uplink})
			peers = append(peers, fabric.BGPNeighborConfig{Name: port, Type: stringPointer("unnumbered"), EffectiveRemoteAS: stringPointer("external"), EffectiveAddressFamilies: []string{"ipv4-unicast"}})
			sw.Snapshot.Configured.Interfaces = append(sw.Snapshot.Configured.Interfaces, fabric.InterfaceConfig{Name: port, MTU: &profile.UplinkMTU, VRF: &profile.FabricVRF, BridgeDomain: stringPointer("none")})
			sw.Snapshot.Operational.Interfaces = append(sw.Snapshot.Operational.Interfaces, fabric.InterfaceState{Name: port, AdminState: stringPointer("up"), OperState: stringPointer("up"), MTU: &profile.UplinkMTU, VRF: &profile.FabricVRF})
			sw.Snapshot.Operational.LLDP = append(sw.Snapshot.Operational.LLDP, fabric.LLDPPeerHint{LocalInterface: port, PortID: stringPointer(mac), PortDescription: stringPointer(uplink)})
		}
		vrf.Neighbors = &peers
		sw.Snapshot.Configured.VRFs = []fabric.VRFConfig{vrf}
		inv = inventory.Inventory{
			DPUs: []inventory.Device{{ID: "dpu-a", SerialNumber: "DPU-A", ObservationState: "observed", NetworkFunctionMACs: macs}},
			HBN: &inventory.HBNInventory{
				Profile: profile, ASNs: []inventory.ASNAssignment{{DPUSerial: "DPU-A", ASN: asns["DPU-A"]}},
				Switches: []inventory.FabricSwitch{sw},
			},
		}
	})

	It("shares current fabric checks while keeping discovery-specific evidence separate", func() {
		inv.HBN.Switches[0].Snapshot.Issues = []fabric.Issue{{Input: "asic", Code: "ASIC_SOURCE_UNAVAILABLE", Message: "Direct ASIC access is unavailable."}}
		before, err := json.Marshal(inv)
		Expect(err).NotTo(HaveOccurred())
		checks := HBNFabricConfigurationChecks(&inv)
		Expect(checks).NotTo(BeEmpty())
		for _, check := range checks {
			Expect(check.State).To(Equal("passed"), check.ID, check.ReasonCode, check.Message)
		}
		Expect(checks).To(ContainElement(HaveField("ID", "dpf.hbn.cabling")))
		Expect(checks).To(ContainElement(HaveField("ReasonCode", "ROUTING_POLICY_UNFILTERED")))
		Expect(checks).NotTo(ContainElement(HaveField("ID", "dpf.hbn.fabric-address-conflicts")))
		Expect(checks).NotTo(ContainElement(HaveField("ID", "dpf.hbn.port-asic")))
		discovery := evaluateHBN(&inv, nil, nil)
		Expect(discovery.State).To(Equal("blocked"))
		for _, check := range checks {
			Expect(discovery.Checks).To(ContainElement(check))
		}
		Expect(discovery.Checks).To(ContainElement(HaveField("ID", "dpf.hbn.fabric-address-conflicts")))
		Expect(discovery.Checks).To(ContainElement(HaveField("ID", "dpf.hbn.port-asic")))
		Expect(discovery.Checks).To(ContainElement(And(HaveField("ID", "dpf.hbn.switch-source"), HaveField("ReasonCode", "ASIC_SOURCE_UNAVAILABLE"))))
		after, err := json.Marshal(inv)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
	})

	It("keeps each source issue and evaluates the available uplinks", func() {
		sw := &inv.HBN.Switches[0]
		sw.Snapshot.Issues = []fabric.Issue{
			{Input: "operationalRoutesIPv4", Code: "ROUTE_READ_FAILED", Message: "Route read failed."},
			{Input: "sshCleanup", Code: "SSH_CLOSE_FAILED", Message: "SSH close failed."},
		}
		sw.Snapshot.Operational.Interfaces[0].VRF = stringPointer("other")
		checks := HBNFabricConfigurationChecks(&inv)
		for _, code := range []string{"ROUTE_READ_FAILED", "SSH_CLOSE_FAILED", "PORT_VRF_MISMATCH"} {
			Expect(checks).To(ContainElement(And(HaveField("State", "failed"), HaveField("ReasonCode", code))))
		}
		Expect(checks).To(ContainElement(And(HaveField("ID", "dpf.hbn.port-vrf"), HaveField("Subject", "leaf-a/swp2"), HaveField("State", "passed"))))
	})

	It("fails closed when the selected profile, devices or switches are missing", func() {
		Expect(HBNFabricConfigurationChecks(nil)).To(ContainElement(And(HaveField("State", "failed"), HaveField("ReasonCode", "HBN_INPUTS_MISSING"))))
		inv.DPUs = nil
		inv.HBN.Switches = nil
		checks := HBNFabricConfigurationChecks(&inv)
		for _, code := range []string{"HBN_DEVICES_MISSING", "SWITCH_ACCESS_REQUIRED"} {
			Expect(checks).To(ContainElement(And(HaveField("State", "failed"), HaveField("ReasonCode", code))))
		}
		inv.HBN.Switches = []inventory.FabricSwitch{{Name: "leaf-a"}}
		Expect(HBNFabricConfigurationChecks(&inv)).To(ContainElement(And(HaveField("State", "failed"), HaveField("ReasonCode", "HBN_SWITCH_INPUTS_INCOMPLETE"))))
	})
})
