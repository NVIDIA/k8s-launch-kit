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
	It("keeps System-first and fallback provenance while rejecting unsafe or contradictory values", func() {
		const a, b = "MT_0000001774", "MT_0000001775"
		for _, tc := range []struct {
			name         string
			system, card Document
			reason       string
			sources      int
		}{
			{"System value", Document{"AssetTag": a}, Document{}, "PSID_OBSERVED", 1},
			{"chassis fallback", Document{}, Document{"AssetTag": a}, "PSID_OBSERVED", 1},
			{"null System fallback", Document{"AssetTag": nil}, Document{"AssetTag": a}, "PSID_OBSERVED", 1},
			{"empty System fallback", Document{"AssetTag": ""}, Document{"AssetTag": a}, "PSID_OBSERVED", 1},
			{"N/A System fallback", Document{"AssetTag": "N/A"}, Document{"AssetTag": a}, "PSID_OBSERVED", 1},
			{"matching values", Document{"AssetTag": a}, Document{"AssetTag": a}, "PSID_OBSERVED", 2},
			{"different spelling", Document{"AssetTag": a}, Document{"AssetTag": strings.ToLower(a)}, "PSID_CONFLICT", 0},
			{"System with N/A chassis", Document{"AssetTag": a}, Document{"AssetTag": "N/A"}, "PSID_OBSERVED", 1},
			{"contradictory values", Document{"AssetTag": a}, Document{"AssetTag": b}, "PSID_CONFLICT", 0},
			{"malformed System type", Document{"AssetTag": 42}, Document{"AssetTag": a}, "PSID_INVALID", 0},
			{"malformed chassis type", Document{"AssetTag": a}, Document{"AssetTag": false}, "PSID_INVALID", 0},
			{"invalid control text", Document{"AssetTag": "credential-canary\n"}, Document{"AssetTag": a}, "PSID_INVALID", 0},
			{"invalid whitespace", Document{"AssetTag": " MT_0000001774"}, Document{}, "PSID_INVALID", 0},
			{"invalid oversized", Document{"AssetTag": strings.Repeat("A", 65)}, Document{}, "PSID_INVALID", 0},
			{"missing", Document{}, Document{}, "PSID_NOT_EXPOSED", 0},
			{"null", Document{"AssetTag": nil}, Document{"AssetTag": nil}, "PSID_NOT_EXPOSED", 0},
			{"sentinels", Document{"AssetTag": "N/A"}, Document{"AssetTag": ""}, "PSID_NOT_EXPOSED", 0},
		} {
			By(tc.name)
			sources := []string{}
			o := bf4PSID(Resource{URI: "/redfish/v1/Systems/DPU", Document: tc.system}, Resource{URI: "/redfish/v1/Chassis/Card", Document: tc.card}, tc.system, tc.card, func(uri, property string) string { sources = append(sources, uri+"#"+property); return "evidence" })
			Expect(o.ReasonCode).To(Equal(tc.reason))
			Expect(o.Tool).To(Equal("redfish"))
			Expect(o.Message).NotTo(BeEmpty())
			Expect(sources).To(HaveLen(tc.sources))
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
			o := bf4PSID(Resource{URI: "/redfish/v1/Systems/DPU", Document: tc.system}, Resource{URI: "/redfish/v1/Chassis/Card", Document: tc.card}, tc.currentSystem, tc.currentCard, func(string, string) string { Fail("changed source must not gain eligible evidence"); return "" })
			Expect(o.State).To(Equal("unavailable"))
			Expect(o.ReasonCode).To(Equal("PSID_CHANGED"))
			Expect(o.Values).To(BeEmpty())
		}
	})
})
