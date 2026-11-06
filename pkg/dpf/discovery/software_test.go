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
		inv := &inventory.Inventory{Software: &r, DPUs: []inventory.Device{{RecordKey: "bf4", Model: "BlueField-4"}}}
		checks := SoftwareChecks(inv)
		Expect(checks).To(HaveLen(1))
		Expect(checks[0].ReasonCode).To(Equal("BOM_MODEL_UNSUPPORTED"))
		Expect(checks[0].RequiredPhase).To(Equal("generate"))
	})
	It("requires active evidence and retains unknown requirements", func() {
		r, err := releases.Resolve("")
		Expect(err).NotTo(HaveOccurred())
		d := inventory.Device{RecordKey: "bf3", Model: "B3210", SerialNumber: "MT1234567890", Firmware: []inventory.Firmware{{Component: "nic", State: "known", RawVersion: "32.38.1002", Activation: "unknown"}}}
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
		inv.DPUs[0].Firmware[0].Activation = "active"
		Expect(find().State).To(Equal("passed"))
		inv.DPUs[0].Firmware[0].RawVersion = "32.37.9999"
		Expect(find().State).To(Equal("failed"))
		inv.DPUs[0].Firmware = append(inv.DPUs[0].Firmware, inventory.Firmware{Component: "nic", State: "known", RawVersion: "32.38.1002", Activation: "active"})
		Expect(find().ReasonCode).To(Equal("SOFTWARE_VERSION_CONFLICT"))
		for _, c := range SoftwareChecks(inv) {
			if c.State != "passed" {
				Expect(c.ReasonCode).NotTo(BeEmpty())
				Expect(c.Message).NotTo(BeEmpty())
			}
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
