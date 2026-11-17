// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func TestManagementRootReaderMeasuresPhysicalVFAndVirtualParents(t *testing.T) {
	for _, name := range []string{"physical", "VF", "VLAN", "bridge", "unrelated virtual", "missing lower", "lower cycle", "physfn cycle", "missing device", "nested virtio device", "non-PCI subsystem", "non-network PCI class"} {
		t.Run(name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			netdir := filepath.Join(root, "net")
			devices := filepath.Join(root, "devices")
			mkdir := func(path string) {
				t.Helper()
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			link := func(target, path string) {
				t.Helper()
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			mkdir(netdir)
			mkdir(devices)
			pciBus := filepath.Join(root, "bus", "pci")
			mkdir(pciBus)
			physicalNIC := func(dir string) {
				mkdir(dir)
				link(pciBus, filepath.Join(dir, "subsystem"))
				if err := os.WriteFile(filepath.Join(dir, "class"), []byte("0x020000\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			physical := filepath.Join(devices, "0000:35:00.0")
			physicalNIC(physical)
			iface := filepath.Join(netdir, "mgmt0")
			mkdir(iface)
			link(physical, filepath.Join(iface, "device"))
			wanted := "mgmt0"
			expectedRoot := "0000:35:00.0"
			pass := true
			switch name {
			case "VF", "physfn cycle":
				vf := filepath.Join(devices, "0000:43:00.0")
				physicalNIC(vf)
				if err := os.Remove(filepath.Join(iface, "device")); err != nil {
					t.Fatal(err)
				}
				link(vf, filepath.Join(iface, "device"))
				parent := filepath.Join(devices, "0000:21:00.0")
				physicalNIC(parent)
				link(parent, filepath.Join(vf, "physfn"))
				expectedRoot = "0000:21:00.0"
				if name == "physfn cycle" {
					link(vf, filepath.Join(parent, "physfn"))
					pass = false
				}
			case "VLAN", "bridge":
				wanted = "upper0"
				upper := filepath.Join(netdir, wanted)
				mkdir(upper)
				link(iface, filepath.Join(upper, "lower_mgmt0"))
			case "unrelated virtual":
				other := filepath.Join(netdir, "docker0")
				mkdir(other)
				link(other, filepath.Join(other, "lower_cycle"))
			case "missing lower":
				wanted = "tunnel0"
				mkdir(filepath.Join(netdir, wanted))
				pass = false
			case "lower cycle":
				wanted = "upper0"
				a := filepath.Join(netdir, wanted)
				b := filepath.Join(netdir, "other0")
				mkdir(a)
				mkdir(b)
				link(b, filepath.Join(a, "lower_other0"))
				link(a, filepath.Join(b, "lower_upper0"))
				pass = false
			case "nested virtio device":
				virtual := filepath.Join(physical, "virtio0")
				mkdir(virtual)
				if err := os.Remove(filepath.Join(iface, "device")); err != nil {
					t.Fatal(err)
				}
				link(virtual, filepath.Join(iface, "device"))
				pass = false
			case "non-PCI subsystem":
				if err := os.Remove(filepath.Join(physical, "subsystem")); err != nil {
					t.Fatal(err)
				}
				other := filepath.Join(root, "bus", "virtual")
				mkdir(other)
				link(other, filepath.Join(physical, "subsystem"))
				pass = false
			case "non-network PCI class":
				if err := os.WriteFile(filepath.Join(physical, "class"), []byte("0x010802\n"), 0600); err != nil {
					t.Fatal(err)
				}
				pass = false
			case "missing device":
				if err := os.RemoveAll(physical); err != nil {
					t.Fatal(err)
				}
				pass = false
			}
			program := strings.ReplaceAll(strings.ReplaceAll(managementRootReader, "/sys/class/net", netdir), "/sys/bus/pci", pciBus)
			output, err := exec.Command("bash", "-c", program, "--", "64", wanted).CombinedOutput()
			t.Logf("root reader output: %s", output)
			if (err == nil) != pass {
				t.Fatalf("reader err=%v output=%s", err, output)
			}
			if pass {
				roots, err := parseManagementRoots(string(output), []string{wanted})
				if err != nil || !reflect.DeepEqual(roots[wanted], []string{expectedRoot}) {
					t.Fatalf("roots=%+v err=%v", roots, err)
				}
				if name == "VF" {
					host := managementRootHost()
					host.Runtime.Network.Links[0].PhysicalPFRoots = roots[wanted]
					adapter := inventory.Adapter{PCIAddress: "0000:21:00.0", PCIFunctions: []inventory.PCIFunction{{Address: "0000:21:00.0"}}}
					if err := VerifyHostManagementPhysicalSeparation(host, adapter); err == nil {
						t.Fatal("measured same-card VF parent accepted")
					}
				}
			}
		})
	}
}

func managementRootHost() inventory.Device {
	return inventory.Device{Runtime: &inventory.RuntimeObservation{VirtualMachine: new(bool), SSHServerAddress: "192.0.2.10", Network: &inventory.NetworkObservation{Links: []inventory.NetworkLink{{Name: "mgmt0", PhysicalPFRoots: []string{"0000:35:00.0"}}}}, Interfaces: []inventory.Interface{{Name: "mgmt0", Index: 2, Addresses: []string{"192.0.2.10/24"}}}, Routes: []inventory.Route{{Destination: "0.0.0.0/0", InterfaceIndex: 2}, {Destination: "::/0", InterfaceIndex: 2}}, Observations: map[string]inventory.Observation{"interfaces": {State: "observed"}, "routes": {State: "observed"}, "host-management-roots": {State: "observed"}}}}
}

func TestManagementRootPolicyRejectsSameCardAndUnmeasuredPaths(t *testing.T) {
	adapter := inventory.Adapter{PCIAddress: "0000:21:00.0", PCIFunctions: []inventory.PCIFunction{{Address: "0000:21:00.0"}, {Address: "0000:21:00.1"}}}
	for _, test := range []struct {
		name, root string
		erase      bool
		pass       bool
	}{{"separate card", "0000:35:00.0", false, true}, {"selected PF", "0000:21:00.0", false, false}, {"selected other function", "0000:21:00.7", false, false}, {"VF resolved parent", "0000:21:00.1", false, false}, {"unknown roots", "", true, false}, {"invalid root", "foreign", false, false}} {
		t.Run(test.name, func(t *testing.T) {
			host := managementRootHost()
			host.Runtime.Network.Links[0].PhysicalPFRoots = []string{test.root}
			if test.erase {
				host.Runtime.Network.Links[0].PhysicalPFRoots = nil
			}
			if err := VerifyHostManagementPhysicalSeparation(host, adapter); (err == nil) != test.pass {
				t.Fatalf("physical separation=%v", err)
			}
		})
	}
}

func TestManagementRootCollectionDoesNotManufacturePartialOrFailedRoots(t *testing.T) {
	for _, test := range []struct {
		name, output string
		failure      bool
		pass         bool
	}{{"complete", "mgmt0 0000:35:00.0", false, true}, {"missing output", "", false, false}, {"foreign interface", "other0 0000:35:00.0", false, false}, {"partial failed read", "mgmt0 0000:35:00.0", true, false}} {
		t.Run(test.name, func(t *testing.T) {
			host := managementRootHost()
			delete(host.Runtime.Observations, "host-management-roots")
			host.Runtime.Network.Links[0].PhysicalPFRoots = nil
			r := &fakeRunner{values: map[string]string{"host-virtual-machine": "none", "host-ssh-connection": "198.51.100.1 12345 192.0.2.10 22", "host-management-roots": test.output}, errors: map[string]error{}}
			if test.failure {
				r.errors["host-management-roots"] = &Error{Code: "PERMISSION_DENIED", Tool: "find"}
			}
			collectManagementRoots(context.Background(), r, host.Runtime, func(string, string) string { return "evidence" })
			if (host.Runtime.Observations["host-management-roots"].State == "observed") != test.pass {
				t.Fatalf("observations=%+v", host.Runtime.Observations)
			}
			if test.failure && host.Runtime.Observations["host-management-roots"].ReasonCode != "PERMISSION_DENIED" {
				t.Fatal("safe transport failure classification lost")
			}
		})
	}
}

func TestManagementRootSelectorUsesParsedNonforwardingRouteTypes(t *testing.T) {
	for _, test := range []struct {
		name, input string
		pass        bool
	}{
		{"throw policy default", `[{"dst":"default","type":"throw","table":100}]`, true},
		{"unreachable policy default", `[{"dst":"default","type":"unreachable","table":100}]`, true},
		{"blackhole policy default", `[{"dst":"default","type":"blackhole","table":100}]`, true},
		{"prohibit policy default", `[{"dst":"default","type":"prohibit","table":100}]`, true},
		{"forwarding without interface", `[{"dst":"default","table":100}]`, false},
		{"forwarding unknown interface", `[{"dst":"default","dev":"missing0","table":100}]`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := managementRootHost()
			parsed, err := routes(test.input, host.Runtime.Interfaces, 4)
			if !test.pass {
				if err == nil {
					t.Fatal("unresolved forwarding route accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			host.Runtime.Routes = append(host.Runtime.Routes, parsed...)
			names, err := HostManagementInterfaces(host.Runtime)
			if err != nil || !reflect.DeepEqual(names, []string{"mgmt0"}) {
				t.Fatalf("names=%v err=%v", names, err)
			}
		})
	}
	host := managementRootHost()
	host.Runtime.Routes = append(host.Runtime.Routes, inventory.Route{Destination: "0.0.0.0/0"})
	if _, err := HostManagementInterfaces(host.Runtime); err == nil {
		t.Fatal("untyped zero-index default accepted")
	}
}

func TestManagementRootBudgetAndUnrelatedVirtualInterfaces(t *testing.T) {
	host := managementRootHost()
	host.Runtime.Interfaces = append(host.Runtime.Interfaces, inventory.Interface{Name: "docker0", Index: 3, Addresses: []string{"172.17.0.1/16"}})
	host.Runtime.Network.Links = append(host.Runtime.Network.Links, inventory.NetworkLink{Name: "docker0", Kind: "bridge", Master: "unobserved-unrelated"})
	adapter := inventory.Adapter{PCIAddress: "0000:21:00.0", PCIFunctions: []inventory.PCIFunction{{Address: "0000:21:00.0"}}}
	if err := VerifyHostManagementPhysicalSeparation(host, adapter); err != nil {
		t.Fatal(err)
	}
	rows := []string{}
	for i := 0; i < maxManagementRoots; i++ {
		rows = append(rows, fmt.Sprintf("mgmt0 0000:%02x:00.0", i))
	}
	if _, err := parseManagementRoots(strings.Join(rows, "\n"), []string{"mgmt0"}); err != nil {
		t.Fatal(err)
	}
	rows = append(rows, "mgmt0 0000:ff:00.0")
	if _, err := parseManagementRoots(strings.Join(rows, "\n"), []string{"mgmt0"}); err == nil {
		t.Fatal("excessive root rows accepted")
	}
	if _, err := parseManagementRoots("mgmt0 0000:ab:00.0\nmgmt0 0000:AB:00.0", []string{"mgmt0"}); err == nil {
		t.Fatal("canonical duplicate root accepted")
	}
}

func TestManagementRootProofRequiresMeasuredRouteFacts(t *testing.T) {
	adapter := inventory.Adapter{PCIAddress: "0000:21:00.0", PCIFunctions: []inventory.PCIFunction{{Address: "0000:21:00.0"}}}
	for _, test := range []struct {
		name   string
		routes []inventory.Route
	}{
		{"omitted without issue", nil}, {"empty without issue", []inventory.Route{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := managementRootHost()
			host.Runtime.Routes = test.routes
			delete(host.Runtime.Observations, "routes")
			if err := VerifyHostManagementPhysicalSeparation(host, adapter); err == nil {
				t.Fatal("unmeasured routes became physical separation proof")
			}
		})
	}
	host := managementRootHost()
	parsed, err := routes(`[{"dst":"192.0.2.0/24","dev":"mgmt0","protocol":"kernel","table":254}]`, host.Runtime.Interfaces, 4)
	if err != nil || len(parsed) == 0 {
		t.Fatalf("measured connected routes=%+v err=%v", parsed, err)
	}
	host.Runtime.Routes = parsed
	if err := VerifyHostManagementPhysicalSeparation(host, adapter); err != nil {
		t.Fatalf("measured complete current routes rejected: %v", err)
	}
}
