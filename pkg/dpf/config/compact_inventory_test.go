// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("compact discovery inventory input", func() {
	It("restores compact evidence after strict YAML decoding", func() {
		in, err := Decode([]byte(seed), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		inv := &inventory.Inventory{FormatVersion: inventory.FormatVersion, DPUs: []inventory.Device{{
			ID: "dpu-one", BMCEndpoint: "https://dpu.invalid", Model: "B4240", ReaderAdapter: "bluefield-card-v1", ObservationState: "observed", Observations: map[string]inventory.Observation{
				"bmc-administrator":  {State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED"},
				"firmware-inventory": {State: "observed"},
				"psid":               {State: "observed", Values: []string{"MT_0000001774"}, ReasonCode: "PSID_OBSERVED", Tool: "redfish"},
			},
		}}}
		encoded, err := Saved(in, inv, in.Path)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(encoded)).NotTo(ContainSubstring("observationState: observed"))
		restored, err := Decode(encoded, in.Path)
		Expect(err).NotTo(HaveOccurred())
		d := restored.Site.Inventory.DPUs[0]
		Expect(d.ObservationState).To(Equal("observed"))
		Expect(d.Observations["bmc-administrator"].ReasonCode).To(Equal("BMC_ADMIN_VERIFIED"))
		Expect(d.Observations["firmware-inventory"].State).To(Equal("observed"))
		Expect(d.Observations["psid"]).To(Equal(inv.DPUs[0].Observations["psid"]))
	})

	It("continues rejecting unknown fields inside compact device and authentication records", func() {
		for _, field := range []string{"      deviceTypo: value\n", "      firmware: {firmwareTypo: value}\n", "      runtime: {runtimeTypo: value}\n"} {
			_, err := Decode([]byte(seed+"inventory:\n  formatVersion: v1alpha3\n  dpus:\n    - id: dpu-one\n"+field), "/site/input.yaml")
			Expect(err).To(HaveOccurred())
		}
	})

	It("does not infer success from omitted legacy state", func() {
		text := seed + "inventory:\n  formatVersion: v1alpha1\n  dpus: [{id: dpu-one}]\n"
		in, err := Decode([]byte(text), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(in.Site.Inventory.DPUs[0].ObservationState).To(BeEmpty())
		_, err = Decode([]byte(strings.Replace(text, "id: dpu-one", "id: dpu-one, typo: value", 1)), "/site/input.yaml")
		Expect(err).To(HaveOccurred())
	})
})
