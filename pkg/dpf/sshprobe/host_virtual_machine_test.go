// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func TestHostVMReaderNormalizesOnlyExactSupportedStatuses(t *testing.T) {
	for _, test := range []struct {
		name, body, want string
		pass             bool
	}{
		{"bare metal", "printf 'none\\n'; exit 1", "none", true},
		{"VMware PCI virtual root", "printf 'vmware\\n'; exit 0", "vm", true},
		{"KVM", "printf 'kvm\\n'; exit 0", "vm", true},
		{"tool exit 127", "exit 127", "", false},
		{"missing executable", "", "", false},
		{"none wrong status", "printf 'none\\n'; exit 0", "", false},
		{"none error status", "printf 'none\\n'; exit 2", "", false},
		{"stderr-only none", "printf 'none\\n' >&2; exit 1", "", false},
		{"missing stdout newline", "printf none; exit 1", "", false},
		{"none with blank stderr", "printf 'none\\n'; printf '\\n' >&2; exit 1", "", false},
		{"none with stderr", "printf 'none\\n'; printf 'warning\\n' >&2; exit 1", "", false},
		{"unknown", "printf 'unknown result\\n'; exit 1", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "systemd-detect-virt")
			if err := os.WriteFile(file, []byte("#!/bin/sh\n"+test.body+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", "-c", hostVirtualMachineCommand())
			command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
			if test.body == "" {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				command = exec.Command("bash", "-c", hostVirtualMachineReader)
				command.Env = append(os.Environ(), "PATH="+dir)
			}
			out, err := command.CombinedOutput()
			t.Logf("status=%v output=%q", err, out)
			if (err == nil) != test.pass || test.pass && string(out) != test.want+"\n" {
				t.Fatalf("status=%v output=%q", err, out)
			}
		})
	}
}

func TestHostVMClassificationRequiresMeasuredNoVMForPhysicalRoots(t *testing.T) {
	adapter := inventory.Adapter{PCIAddress: "0000:21:00.0", PCIFunctions: []inventory.PCIFunction{{Address: "0000:21:00.0"}}}
	for _, test := range []struct {
		name, result string
		failure      bool
		pass         bool
	}{
		{"bare metal", "none", false, true}, {"PCI virtual root in VM", "vm", false, false}, {"unknown", "unknown", false, false}, {"tool failure", "none", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := managementRootHost()
			host.Runtime.Network.Links[0].PCIAddress = "0000:35:00.0"
			remote := &fakeRunner{values: map[string]string{"host-virtual-machine": test.result}, errors: map[string]error{}}
			if test.failure {
				remote.errors["host-virtual-machine"] = &Error{Code: "TOOL_UNAVAILABLE", Tool: "systemd-detect-virt"}
			}
			collectHostVirtualMachine(context.Background(), remote, host.Runtime, func(string, string) string { return "evidence" })
			if err := VerifyHostManagementPhysicalSeparation(host, adapter); (err == nil) != test.pass {
				t.Fatalf("virtual classification=%v physical proof=%v", host.Runtime.VirtualMachine, err)
			}
			if test.result == "vm" {
				if err := VerifyHostManagementPhysicalSeparation(host, adapter); Code(err) != "HOST_VIRTUAL_MACHINE_UNSUPPORTED" {
					t.Fatalf("VM support reason=%v", err)
				}
			}
			if test.failure && !strings.Contains(host.Runtime.Observations["host-virtual-machine"].ReasonCode, "TOOL_UNAVAILABLE") {
				t.Fatal("safe transport diagnostic lost")
			}
		})
	}
	host := managementRootHost()
	host.Runtime.VirtualMachine = nil
	if err := VerifyHostManagementPhysicalSeparation(host, adapter); err == nil {
		t.Fatal("missing VM classification accepted")
	}
}
