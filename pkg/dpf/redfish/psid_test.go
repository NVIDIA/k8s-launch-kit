// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("BF4 Redfish PSID observation", func() {
	It("matches DPF System AssetTag precedence and validates only the selected source", func() {
		const a = "MT_0000001774"
		for _, tc := range []struct {
			name         string
			system, card Document
			reason       string
		}{
			{"System value", Document{"AssetTag": a}, Document{}, "PSID_OBSERVED"},
			{"chassis fallback", Document{}, Document{"AssetTag": a}, "PSID_OBSERVED"},
			{"null System fallback", Document{"AssetTag": nil}, Document{"AssetTag": a}, "PSID_OBSERVED"},
			{"empty System value does not fall back", Document{"AssetTag": ""}, Document{"AssetTag": a}, "PSID_INVALID"},
			{"N/A System value does not fall back", Document{"AssetTag": "N/A"}, Document{"AssetTag": a}, "PSID_INVALID"},
			{"matching values", Document{"AssetTag": a}, Document{"AssetTag": a}, "PSID_OBSERVED"},
			{"different chassis spelling is ignored", Document{"AssetTag": a}, Document{"AssetTag": strings.ToLower(a)}, "PSID_OBSERVED"},
			{"System with N/A chassis", Document{"AssetTag": a}, Document{"AssetTag": "N/A"}, "PSID_OBSERVED"},
			{"different chassis value is ignored", Document{"AssetTag": a}, Document{"AssetTag": "MT_0000001775"}, "PSID_OBSERVED"},
			{"malformed System type", Document{"AssetTag": 42}, Document{"AssetTag": a}, "PSID_INVALID"},
			{"malformed unselected chassis type is ignored", Document{"AssetTag": a}, Document{"AssetTag": false}, "PSID_OBSERVED"},
			{"invalid control text", Document{"AssetTag": "credential-canary\n"}, Document{"AssetTag": a}, "PSID_INVALID"},
			{"invalid whitespace", Document{"AssetTag": " MT_0000001774"}, Document{}, "PSID_INVALID"},
			{"invalid selected prefix", Document{"AssetTag": "ABC123"}, Document{}, "PSID_INVALID"},
			{"invalid selected lowercase prefix", Document{"AssetTag": strings.ToLower(a)}, Document{}, "PSID_INVALID"},
			{"invalid 33-character selected PSID", Document{"AssetTag": "MT_" + strings.Repeat("A", 30)}, Document{}, "PSID_INVALID"},
			{"invalid oversized", Document{"AssetTag": strings.Repeat("A", 65)}, Document{}, "PSID_INVALID"},
			{"missing", Document{}, Document{}, "PSID_NOT_EXPOSED"},
			{"null", Document{"AssetTag": nil}, Document{"AssetTag": nil}, "PSID_NOT_EXPOSED"},
			{"selected System sentinel", Document{"AssetTag": "N/A"}, Document{"AssetTag": ""}, "PSID_INVALID"},
		} {
			By(tc.name)
			o := bf4PSID(tc.system, tc.card, tc.system, tc.card)
			Expect(o.ReasonCode).To(Equal(tc.reason))
			Expect(o.Tool).To(Equal("redfish"))
			Expect(o.Message).NotTo(BeEmpty())
			if tc.reason == "PSID_OBSERVED" {
				Expect(o.State).To(Equal("observed"))
				Expect(o.Values).To(Equal([]string{a}))
			} else {
				Expect(o.State).NotTo(Equal("observed"))
				Expect(o.Values).To(BeEmpty())
			}
			encoded, err := json.Marshal(o)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(encoded)).NotTo(ContainSubstring("credential-canary"))
		}
	})
	It("invalidates the selected value or source when it changes during reread", func() {
		for _, tc := range []struct{ system, card, currentSystem, currentCard Document }{
			{Document{"AssetTag": "MT_0000001774"}, Document{}, Document{"AssetTag": "MT_0000001775"}, Document{}},
			{Document{}, Document{"AssetTag": "MT_0000001774"}, Document{}, Document{"AssetTag": "N/A"}},
			{Document{}, Document{"AssetTag": "MT_0000001774"}, Document{"AssetTag": "MT_0000001774"}, Document{"AssetTag": "MT_0000001774"}},
		} {
			o := bf4PSID(tc.system, tc.card, tc.currentSystem, tc.currentCard)
			Expect(o.State).To(Equal("unavailable"))
			Expect(o.ReasonCode).To(Equal("PSID_CHANGED"))
			Expect(o.Values).To(BeEmpty())
		}
	})
	It("ignores unrelated chassis AssetTag changes when DPF selects System", func() {
		const systemPSID = "MT_0000001774"
		observation := bf4PSID(
			Document{"AssetTag": systemPSID}, Document{"AssetTag": "MT_0000001775"},
			Document{"AssetTag": systemPSID}, Document{"AssetTag": "N/A"},
		)
		Expect(observation.State).To(Equal("observed"))
		Expect(observation.Values).To(Equal([]string{systemPSID}))
	})
	It("treats absent and null System AssetTag as the same DPF fallback selection", func() {
		const chassisPSID = "MT_0000001774"
		observation := bf4PSID(Document{}, Document{"AssetTag": chassisPSID}, Document{"AssetTag": nil}, Document{"AssetTag": chassisPSID})
		Expect(observation.State).To(Equal("observed"))
		Expect(observation.Values).To(Equal([]string{chassisPSID}))
	})
})
