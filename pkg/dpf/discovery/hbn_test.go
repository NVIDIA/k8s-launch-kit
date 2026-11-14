// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"encoding/json"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	yaml "gopkg.in/yaml.v3"
)

var _ = Describe("HBN discovery readiness", func() {
	var inv inventory.Inventory
	BeforeEach(func() {
		profile, err := hbn.Resolve(hbn.Options{})
		Expect(err).NotTo(HaveOccurred())
		inv = inventory.Inventory{FormatVersion: inventory.FormatVersion, RequestedScope: []inventory.ScopeEndpoint{}, HBN: &inventory.HBNInventory{Profile: profile}}
	})
	check := func(r inventory.Readiness, id string) inventory.Check {
		for _, c := range r.Checks {
			if c.ID == "dpf.hbn."+id {
				return c
			}
		}
		Fail("missing check: " + id)
		return inventory.Check{}
	}
	It("blocks missing required facts instead of treating inventory success as readiness", func() {
		before, err := json.Marshal(inv)
		Expect(err).NotTo(HaveOccurred())
		r := evaluateHBN(&inv, nil)
		Expect(r.State).To(Equal("blocked"))
		Expect(check(r, "switch-access").ReasonCode).To(Equal("SWITCH_ACCESS_REQUIRED"))
		Expect(check(r, "software-artifacts").State).To(Equal("failed"))
		after, err := json.Marshal(inv)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
		Expect(ExitCode(inventory.Report{Discovery: inventory.Discovery{Outcome: "complete"}, Readiness: &r})).To(Equal(2))
	})
	It("does not let matching applied values hide operational MTU and VRF contradictions", func() {
		mtu, liveMTU := 9216, 1500
		vrf, liveVRF, none, up := "default", "different", "none", "up"
		inv.HBN.Switches = []inventory.FabricSwitch{{Name: "leaf-a", Endpoint: "ssh://leaf.example:22", Ports: []inventory.SwitchPort{{Name: "swp1", DPUSerial: "DPU-A", DPUInterface: "p0"}}, Snapshot: fabric.Snapshot{
			Configured:  fabric.ConfiguredFacts{Interfaces: []fabric.InterfaceConfig{{Name: "swp1", MTU: &mtu, VRF: &vrf, BridgeDomain: &none}}},
			Operational: fabric.OperationalFacts{Interfaces: []fabric.InterfaceState{{Name: "swp1", MTU: &liveMTU, VRF: &liveVRF, AdminState: &up, OperState: &up}}},
		}}}
		r := evaluateHBN(&inv, nil)
		Expect(check(r, "port-mtu").State).To(Equal("failed"))
		Expect(check(r, "port-vrf").State).To(Equal("failed"))
		Expect(check(r, "link").State).To(Equal("passed"))
		Expect(check(r, "cabling").State).To(Equal("unknown"))
		inv.HBN.Switches[0].Snapshot.Operational.Bridges = []fabric.BridgeState{{Name: "br_default", Ports: []fabric.BridgePortState{{Name: "swp1"}}}}
		r = evaluateHBN(&inv, nil)
		Expect(check(r, "routed-port").State).To(Equal("failed"))
	})
	It("retains the HBN public handoff and its scope through YAML without raw dumps", func() {
		inv.HBN.Switches = []inventory.FabricSwitch{{Name: "leaf-a", Endpoint: "ssh://leaf.example:22", Platform: "cumulus-nvue", Ports: []inventory.SwitchPort{{Name: "swp1", DPUSerial: "DPU-A", DPUInterface: "p0"}}, Snapshot: fabric.Snapshot{Issues: []fabric.Issue{{Input: "applied-vrfs", Code: "SOURCE_UNAVAILABLE", Message: "VRF inspection denied."}}}}}
		Canonicalize(&inv)
		data, err := yaml.Marshal(inv)
		Expect(err).NotTo(HaveOccurred())
		var saved inventory.Inventory
		Expect(yaml.Unmarshal(data, &saved)).To(Succeed())
		Canonicalize(&saved)
		Expect(saved.ScopeDigest).To(Equal(inv.ScopeDigest))
		Expect(saved.Fingerprint).To(Equal(inv.Fingerprint))
		Expect(saved.HBN.Profile).To(Equal(inv.HBN.Profile))
		Expect(saved.HBN.Switches).To(Equal(inv.HBN.Switches))
		before := inv.ScopeDigest
		inv.HBN.Profile.FabricASN++
		Canonicalize(&inv)
		Expect(inv.ScopeDigest).NotTo(Equal(before))
		before = inv.ScopeDigest
		inv.HBN.Namespace = "other-operator"
		Canonicalize(&inv)
		Expect(inv.ScopeDigest).NotTo(Equal(before))
	})
	It("limits the saved fabric to selected ports and VRF", func() {
		sw := inventory.FabricSwitch{Ports: []inventory.SwitchPort{{Name: "swp1"}}, Snapshot: fabric.Snapshot{
			Configured:  fabric.ConfiguredFacts{Interfaces: []fabric.InterfaceConfig{{Name: "swp1"}, {Name: "swp9"}}, VRFs: []fabric.VRFConfig{{Name: "default"}, {Name: "unrelated"}}},
			Operational: fabric.OperationalFacts{LLDP: []fabric.LLDPPeerHint{{LocalInterface: "swp1"}, {LocalInterface: "swp9"}}},
		}}
		restrictFabric(&sw, "default", "10.0.120.0/22")
		Expect(sw.Snapshot.Configured.Interfaces).To(HaveLen(1))
		Expect(sw.Snapshot.Configured.VRFs).To(HaveLen(1))
		Expect(sw.Snapshot.Operational.LLDP).To(HaveLen(1))
	})
})
