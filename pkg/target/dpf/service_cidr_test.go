// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"encoding/json"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("selected controller tenant service range", func() {
	It("uses current selected catalog authority without hydrating or trusting the saved software summary", func() {
		for _, saved := range []string{"", "172.31.0.0/16"} {
			input := generationSite()
			intent, err := config.ResolveIntent(input.Site)
			Expect(err).NotTo(HaveOccurred())
			input.Site.Inventory.Software.TenantServiceCIDR = saved
			before, err := json.Marshal(input.Site)
			Expect(err).NotTo(HaveOccurred())
			got, err := generationInputs(input)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Operator.Bootstrap.Tenant.ServiceCIDR).To(Equal(intent.Release.TenantServiceCIDR))
			Expect(input.Site.Inventory.Software.TenantServiceCIDR).To(Equal(saved))
			after, err := json.Marshal(input.Site)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))
		}
	})
})
