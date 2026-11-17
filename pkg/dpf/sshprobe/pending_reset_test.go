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

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func TestPendingResetNetdevAbsenceRequiresIndependentPositiveRead(t *testing.T) {
	const stage = "host-pf:0000:21:00.0:netdevs"
	for _, test := range []struct {
		name, result                      string
		failed, absenceFailed, wantAbsent bool
	}{
		{"normal", "pf0", false, false, false}, {"empty directory", "", false, false, true},
		{"measured absent directory", "absent", true, false, true},
		{"SSH error is not absence", "", true, true, false}, {"unrecognized read is not absence", "other", true, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := &fakeRunner{values: map[string]string{"host-pf:0000:21:00.0:driver": "../../drivers/mlx5_core", "host-pf:0000:21:00.0:numa": "0"}, errors: map[string]error{}}
			if test.failed {
				r.errors[stage] = &Error{Code: "SSH_COMMAND_FAILED"}
				r.values[stage+":absence"] = test.result
			} else {
				r.values[stage] = test.result
			}
			if test.absenceFailed {
				r.errors[stage+":absence"] = &Error{Code: "SSH_COMMAND_FAILED"}
			}
			runtime := &inventory.RuntimeObservation{Observations: map[string]inventory.Observation{}}
			pf := collectPF(context.Background(), r, "0000:21:00.0", runtime, func(string, string) string { return "evidence" })
			obs := runtime.Observations[stage]
			absent := pf.NetdevsAbsent
			if absent && (obs.State != "observed" || obs.ReasonCode != "HOST_PF_NETDEV_ABSENT" || len(pf.Netdevs) != 0) {
				t.Fatal("absence fact lacks successful source read")
			}
			if absent != test.wantAbsent {
				t.Fatalf("pf=%+v observation=%+v", pf, obs)
			}
			if test.failed && !test.wantAbsent && obs.State == "observed" {
				t.Fatal("failed command became a positive observation")
			}
		})
	}
}

func TestPendingResetSysfsReaderDistinguishesDirectoryShapes(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable for independent sysfs-reader test")
	}
	for _, name := range []string{"missing net", "empty net", "populated net", "missing PCI", "net is file", "denied net"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			pci := filepath.Join(root, "pci")
			if name != "missing PCI" {
				if err := os.Mkdir(pci, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if name == "empty net" || name == "populated net" || name == "denied net" {
				if err := os.Mkdir(filepath.Join(pci, "net"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if name == "populated net" {
				if err := os.WriteFile(filepath.Join(pci, "net", "pf0"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if name == "net is file" {
				if err := os.WriteFile(filepath.Join(pci, "net"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if name == "denied net" {
				if os.Geteuid() == 0 {
					t.Skip("root can bypass discretionary permissions")
				}
				if err := os.Chmod(filepath.Join(pci, "net"), 0); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(filepath.Join(pci, "net"), 0700)
			}
			output, err := exec.Command("sh", "-c", pfNetdevAbsenceCommand(pci)).CombinedOutput()
			t.Logf("sysfs reader output: %s", output)
			want := name == "missing net" || name == "empty net"
			if (err == nil && strings.TrimSpace(string(output)) == "absent") != want {
				t.Fatalf("absence=%q error=%v", output, err)
			}
		})
	}
}

func TestPendingResetHostBootObservationIsRechecked(t *testing.T) {
	for _, changed := range []bool{false, true} {
		r := hostRunner()
		r.values["host-boot-id"] = testUUID
		r.values["host-boot-id-recheck"] = testUUID
		if changed {
			r.values["host-boot-id-recheck"] = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
		}
		device, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.1:22"}, r)
		if changed {
			if Code(err) != "INVENTORY_CHANGED" {
				t.Fatalf("changed boot: %v", err)
			}
		} else if err != nil || device.Runtime.BootID != testUUID {
			t.Fatalf("boot observation: %+v %v", device.Runtime.Observations["host-boot-id"], err)
		}
	}
}
