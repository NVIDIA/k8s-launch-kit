// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("legacy discovery inventory input", func() {
	It("loads and refreshes legacy records while dropping retired metadata", func() {
		for _, version := range []string{"v1alpha1", "v1alpha2"} {
			for _, metadata := range []int{1, 2, 3} {
				base := legacyInventorySite(version, 0)
				withRetired := legacyInventorySite(version, metadata)
				baseline, err := Decode([]byte(base), "/site/input.yaml")
				Expect(err).NotTo(HaveOccurred())
				baselineSaved, err := Saved(baseline, baseline.Site.Inventory, baseline.Path)
				Expect(err).NotTo(HaveOccurred())
				baselineRefreshed, err := Decode(baselineSaved, baseline.Path)
				Expect(err).NotTo(HaveOccurred())
				original := []byte(withRetired)
				in, err := Decode(original, "/site/input.yaml")
				Expect(err).NotTo(HaveOccurred(), "%s metadata mask %d", version, metadata)
				Expect(in.Site.Inventory).To(Equal(baseline.Site.Inventory))
				Expect(in.Source).To(Equal(original))
				sourceBeforeSave := append([]byte(nil), in.Source...)
				for _, device := range append(append([]inventory.Device{}, in.Site.Inventory.Hosts...), in.Site.Inventory.DPUs...) {
					Expect(device.IdentityKey).To(BeEmpty())
					Expect(device.SerialNumber).To(BeEmpty())
					Expect(device.Authentication.BMCAdministrator).To(BeEmpty())
				}

				saved, err := Saved(in, in.Site.Inventory, in.Path)
				Expect(err).NotTo(HaveOccurred())
				Expect(string(saved)).NotTo(ContainSubstring("qualification"))
				Expect(string(saved)).NotTo(ContainSubstring("inventoryCompleteness"))
				Expect(in.Source).To(Equal(sourceBeforeSave), "Saved must not mutate caller-owned source bytes")
				refreshed, err := Decode(saved, in.Path)
				Expect(err).NotTo(HaveOccurred())
				Expect(refreshed.Site.Inventory).To(Equal(baselineRefreshed.Site.Inventory))
				Expect(refreshed.Site.Inventory.Hosts[0].Model).To(Equal("host-model"))
				Expect(refreshed.Site.Inventory.DPUs[0].Model).To(Equal("dpu-model"))
			}
		}
	})

	It("continues rejecting unknown legacy device fields and retired fields in modern inventory", func() {
		for _, extra := range []string{
			"      deviceTypo: value\n",
			"      qualification: {old: value}\n",
			"      inventoryCompleteness: [old]\n",
		} {
			text := legacyInventorySite("v1alpha2", 0)
			text = strings.Replace(text, "      model: dpu-model\n", "      model: dpu-model\n"+extra, 1)
			_, err := Decode([]byte(text), "/site/input.yaml")
			Expect(err).To(HaveOccurred(), "%s", extra)
		}
		for _, field := range []string{"qualification: old", "inventoryCompleteness: old"} {
			text := legacyInventorySite(inventory.FormatVersion, 0)
			text = strings.Replace(text, "      model: dpu-model\n", "      model: dpu-model\n      "+field+"\n", 1)
			_, err := Decode([]byte(text), "/site/input.yaml")
			Expect(err).To(HaveOccurred(), "%s", field)
		}
	})
})

func legacyInventorySite(version string, metadata int) string {
	fields := func() string {
		var out strings.Builder
		if metadata&1 != 0 {
			out.WriteString("      qualification: legacy-qualification\n")
		}
		if metadata&2 != 0 {
			out.WriteString("      inventoryCompleteness: legacy-completeness\n")
		}
		return out.String()
	}
	device := func(id, model string) string {
		return fmt.Sprintf("    - id: %s\n      model: %s\n%s", id, model, fields())
	}
	return seed + fmt.Sprintf("inventory:\n  formatVersion: %s\n  hosts:\n%s  dpus:\n%s", version, device("host-one", "host-model"), device("dpu-one", "dpu-model"))
}
