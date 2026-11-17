// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"encoding/json"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("required HBN ASIC programming prerequisite", func() {
	It("compares direct hardware state against resolved defaults and overrides without mutation", func() {
		profile, err := hbn.Resolve(hbn.Options{})
		Expect(err).NotTo(HaveOccurred())
		binding := inventory.SwitchPort{Name: "swp1s0", DPUSerial: "DPU-A", DPUInterface: "p0"}
		base := fabric.ASICPort{Name: binding.Name, RIF: 219, RouterID: 6, RouterTable: 1006, LinuxVRF: profile.FabricVRF, LinuxTable: 1006, MAC: "02:00:00:00:00:01", MTU: 9216, IPv4Enabled: true, IPv6Enabled: true}
		for _, test := range []struct {
			name, state string
			edit        func(*inventory.FabricSwitch, *hbn.Profile)
		}{
			{name: "observed match", state: "passed", edit: func(*inventory.FabricSwitch, *hbn.Profile) {}},
			{name: "software hbn cannot hide Router0", state: "failed", edit: func(s *inventory.FabricSwitch, _ *hbn.Profile) {
				s.Snapshot.ASIC.Ports[0].RouterID = 0
				s.Snapshot.ASIC.Ports[0].RouterTable = 0
			}},
			{name: "VRF override", state: "passed", edit: func(s *inventory.FabricSwitch, p *hbn.Profile) {
				p.FabricVRF = "tenant"
				s.Snapshot.ASIC.Ports[0].LinuxVRF = "tenant"
			}},
			{name: "different VRF", state: "failed", edit: func(_ *inventory.FabricSwitch, p *hbn.Profile) { p.FabricVRF = "tenant" }},
			{name: "MTU override", state: "failed", edit: func(_ *inventory.FabricSwitch, p *hbn.Profile) { p.UplinkMTU = 9217 }},
			{name: "IPv4 disabled", state: "failed", edit: func(s *inventory.FabricSwitch, _ *hbn.Profile) { s.Snapshot.ASIC.Ports[0].IPv4Enabled = false }},
			{name: "IPv6 disabled", state: "failed", edit: func(s *inventory.FabricSwitch, _ *hbn.Profile) { s.Snapshot.ASIC.Ports[0].IPv6Enabled = false }},
			{name: "default explicit sentinel", state: "passed", edit: func(s *inventory.FabricSwitch, p *hbn.Profile) {
				p.FabricVRF = "default"
				r := &s.Snapshot.ASIC.Ports[0]
				r.LinuxVRF = "default"
				r.RouterID = 0
				r.RouterTable = 0
				r.LinuxTable = 0
			}},
			{name: "unmeasured", state: "unknown", edit: func(s *inventory.FabricSwitch, _ *hbn.Profile) { s.Snapshot.ASIC = nil }},
			{name: "selected row absent", state: "unknown", edit: func(s *inventory.FabricSwitch, _ *hbn.Profile) { s.Snapshot.ASIC.Ports = nil }},
			{name: "duplicate selected rows", state: "unknown", edit: func(s *inventory.FabricSwitch, _ *hbn.Profile) {
				s.Snapshot.ASIC.Ports = append(s.Snapshot.ASIC.Ports, s.Snapshot.ASIC.Ports[0])
			}},
			{name: "unconfirmed diagnostic supersedes stale facts", state: "unknown", edit: func(s *inventory.FabricSwitch, _ *hbn.Profile) {
				s.Snapshot.Issues = []fabric.Issue{{Input: "asic", Code: "ASIC_DIAGNOSTIC_FAILED", Message: "SDK diagnostic could not be confirmed."}}
			}},
		} {
			By(test.name)
			p := profile
			sw := inventory.FabricSwitch{Name: "leaf-a", Ports: []inventory.SwitchPort{binding}, Snapshot: fabric.Snapshot{ASIC: &fabric.ASICObservation{Ports: []fabric.ASICPort{base}}}}
			test.edit(&sw, &p)
			before, err := json.Marshal(sw)
			Expect(err).NotTo(HaveOccurred())
			check := hbnASICCheck(sw, binding, p)
			Expect(check.State).To(Equal(test.state))
			Expect(check.RequiredPhase).To(Equal("deploy"))
			after, err := json.Marshal(sw)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
		}
	})
	It("requires current ASIC coverage for every selected port in aggregate readiness", func() {
		profile, err := hbn.Resolve(hbn.Options{})
		Expect(err).NotTo(HaveOccurred())
		inv := inventory.Inventory{HBN: &inventory.HBNInventory{Profile: profile, Switches: []inventory.FabricSwitch{{Name: "leaf-a", Ports: []inventory.SwitchPort{{Name: "swp1s0"}, {Name: "swp1s1"}}}}}}
		readiness := evaluateHBN(&inv, nil, nil)
		Expect(readiness.State).To(Equal("blocked"))
		selected := 0
		for _, check := range readiness.Checks {
			if check.ID == "dpf.hbn.port-asic" {
				selected++
				Expect(check.State).To(Equal("unknown"))
				Expect(check.RequiredPhase).To(Equal("deploy"))
			}
		}
		Expect(selected).To(Equal(2))
	})
	It("projects compact selected ASIC facts without vendor tables or unrelated ports", func() {
		sw := inventory.FabricSwitch{Ports: []inventory.SwitchPort{{Name: "swp1"}}, Snapshot: fabric.Snapshot{ASIC: &fabric.ASICObservation{Ports: []fabric.ASICPort{{Name: "swp1", RIF: 219}, {Name: "swp9", RIF: 9}}}}}
		restrictFabric(&sw, "default")
		Expect(sw.Snapshot.ASIC.Ports).To(HaveLen(1))
		raw, err := json.Marshal(sw)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).NotTo(ContainSubstring("swp9"))
		Expect(strings.ToLower(string(raw))).NotTo(ContainSubstring("dump"))
	})
})
