// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Software BOM readiness", func() {
	It("does not confuse inventory collection, software applicability and hardware qualification", func() {
		r, err := releases.Resolve("")
		Expect(err).NotTo(HaveOccurred())
		inv := &inventory.Inventory{Software: &r, DPUs: []inventory.Device{{RecordKey: "bf4", ID: "bf4", SerialNumber: "CARD4", ObservationState: "observed", Model: "BlueField-4"}}}
		checks := SoftwareChecks(inv)
		Expect(checks).To(HaveLen(1))
		Expect(checks[0].ReasonCode).To(Equal("BOM_MODEL_UNSUPPORTED"))
		Expect(checks[0].RequiredPhase).To(Equal("generate"))
	})
	It("requires active evidence and retains unknown requirements", func() {
		r, err := releases.Resolve("")
		Expect(err).NotTo(HaveOccurred())
		d := inventory.Device{RecordKey: "bf3", ID: "bf3", ObservationState: "observed", Observations: map[string]inventory.Observation{"firmware-inventory": {State: "observed"}}, Model: "B3210", SerialNumber: "MT1234567890", Firmware: []inventory.Firmware{{Component: "nic", State: "known", RawVersion: "32.38.1002", Activation: "unknown"}}}
		inv := &inventory.Inventory{Software: &r, DPUs: []inventory.Device{d}}
		find := func() inventory.Check {
			for _, c := range SoftwareChecks(inv) {
				if c.ID == "dpf.software.nic" {
					return c
				}
			}
			Fail("missing NIC check")
			return inventory.Check{}
		}
		Expect(find().ReasonCode).To(Equal("SOFTWARE_VERSION_UNAVAILABLE"))
		inv.DPUs[0].Firmware[0].Activation = "running"
		Expect(find().State).To(Equal("passed"))
		inv.DPUs[0].Firmware[0].RawVersion = "32.37.9999"
		Expect(find().State).To(Equal("failed"))
		inv.DPUs[0].Firmware = append(inv.DPUs[0].Firmware, inventory.Firmware{Component: "nic", State: "known", RawVersion: "32.38.1002", Activation: "running"})
		Expect(find().ReasonCode).To(Equal("SOFTWARE_VERSION_CONFLICT"))
		for _, c := range SoftwareChecks(inv) {
			if c.State != "passed" {
				Expect(c.ReasonCode).NotTo(BeEmpty())
				Expect(c.Message).NotTo(BeEmpty())
			}
		}
	})
	It("accepts only current identity-bound source measurements", func() {
		for _, scenario := range []struct {
			name   string
			mutate func(*inventory.Inventory)
			want   string
		}{
			{"accepted host", func(*inventory.Inventory) {}, "passed"},
			{"failed DPU", func(i *inventory.Inventory) {
				i.DPUs[0].ObservationState = "unavailable"
				i.DPUs[0].ReasonCode = "IDENTITY_CHANGED"
			}, "identity"},
			{"missing associated adapter", func(i *inventory.Inventory) { i.Hosts[0].Adapters = nil }, "unknown"},
			{"failed host", func(i *inventory.Inventory) { i.Hosts[0].ObservationState = "unavailable" }, "unknown"},
			{"unassociated host", func(i *inventory.Inventory) { i.Associations = nil }, "unknown"},
			{"duplicate association", func(i *inventory.Inventory) { i.Associations = append(i.Associations, i.Associations[0]) }, "unknown"},
			{"duplicate DPU", func(i *inventory.Inventory) { i.DPUs = append(i.DPUs, i.DPUs[0]) }, "identity"},
			{"unrelated serial", func(i *inventory.Inventory) { i.Hosts[0].Runtime.Hardware.Cards[0].SerialNumber = "OTHER" }, "unknown"},
		} {
			inv := softwareHostFixture()
			scenario.mutate(inv)
			checks := SoftwareChecks(inv)
			if scenario.want == "identity" {
				for _, c := range checks {
					Expect(c.ReasonCode).To(Equal("SOFTWARE_IDENTITY_UNAVAILABLE"), scenario.name)
					Expect(c.State).To(Equal("unknown"))
				}
			} else {
				Expect(softwareCheck(checks, "nic").State).To(Equal(scenario.want), scenario.name)
			}
		}
	})
	It("keeps explicit firmware and activation conflicts dominant over valid SSH", func() {
		for _, fw := range []inventory.Firmware{
			{Component: "nic", State: "conflict", ReasonCode: "FIRMWARE_VERSION_CONFLICT"},
			{Component: "nic", State: "known", RawVersion: "32.38.1002", Activation: "unknown", ActivationReasonCode: "ACTIVATION_CONFLICT"},
		} {
			inv := softwareHostFixture()
			inv.DPUs[0].Firmware = []inventory.Firmware{fw}
			check := softwareCheck(SoftwareChecks(inv), "nic")
			Expect(check.State).To(Equal("unknown"))
			Expect(check.ReasonCode).To(Equal("SOFTWARE_VERSION_CONFLICT"))
		}
	})
	It("retains incomplete source reasons and accepts an independent associated SSH source", func() {
		inv := softwareHostFixture()
		d := &inv.DPUs[0]
		d.Observations = map[string]inventory.Observation{"firmware-inventory": {State: "unavailable", ReasonCode: "ACCESS_DENIED", Message: "A firmware member returned HTTP 403."}}
		d.Firmware = []inventory.Firmware{{Component: "nic", State: "known", RawVersion: "32.38.1002", Activation: "running", EvidenceRefs: []string{"partial-redfish"}}}
		check := softwareCheck(SoftwareChecks(inv), "nic")
		Expect(check.State).To(Equal("passed"))
		Expect(check.EvidenceRefs).To(ContainElement("associated-ssh"))
		Expect(check.EvidenceRefs).NotTo(ContainElement("partial-redfish"))
		Expect(d.Observations["firmware-inventory"].ReasonCode).To(Equal("ACCESS_DENIED"))
		inv.Associations = nil
		check = softwareCheck(SoftwareChecks(inv), "nic")
		Expect(check.State).To(Equal("unknown"))
		Expect(check.ReasonCode).To(Equal("ACCESS_DENIED"))
		Expect(check.Message).To(ContainSubstring("403"))
	})
	It("does not turn a desired target into a compatibility predicate", func() {
		inv := softwareHostFixture()
		inv.Software.Hardware[0].Requirements = append(inv.Software.Hardware[0].Requirements, releases.Requirement{Component: "desired-only", State: "known", Target: "1.2.3"})
		for _, c := range SoftwareChecks(inv) {
			Expect(c.ID).NotTo(Equal("dpf.software.desired-only"))
		}
	})
	It("compares numeric build versions without ordering vendor text", func() {
		c, ok := compareVersion("4.33.0-169", "4.33.0-168")
		Expect(ok).To(BeTrue())
		Expect(c).To(Equal(1))
		_, ok = compareVersion("unverified", "4.33.0-169")
		Expect(ok).To(BeFalse())
	})
	It("explains every nonpassing acceptance predicate", func() {
		for _, code := range []string{"EXACTLY_THREE_CONTROL_PLANES", "PHYSICAL_IDENTITY_REQUIRED", "QUALIFIED_ADAPTER_REQUIRED", "DPU_IDENTITY_REQUIRED", "UNIQUE_PHYSICAL_MATCH_REQUIRED", "REDFISH_AUTHORIZATION_NOT_EVALUATED", "DPF_AUTHENTICATION_NOT_EVALUATED", "INVENTORY_MODE", "EXACT_NETWORK_COVERAGE_REQUIRED"} {
			c := inventory.Check{State: "unknown", ReasonCode: code}
			ExplainCheck(&c)
			Expect(c.Message).NotTo(BeEmpty())
		}
	})
})

