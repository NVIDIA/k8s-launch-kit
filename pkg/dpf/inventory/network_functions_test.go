// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"encoding/json"
	"strings"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

var _ = ginkgo.Describe("typed NetworkDeviceFunction MAC facts", func() {
	ginkgo.It("round trips normalized identity-bound MACs without publishing success observations", func() {
		d := outputDeviceFixture()
		d.NetworkFunctionMACs = []string{"02:00:00:00:00:01", "f4:20:4d:16:19:b0"}
		d.Observations["networkFunctionMACs"] = Observation{State: "observed", Values: []string{"02:00:00:00:00:01", "f4:20:4d:16:19:b0"}, Tool: "redfish"}
		wire, err := DeviceToOutput(d, "dpus")
		Expect(err).NotTo(HaveOccurred())
		Expect(wire.NetworkFunctionMACs).To(Equal(d.NetworkFunctionMACs))
		for _, marshal := range []func(any) ([]byte, error){json.Marshal, yaml.Marshal} {
			encoded, err := marshal(wire)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(encoded)).NotTo(ContainSubstring("observations"))
			var decoded DeviceOutput
			if strings.HasPrefix(string(encoded), "{") {
				Expect(json.Unmarshal(encoded, &decoded)).To(Succeed())
			} else {
				Expect(yaml.Unmarshal(encoded, &decoded)).To(Succeed())
			}
			restored, err := DeviceFromOutput(decoded, "dpus")
			Expect(err).NotTo(HaveOccurred())
			Expect(restored.NetworkFunctionMACs).To(Equal(d.NetworkFunctionMACs))
			Expect(restored.Observations["networkFunctionMACs"].State).To(Equal("observed"))
			again, err := DeviceToOutput(restored, "dpus")
			Expect(err).NotTo(HaveOccurred())
			Expect(again).To(Equal(wire))
		}
		inventoryValue := Inventory{FormatVersion: FormatVersion, DPUs: []Device{d}}
		encodedJSON, err := json.Marshal(inventoryValue)
		Expect(err).NotTo(HaveOccurred())
		var fromJSON Inventory
		Expect(json.Unmarshal(encodedJSON, &fromJSON)).To(Succeed())
		Expect(fromJSON.DPUs[0].NetworkFunctionMACs).To(Equal(d.NetworkFunctionMACs))
		encodedYAML, err := yaml.Marshal(inventoryValue)
		Expect(err).NotTo(HaveOccurred())
		var fromYAML Inventory
		Expect(yaml.Unmarshal(encodedYAML, &fromYAML)).To(Succeed())
		Expect(fromYAML.DPUs[0].NetworkFunctionMACs).To(Equal(d.NetworkFunctionMACs))
	})

	ginkgo.It("preserves a failed optional MAC read as a reason without asserting MACs", func() {
		d := outputDeviceFixture()
		d.Observations["networkFunctionMACs"] = Observation{State: "unavailable", ReasonCode: "ACCESS_DENIED", Message: "The account is not authorized to read this Redfish resource.", Tool: "redfish"}
		wire, err := DeviceToOutput(d, "dpus")
		Expect(err).NotTo(HaveOccurred())
		Expect(wire.NetworkFunctionMACs).To(BeEmpty())
		Expect(wire.Issues).To(ContainElement(DeviceIssue{Field: "networkFunctionMACs", Code: "ACCESS_DENIED", Message: "The account is not authorized to read this Redfish resource.", Tool: "redfish"}))
		restored, err := DeviceFromOutput(wire, "dpus")
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.NetworkFunctionMACs).To(BeEmpty())
		Expect(restored.Observations["networkFunctionMACs"].ReasonCode).To(Equal("ACCESS_DENIED"))
	})

	ginkgo.It("rejects malformed or ineligible public MAC claims", func() {
		for _, scenario := range []struct {
			name string
			role string
			edit func(*DeviceOutput)
		}{
			{name: "multicast", role: "dpus", edit: func(w *DeviceOutput) { w.NetworkFunctionMACs = []string{"01:00:00:00:00:01"} }},
			{name: "zero", role: "dpus", edit: func(w *DeviceOutput) { w.NetworkFunctionMACs = []string{"00:00:00:00:00:00"} }},
			{name: "malformed", role: "dpus", edit: func(w *DeviceOutput) { w.NetworkFunctionMACs = []string{"02:00:00:00:00:0g"} }},
			{name: "not normalized", role: "dpus", edit: func(w *DeviceOutput) { w.NetworkFunctionMACs = []string{"02:00:00:00:00:0A"} }},
			{name: "duplicate or unsorted", role: "dpus", edit: func(w *DeviceOutput) { w.NetworkFunctionMACs = []string{"02:00:00:00:00:02", "02:00:00:00:00:01"} }},
			{name: "host role", role: "hosts", edit: func(w *DeviceOutput) { w.NetworkFunctionMACs = []string{"02:00:00:00:00:01"} }},
			{name: "not redfish", role: "dpus", edit: func(w *DeviceOutput) {
				w.Method, w.SSHEndpoint, w.BMCEndpoint = "ssh", "ssh://host.example:22", ""
				w.NetworkFunctionMACs = []string{"02:00:00:00:00:01"}
			}},
			{name: "identity absent", role: "dpus", edit: func(w *DeviceOutput) { w.ID = ""; w.NetworkFunctionMACs = []string{"02:00:00:00:00:01"} }},
			{name: "contradictory issue", role: "dpus", edit: func(w *DeviceOutput) {
				w.NetworkFunctionMACs = []string{"02:00:00:00:00:01"}
				w.Issues = append(w.Issues, DeviceIssue{Field: "networkFunctionMACs", Code: "IDENTITY_CHANGED"})
			}},
		} {
			ginkgo.By(scenario.name)
			wire := DeviceOutput{RecordKey: "record-one", Method: "redfish", ID: "dpu-one", BMCEndpoint: "https://bmc.example", Model: "B4240"}
			scenario.edit(&wire)
			_, err := DeviceFromOutput(wire, scenario.role)
			Expect(err).To(HaveOccurred())
		}
	})

	ginkgo.It("rejects a contradictory private observation when the typed value is present", func() {
		d := outputDeviceFixture()
		d.NetworkFunctionMACs = []string{"02:00:00:00:00:01"}
		wire, err := DeviceToOutput(d, "dpus")
		Expect(err).NotTo(HaveOccurred())
		Expect(wire.NetworkFunctionMACs).To(Equal(d.NetworkFunctionMACs))
		d.Observations["networkFunctionMACs"] = Observation{State: "observed", Values: []string{"02:00:00:00:00:02"}, Tool: "redfish"}
		_, err = DeviceToOutput(d, "dpus")
		Expect(err).To(HaveOccurred())
	})
})
