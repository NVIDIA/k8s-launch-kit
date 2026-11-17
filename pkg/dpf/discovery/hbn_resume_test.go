// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

func TestResumeHostPathAllowsOnlyInspectedExactPhysicalAddress(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*inventory.Inventory)
		want string
	}{
		{"original address", func(*inventory.Inventory) {}, "passed"},
		{"extra address", func(i *inventory.Inventory) {
			i.Hosts[0].Runtime.Interfaces[0].Addresses = append(i.Hosts[0].Runtime.Interfaces[0].Addresses, "198.51.100.3/29")
		}, "failed"},
		{"other PF address", func(i *inventory.Inventory) { i.Hosts[0].Runtime.Interfaces[1].Addresses = []string{"198.51.100.2/29"} }, "failed"},
		{"IPv4 default", func(i *inventory.Inventory) {
			i.Hosts[0].Runtime.Routes = []inventory.Route{{Destination: "0.0.0.0/0", InterfaceIndex: 10}}
		}, "failed"},
		{"IPv6 default", func(i *inventory.Inventory) {
			i.Hosts[0].Runtime.Routes = []inventory.Route{{Destination: "::/0", InterfaceIndex: 10}}
		}, "failed"},
		{"unknown route coverage", func(i *inventory.Inventory) { i.Hosts[0].Runtime.Routes = nil }, "unknown"},
		{"unknown links", func(i *inventory.Inventory) { i.Hosts[0].Runtime.Network.Links = nil }, "unknown"},
		{"ancestor address", func(i *inventory.Inventory) {
			setHostMaster(i.Hosts[0].Runtime, "hostpf0", "bond0", "bond", 20)
			i.Hosts[0].Runtime.Interfaces[2].Addresses = []string{"198.51.100.4/29"}
		}, "unknown"},
		{"ancestor default", func(i *inventory.Inventory) {
			setHostMaster(i.Hosts[0].Runtime, "hostpf0", "bond0", "bond", 20)
			i.Hosts[0].Runtime.Routes = []inventory.Route{{Destination: "::/0", InterfaceIndex: 20}}
		}, "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			inv, _ := hostBackendDiscoveryFixture(t, 9000)
			identity, err := sshprobe.SelectHostWorkloadNetworkIdentity(inv.Hosts[0], inv.Hosts[0].Adapters[0].Key, inv.DPUs[0].SerialNumber)
			if err != nil {
				t.Fatal(err)
			}
			inv.Hosts[0].Runtime.Interfaces[0].Addresses = []string{"198.51.100.2/29"}
			test.edit(&inv)
			before := rawHostHBNTestInventory(t, inv)
			check := ResumeHostManagementPathCheck(&inv, inv.DPUs[0].SerialNumber, identity, "198.51.100.2/29")
			if check.State != test.want {
				t.Fatalf("check=%+v want %s", check, test.want)
			}
			if !bytes.Equal(before, rawHostHBNTestInventory(t, inv)) {
				t.Fatal("resume assessment mutated observations")
			}
			if check.State == "passed" && managementPathCheck(inv.Hosts[0], []string{"hostpf0", "hostpf1"}, check.Subject).State != "failed" {
				t.Fatal("initial vacancy policy was weakened")
			}
		})
	}
}

func resumeFabricFixture() inventory.Inventory {
	return inventory.Inventory{DPUs: []inventory.Device{{ID: "dpu-a", SerialNumber: "SERIAL-A", ObservationState: "observed", NetworkFunctionMACs: []string{"02:00:00:00:00:01"}}}, HBN: &inventory.HBNInventory{Switches: []inventory.FabricSwitch{{Name: "leaf", Ports: []inventory.SwitchPort{{Name: "swp1", DPUSerial: "SERIAL-A", DPUInterface: "p0"}, {Name: "swp2", DPUSerial: "SERIAL-B", DPUInterface: "p0"}}, Snapshot: fabric.Snapshot{Operational: fabric.OperationalFacts{Addresses: &[]fabric.InterfaceAddress{}, IPv6Routes: &[]fabric.Route{}, IPv4Routes: &[]fabric.Route{{Family: "ipv4", Prefix: "198.51.100.0/29", Table: "main", Protocol: "bgp", NextHops: []fabric.RouteNextHop{{Interface: "swp1", Gateway: "fe80::1"}}}}, LLDP: []fabric.LLDPPeerHint{{LocalInterface: "swp1", PortID: stringPointer("02:00:00:00:00:01"), PortDescription: stringPointer("p0")}}}}}}}}
}

