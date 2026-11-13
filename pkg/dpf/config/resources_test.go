// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HBN service resource defaults", func() {
	It("uses profile resources when provisioning values are omitted or null", func() {
		for _, document := range []string{"", "provisioning: {resources: null}\n"} {
			input, err := Decode([]byte(document+seed), "/site/input.yaml")
			Expect(err).NotTo(HaveOccurred())
			var override map[string]string
			if input.Site.Provisioning != nil {
				override = input.Site.Provisioning.Resources
			}
			got, err := ResolveHBNResources(override)
			Expect(err).NotTo(HaveOccurred())
			Expect(map[string]string(got)).To(Equal(map[string]string{"memory": "6Gi", "nvidia.com/bf_sf": "4"}))
		}
	})

	It("overlays supplied quantities and returns independent values", func() {
		input, err := Decode([]byte("provisioning:\n  resources:\n    memory: 8Gi\n"+seed), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		got, err := ResolveHBNResources(input.Site.Provisioning.Resources)
		Expect(err).NotTo(HaveOccurred())
		Expect(map[string]string(got)).To(Equal(map[string]string{"memory": "8Gi", "nvidia.com/bf_sf": "4"}))
		input.Site.Provisioning.Resources["memory"] = "2Gi"
		Expect(got["memory"]).To(Equal("8Gi"))

		got["memory"] = "1Gi"
		got["new.example/resource"] = "2"
		again, err := ResolveHBNResources(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(map[string]string(again)).To(Equal(map[string]string{"memory": "6Gi", "nvidia.com/bf_sf": "4"}))
	})

	It("rejects explicit empty maps and invalid quantities", func() {
		for _, document := range []string{
			"provisioning: {resources: {}}\n",
			"provisioning: {resources: {memory: nonsense}}\n",
			"provisioning: {resources: {nvidia.com/bf_sf: '0'}}\n",
			"provisioning: {resources: {nvidia.com/bf_sf: '500m'}}\n",
			"provisioning: {resources: {bad/name/extra: '1'}}\n",
			"provisioning: {resources: {banana: '1'}}\n",
		} {
			input, err := Decode([]byte(document+seed), "/site/input.yaml")
			Expect(err).NotTo(HaveOccurred())
			var override map[string]string
			if input.Site.Provisioning != nil {
				override = input.Site.Provisioning.Resources
			}
			_, err = ResolveHBNResources(override)
			Expect(err).To(HaveOccurred(), document)
		}
	})
})
