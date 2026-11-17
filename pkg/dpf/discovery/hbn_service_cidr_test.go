// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("bootstrap current controller contract", func() {
	It("evaluates overlaps against the explicit selected scalar without changing inputs", func() {
		input, inv, _ := hbnBootstrapNetworkFixture()
		podCIDR := "172.31.0.0/16"
		input.Bootstrap.PodCIDR = &podCIDR
		before, err := json.Marshal(input)
		Expect(err).NotTo(HaveOccurred())
		for _, service := range []string{"172.31.0.0/16", ""} {
			checks, _ := hbnBootstrapChecks(input, "dpf-system", inv, nil, service)
			found := false
			for _, check := range checks {
				if check.ID == "dpf.hbn.bootstrap-inputs" && check.ReasonCode == "BOOTSTRAP_INPUT_INVALID" {
					found = true
					if service != "" {
						Expect(strings.Contains(check.Message, "overlaps")).To(BeTrue())
					}
				}
			}
			Expect(found).To(BeTrue(), "selected controller constraint must remain explicit")
		}
		after, err := json.Marshal(input)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
	})
})