func softwareCheck(checks []inventory.Check, component string) inventory.Check {
	for _, c := range checks {
		if c.ID == "dpf.software."+component {
			return c
		}
	}
	Fail("missing software check for " + component)
	return inventory.Check{}
}
func softwareHostFixture() *inventory.Inventory {
	r, err := releases.Resolve("")
	Expect(err).NotTo(HaveOccurred())
	return &inventory.Inventory{Software: &r,
		DPUs:         []inventory.Device{{RecordKey: "card", ID: "dpu-a", SerialNumber: "CARD-A", ObservationState: "observed", Model: "B3210"}},
		Hosts:        []inventory.Device{{ID: "host-a", ObservationState: "observed", Adapters: []inventory.Adapter{{Key: "adapter-a", SerialNumber: "CARD-A"}}, Runtime: &inventory.RuntimeObservation{Hardware: &inventory.HardwareObservation{Cards: []inventory.CardHardware{{SerialNumber: "CARD-A", Firmware: inventory.Observation{State: "observed", Values: []string{"32.38.1002"}}, EvidenceRefs: []string{"associated-ssh"}}}}}}},
		Associations: []inventory.Association{{DPUID: "dpu-a", HostID: "host-a", SerialNumber: "CARD-A", HostAdapterKey: "adapter-a"}},
	}
}
