// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HBN expected configuration", func() {
	It("requires positive routed-port observations and preserves bridge conflicts", func() {
		profile, err := hbn.Resolve(hbn.Options{})
		Expect(err).NotTo(HaveOccurred())
		physical, bridge := "swp", "br-other"
		snapshot := fabric.Snapshot{
			Configured:  fabric.ConfiguredFacts{Interfaces: []fabric.InterfaceConfig{{Name: "swp1"}}},
			Operational: fabric.OperationalFacts{Interfaces: []fabric.InterfaceState{{Name: "swp1", Type: &physical, VRF: &profile.FabricVRF, Parent: &profile.FabricVRF}}},
		}
		inv := &inventory.Inventory{HBN: &inventory.HBNInventory{Profile: profile, Switches: []inventory.FabricSwitch{{Name: "leaf", Ports: []inventory.SwitchPort{{Name: "swp1", DPUSerial: "DPU-A", DPUInterface: "p0"}}, Snapshot: snapshot}}}}
		routedState := func() string {
			for _, check := range evaluateHBN(inv, nil, nil).Checks {
				if check.ID == "dpf.hbn.routed-port" {
					return check.State
				}
			}
			Fail("routed-port check is missing")
			return ""
		}
		Expect(routedState()).To(Equal("passed"))
		inv.HBN.Switches[0].Snapshot.Configured.Interfaces[0].BridgeDomain = &bridge
		Expect(routedState()).To(Equal("failed"))
		inv.HBN.Switches[0].Snapshot.Configured.Interfaces[0].BridgeDomain = nil
		inv.HBN.Switches[0].Snapshot.Operational.Interfaces[0].Parent = nil
		Expect(routedState()).To(Equal("failed"))
		inv.HBN.Switches[0].Snapshot.Operational.Interfaces[0].Parent = &profile.FabricVRF
		inv.HBN.Switches[0].Snapshot.Operational.Bridges = []fabric.BridgeState{{Name: bridge, Ports: []fabric.BridgePortState{{Name: "swp1"}}}}
		Expect(routedState()).To(Equal("failed"))
	})

	It("uses user VRF, ASN, MTU and address overrides without adopting observed values", func() {
		vrf, cidr, none, up := "tenant-fabric", "172.20.0.0/22", "none", "up"
		asn, mtu, workloadMTU, enabled := uint32(65010), 9000, 8800, true
		profile, err := hbn.Resolve(hbn.Options{FabricVRF: &vrf, FabricASN: &asn, UplinkMTU: &mtu, WorkloadMTU: &workloadMTU, WorkloadCIDR: &cidr})
		Expect(err).NotTo(HaveOccurred())
		interfaces := []fabric.InterfaceConfig{{Name: "swp1", MTU: &mtu, VRF: &vrf, BridgeDomain: &none}}
		snapshot := fabric.Snapshot{
			Configured:  fabric.ConfiguredFacts{Interfaces: interfaces, VRFs: []fabric.VRFConfig{{Name: vrf, ASN: &asn, BGPEnabled: &enabled}, {Name: "default", ASN: &asn, BGPEnabled: &enabled}}},
			Operational: fabric.OperationalFacts{Interfaces: []fabric.InterfaceState{{Name: "swp1", MTU: &mtu, VRF: &vrf, AdminState: &up, OperState: &up}}},
		}
		inv := &inventory.Inventory{HBN: &inventory.HBNInventory{Profile: profile, Switches: []inventory.FabricSwitch{{Name: "leaf", Ports: []inventory.SwitchPort{{Name: "swp1", DPUSerial: "DPU-A", DPUInterface: "p0"}}, Snapshot: snapshot}}}}
		find := func(result inventory.Readiness, suffix string) inventory.Check {
			for _, check := range result.Checks {
				if check.ID == "dpf.hbn."+suffix {
					return check
				}
			}
			Fail("missing check " + suffix)
			return inventory.Check{}
		}
		result := evaluateHBN(inv, nil, nil)
		for _, id := range []string{"vrf", "fabric-asn", "port-vrf", "port-mtu"} {
			Expect(find(result, id).State).To(Equal("passed"), id)
		}
		Expect(inv.HBN.Profile).To(Equal(profile))
		defaults, err := hbn.Resolve(hbn.Options{})
		Expect(err).NotTo(HaveOccurred())
		inv.HBN.Profile = defaults
		result = evaluateHBN(inv, nil, nil)
		for _, id := range []string{"fabric-asn", "port-vrf", "port-mtu"} {
			Expect(find(result, id).State).To(Equal("failed"), id)
		}
		Expect(find(result, "fabric-asn").Message).To(ContainSubstring("expected ASN 65000; observed 65010"))
		Expect(find(result, "port-vrf").Message).To(ContainSubstring("expected default; configured tenant-fabric; operational tenant-fabric"))
		Expect(find(result, "port-mtu").Message).To(ContainSubstring("expected at least 9216 bytes; configured 9000; operational 9000"))
		inv.HBN.Switches[0].Snapshot.Configured.Interfaces[0].MTU = nil
		inv.HBN.Switches[0].Snapshot.Operational.Interfaces[0].MTU = nil
		result = evaluateHBN(inv, nil, nil)
		Expect(find(result, "port-mtu").State).To(Equal("failed"))
		Expect(find(result, "port-mtu").Message).To(ContainSubstring("configured not measured; operational not measured"))
		// The checker must not silently replace desired defaults with live values.
		Expect(inv.HBN.Profile).To(Equal(defaults))
		Expect(profile.WorkloadCIDR).To(Equal(cidr))
	})
})
