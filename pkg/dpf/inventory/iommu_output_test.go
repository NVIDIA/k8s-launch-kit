// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

var _ = ginkgo.Describe("host IOMMU facts in the public handoff", func() {
	ginkgo.It("retains group zero without sharing a mutable pointer or losing absence", func() {
		group := 0
		wire := DeviceOutput{RecordKey: "host-source", Method: "ssh", SSHEndpoint: "ssh://host.example:22", Adapters: []AdapterOutput{{Key: "card", PCIAddress: "0000:01:00.0", PCIFunctions: []PCIFunction{{Address: "0000:01:00.0", IOMMUGroup: &group}, {Address: "0000:01:00.1"}}}}}
		data, err := yaml.Marshal(wire)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(ContainSubstring("iommuGroup: 0"))
		var decoded DeviceOutput
		Expect(yaml.Unmarshal(data, &decoded)).To(Succeed())
		restored, err := DeviceFromOutput(decoded, "hosts")
		Expect(err).NotTo(HaveOccurred())
		Expect(*restored.Adapters[0].PCIFunctions[0].IOMMUGroup).To(Equal(0))
		Expect(restored.Adapters[0].PCIFunctions[1].IOMMUGroup).To(BeNil())
		*decoded.Adapters[0].PCIFunctions[0].IOMMUGroup = 17
		Expect(*restored.Adapters[0].PCIFunctions[0].IOMMUGroup).To(Equal(0))
		published, err := DeviceToOutput(restored, "hosts")
		Expect(err).NotTo(HaveOccurred())
		*published.Adapters[0].PCIFunctions[0].IOMMUGroup = 23
		Expect(*restored.Adapters[0].PCIFunctions[0].IOMMUGroup).To(Equal(0))
	})
	ginkgo.It("rejects negative, contradictory and Redfish IOMMU assertions", func() {
		for _, mode := range []string{"negative", "conflict", "redfish", "invalid-bdf", "missing-adapter-bdf", "different-slot"} {
			group := 0
			wire := DeviceOutput{RecordKey: "host-source", Method: "ssh", SSHEndpoint: "ssh://host.example:22", Adapters: []AdapterOutput{{Key: "card", PCIAddress: "0000:01:00.0", PCIFunctions: []PCIFunction{{Address: "0000:01:00.0", IOMMUGroup: &group}}}}}
			switch mode {
			case "negative":
				group = -1
			case "conflict":
				wire.Issues = []DeviceIssue{{Field: "runtime.host-pf:0000:01:00.0:iommu-group", Code: "SSH_COMMAND_FAILED"}}
			case "invalid-bdf":
				wire.Adapters[0].PCIFunctions[0].Address = "not-a-pci-bdf"
			case "missing-adapter-bdf":
				wire.Adapters[0].PCIAddress = ""
			case "different-slot":
				wire.Adapters[0].PCIFunctions[0].Address = "0000:02:00.0"
			case "redfish":
				wire.Method, wire.SSHEndpoint, wire.BMCEndpoint = "redfish", "", "https://host.example"
			}
			_, err := DeviceFromOutput(wire, "hosts")
			Expect(err).To(HaveOccurred(), mode)
		}
	})
})
