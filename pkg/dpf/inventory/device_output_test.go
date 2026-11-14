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

func outputDeviceFixture() Device {
	return Device{ID: "dpu-one", RecordKey: "record-one", InputRef: "/discovery/dpus/0", ObservationState: "observed",
		BMCEndpoint: "https://192.0.2.1", Model: "B4240", SerialNumber: "MT001234", ReaderAdapter: "bluefield-card-v1",
		Observations: map[string]Observation{
			"psid":               {State: "observed", Values: []string{"MT_000001"}, ReasonCode: "PSID_OBSERVED", Tool: "redfish"},
			"bmc-administrator":  {State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED", Tool: "redfish"},
			"firmware-inventory": {State: "observed"},
		},
	}
}

var _ = ginkgo.Describe("typed DeviceOutput", func() {
	ginkgo.It("publishes named component versions and required authorization without bookkeeping", func() {
		d := outputDeviceFixture()
		d.Qualification, d.InventoryCompleteness = "unqualified", "partial"
		d.Time = TimeObservation{State: "known", DateTime: "2026-11-13"}
		d.Firmware = []Firmware{
			{Component: "bmc", RawVersion: "26.1", State: "known", Activation: "running", RawID: "private-id", RawName: "private-name", ResourceURI: "/private/resource", EvidenceRefs: []string{"private-evidence"}},
			{Component: "bmc", RawVersion: "26.2", State: "known", Activation: "staged"},
			{Component: "nic", RawVersion: "32.4", State: "known", Activation: "unknown", ActivationReasonCode: "ACTIVATION_NOT_EXPOSED"},
			{Component: "dpu-os", State: "unknown", ReasonCode: "COMPONENT_NOT_EXPOSED"},
			{Component: "other", RawVersion: "arbitrary", State: "known", Activation: "unknown"},
		}
		w, err := DeviceToOutput(d, "dpus")
		Expect(err).NotTo(HaveOccurred())
		Expect(w.PSID).To(Equal("MT_000001"))
		Expect(w.BMCAdministratorVerified).To(BeTrue())
		Expect(w.FirmwareInventoryComplete).To(BeTrue())
		Expect(w.Firmware.BMC).To(HaveLen(2))
		Expect(w.Firmware.NIC[0].Activation).To(BeEmpty())
		Expect(w.Issues).To(BeEmpty())
		encoded, err := json.Marshal(w)
		Expect(err).NotTo(HaveOccurred())
		for _, removed := range []string{"observations", "observationState", "readerAdapter", "qualification", "inventoryCompleteness", "authentication", "evidenceRefs", "rawID", "rawName", "resourceURI", "private-", "COMPONENT_NOT_EXPOSED", "ACTIVATION_NOT_EXPOSED", "arbitrary"} {
			Expect(string(encoded)).NotTo(ContainSubstring(removed))
		}
		for _, marshal := range []func(any) ([]byte, error){json.Marshal, yaml.Marshal} {
			data, err := marshal(w)
			Expect(err).NotTo(HaveOccurred())
			var copy DeviceOutput
			if strings.HasPrefix(string(data), "{") {
				Expect(json.Unmarshal(data, &copy)).To(Succeed())
			} else {
				Expect(yaml.Unmarshal(data, &copy)).To(Succeed())
			}
			restored, err := DeviceFromOutput(copy, "dpus")
			Expect(err).NotTo(HaveOccurred())
			Expect(restored.ObservationState).To(Equal("observed"))
			Expect(restored.ReaderAdapter).To(Equal("bluefield-card-v1"))
			Expect(restored.Observations["psid"].Values).To(Equal([]string{"MT_000001"}))
			again, err := DeviceToOutput(restored, "dpus")
			Expect(err).NotTo(HaveOccurred())
			Expect(again).To(Equal(w))
		}
	})

	ginkgo.It("does not promote an ID assigned before a failed identity recheck", func() {
		d := outputDeviceFixture()
		d.ObservationState, d.ReasonCode = "unavailable", "INVENTORY_CHANGED"
		w, err := DeviceToOutput(d, "dpus")
		Expect(err).NotTo(HaveOccurred())
		Expect(w.ID).To(BeEmpty())
		Expect(w.SerialNumber).To(Equal(d.SerialNumber))
		Expect(w.PSID).To(BeEmpty())
		Expect(w.BMCAdministratorVerified).To(BeFalse())
		Expect(w.FirmwareInventoryComplete).To(BeFalse())
		Expect(w.Issues).To(ContainElement(DeviceIssue{Field: "identity", Code: "INVENTORY_CHANGED"}))
		restored, err := DeviceFromOutput(w, "dpus")
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.ID).To(BeEmpty())
		Expect(restored.ObservationState).NotTo(Equal("observed"))
		Expect(restored.Observations["psid"].State).NotTo(Equal("observed"))
	})

	ginkgo.DescribeTable("preserves rejected PSID evidence independently of a matching SSH measurement", func(code string) {
		d := outputDeviceFixture()
		d.Observations["psid"] = Observation{State: "unknown", ReasonCode: code, Tool: "redfish"}
		d.Runtime = &RuntimeObservation{Hardware: &HardwareObservation{Cards: []CardHardware{{Domain: "host-pci", PCIAddress: "0000:21:00.0", SerialNumber: d.SerialNumber, PSID: Observation{State: "observed", Values: []string{"MT_000001"}}}}}}
		w, err := DeviceToOutput(d, "dpus")
		Expect(err).NotTo(HaveOccurred())
		restored, err := DeviceFromOutput(w, "dpus")
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.Observations["psid"].ReasonCode).To(Equal(code))
		Expect(restored.Observations["psid"].State).NotTo(Equal("observed"))
		Expect(restored.Runtime.Hardware.Cards[0].PSID.Values).To(Equal([]string{"MT_000001"}))
	}, ginkgo.Entry("conflict", "PSID_CONFLICT"), ginkgo.Entry("changed", "PSID_CHANGED"), ginkgo.Entry("invalid", "PSID_INVALID"), ginkgo.Entry("late recheck", "PSID_RECHECK_FAILED"))

	ginkgo.It("retains named next-boot values, explicit zero, host restrictions and real failures", func() {
		d := outputDeviceFixture()
		d.Observations["psid"] = Observation{State: "unknown", ReasonCode: "PSID_NOT_EXPOSED"}
		d.Runtime = &RuntimeObservation{Observations: map[string]Observation{
			"board-product": {State: "observed", Values: []string{"B4240"}}, "mft-version": {State: "observed", Values: []string{"4.33.0-169"}},
			"hostname": {State: "observed"}, "neighbors": {State: "unavailable", ReasonCode: "TOOL_NOT_AVAILABLE", Tool: "ip"},
		}, Hardware: &HardwareObservation{Cards: []CardHardware{{Domain: "arm-pci", PCIAddress: "0002:21:00.0", Firmware: Observation{State: "observed", Values: []string{"32.4"}},
			NVConfig: map[string]Observation{
				"INTERNAL_CPU_OFFLOAD_ENGINE": {State: "observed", Values: []string{"next-boot=ENABLED(1)"}},
				"NUM_OF_VFS":                  {State: "observed", Values: []string{"next-boot=0"}},
			}, Privilege: Observation{State: "observed", Values: []string{"host[0].level=RESTRICTED", "host[0].disable_rshim=FALSE", "host[1].level=PRIVILEGED"}},
		}}}}
		w, err := DeviceToOutput(d, "dpus")
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Runtime.MFTVersion).To(Equal("4.33.0-169"))
		Expect(*w.Runtime.Hardware.Cards[0].NextBoot.NumVFs).To(BeZero())
		Expect(*w.Runtime.Hardware.Cards[0].HostPrivileges[0].DisableRShim).To(BeFalse())
		Expect(w.Runtime.Hardware.Cards[0].HostPrivileges[1].DisableRShim).To(BeNil())
		Expect(w.Issues).To(ContainElement(DeviceIssue{Field: "runtime.neighbors", Code: "TOOL_NOT_AVAILABLE", Tool: "ip"}))
		Expect(hasIssue(w.Issues, "runtime.hostname")).To(BeFalse())
		restored, err := DeviceFromOutput(w, "dpus")
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.Runtime.Hardware.Cards[0].NVConfig["NUM_OF_VFS"].Values).To(Equal([]string{"next-boot=0"}))
		Expect(restored.Runtime.Hardware.Cards[0].NVConfig["INTERNAL_CPU_RSHIM"].State).To(BeEmpty())
		Expect(restored.Runtime.Hardware.Cards[0].Privilege.Values).To(ContainElement("host[0].disable_rshim=FALSE"))
	})

	ginkgo.It("exports host rshim and rejects promoting failed adapter serial context", func() {
		d := Device{ID: "host-one", RecordKey: "host-record", ObservationState: "observed", SSHEndpoint: "ssh://192.0.2.2:22", ReaderAdapter: "linux-host-ssh-v1",
			Adapters: []Adapter{{Key: "card-one", SerialNumber: "candidate", IdentityState: "unknown", ReasonCode: "CARD_SERIAL_MISSING"}},
			Runtime:  &RuntimeObservation{Hardware: &HardwareObservation{RShim: Observation{State: "observed", Values: []string{"LoadState=loaded", "ActiveState=active"}}}}}
		w, err := DeviceToOutput(d, "hosts")
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Runtime.Hardware.RShim.ActiveState).To(Equal("active"))
		restored, err := DeviceFromOutput(w, "hosts")
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.Adapters[0].SerialNumber).To(BeEmpty())
		Expect(restored.Adapters[0].SerialNumberRaw).To(Equal("candidate"))
		Expect(restored.Adapters[0].IdentityState).To(Equal("unknown"))
		Expect(restored.Adapters[0].ReasonCode).To(Equal("CARD_SERIAL_MISSING"))
	})

	ginkgo.It("keeps component and activation conflicts ineligible across the boundary", func() {
		d := outputDeviceFixture()
		d.Firmware = []Firmware{
			{Component: "nic", RawVersion: "32.1", State: "conflict", ReasonCode: "FIRMWARE_VERSION_CONFLICT", Activation: "running"},
			{Component: "nic", RawVersion: "32.2", State: "conflict", ReasonCode: "FIRMWARE_VERSION_CONFLICT", Activation: "unknown", ActivationReasonCode: "ACTIVATION_CONFLICT"},
		}
		w, err := DeviceToOutput(d, "dpus")
		Expect(err).NotTo(HaveOccurred())
		restored, err := DeviceFromOutput(w, "dpus")
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.Firmware).To(HaveLen(2))
		for _, f := range restored.Firmware {
			Expect(f.State).To(Equal("conflict"))
		}
		Expect(restored.Firmware[0].ActivationReasonCode).To(Equal("ACTIVATION_CONFLICT"))
	})

	ginkgo.It("rejects inconsistent source, authorization, component and configuration claims", func() {
		d := outputDeviceFixture()
		d.ReaderAdapter = "unsupported-reader"
		_, err := DeviceToOutput(d, "dpus")
		Expect(err).To(HaveOccurred())
		w, err := DeviceToOutput(outputDeviceFixture(), "dpus")
		Expect(err).NotTo(HaveOccurred())
		for _, mutate := range []func(*DeviceOutput){
			func(w *DeviceOutput) {
				w.Issues = append(w.Issues, DeviceIssue{Field: "identity", Code: "INVENTORY_CHANGED"})
			},
			func(w *DeviceOutput) { w.SSHEndpoint = "ssh://192.0.2.3:22" },
			func(w *DeviceOutput) { w.PSID = "invalid" },
			func(w *DeviceOutput) { w.Issues = append(w.Issues, DeviceIssue{Field: "psid", Code: "PSID_CONFLICT"}) },
			func(w *DeviceOutput) {
				w.Firmware = &FirmwareOutput{NIC: []FirmwareVersionOutput{{Version: "32.1", Activation: "running"}, {Version: "32.2", Activation: "running"}}}
			},
			func(w *DeviceOutput) {
				w.Firmware = &FirmwareOutput{NIC: []FirmwareVersionOutput{{Version: "32.1", Activation: "unknown"}}}
			},
		} {
			copy := w
			mutate(&copy)
			_, err := DeviceFromOutput(copy, "dpus")
			Expect(err).To(HaveOccurred())
		}
	})
})

var _ = ginkgo.Describe("interface output ownership", func() {
	ginkgo.It("copies optional administrative state without aliasing the observation", func() {
		up := true
		source := []Interface{{Name: "eth0", Index: 2, MTU: 1500, AdminUp: &up, Addresses: []string{"192.0.2.10/24"}}}
		cloned := cloneInterfaces(source)
		*cloned[0].AdminUp = false
		cloned[0].Addresses[0] = "192.0.2.20/24"
		Expect(*source[0].AdminUp).To(BeTrue())
		Expect(source[0].Addresses).To(Equal([]string{"192.0.2.10/24"}))
	})
})
