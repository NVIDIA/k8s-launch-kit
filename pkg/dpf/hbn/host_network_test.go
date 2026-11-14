// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package hbn

import (
	"strings"
	"testing"
)

func TestHostReservationsCoverEachChildPrefix(t *testing.T) {
	profile, err := Resolve(Options{})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := ResolveGeneration(GenerationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ranges, err := HostReservations(profile, generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 128 {
		t.Fatalf("got %d exclusions, want one for each of 128 /29 allocations", len(ranges))
	}
	if ranges[0] != (AddressRange{StartIP: "10.0.120.2", EndIP: "10.0.120.2"}) || ranges[1] != (AddressRange{StartIP: "10.0.120.10", EndIP: "10.0.120.10"}) || ranges[len(ranges)-1] != (AddressRange{StartIP: "10.0.123.250", EndIP: "10.0.123.250"}) {
		t.Fatalf("unexpected deterministic singleton exclusions: first=%#v second=%#v last=%#v", ranges[0], ranges[1], ranges[len(ranges)-1])
	}
}

func TestHostReservationsHandleSmallestSupportedPrefix(t *testing.T) {
	profile, err := Resolve(Options{WorkloadCIDR: stringPointer("192.0.2.0/30"), WorkloadPrefix: intPointer(30)})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := ResolveGeneration(GenerationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ranges, err := HostReservations(profile, generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 || ranges[0] != (AddressRange{StartIP: "192.0.2.2", EndIP: "192.0.2.2"}) {
		t.Fatalf("/30 reservation = %#v", ranges)
	}
}

func TestHostReservationsRejectInvalidPlans(t *testing.T) {
	profile, err := Resolve(Options{})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := ResolveGeneration(GenerationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		profile    Profile
		generation Generation
	}{
		{name: "host collides with gateway", profile: profile, generation: func() Generation { g := generation; g.HostAddressIndex = g.WorkloadGatewayIndex; return g }()},
		{name: "child prefix /31", profile: func() Profile { p := profile; p.WorkloadCIDR = "192.0.2.0/30"; p.WorkloadPrefix = 31; return p }(), generation: generation},
		{name: "child prefix /32", profile: func() Profile { p := profile; p.WorkloadCIDR = "192.0.2.0/30"; p.WorkloadPrefix = 32; return p }(), generation: generation},
		{name: "too many child prefixes", profile: func() Profile { p := profile; p.WorkloadCIDR = "10.0.0.0/16"; return p }(), generation: generation},
		{name: "host index outside block", profile: func() Profile { p := profile; p.WorkloadCIDR = "192.0.2.0/30"; p.WorkloadPrefix = 30; return p }(), generation: func() Generation { g := generation; g.HostAddressIndex = 3; return g }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if ranges, err := HostReservations(tt.profile, tt.generation); err == nil || ranges != nil {
				t.Fatalf("HostReservations() = %d ranges, %v; want nil and an error", len(ranges), err)
			}
		})
	}
}

func TestHostReservationsBoundIsExplicit(t *testing.T) {
	profile, err := Resolve(Options{})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := ResolveGeneration(GenerationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	profile.WorkloadCIDR = "10.0.0.0/16"
	generation.LoopbackCIDR = "192.168.0.0/24"
	if _, err := HostReservations(profile, generation); err == nil {
		t.Fatal("reservation plan larger than bounded output was accepted")
	} else if want := "4096"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not explain the %s-entry bound", err, want)
	}
}