func TestResumeFabricRequiresExactAllocationAndItsOwnCorroboratedUplinks(t *testing.T) {
	for _, test := range []struct {
		name  string
		edit  func(*inventory.Inventory)
		allow []ResumeRouteAllocation
		pass  bool
	}{
		{"owned", func(*inventory.Inventory) {}, []ResumeRouteAllocation{{DPUSerial: "SERIAL-A", Pool: "workload", Prefix: "198.51.100.0/29"}}, true},
		{"pending allocation", func(*inventory.Inventory) {}, nil, false},
		{"other DPU allocation", func(*inventory.Inventory) {}, []ResumeRouteAllocation{{DPUSerial: "SERIAL-B", Pool: "workload", Prefix: "198.51.100.0/29"}}, false},
		{"foreign same prefix hop", func(i *inventory.Inventory) {
			(*i.HBN.Switches[0].Snapshot.Operational.IPv4Routes)[0].NextHops[0].Interface = "swp2"
		}, []ResumeRouteAllocation{{DPUSerial: "SERIAL-A", Pool: "workload", Prefix: "198.51.100.0/29"}}, false},
		{"mixed ECMP", func(i *inventory.Inventory) {
			r := &(*i.HBN.Switches[0].Snapshot.Operational.IPv4Routes)[0]
			r.NextHops = append(r.NextHops, fabric.RouteNextHop{Interface: "swp2", Gateway: "fe80::2"})
		}, []ResumeRouteAllocation{{DPUSerial: "SERIAL-A", Pool: "workload", Prefix: "198.51.100.0/29"}}, false},
		{"missing next hops", func(i *inventory.Inventory) { (*i.HBN.Switches[0].Snapshot.Operational.IPv4Routes)[0].NextHops = nil }, []ResumeRouteAllocation{{DPUSerial: "SERIAL-A", Pool: "workload", Prefix: "198.51.100.0/29"}}, false},
		{"aggregate", func(i *inventory.Inventory) {
			(*i.HBN.Switches[0].Snapshot.Operational.IPv4Routes)[0].Prefix = "198.51.100.0/24"
		}, []ResumeRouteAllocation{{DPUSerial: "SERIAL-A", Pool: "workload", Prefix: "198.51.100.0/29"}}, false},
		{"static", func(i *inventory.Inventory) {
			(*i.HBN.Switches[0].Snapshot.Operational.IPv4Routes)[0].Protocol = "static"
		}, []ResumeRouteAllocation{{DPUSerial: "SERIAL-A", Pool: "workload", Prefix: "198.51.100.0/29"}}, false},
		{"blackhole", func(i *inventory.Inventory) {
			(*i.HBN.Switches[0].Snapshot.Operational.IPv4Routes)[0].Type = "blackhole"
		}, []ResumeRouteAllocation{{DPUSerial: "SERIAL-A", Pool: "workload", Prefix: "198.51.100.0/29"}}, false},
		{"unknown VRF", func(i *inventory.Inventory) { (*i.HBN.Switches[0].Snapshot.Operational.IPv4Routes)[0].Table = "1002" }, []ResumeRouteAllocation{{DPUSerial: "SERIAL-A", Pool: "workload", Prefix: "198.51.100.0/29"}}, false},
		{"missing LLDP", func(i *inventory.Inventory) { i.HBN.Switches[0].Snapshot.Operational.LLDP = nil }, []ResumeRouteAllocation{{DPUSerial: "SERIAL-A", Pool: "workload", Prefix: "198.51.100.0/29"}}, false},
		{"owned prefix address still foreign", func(i *inventory.Inventory) {
			i.HBN.Switches[0].Snapshot.Operational.Addresses = &[]fabric.InterfaceAddress{{Interface: "swp1", Prefix: "198.51.100.1/29"}}
		}, []ResumeRouteAllocation{{DPUSerial: "SERIAL-A", Pool: "workload", Prefix: "198.51.100.0/29"}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			inv := resumeFabricFixture()
			test.edit(&inv)
			before, _ := json.Marshal(inv)
			sw := inv.HBN.Switches[0]
			got := ResumeFabricPoolCheck(&inv, sw, "default", "198.51.100.0/24", "workload", test.allow)
			if (got.State == "passed") != test.pass {
				t.Fatalf("got=%+v", got)
			}
			after, _ := json.Marshal(inv)
			if !bytes.Equal(before, after) {
				t.Fatal("resume mutated source facts")
			}
			if test.pass && hbnFabricPoolCheck(sw, "default", "198.51.100.0/24", "workload").State != "failed" {
				t.Fatal("initial vacancy check changed")
			}
		})
	}
}

func TestResumePresentationDefersOnlyEligibilityMessages(t *testing.T) {
	profile, err := hbn.Resolve(hbn.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var progress strings.Builder
	report := &inventory.Report{Inventory: &inventory.Inventory{}, Discovery: inventory.Discovery{Outcome: "complete"}}
	collectHBN(ui.WithOutput(context.Background(), ui.NewWithWriter(&progress)), config.Request{HBN: &profile}, report, nil, Dependencies{DeferHBNReadiness: true})
	if report.Readiness == nil || report.Readiness.State != "blocked" || len(report.Readiness.Checks) == 0 {
		t.Fatal("presentation option changed eligibility")
	}
	if strings.Contains(progress.String(), "[BLOCKED] HBN-BGP") || !strings.Contains(progress.String(), "owned deployment-phase eligibility will be assessed") {
		t.Fatalf("raw initial eligibility leaked into resume progress: %s", progress.String())
	}
}
