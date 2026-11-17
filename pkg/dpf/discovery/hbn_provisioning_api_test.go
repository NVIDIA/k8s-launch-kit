// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HBN release-specific BMC compatibility", func() {
	It("blocks absent, mismatched and denied controller API observations independently of physical inventory", func() {
		release, err := releases.Resolve("26.8.0")
		Expect(err).NotTo(HaveOccurred())
		inv := &inventory.Inventory{Software: &release, DPUs: []inventory.Device{{SerialNumber: "DPU-A", ObservationState: "observed", Observations: map[string]inventory.Observation{}}}}
		Expect(hbnProvisioningAPIChecks(inv)[0].State).To(Equal("unknown"))
		for _, code := range []string{"DPF_PROVISIONING_API_NOT_EXPOSED", "DPF_PROVISIONING_PSID_UNAVAILABLE"} {
			inv.DPUs[0].Observations["dpf-provisioning-api"] = inventory.Observation{State: "unknown", Tool: "redfish", ReasonCode: code, Message: "Required controller read unavailable."}
			check := hbnProvisioningAPIChecks(inv)[0]
			Expect(check.State).To(Equal("failed"))
			Expect(check.ReasonCode).To(Equal(code))
		}
		inv.DPUs[0].Observations["dpf-provisioning-api"] = inventory.Observation{State: "unknown", Tool: "redfish", ReasonCode: "ACCESS_DENIED", Message: "Read denied."}
		Expect(hbnProvisioningAPIChecks(inv)[0].State).To(Equal("unknown"))
		valid := inventory.Observation{State: "observed", Tool: "redfish", ReasonCode: "DPF_PROVISIONING_API_READS_VERIFIED", Message: "Reads verified.", Values: []string{"26.8.0", "02:00:00:00:01:01"}}
		inv.DPUs[0].Observations["dpf-provisioning-api"] = valid
		Expect(hbnProvisioningAPIChecks(inv)[0].State).To(Equal("passed"))
		valid.Values = []string{"26.4.0", "02:00:00:00:01:01"}
		inv.DPUs[0].Observations["dpf-provisioning-api"] = valid
		Expect(hbnProvisioningAPIChecks(inv)[0].State).To(Equal("unknown"))
	})
})
