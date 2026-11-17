// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HBN host reset eligibility", func() {
	It("blocks missing or incompatible static support independently of usable physical identity", func() {
		release, err := releases.Resolve("26.8.0")
		Expect(err).NotTo(HaveOccurred())
		inv := &inventory.Inventory{Software: &release, DPUs: []inventory.Device{{SerialNumber: "DPU-A", Model: "B4240", Observations: map[string]inventory.Observation{}}}}
		Expect(hbnResetSupportChecks(inv)[0].State).To(Equal("unknown"))
		observed := inventory.Observation{State: "observed", Tool: "redfish", ReasonCode: "RESET_SUPPORT_VERIFIED", Message: "Static support verified; live reset safety remains required.", Values: []string{"26.8.0", "bf4-reset-safety-v1", "B4240", "BF4-26.10-0001"}}
		inv.DPUs[0].Observations["host-reset-support"] = observed
		Expect(hbnResetSupportChecks(inv)[0].State).To(Equal("passed"))
		observed.Values[3] = "BF4-older"
		inv.DPUs[0].Observations["host-reset-support"] = observed
		Expect(hbnResetSupportChecks(inv)[0].State).NotTo(Equal("passed"))
		inv.DPUs[0].Observations["host-reset-support"] = inventory.Observation{State: "failed", ReasonCode: "RESET_PLATFORM_UNQUALIFIED", Message: "Unsupported BMC build."}
		check := hbnResetSupportChecks(inv)[0]
		Expect(check.State).To(Equal("failed"))
		Expect(check.RequiredPhase).To(Equal("deploy"))
		Expect(check.ReasonCode).To(Equal("RESET_PLATFORM_UNQUALIFIED"))
	})
})
