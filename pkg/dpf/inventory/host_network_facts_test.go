// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func hostNetworkFactsFixture() DeviceOutput {
	return DeviceOutput{RecordKey: "host-a", InputRef: "/discovery/hosts/0", Method: "ssh", SSHEndpoint: "ssh://host.example:22", Runtime: &RuntimeOutput{VirtualMachine: new(bool), BootID: "12345678-1234-4234-8234-123456789abc", SSHServerAddress: "192.0.2.10", Network: &NetworkObservation{Links: []NetworkLink{{Name: "mgmt0", PhysicalPFRoots: []string{"0000:35:00.0"}}}}}, Adapters: []AdapterOutput{{Key: "card-a", PCIAddress: "0000:21:00.0", PCIFunctions: []PCIFunction{{Address: "0000:21:00.0", NetdevsAbsent: true}}}}}
}

func TestHostNetworkFactsRoundTripAndMutationIsolation(t *testing.T) {
	original := hostNetworkFactsFixture()
	decoded, err := DeviceFromOutput(original, "hosts")
	if err != nil {
		t.Fatal(err)
	}
	published, err := DeviceToOutput(decoded, "hosts")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original.Runtime, published.Runtime) || !reflect.DeepEqual(original.Adapters, published.Adapters) {
		t.Fatalf("roundtrip changed typed facts: %+v", published)
	}
	original.Runtime.Network.Links[0].PhysicalPFRoots[0] = "0000:ff:00.0"
	published.Runtime.Network.Links[0].PhysicalPFRoots[0] = "0000:ee:00.0"
	*published.Runtime.VirtualMachine = true
	if *decoded.Runtime.VirtualMachine {
		t.Fatal("VM classification shares mutable storage")
	}
	if decoded.Runtime.Network.Links[0].PhysicalPFRoots[0] != "0000:35:00.0" {
		t.Fatal("physical roots share mutable backing storage")
	}
	if decoded.Runtime.BootID == "" || decoded.Runtime.SSHServerAddress == "" || !decoded.Adapters[0].PCIFunctions[0].NetdevsAbsent {
		t.Fatal("typed positive facts lost")
	}
	for _, obs := range decoded.Runtime.Observations {
		if len(obs.Values) > 0 {
			t.Fatal("codec reconstructed generic successful values")
		}
	}
}

func TestHostNetworkFactsRejectConflictsAndInvalidSources(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*DeviceOutput)
	}{
		{"VM source issue", func(w *DeviceOutput) {
			w.Issues = []DeviceIssue{{Field: "runtime.host-virtual-machine", Code: "SSH_COMMAND_FAILED"}}
		}},
		{"VM canonical issue", func(w *DeviceOutput) {
			w.Issues = []DeviceIssue{{Field: "runtime.virtualMachine", Code: "SSH_COMMAND_FAILED"}}
		}},
		{"boot canonical issue", func(w *DeviceOutput) { w.Issues = []DeviceIssue{{Field: "runtime.bootID", Code: "SSH_COMMAND_FAILED"}} }},
		{"SSH canonical issue", func(w *DeviceOutput) {
			w.Issues = []DeviceIssue{{Field: "runtime.sshServerAddress", Code: "SSH_COMMAND_FAILED"}}
		}},
		{"bad boot", func(w *DeviceOutput) { w.Runtime.BootID = "bad" }},
		{"boot issue", func(w *DeviceOutput) {
			w.Issues = []DeviceIssue{{Field: "runtime.host-boot-id", Code: "SSH_COMMAND_FAILED"}}
		}},
		{"bad SSH address", func(w *DeviceOutput) { w.Runtime.SSHServerAddress = "not-an-address" }},
		{"SSH issue", func(w *DeviceOutput) {
			w.Issues = []DeviceIssue{{Field: "runtime.ssh-server-address", Code: "SSH_COMMAND_FAILED"}}
		}},
		{"roots issue", func(w *DeviceOutput) {
			w.Issues = []DeviceIssue{{Field: "runtime.host-management-roots", Code: "SSH_COMMAND_FAILED"}}
		}},
		{"bad root", func(w *DeviceOutput) { w.Runtime.Network.Links[0].PhysicalPFRoots = []string{"bad"} }},
		{"duplicate root", func(w *DeviceOutput) {
			w.Runtime.Network.Links[0].PhysicalPFRoots = []string{"0000:ab:00.0", "0000:AB:00.0"}
		}},
		{"root bound", func(w *DeviceOutput) {
			w.Runtime.Network.Links[0].PhysicalPFRoots = make([]string, MaxHostManagementRoots+1)
		}},
		{"nonempty absent PF", func(w *DeviceOutput) { w.Adapters[0].PCIFunctions[0].Netdevs = []string{"pf0"} }},
		{"absent PF read issue", func(w *DeviceOutput) {
			w.Issues = []DeviceIssue{{Field: "runtime.host-pf:0000:21:00.0:netdevs", Code: "SSH_COMMAND_FAILED"}}
		}},
		{"foreign PCI slot", func(w *DeviceOutput) { w.Adapters[0].PCIFunctions[0].Address = "0000:22:00.0" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := hostNetworkFactsFixture()
			test.edit(&w)
			before, _ := json.Marshal(w)
			if _, err := DeviceFromOutput(w, "hosts"); err == nil {
				t.Fatal("invalid typed facts accepted")
			}
			after, _ := json.Marshal(w)
			if string(before) != string(after) {
				t.Fatal("validation mutated input")
			}
		})
	}
	w := hostNetworkFactsFixture()
	w.Method = "redfish"
	w.SSHEndpoint = ""
	w.BMCEndpoint = "https://bmc.example"
	if _, err := DeviceFromOutput(w, "dpus"); err == nil {
		t.Fatal("host-only facts accepted for DPU")
	}
}

func TestLegacyHostFactsRemainAbsentAndByteStable(t *testing.T) {
	const legacy = `{"hostname":"worker-a","interfaces":[{"name":"mgmt0","index":2,"mtu":1500,"addresses":["192.0.2.10/24"]}],"routes":[{"destination":"0.0.0.0/0","interfaceIndex":2,"table":254}]}`
	var runtime RuntimeOutput
	if err := json.Unmarshal([]byte(legacy), &runtime); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(runtime)
	if err != nil || string(encoded) != legacy {
		t.Fatalf("legacy runtime changed: %s %v", encoded, err)
	}
	w := DeviceOutput{RecordKey: "host-a", Method: "ssh", SSHEndpoint: "ssh://host.example:22", Runtime: &runtime, Adapters: []AdapterOutput{{Key: "card-a", PCIFunctions: []PCIFunction{{Address: "0000:21:00.0"}}}}}
	decoded, err := DeviceFromOutput(w, "hosts")
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Runtime.BootID != "" || decoded.Runtime.SSHServerAddress != "" || decoded.Adapters[0].PCIFunctions[0].NetdevsAbsent {
		t.Fatal("old missing facts became positive proof")
	}
	encoded, _ = json.Marshal(w)
	for _, newField := range []string{"virtualMachine", "bootID", "sshServerAddress", "physicalPFRoots", "netdevsAbsent"} {
		if strings.Contains(string(encoded), newField) {
			t.Fatalf("omitted field %s changed legacy encoding", newField)
		}
	}
}
