// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"encoding/json"
	"testing"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	yaml "gopkg.in/yaml.v3"
)

func TestInventory(t *testing.T) {
	RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "DPF inventory encoding")
}

func compactFixture() Inventory {
	return Inventory{FormatVersion: "v1alpha2", Hosts: []Device{{ID: "host-one", ObservationState: "observed"}}, DPUs: []Device{{
		ID: "dpu-one", RecordKey: "record-one", ObservationState: "observed", BMCEndpoint: "https://192.0.2.1",
		Observations: map[string]Observation{
			"psid":                       {State: "observed", Values: []string{"MT_0000001774"}, ReasonCode: "PSID_OBSERVED", Tool: "redfish"},
			"bmc-administrator":          {State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED", Tool: "redfish"},
			"firmware-inventory":         {State: "observed"},
			"managers":                   {State: "observed"},
			"client-certificate-support": {State: "unknown", ReasonCode: "NOT_EVALUATED"},
			"bmc-time":                   {State: "unknown", ReasonCode: "PROPERTY_NOT_EXPOSED", Message: "DateTime is absent"},
			"bmc-ntp":                    {State: "unavailable", ReasonCode: "HTTP_FORBIDDEN", Message: "Read denied"},
		},
	}}}
}

var _ = ginkgo.Describe("compact inventory", func() {
	ginkgo.It("writes only diagnostics in device observations without mutating retained evidence", func() {
		in := compactFixture()
		before, err := json.Marshal(inventoryWire(in))
		Expect(err).NotTo(HaveOccurred())
		encoded, err := json.Marshal(in)
		Expect(err).NotTo(HaveOccurred())
		var wire inventoryWire
		Expect(json.Unmarshal(encoded, &wire)).To(Succeed())
		Expect(wire.DPUs[0].Observations).To(HaveLen(2))
		Expect(wire.DPUs[0].Observations).To(HaveKey("bmc-time"))
		Expect(wire.DPUs[0].Observations).To(HaveKey("bmc-ntp"))
		Expect(wire.DPUs[0].PSID).To(Equal("MT_0000001774"))
		Expect(wire.DPUs[0].Authentication.BMCAdministrator).To(Equal("verified"))
		Expect(wire.DPUs[0].FirmwareInventoryComplete).To(BeTrue())
		Expect(string(encoded)).NotTo(ContainSubstring("observationState"))
		after, err := json.Marshal(inventoryWire(in))
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
	})

	ginkgo.It("restores identity and required positive evidence from JSON and YAML", func() {
		in := compactFixture()
		encoded, err := json.Marshal(in)
		Expect(err).NotTo(HaveOccurred())
		var fromJSON Inventory
		Expect(json.Unmarshal(encoded, &fromJSON)).To(Succeed())
		encodedYAML, err := yaml.Marshal(in)
		Expect(err).NotTo(HaveOccurred())
		var fromYAML Inventory
		Expect(yaml.Unmarshal(encodedYAML, &fromYAML)).To(Succeed())
		Expect(NormalizeInventory(&fromYAML)).To(Succeed())
		for _, restored := range []Inventory{fromJSON, fromYAML} {
			Expect(restored.DPUs[0].ObservationState).To(Equal("observed"))
			Expect(restored.Hosts[0].ObservationState).To(Equal("observed"))
			Expect(restored.DPUs[0].Observations["psid"]).To(Equal(in.DPUs[0].Observations["psid"]))
			Expect(restored.DPUs[0].Observations["bmc-administrator"].ReasonCode).To(Equal("BMC_ADMIN_VERIFIED"))
			Expect(restored.DPUs[0].Observations["firmware-inventory"].State).To(Equal("observed"))
			Expect(NormalizeInventory(&restored)).To(Succeed())
		}
		again, err := json.Marshal(fromJSON)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(encoded))
		againYAML, err := yaml.Marshal(fromYAML)
		Expect(err).NotTo(HaveOccurred())
		Expect(againYAML).To(Equal(encodedYAML))
	})

	ginkgo.It("preserves explicit failures and unknown records even when an ID was assigned", func() {
		in := Inventory{FormatVersion: "v1alpha2", DPUs: []Device{
			{ID: "before-recheck", ObservationState: "unavailable", ReasonCode: "IDENTITY_CHANGED"},
			{ID: "not-verified", ObservationState: ""},
			{ObservationState: "observed"},
		}}
		encoded, err := json.Marshal(in)
		Expect(err).NotTo(HaveOccurred())
		var restored Inventory
		Expect(json.Unmarshal(encoded, &restored)).To(Succeed())
		Expect(restored.DPUs[0].ObservationState).To(Equal("unavailable"))
		Expect(restored.DPUs[0].ReasonCode).To(Equal("IDENTITY_CHANGED"))
		Expect(restored.DPUs[1].ObservationState).To(Equal("unknown"))
		Expect(restored.DPUs[2].ObservationState).To(Equal("unknown"))
	})

	ginkgo.It("retains legacy encoding and never infers missing legacy state or proof", func() {
		in := compactFixture()
		in.FormatVersion = "v1alpha1"
		in.Hosts[0].ObservationState = ""
		encoded, err := json.Marshal(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(encoded)).To(ContainSubstring(`"observationState":"observed"`))
		var restored Inventory
		Expect(json.Unmarshal(encoded, &restored)).To(Succeed())
		Expect(restored.Hosts[0].ObservationState).To(BeEmpty())
		Expect(restored.DPUs[0].Observations).To(Equal(in.DPUs[0].Observations))
	})

	ginkgo.DescribeTable("rejects conflicting compact positive facts", func(extra string) {
		var result Inventory
		data := `{"formatVersion":"v1alpha2","dpus":[{"id":"dpu-one",` + extra + `}]}`
		Expect(json.Unmarshal([]byte(data), &result)).To(MatchError(ContainSubstring("conflicting")))
		var raw inventoryWire
		Expect(json.Unmarshal([]byte(data), &raw)).To(Succeed())
		before, err := json.Marshal(raw)
		Expect(err).NotTo(HaveOccurred())
		_, err = json.Marshal(Inventory(raw))
		Expect(err).To(MatchError(ContainSubstring("conflicting")))
		after, err := json.Marshal(raw)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
	},
		ginkgo.Entry("administrator", `"authentication":{"bmcAdministrator":"verified"},"observations":{"bmc-administrator":{"state":"failed","reasonCode":"BMC_ADMIN_REQUIRED"}}`),
		ginkgo.Entry("PSID", `"psid":"MT_A","observations":{"psid":{"state":"unavailable","reasonCode":"PSID_CHANGED"}}`),
		ginkgo.Entry("different successful PSID", `"psid":"MT_A","observations":{"psid":{"state":"observed","values":["MT_B"],"reasonCode":"PSID_OBSERVED","tool":"redfish"}}`),
		ginkgo.Entry("firmware source", `"firmwareInventoryComplete":true,"observations":{"firmware-inventory":{"state":"unavailable","reasonCode":"HTTP_FORBIDDEN"}}`),
	)

	ginkgo.It("retains failures whose reason happens to say not evaluated", func() {
		in := Inventory{FormatVersion: "v1alpha2", Hosts: []Device{{ID: "host-one", ObservationState: "observed", Observations: map[string]Observation{
			"client-certificate-support": {State: "failed", ReasonCode: "NOT_EVALUATED"},
			"other-fact":                 {State: "unknown", ReasonCode: "NOT_EVALUATED"},
		}}}}
		encoded, err := json.Marshal(in)
		Expect(err).NotTo(HaveOccurred())
		var restored Inventory
		Expect(json.Unmarshal(encoded, &restored)).To(Succeed())
		Expect(restored.Hosts[0].Observations).To(Equal(in.Hosts[0].Observations))
	})

	ginkgo.It("keeps JSON unknown-field validation inside nested inventory records", func() {
		var result Inventory
		Expect(json.Unmarshal([]byte(`{"formatVersion":"v1alpha2","dpus":[{"authentication":{"administratorTypo":"verified"}}]}`), &result)).To(HaveOccurred())
	})
})
