// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package probe

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func TestProbeTLSBypassIsBoundToExplicitBMCPolicy(t *testing.T) {
	now := time.Now()
	request := Request{
		Version: ProtocolVersion, RunID: "tls-policy", Deadline: now.Add(time.Minute),
		RequestTimeout: time.Second,
		Destinations:   []Destination{{ID: "bmc", Endpoint: "https://192.0.2.10", Path: "/redfish/v1/", InsecureSkipVerify: true}},
	}
	files, err := requestFiles(request)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Destinations []struct {
			ID                 string `json:"id"`
			InsecureSkipVerify bool   `json:"insecureSkipVerify"`
		} `json:"destinations"`
	}
	if err := json.Unmarshal([]byte(files["request.json"]), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Destinations) != 1 || document.Destinations[0].ID != "bmc" || !document.Destinations[0].InsecureSkipVerify {
		t.Fatalf("request lost explicit BMC TLS policy: %+v", document)
	}

	response := Response{
		Version: ProtocolVersion, RunID: request.RunID, NodeUID: "node", PodUID: "pod",
		NetworkMode: "pod-network", StartedAt: now, FinishedAt: now,
		Results: []inventory.ProbeResult{{NodeUID: "node", NetworkMode: "pod-network", Destination: "bmc", DNS: "not-applicable", Addresses: []inventory.ProbeAddress{{Address: "192.0.2.10", TCP: "reachable", TLS: "unverified", HTTP: "authentication-required"}}}},
	}
	if err := ValidateResponse(response, request, "node", "pod", "pod-network"); err != nil {
		t.Fatalf("explicitly unverified BMC response was rejected: %v", err)
	}
	response.Results[0].Addresses[0].TLS = "verified"
	if err := ValidateResponse(response, request, "node", "pod", "pod-network"); err == nil {
		t.Fatal("response falsely claimed verification when bypass was requested")
	}
	response.Results[0].Addresses[0].TLS = "unverified"
	request.Destinations[0].InsecureSkipVerify = false
	if err := ValidateResponse(response, request, "node", "pod", "pod-network"); err == nil {
		t.Fatal("unverified BMC response was accepted without an explicit bypass")
	}
}

func TestProbeRejectsManagementTLSBypassAndConflictingTrust(t *testing.T) {
	request := Request{Version: ProtocolVersion, RunID: "tls-policy", Deadline: time.Now().Add(time.Minute), RequestTimeout: time.Second,
		Destinations: []Destination{{ID: "management-api", Endpoint: "https://api.invalid", Path: "/version", InsecureSkipVerify: true}}}
	if _, err := requestFiles(request); err == nil {
		t.Fatal("management API accepted TLS bypass")
	}
	request.Destinations[0] = Destination{ID: "bmc", Endpoint: "https://192.0.2.10", Path: "/redfish/v1/", CA: []byte("not a CA"), InsecureSkipVerify: true}
	if _, err := requestFiles(request); err == nil {
		t.Fatal("BMC request accepted simultaneous bypass and CA trust")
	}
}
