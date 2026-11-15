// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HBN generation inputs during discovery", func() {
	It("rejects generation settings the selected profile cannot render before discovery runs", func() {
		for _, test := range []struct{ document, reason string }{
			{document: "dpf: {useCase: hbn-bgp, release: '26.4.0'}\n", reason: "unsupported for hbn-bgp"},
			{document: "dpf: {useCase: hbn-bgp}\nprovisioning: {hbn: {vxlanStartupProbeEnabled: true}}\n", reason: "vxlanStartupProbeEnabled"},
			{document: "dpf: {useCase: hbn-bgp}\nprovisioning: {hbn: {chartName: a1234567890123456789012345678}}\n", reason: "chartName"},
		} {
			input, err := config.Decode([]byte(test.document), "/site/input.yaml")
			Expect(err).NotTo(HaveOccurred())
			_, err = config.Resolve(input)
			Expect(err).To(HaveOccurred(), test.document)
			Expect(err.Error()).To(ContainSubstring(test.reason))
		}
	})
	It("reports missing hardware flavor while defaulting resources", func() {
		checks := hbnGenerationInputChecks(nil)
		Expect(checks[0].State).To(Equal("unknown"))
		Expect(checks[0].ReasonCode).To(Equal("DPU_FLAVOR_INPUT_MISSING"))
		Expect(checks[1].State).To(Equal("passed"))
	})
	It("retains the reason for invalid resource input without mutating the source", func() {
		input := &config.Provisioning{Flavor: map[string]any{"dpuMode": "dpu"}, Resources: map[string]string{}}
		checks := hbnGenerationInputChecks(input)
		Expect(checks[0].ReasonCode).To(Equal("DPU_FLAVOR_INPUT_PRESENT"))
		Expect(checks[1].State).To(Equal("failed"))
		Expect(checks[1].Message).To(ContainSubstring("must not be empty"))
		Expect(input.Resources).To(BeEmpty())
	})
})
