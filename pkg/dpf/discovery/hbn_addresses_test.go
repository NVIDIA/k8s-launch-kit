// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/fabric"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"testing"
)

func TestHBNLoopbackPoolCollisionAndPublicProjection(t *testing.T) {
	profile, err := hbn.Resolve(hbn.Options{})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := hbn.ResolveGeneration(hbn.GenerationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sw := inventory.FabricSwitch{Name: "switch-a", Snapshot: fabric.Snapshot{Operational: fabric.OperationalFacts{
		Addresses: &[]fabric.InterfaceAddress{}, IPv6Routes: &[]fabric.Route{},
		IPv4Routes: &[]fabric.Route{{Prefix: generation.LoopbackCIDR, Table: "main"}, {Prefix: "192.0.2.0/24", Table: "main"}},
	}}}
	if check := hbnFabricAddressCheck(sw, profile); check.State != "passed" {
		t.Fatalf("separate workload pool unexpectedly blocked: %+v", check)
	}
	check := hbnFabricPoolCheck(sw, profile.FabricVRF, generation.LoopbackCIDR, "loopback")
	if check.State != "failed" || check.ReasonCode != "LOOPBACK_ROUTE_OVERLAP" || check.Subject != "switch-a/loopback" {
		t.Fatalf("loopback collision lost: %+v", check)
	}
	restrictFabric(&sw, profile.FabricVRF, profile.WorkloadCIDR, generation.LoopbackCIDR)
	if sw.Snapshot.Operational.IPv4Routes == nil || len(*sw.Snapshot.Operational.IPv4Routes) != 1 || (*sw.Snapshot.Operational.IPv4Routes)[0].Prefix != generation.LoopbackCIDR {
		t.Fatalf("handoff lost relevant loopback collision: %+v", sw.Snapshot.Operational.IPv4Routes)
	}
}

func TestHBNWorkloadPoolRequiresCompleteScopedRouteCoverage(t *testing.T) {
	profile, err := hbn.Resolve(hbn.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, want string
		edit       func(*inventory.FabricSwitch)
	}{
		{"empty complete", "passed", func(*inventory.FabricSwitch) {}},
		{"IPv6 failed", "unknown", func(s *inventory.FabricSwitch) { s.Snapshot.Operational.IPv6Routes = nil }},
		{"default route", "passed", func(s *inventory.FabricSwitch) {
			s.Snapshot.Operational.IPv4Routes = &[]fabric.Route{{Prefix: "0.0.0.0/0", Table: "main"}}
		}},
		{"main allocation", "failed", func(s *inventory.FabricSwitch) {
			s.Snapshot.Operational.IPv4Routes = &[]fabric.Route{{Prefix: "10.0.120.0/24", Table: "main"}}
		}},
		{"unknown table overlap", "unknown", func(s *inventory.FabricSwitch) {
			s.Snapshot.Operational.IPv4Routes = &[]fabric.Route{{Prefix: "10.0.120.0/24", Table: "1002"}}
		}},
		{"other known VRF", "passed", func(s *inventory.FabricSwitch) {
			s.Snapshot.Configured.VRFs = []fabric.VRFConfig{{Name: "isolated"}}
			s.Snapshot.Operational.IPv4Routes = &[]fabric.Route{{Prefix: "10.0.120.0/24", Table: "isolated"}}
		}},
		{"address unknown VRF", "unknown", func(s *inventory.FabricSwitch) {
			s.Snapshot.Operational.Addresses = &[]fabric.InterfaceAddress{{Interface: "lo1", Prefix: "10.0.120.1/29"}}
		}},
		{"address selected VRF", "failed", func(s *inventory.FabricSwitch) {
			s.Snapshot.Operational.Addresses = &[]fabric.InterfaceAddress{{Interface: "lo1", Prefix: "10.0.120.1/29"}}
			s.Snapshot.Operational.Interfaces = []fabric.InterfaceState{{Name: "lo1", VRF: stringPointer("default")}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sw := inventory.FabricSwitch{Name: "leaf", Snapshot: fabric.Snapshot{Operational: fabric.OperationalFacts{Addresses: &[]fabric.InterfaceAddress{}, IPv4Routes: &[]fabric.Route{}, IPv6Routes: &[]fabric.Route{}}}}
			tc.edit(&sw)
			got := hbnFabricAddressCheck(sw, profile)
			if got.State != tc.want {
				t.Fatalf("got %#v want %s", got, tc.want)
			}
		})
	}
}

func TestHBNPublicFabricViewKeepsOnlySelectedPeersAndRelevantPrefixes(t *testing.T) {
	sw := inventory.FabricSwitch{Ports: []inventory.SwitchPort{{Name: "swp1"}}, Snapshot: fabric.Snapshot{
		Configured:  fabric.ConfiguredFacts{VRFs: []fabric.VRFConfig{{Name: "default", Neighbors: &[]fabric.BGPNeighborConfig{{Name: "swp1", PeerGroup: stringPointer("selected")}, {Name: "swp2", PeerGroup: stringPointer("other")}}, PeerGroups: []fabric.PeerGroupConfig{{Name: "selected"}, {Name: "other"}}}}},
		Operational: fabric.OperationalFacts{Addresses: &[]fabric.InterfaceAddress{{Interface: "swp1", Prefix: "fe80::1/64"}, {Interface: "eth0", Prefix: "192.0.2.1/24"}, {Interface: "lo1", Prefix: "10.0.120.1/29"}}, IPv4Routes: &[]fabric.Route{{Prefix: "0.0.0.0/0", Table: "mgmt"}, {Prefix: "10.0.120.0/29", Table: "main"}}, IPv6Routes: &[]fabric.Route{}, BGP: []fabric.BGPSession{{VRF: "default", Peers: []fabric.BGPPeerState{{Name: "swp1"}, {Name: "swp2"}}}}},
	}}
	restrictFabric(&sw, "default", "10.0.120.0/22")
	if len(*sw.Snapshot.Configured.VRFs[0].Neighbors) != 1 || len(sw.Snapshot.Configured.VRFs[0].PeerGroups) != 1 || len(sw.Snapshot.Operational.BGP[0].Peers) != 1 {
		t.Fatalf("unrelated peers retained: %#v", sw.Snapshot)
	}
	if len(*sw.Snapshot.Operational.Addresses) != 2 || len(*sw.Snapshot.Operational.IPv4Routes) != 1 {
		t.Fatalf("unrelated prefixes retained: %#v", sw.Snapshot.Operational)
	}
}
