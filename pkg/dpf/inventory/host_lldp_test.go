// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHostLLDPCaptureRoundTripsAndDeepClones(t *testing.T) {
	runtime := &RuntimeObservation{Network: &NetworkObservation{
		Links: []NetworkLink{{Name: "ens5f0np0", PCIAddress: "0000:82:00.0", PhysicalPortName: "p0"}},
		HostLLDP: &HostLLDPCapture{Ports: []HostLLDPCaptureResult{{
			PCIAddress: "0000:82:00.0", Interface: "ens5f0np0", PhysicalPortName: "p0", State: "observed", ReasonCode: "HOST_LLDP_PEER_OBSERVED", Message: "one peer", Neighbors: []HostLLDPNeighbor{{SourceMAC: "20:4d:52:3e:9a:b8", ChassisIDSubtype: 4, ChassisID: "cc:40:f3:55:e1:2c", PortIDSubtype: 5, PortID: "swp19s0", SystemName: "leaf-a"}},
		}}},
	}}

	copy := cloneNetwork(runtime.Network)
	copy.HostLLDP.Ports[0].Neighbors[0].PortID = "changed"
	copy.Links[0].PCIAddress = "0000:83:00.0"
	if runtime.Network.HostLLDP.Ports[0].Neighbors[0].PortID != "swp19s0" || runtime.Network.Links[0].PCIAddress != "0000:82:00.0" {
		t.Fatal("network clone aliases nested LLDP or link facts")
	}

	var issues []DeviceIssue
	encoded, err := runtimeToOutput(runtime, "hosts", &issues)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := runtimeFromOutput(encoded, "hosts", issues)
	if err != nil {
		t.Fatal(err)
	}
	got := decoded.Network.HostLLDP
	if got == nil || len(got.Ports) != 1 || len(got.Ports[0].Neighbors) != 1 || got.Ports[0].Neighbors[0].PortID != "swp19s0" || got.Ports[0].State != "" || got.Ports[0].ReasonCode != "" || got.Ports[0].Message != "" {
		t.Fatalf("unexpected host LLDP roundtrip: %#v", got)
	}
	if runtime.Network.HostLLDP.Ports[0].State != "observed" || runtime.Network.HostLLDP.Ports[0].ReasonCode != "HOST_LLDP_PEER_OBSERVED" || runtime.Network.HostLLDP.Ports[0].Message != "one peer" {
		t.Fatal("public projection mutated same-run host LLDP status")
	}
	jsonOutput, err := json.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, successMetadata := range []string{"\"state\":\"observed\"", "HOST_LLDP_PEER_OBSERVED", "one peer"} {
		if strings.Contains(string(jsonOutput), successMetadata) {
			t.Fatalf("successful capture bookkeeping leaked to public inventory: %s", jsonOutput)
		}
	}
}

func TestHostLLDPPublicProjectionRetainsFailureReasons(t *testing.T) {
	runtime := &RuntimeObservation{Network: &NetworkObservation{HostLLDP: &HostLLDPCapture{Ports: []HostLLDPCaptureResult{
		{PCIAddress: "0000:82:00.0", Interface: "ens5f0np0", PhysicalPortName: "p0", State: "unknown", ReasonCode: "HOST_LLDP_PEER_UNOBSERVED", Message: "No peer during the bounded capture.", Neighbors: []HostLLDPNeighbor{}},
		{PCIAddress: "0000:82:00.1", Interface: "ens5f1np1", PhysicalPortName: "p1", State: "failed", ReasonCode: "HOST_LLDP_PEER_AMBIGUOUS", Message: "Conflicting peers were observed.", Neighbors: []HostLLDPNeighbor{}},
	}}}}
	var issues []DeviceIssue
	output, err := runtimeToOutput(runtime, "hosts", &issues)
	if err != nil {
		t.Fatal(err)
	}
	ports := output.Network.HostLLDP.Ports
	if len(ports) != 2 || ports[0].State != "unknown" || ports[0].ReasonCode != "HOST_LLDP_PEER_UNOBSERVED" || ports[1].State != "failed" || ports[1].ReasonCode != "HOST_LLDP_PEER_AMBIGUOUS" {
		t.Fatalf("public output discarded actionable unsuccessful capture reasons: %#v", ports)
	}
}
