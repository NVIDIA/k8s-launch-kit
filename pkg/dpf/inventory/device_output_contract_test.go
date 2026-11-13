// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory_test

import (
	"encoding/json"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func firmwareDevice() inventory.Device {
	return inventory.Device{ID: "dpu-one", RecordKey: "dpu-source", InputRef: "/discovery/dpus/0", BMCEndpoint: "https://dpu.invalid", SerialNumber: "CARDONE", Model: "B3210", ObservationState: "observed", Observations: map[string]inventory.Observation{"firmware-inventory": {State: "observed"}}}
}

func nicCheck(d inventory.Device) inventory.Check {
	release, err := releases.Resolve("26.4.0")
	Expect(err).NotTo(HaveOccurred())
	for _, check := range discovery.SoftwareChecks(&inventory.Inventory{Software: &release, DPUs: []inventory.Device{d}}) {
		if check.ID == "dpf.software.nic" {
			return check
		}
	}
	ginkgo.Fail("NIC software check was not evaluated")
	return inventory.Check{}
}

var _ = ginkgo.Describe("typed firmware compatibility boundary", func() {
	ginkgo.It("preserves padded measured versions without promoting compatibility", func() {
		d := firmwareDevice()
		d.Firmware = []inventory.Firmware{{Component: "nic", State: "known", RawVersion: " 32.38.1002 ", Activation: "running"}}
		before := nicCheck(d)
		Expect(before.State).To(Equal("unknown"))
		Expect(before.ReasonCode).To(Equal("SOFTWARE_VERSION_UNCOMPARABLE"))
		in := inventory.Inventory{FormatVersion: inventory.FormatVersion, DPUs: []inventory.Device{d}}
		encoded, err := json.Marshal(in)
		Expect(err).NotTo(HaveOccurred())
		var restored inventory.Inventory
		Expect(json.Unmarshal(encoded, &restored)).To(Succeed())
		Expect(restored.DPUs[0].Firmware[0].RawVersion).To(Equal(" 32.38.1002 "))
		Expect(nicCheck(restored.DPUs[0]).State).To(Equal(before.State))
		Expect(nicCheck(restored.DPUs[0]).ReasonCode).To(Equal(before.ReasonCode))
	})

	ginkgo.DescribeTable("keeps unusable optional versions diagnostic without aborting publication", func(raw string) {
		d := firmwareDevice()
		d.Firmware = []inventory.Firmware{
			{Component: "nic", State: "known", RawVersion: "32.38.1002", Activation: "running"},
			{Component: "nic", State: "unknown", RawVersion: raw, Activation: "unknown", ReasonCode: "VERSION_NOT_EXPOSED"},
		}
		w, err := inventory.DeviceToOutput(d, "dpus")
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Firmware.NIC).To(HaveLen(1))
		Expect(w.Issues).To(ContainElement(inventory.DeviceIssue{Field: "firmware.nic", Code: "VERSION_INVALID", Message: "The firmware version is blank or contains unsupported characters."}))
		restored, err := inventory.DeviceFromOutput(w, "dpus")
		Expect(err).NotTo(HaveOccurred())
		Expect(nicCheck(restored).State).To(Equal("passed"))
		_, err = json.Marshal(inventory.Inventory{FormatVersion: inventory.FormatVersion, DPUs: []inventory.Device{d}})
		Expect(err).NotTo(HaveOccurred())
	}, ginkgo.Entry("blank whitespace", "  "), ginkgo.Entry("newline", "v1.2\n"), ginkgo.Entry("control character", "v1.2\x00"))

	ginkgo.It("retains actual conflicts when an optional version is unusable", func() {
		d := firmwareDevice()
		d.Firmware = []inventory.Firmware{
			{Component: "nic", State: "known", RawVersion: "32.38.1002", Activation: "running"},
			{Component: "nic", State: "unknown", RawVersion: "  ", Activation: "unknown", ActivationReasonCode: "ACTIVATION_CONFLICT"},
		}
		w, err := inventory.DeviceToOutput(d, "dpus")
		Expect(err).NotTo(HaveOccurred())
		restored, err := inventory.DeviceFromOutput(w, "dpus")
		Expect(err).NotTo(HaveOccurred())
		Expect(nicCheck(restored).State).To(Equal("unknown"))
		Expect(nicCheck(restored).ReasonCode).To(Equal("SOFTWARE_VERSION_CONFLICT"))
	})

	ginkgo.DescribeTable("rejects failure locations that would otherwise be ignored", func(field, code string) {
		w := inventory.DeviceOutput{ID: "dpu-one", RecordKey: "dpu-source", Method: "redfish", BMCEndpoint: "https://dpu.invalid", SerialNumber: "CARDONE", Model: "B3210", FirmwareInventoryComplete: true,
			Firmware: &inventory.FirmwareOutput{NIC: []inventory.FirmwareVersionOutput{{Version: "32.38.1002", Activation: "running"}}},
			Issues:   []inventory.DeviceIssue{{Field: field, Code: code}}}
		_, err := inventory.DeviceFromOutput(w, "dpus")
		Expect(err).To(HaveOccurred())
	},
		ginkgo.Entry("mis-scoped activation conflict", "firmware.nic", "ACTIVATION_CONFLICT"),
		ginkgo.Entry("absent firmware index", "firmware.nic[9]", "ACTIVATION_CONFLICT"),
		ginkgo.Entry("unknown component", "firmware.unknown[0].activation", "ACTIVATION_CONFLICT"),
		ginkgo.Entry("unsupported firmware property", "firmware.nic[0].unrecognized", "ACTIVATION_CONFLICT"),
		ginkgo.Entry("noncanonical index", "firmware.nic[00].activation", "ACTIVATION_CONFLICT"),
		ginkgo.Entry("absent adapter index", "adapters[9].identity", "CARD_SERIAL_UNVERIFIED"),
		ginkgo.Entry("absent hardware card", "runtime.hardware.cards[9].psid", "PSID_CONFLICT"),
	)
})
