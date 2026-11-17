// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

var _ = Describe("devlink firmware observations", func() {
	It("keeps running, stored and fixed facts separate and keyed by exact PCI address", func() {
		facts, err := parseDevlinkInfo(`{"info":{"pci/0000:21:00.0":{"driver":"mlx5_core","versions":{"fixed":{"fw.psid":"MT_0000000001"},"running":{"fw.version":"82.48.4036","fw":"82.48.4036"},"stored":{"fw.version":"82.48.4000","fw":"82.48.4000"}}},"auxiliary/mlx5_core.eth.0":{"driver":"mlx5_core.eth"}}}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(facts).To(HaveKey("0000:21:00.0"))
		Expect(facts["0000:21:00.0"].running).To(Equal("82.48.4036"))
		Expect(facts["0000:21:00.0"].stored).To(Equal("82.48.4000"))
		Expect(facts["0000:21:00.0"].psid).To(Equal("MT_0000000001"))
		Expect(facts).NotTo(HaveKey("auxiliary/mlx5_core.eth.0"))
	})

	It("rejects malformed containers, bad values, alias conflicts and duplicate normalized BDFs", func() {
		for _, text := range []string{
			`[]`,
			`{"notInfo":{}}`,
			`{"info":[]}`,
			`{"info":{"pci/0000:2A:00.0":{},"pci/0000:2a:00.0":{}}}`,
		} {
			_, err := parseDevlinkInfo(text)
			Expect(err).To(HaveOccurred(), "input: %s", text)
		}
		facts, err := parseDevlinkInfo(`{"info":{"pci/0000:21:00.0":{"versions":{"running":{"fw.version":42}}}}}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(facts["0000:21:00.0"].runningErr).To(HaveOccurred())
		facts, err = parseDevlinkInfo(`{"info":{"pci/0000:21:00.0":{"versions":{"running":{"fw.version":"82.1.0","fw":"82.1.1"}}}}}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(facts["0000:21:00.0"].runningConflict).To(BeTrue())
		facts, err = parseDevlinkInfo(`{"info":{"pci/0000:21:00.0":{"versions":{"running":{"fw.version":"82.1.0"},"stored":{"fw.version":42},"fixed":{"fw.psid":"MT_0000000001"}}}}}`)
		Expect(err).NotTo(HaveOccurred())
		Expect(facts["0000:21:00.0"].running).To(Equal("82.1.0"))
		Expect(facts["0000:21:00.0"].storedErr).To(HaveOccurred())
		Expect(facts["0000:21:00.0"].psid).To(Equal("MT_0000000001"))
	})

	It("uses one read-only host-wide fallback only when an MFT card read is unavailable", func() {
		r := hostRunner()
		r.values["mft-version"] = "mlxfwmanager, MFT 4.33.0-169"
		r.errors["firmware:0000:21:00.0"] = &Error{Code: "DEVICE_ACCESS_FAILED", Stage: "firmware:0000:21:00.0", Tool: "mlxfwmanager"}
		r.values["devlink-firmware"] = devlinkFixture("0000:21:00.0", "82.48.4036", "82.48.4000", "MT_0000000001")
		d, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Runtime.Observations["mft-version"].State).To(Equal("observed"))
		Expect(d.Runtime.Observations["firmware:0000:21:00.0"].ReasonCode).To(Equal("DEVICE_ACCESS_FAILED"))
		Expect(d.Runtime.Observations["devlink-firmware"].State).To(Equal("observed"))
		Expect(d.Runtime.Observations["devlink-firmware:0000:21:00.0"].State).To(Equal("observed"))
		Expect(d.Runtime.Hardware.Cards[0].SerialNumber).To(Equal(testSerial))
		Expect(d.Runtime.Hardware.Cards[0].Firmware.Values).To(Equal([]string{"82.48.4036"}))
		Expect(d.Runtime.Hardware.Cards[0].Firmware.Tool).To(Equal("devlink"))
		Expect(d.Runtime.Hardware.Cards[0].PSID.Values).To(Equal([]string{"MT_0000000001"}))
		Expect(strings.Count(strings.Join(r.calls, "\n"), " devlink -j dev info") == 1).To(BeTrue())

		wire, err := inventory.DeviceToOutput(d, "hosts")
		Expect(err).NotTo(HaveOccurred())
		Expect(wire.Runtime.Hardware.Cards[0].FirmwareVersion).To(Equal("82.48.4036"))
		restored, err := inventory.DeviceFromOutput(wire, "hosts")
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.Runtime.Hardware.Cards[0].Firmware.Values).To(Equal([]string{"82.48.4036"}))

		handoff := inventory.Inventory{FormatVersion: inventory.FormatVersion, Hosts: []inventory.Device{d}}
		jsonBytes, err := json.Marshal(handoff)
		Expect(err).NotTo(HaveOccurred())
		var jsonSaved inventory.Inventory
		Expect(json.Unmarshal(jsonBytes, &jsonSaved)).To(Succeed())
		Expect(jsonSaved.Hosts[0].Runtime.Hardware.Cards[0].Firmware.Values).To(Equal([]string{"82.48.4036"}))
		Expect(string(jsonBytes)).NotTo(ContainSubstring("stored=82.48.4000"))
		yamlBytes, err := yaml.Marshal(handoff)
		Expect(err).NotTo(HaveOccurred())
		var yamlSaved inventory.Inventory
		Expect(yaml.Unmarshal(yamlBytes, &yamlSaved)).To(Succeed())
		Expect(yamlSaved.Hosts[0].Runtime.Hardware.Cards[0].Firmware.Values).To(Equal([]string{"82.48.4036"}))
		Expect(string(yamlBytes)).NotTo(ContainSubstring("stored=82.48.4000"))
	})

	It("retains an unavailable MFT-version observation when devlink supplies card facts", func() {
		r := hostRunner()
		r.errors["mft-version"] = &Error{Code: "TOOL_NOT_AVAILABLE", Stage: "mft-version", Tool: "mlxfwmanager"}
		r.errors["firmware:0000:21:00.0"] = &Error{Code: "TOOL_NOT_AVAILABLE", Stage: "firmware:0000:21:00.0", Tool: "mlxfwmanager"}
		r.values["devlink-firmware"] = devlinkFixture("0000:21:00.0", "82.48.4036", "82.48.4000", "MT_0000000001")
		d, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Runtime.Observations["mft-version"].ReasonCode).To(Equal("TOOL_NOT_AVAILABLE"))
		Expect(d.Runtime.Observations["mft-version"].Tool).To(Equal("mlxfwmanager"))
		Expect(d.Runtime.Hardware.Cards[0].Firmware.State).To(Equal("observed"))
	})

	It("does not run devlink when MFT already observed every card", func() {
		r := hostRunner()
		r.values["mft-version"] = "mlxfwmanager, MFT 4.33.0-169"
		r.values["firmware:0000:21:00.0"] = "PCI Device Name: 0000:21:00.0\nPSID: MT_0000000001\nFW 82.48.4036 N/A\n"
		r.values["firmware:0000:41:00.0"] = "PCI Device Name: 0000:41:00.0\nPSID: MT_0000000002\nFW 32.1.2 N/A\n"
		d, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.calls).NotTo(ContainElement(ContainSubstring("devlink -j dev info")))
		Expect(d.Runtime.Hardware.Cards[0].Firmware.Values).To(Equal([]string{"82.48.4036"}))
	})

	It("keeps staged firmware separate without overwriting successful MFT facts", func() {
		r := hostRunner()
		r.values["mft-version"] = "mlxfwmanager, MFT 4.33.0-169"
		r.errors["firmware:0000:21:00.0"] = &Error{Code: "DEVICE_ACCESS_FAILED", Stage: "firmware:0000:21:00.0", Tool: "mlxfwmanager"}
		r.values["firmware:0000:41:00.0"] = "PCI Device Name: 0000:41:00.0\nPSID: MT_0000000002\nFW 32.1.2 N/A\n"
		r.values["devlink-firmware"] = `{"info":{"pci/0000:21:00.0":{"versions":{"running":{"fw.version":"82.48.4036"}}},"pci/0000:41:00.0":{"versions":{"running":{"fw.version":"32.1.2"},"stored":{"fw.version":"32.1.1"}}}}}`
		d, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Runtime.Hardware.Cards[1].Firmware.Values).To(Equal([]string{"32.1.2"}))
		Expect(d.Runtime.Hardware.Cards[1].Firmware.Tool).To(Equal("mlxfwmanager"))
		Expect(d.Runtime.Observations["devlink-firmware:0000:41:00.0:running"].Values).To(Equal([]string{"running=32.1.2"}))
		Expect(d.Runtime.Observations["devlink-firmware:0000:41:00.0:stored"].Values).To(Equal([]string{"stored=32.1.1"}))
	})

	It("retains MFT and devlink failures and does not assign a different BDF's data", func() {
		r := hostRunner()
		r.values["mft-version"] = "mlxfwmanager, MFT 4.33.0-169"
		r.errors["firmware:0000:21:00.0"] = &Error{Code: "TOOL_NOT_AVAILABLE", Stage: "firmware:0000:21:00.0", Tool: "mlxfwmanager"}
		r.values["devlink-firmware"] = devlinkFixture("0000:22:00.0", "82.48.4036", "82.48.4036", "MT_0000000001")
		d, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Runtime.Observations["firmware:0000:21:00.0"].ReasonCode).To(Equal("TOOL_NOT_AVAILABLE"))
		Expect(d.Runtime.Observations["devlink-firmware:0000:21:00.0"].ReasonCode).To(Equal("PROPERTY_NOT_EXPOSED"))
		Expect(d.Runtime.Hardware.Cards[0].SerialNumber).To(Equal(testSerial))
		Expect(d.Runtime.Hardware.Cards[0].Firmware.ReasonCode).To(Equal("TOOL_NOT_AVAILABLE"))
	})

	It("retains devlink tool and malformed-output failures without promoting stored firmware", func() {
		for _, test := range []struct {
			name      string
			setOutput bool
			output    string
			want      string
		}{
			{name: "tool unavailable", want: "TOOL_NOT_AVAILABLE"},
			{name: "malformed JSON", setOutput: true, output: `[]`, want: "SSH_RUNTIME_INVALID"},
			{name: "stored only", setOutput: true, output: `{"info":{"pci/0000:21:00.0":{"versions":{"stored":{"fw.version":"82.48.4036"}}}}}`, want: "PROPERTY_NOT_EXPOSED"},
		} {
			By(test.name)
			r := hostRunner()
			r.values["mft-version"] = "mlxfwmanager, MFT 4.33.0-169"
			r.errors["firmware:0000:21:00.0"] = &Error{Code: "DEVICE_ACCESS_FAILED", Stage: "firmware:0000:21:00.0", Tool: "mlxfwmanager"}
			if test.setOutput {
				r.values["devlink-firmware"] = test.output
			} else {
				r.errors["devlink-firmware"] = &Error{Code: test.want, Stage: "devlink-firmware", Tool: "devlink"}
			}
			d, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
			Expect(err).NotTo(HaveOccurred())
			if test.want == "PROPERTY_NOT_EXPOSED" {
				Expect(d.Runtime.Observations["devlink-firmware:0000:21:00.0"].State).To(Equal("observed"))
				Expect(d.Runtime.Hardware.Cards[0].Firmware.State).NotTo(Equal("observed"))
				Expect(d.Runtime.Hardware.Cards[0].Firmware.Values).To(BeEmpty())
				Expect(d.Runtime.Hardware.Cards[0].Firmware.ReasonCode).To(Equal("PROPERTY_NOT_EXPOSED"))
			} else {
				Expect(d.Runtime.Observations["devlink-firmware"].ReasonCode).To(Equal(test.want))
			}
			Expect(d.Runtime.Observations["mft-version"].State).To(Equal("observed"))
		}
	})

	It("blocks a component when independently observed MFT and devlink facts conflict", func() {
		r := hostRunner()
		r.values["mft-version"] = "mlxfwmanager, MFT 4.33.0-169"
		r.values["firmware:0000:21:00.0"] = "PCI Device Name: 0000:21:00.0\nPSID: MT_0000000001\nFW 82.48.4036 N/A\n"
		r.values["firmware:0000:41:00.0"] = "PCI Device Name: 0000:41:00.0\nPSID: MT_0000000002\nFW 32.1.2 N/A\n"
		r.errors["firmware:0000:21:00.0"] = &Error{Code: "DEVICE_ACCESS_FAILED", Stage: "firmware:0000:21:00.0", Tool: "mlxfwmanager"}
		r.values["devlink-firmware"] = `{"info":{"pci/0000:21:00.0":{"versions":{"fixed":{"fw.psid":"MT_0000000009"},"running":{"fw.version":"82.48.4036"}}},"pci/0000:41:00.0":{"versions":{"fixed":{"fw.psid":"MT_0000000002"},"running":{"fw.version":"32.1.3"}}}}}`
		d, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Runtime.Hardware.Cards[0].Firmware.Values).To(ConsistOf("82.48.4036"))
		Expect(d.Runtime.Hardware.Cards[0].PSID.Values).To(Equal([]string{"MT_0000000009"}))
		Expect(d.Runtime.Hardware.Cards[1].Firmware.State).To(Equal("unknown"))
		Expect(d.Runtime.Hardware.Cards[1].Firmware.ReasonCode).To(Equal("SOURCE_CONFLICT"))
		Expect(d.Runtime.Hardware.Cards[1].PSID.Values).To(Equal([]string{"MT_0000000002"}))
	})

	It("blocks only PSID when the fixed identifier conflicts", func() {
		r := hostRunner()
		r.values["mft-version"] = "mlxfwmanager, MFT 4.33.0-169"
		r.values["firmware:0000:41:00.0"] = "PCI Device Name: 0000:41:00.0\nPSID: MT_0000000002\nFW 32.1.2 N/A\n"
		r.errors["firmware:0000:21:00.0"] = &Error{Code: "DEVICE_ACCESS_FAILED", Stage: "firmware:0000:21:00.0", Tool: "mlxfwmanager"}
		r.values["devlink-firmware"] = `{"info":{"pci/0000:21:00.0":{"versions":{"running":{"fw.version":"82.48.4036"}}},"pci/0000:41:00.0":{"versions":{"fixed":{"fw.psid":"MT_0000000009"},"running":{"fw.version":"32.1.2"}}}}}`
		d, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Runtime.Hardware.Cards[1].Firmware.Values).To(Equal([]string{"32.1.2"}))
		Expect(d.Runtime.Hardware.Cards[1].PSID.State).To(Equal("unknown"))
		Expect(d.Runtime.Hardware.Cards[1].PSID.ReasonCode).To(Equal("SOURCE_CONFLICT"))
	})

	It("blocks an internally contradictory devlink running-version pair", func() {
		r := hostRunner()
		r.values["mft-version"] = "mlxfwmanager, MFT 4.33.0-169"
		r.values["firmware:0000:41:00.0"] = "PCI Device Name: 0000:41:00.0\nPSID: MT_0000000002\nFW 32.1.2 N/A\n"
		r.errors["firmware:0000:21:00.0"] = &Error{Code: "DEVICE_ACCESS_FAILED", Stage: "firmware:0000:21:00.0", Tool: "mlxfwmanager"}
		r.values["devlink-firmware"] = `{"info":{"pci/0000:21:00.0":{"versions":{"running":{"fw.version":"82.48.4036"}}},"pci/0000:41:00.0":{"versions":{"fixed":{"fw.psid":"MT_0000000002"},"running":{"fw.version":"32.1.2","fw":"32.1.3"}}}}}`
		d, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Runtime.Hardware.Cards[1].Firmware.State).To(Equal("unknown"))
		Expect(d.Runtime.Hardware.Cards[1].Firmware.ReasonCode).To(Equal("SOURCE_CONFLICT"))
		Expect(d.Runtime.Hardware.Cards[1].PSID.Values).To(Equal([]string{"MT_0000000002"}))
	})

	It("does not probe host PCI firmware from the ARM vantage", func() {
		r := armRunner()
		r.values["mft-version"] = "MFT 4.33.0-169"
		r.values["arm-pci"] = strings.ReplaceAll(testPCI, "0000:", "0002:")
		_, _, err := collect(context.Background(), config.Target{Role: "dpus", Endpoint: "ssh://192.0.2.41:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.calls).NotTo(ContainElement(ContainSubstring("devlink -j dev info")))
	})

	It("never interpolates an invalid adapter BDF into a remote command", func() {
		r := hostRunner()
		d := inventory.Device{Adapters: []inventory.Adapter{{PCIAddress: "0000:21:00.0;touch /tmp/nope", Model: "BlueField"}}, Runtime: &inventory.RuntimeObservation{Observations: map[string]inventory.Observation{}}}
		collectHardware(context.Background(), r, &d, "", "hosts", func(string, string) string { return "evidence" })
		Expect(d.Runtime.Hardware.Cards).To(BeEmpty())
		Expect(strings.Join(r.calls, "\n")).NotTo(ContainSubstring("touch /tmp/nope"))
		Expect(strings.Join(r.calls, "\n")).NotTo(ContainSubstring("devlink -j dev info"))
	})
})

func devlinkFixture(address, running, stored, psid string) string {
	return `{"info":{"pci/` + address + `":{"versions":{"fixed":{"fw.psid":"` + psid + `"},"running":{"fw.version":"` + running + `","fw":"` + running + `"},"stored":{"fw.version":"` + stored + `","fw":"` + stored + `"}}}}}`
}
