// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("discovery pending inputs", func() {
	It("keeps the future-generation placeholder for generic DPF and omits it for HBN while retaining missing allocation checks", func() {
		generic := Collect(context.Background(), config.Request{Timeout: time.Second}, "generic", Dependencies{})
		Expect(generic.PendingInputs).To(ConsistOf(HaveField("Code", "LATER_PHASE_INPUT")))
		Expect(generic.PendingInputs[0].InputRef).To(Equal("/networks/dpuAPI"))

		profile, err := hbn.Resolve(hbn.Options{})
		Expect(err).NotTo(HaveOccurred())
		hbnReport := Collect(context.Background(), config.Request{HBN: &profile, Timeout: time.Second}, "hbn", Dependencies{})
		Expect(hbnReport.PendingInputs).To(BeEmpty())

		for _, field := range []string{"vip-reserved", "vip-excluded-from-dhcp", "router-id-unique"} {
			var found *inventory.Check
			for index := range hbnReport.Readiness.Checks {
				if hbnReport.Readiness.Checks[index].ID == "dpf.hbn.management.tenant-allocation."+field {
					found = &hbnReport.Readiness.Checks[index]
					break
				}
			}
			Expect(found).NotTo(BeNil(), field)
			Expect(found.State).To(Equal("unknown"), field)
			Expect(hbnReport.Readiness.State).To(Equal("blocked"), "missing HBN allocation declarations must continue to block readiness")
		}
	})
})
