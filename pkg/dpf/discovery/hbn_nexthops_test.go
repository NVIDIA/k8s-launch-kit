// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func TestReferencedKernelNextHopsFeedExactOwnedAssessment(t *testing.T) {
	const nextHops = `[{"id":100,"group":[{"id":101},{"id":102,"weight":2}],"protocol":"zebra","flags":[]},{"id":101,"gateway":"fe80::1","dev":"swp1","scope":"link","protocol":"zebra","flags":[]},{"id":102,"gateway":"fe80::2","dev":"swp2","scope":"link","protocol":"zebra","flags":[]}]`
	const routes = `[{"dst":"198.51.100.0/29","nhid":100,"protocol":"bgp","metric":20,"flags":[]},{"dst":"203.0.113.0","nhid":100,"protocol":"bgp","metric":20,"flags":[]}]`
	for _, test := range []struct {
		name, before, after, reason string
		allocate                    bool
	}{
		{"current complete owner", nextHops, nextHops, "OWNED_RESUME_FABRIC_ALLOCATIONS_VERIFIED", true},
		{"allocation not ready", nextHops, nextHops, "OWNED_RESUME_ROUTE_ALLOCATION_UNVERIFIED", false},
		{"changed path", nextHops, strings.Replace(nextHops, "fe80::2", "fe80::3", 1), "OWNED_RESUME_ROUTE_NEXTHOPS_UNVERIFIED", true},
		{"missing path", nextHops, `[]`, "OWNED_RESUME_ROUTE_NEXTHOPS_UNVERIFIED", true},
		{"foreign current path", strings.Replace(nextHops, "swp2", "swp9", 1), strings.Replace(nextHops, "swp2", "swp9", 1), "OWNED_RESUME_ROUTE_FOREIGN_UPLINK", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			inv := resumeFabricFixture()
			inv.DPUs[0].NetworkFunctionMACs = append(inv.DPUs[0].NetworkFunctionMACs, "02:00:00:00:00:02")
			sw := &inv.HBN.Switches[0]
			sw.Ports[1].DPUSerial, sw.Ports[1].DPUInterface = "SERIAL-A", "p1"
			sw.Snapshot.Operational.LLDP = append(sw.Snapshot.Operational.LLDP, fabric.LLDPPeerHint{LocalInterface: "swp2", PortID: stringPointer("02:00:00:00:00:02"), PortDescription: stringPointer("p1")})
			parsed := fabric.ParseNVUE(fabric.NVUEInputs{OperationalRoutesIPv4: []byte(routes), OperationalRoutesIPv6: []byte(`[]`), OperationalNextHopsBefore: []byte(test.before), OperationalNextHopsAfter: []byte(test.after)})
			sw.Snapshot.Operational.IPv4Routes = parsed.Operational.IPv4Routes
			before, _ := json.Marshal(inv)
			var allocations []ResumeRouteAllocation
			if test.allocate {
				allocations = []ResumeRouteAllocation{{DPUSerial: "SERIAL-A", Pool: "workload", Prefix: "198.51.100.0/29"}, {DPUSerial: "SERIAL-A", Pool: "loopback", Prefix: "203.0.113.0/32"}}
			}
			for _, pool := range []struct{ name, cidr string }{{"workload", "198.51.100.0/24"}, {"loopback", "203.0.113.0/24"}} {
				got := ResumeFabricPoolCheck(&inv, *sw, "default", pool.cidr, pool.name, allocations)
				if got.ReasonCode != test.reason || (got.State == "passed") != (test.reason == "OWNED_RESUME_FABRIC_ALLOCATIONS_VERIFIED") {
					t.Fatalf("%s check: %+v", pool.name, got)
				}
				if hbnFabricPoolCheck(*sw, "default", pool.cidr, pool.name).State != "failed" {
					t.Fatal("initial vacancy check accepted an existing route")
				}
				// These are the check fields retained by the existing report/event
				// path: no raw kernel data or generic hidden cause is needed.
				raw, _ := json.Marshal(got)
				if !bytes.Contains(raw, []byte(test.reason)) || !bytes.Contains(raw, []byte(got.Message)) {
					t.Fatal("actionable result not serializable")
				}
			}
			after, _ := json.Marshal(inv)
			if !bytes.Equal(before, after) {
				t.Fatal("assessment mutated current facts")
			}
		})
	}
}

func TestOwnedRouteFailureReasonsDoNotMaskForeignOrUnknownPaths(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		edit         func(*inventory.Inventory)
	}{
		{"missing allocation", "OWNED_RESUME_ROUTE_ALLOCATION_UNVERIFIED", func(i *inventory.Inventory) {
			(*i.HBN.Switches[0].Snapshot.Operational.IPv4Routes)[0].Prefix = "198.51.100.8/29"
		}},
		{"missing LLDP", "OWNED_RESUME_ROUTE_UPLINKS_UNVERIFIED", func(i *inventory.Inventory) { i.HBN.Switches[0].Snapshot.Operational.LLDP = nil }},
		{"no nexthops", "OWNED_RESUME_ROUTE_NEXTHOPS_UNVERIFIED", func(i *inventory.Inventory) { (*i.HBN.Switches[0].Snapshot.Operational.IPv4Routes)[0].NextHops = nil }},
		{"foreign path", "OWNED_RESUME_ROUTE_FOREIGN_UPLINK", func(i *inventory.Inventory) {
			(*i.HBN.Switches[0].Snapshot.Operational.IPv4Routes)[0].NextHops[0].Interface = "swp2"
		}},
		{"unsupported protocol", "OWNED_RESUME_ROUTE_KIND_UNSUPPORTED", func(i *inventory.Inventory) {
			(*i.HBN.Switches[0].Snapshot.Operational.IPv4Routes)[0].Protocol = "static"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			inv := resumeFabricFixture()
			test.edit(&inv)
			got := ResumeFabricPoolCheck(&inv, inv.HBN.Switches[0], "default", "198.51.100.0/24", "workload", []ResumeRouteAllocation{{DPUSerial: "SERIAL-A", Pool: "workload", Prefix: "198.51.100.0/29"}})
			if got.State != "failed" || got.ReasonCode != test.reason {
				t.Fatalf("lost ownership diagnostic: %+v", got)
			}
		})
	}
}

func TestInitialFabricQualificationRequiresNextHopReadCapabilityWithEmptyRoutes(t *testing.T) {
	for _, raw := range []string{"", `[]`} {
		in := fabric.NVUEInputs{NextHopCollectionRequested: true, OperationalAddresses: []byte(`[]`), OperationalRoutesIPv4: []byte(`[]`), OperationalRoutesIPv6: []byte(`[]`), OperationalNextHopsBefore: []byte(raw), OperationalNextHopsAfter: []byte(raw)}
		sw := inventory.FabricSwitch{Name: "leaf", Snapshot: fabric.ParseNVUE(in)}
		got := hbnFabricPoolCheck(sw, "default", "198.51.100.0/24", "workload")
		if (got.State == "passed") != (raw == `[]`) {
			t.Fatalf("empty routes masked missing capability: %+v", got)
		}
	}
}
