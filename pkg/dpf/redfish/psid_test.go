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
	It("keeps System-first and fallback values while rejecting unsafe or contradictory values", func() {
		const a, b = "MT_0000001774", "MT_0000001775"
		for _, tc := range []struct {
			name         string
			system, card Document
			reason       string
		}{
			{"System value", Document{"AssetTag": a}, Document{}, "PSID_OBSERVED"},
			{"chassis fallback", Document{}, Document{"AssetTag": a}, "PSID_OBSERVED"},
			{"null System fallback", Document{"AssetTag": nil}, Document{"AssetTag": a}, "PSID_OBSERVED"},
			{"empty System fallback", Document{"AssetTag": ""}, Document{"AssetTag": a}, "PSID_OBSERVED"},
			{"N/A System fallback", Document{"AssetTag": "N/A"}, Document{"AssetTag": a}, "PSID_OBSERVED"},
			{"matching values", Document{"AssetTag": a}, Document{"AssetTag": a}, "PSID_OBSERVED"},
			{"different spelling", Document{"AssetTag": a}, Document{"AssetTag": strings.ToLower(a)}, "PSID_CONFLICT"},
			{"System with N/A chassis", Document{"AssetTag": a}, Document{"AssetTag": "N/A"}, "PSID_OBSERVED"},
			{"contradictory values", Document{"AssetTag": a}, Document{"AssetTag": b}, "PSID_CONFLICT"},
			{"malformed System type", Document{"AssetTag": 42}, Document{"AssetTag": a}, "PSID_INVALID"},
			{"malformed chassis type", Document{"AssetTag": a}, Document{"AssetTag": false}, "PSID_INVALID"},
			{"invalid control text", Document{"AssetTag": "credential-canary\n"}, Document{"AssetTag": a}, "PSID_INVALID"},
			{"invalid whitespace", Document{"AssetTag": " MT_0000001774"}, Document{}, "PSID_INVALID"},
			{"invalid oversized", Document{"AssetTag": strings.Repeat("A", 65)}, Document{}, "PSID_INVALID"},
			{"missing", Document{}, Document{}, "PSID_NOT_EXPOSED"},
			{"null", Document{"AssetTag": nil}, Document{"AssetTag": nil}, "PSID_NOT_EXPOSED"},
			{"sentinels", Document{"AssetTag": "N/A"}, Document{"AssetTag": ""}, "PSID_NOT_EXPOSED"},
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
	It("invalidates initial values when either source field changes, including absence", func() {
		for _, tc := range []struct{ system, card, currentSystem, currentCard Document }{
			{Document{"AssetTag": "MT_0000001774"}, Document{}, Document{"AssetTag": "MT_0000001775"}, Document{}},
			{Document{}, Document{"AssetTag": "MT_0000001774"}, Document{}, Document{"AssetTag": "N/A"}},
			{Document{}, Document{"AssetTag": "MT_0000001774"}, Document{"AssetTag": "MT_0000001774"}, Document{"AssetTag": "MT_0000001774"}},
			{Document{}, Document{"AssetTag": "MT_0000001774"}, Document{"AssetTag": nil}, Document{"AssetTag": "MT_0000001774"}},
		} {
			o := bf4PSID(tc.system, tc.card, tc.currentSystem, tc.currentCard)
			Expect(o.State).To(Equal("unavailable"))
			Expect(o.ReasonCode).To(Equal("PSID_CHANGED"))
			Expect(o.Values).To(BeEmpty())
		}
	})
})
