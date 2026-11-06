// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package sshprobe

import (
	"context"
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("hardware observations", func() {
	It("queries each host card independently and retains specific missing-tool reasons", func() {
		r := hostRunner()
		r.values["firmware:0000:21:00.0"] = "PCI Device Name: 0000:21:00.0\nPSID: MT_000001\nVersions: Current Available\nFW 32.1.2 N/A\n"
		r.errors["firmware:0000:41:00.0"] = &Error{Code: "TOOL_NOT_AVAILABLE", Stage: "firmware:0000:41:00.0", Tool: "mlxfwmanager"}
		r.values["nvconfig:0000:21:00.0:NUM_OF_VFS"] = "Configurations: Next Boot\nNUM_OF_VFS 0\n"
		d, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Runtime.Hardware.Cards).To(HaveLen(2))
		a, b := d.Runtime.Hardware.Cards[0], d.Runtime.Hardware.Cards[1]
		Expect(a.Firmware.Values).To(Equal([]string{"32.1.2"}))
		Expect(a.PSID.Values).To(Equal([]string{"MT_000001"}))
		Expect(a.SerialNumber).To(Equal(testSerial))
		Expect(a.NVConfig["NUM_OF_VFS"].Values).To(Equal([]string{"next-boot=0"}))
		Expect(a.Privilege.ReasonCode).To(Equal("NOT_APPLICABLE"))
		Expect(b.Firmware.State).To(Equal("unavailable"))
		Expect(b.Firmware.ReasonCode).To(Equal("TOOL_NOT_AVAILABLE"))
		Expect(b.Firmware.Tool).To(Equal("mlxfwmanager"))
		Expect(b.Firmware.Message).To(ContainSubstring("not available"))
		Expect(strings.Join(r.calls, "\n")).NotTo(ContainSubstring("mlxprivhost"))
	})
	It("collects ARM-local privileges without treating every ARM-visible NIC as the DPU", func() {
		r := armRunner()
		r.values["mft-version"] = "mlxfwmanager, mft 4.33.0-169, built on a date"
		r.values["arm-pci"] = strings.ReplaceAll(testPCI, "0000:", "0002:")
		r.values["privilege:0002:21:00.0"] = "host index : 0\nlevel : RESTRICTED\ndisable_rshim : TRUE\ndisable_tracer : TRUE\ndisable_port_owner : TRUE\ndisable_counter_rd : TRUE\n"
		d, _, err := collect(context.Background(), config.Target{Role: "dpus", Endpoint: "ssh://192.0.2.41:22", SSH: &config.SSHAccess{Sudo: true}}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Runtime.Hardware.Cards).To(HaveLen(2))
		Expect(d.Runtime.Observations["mft-version"].Values).To(Equal([]string{"4.33.0-169"}))
		Expect(d.Runtime.Observations["board-product"].Values).To(Equal([]string{"B4240"}))
		Expect(d.Runtime.Hardware.Cards[0].Domain).To(Equal("arm-pci"))
		Expect(d.Runtime.Hardware.Cards[0].SerialNumber).To(BeEmpty())
		Expect(d.Runtime.Hardware.Cards[0].Privilege.Values).To(ContainElement("host[0].level=RESTRICTED"))
		Expect(d.Runtime.Hardware.Cards[1].Privilege.ReasonCode).To(Equal("NOT_APPLICABLE"))
		Expect(r.calls).To(ContainElement("privilege:0002:21:00.0 sudo -n mlxprivhost -d 0002:21:00.0 q --full"))
	})
	It("rejects firmware from a different PCI function and ambiguous tool output", func() {
		for _, text := range []string{"PCI Device Name: 0000:22:00.0\nFW 32.1.2 N/A", "FW 32.1.2 N/A\nFW 32.1.3 N/A", "unrecognized text"} {
			_, _, err := parseFirmware(text, "0000:21:00.0")
			Expect(err).To(HaveOccurred())
		}
		for _, text := range []string{"Configurations: Next Boot\nNUM_OF_VFS 2\nNUM_OF_VFS 3", "Configurations: Current Next Boot\nNUM_OF_VFS 2 3", "NUM_OF_VFS 2"} {
			_, err := parseNVConfig(text, "NUM_OF_VFS")
			Expect(err).To(HaveOccurred())
		}
		_, err := parsePrivilege("level : RESTRICTED\n")
		Expect(err).To(HaveOccurred())
	})
	It("retains required-tool failures in the partial identity result", func() {
		r := armRunner()
		r.errors["arm-fru"] = &Error{Code: "TOOL_NOT_AVAILABLE", Stage: "arm-fru", Tool: "ipmitool"}
		d, _, err := collect(context.Background(), config.Target{Role: "dpus", Endpoint: "ssh://192.0.2.41:22"}, r)
		Expect(err).To(HaveOccurred())
		Expect(d.Runtime.Observations["arm-fru"].Message).To(ContainSubstring("not available"))
		Expect(d.Runtime.Observations["arm-fru"].Tool).To(Equal("ipmitool"))
	})
	It("classifies diagnostics without persisting remote text", func() {
		for text, code := range map[string]string{"sudo: mlxfwmanager: command not found": "TOOL_NOT_AVAILABLE", "sudo: a password is required": "PERMISSION_DENIED", "device operation not supported": "OPERATION_UNSUPPORTED", "opaque secret-value": "SSH_COMMAND_FAILED"} {
			err := commandFailure(fmt.Errorf("secret-value"), "firmware", "sudo -n mlxfwmanager --query", text)
			Expect(err.Code).To(Equal(code))
			Expect(err.Tool).To(Equal("mlxfwmanager"))
			Expect(Reason(err)).NotTo(ContainSubstring("secret-value"))
		}
	})
})
