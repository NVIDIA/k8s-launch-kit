// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifactcheck"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

type artifactVerifierFunc func(context.Context, []artifactcheck.Artifact) []artifactcheck.Result

func (f artifactVerifierFunc) Verify(ctx context.Context, artifacts []artifactcheck.Artifact) []artifactcheck.Result {
	return f(ctx, artifacts)
}

func TestHBNArtifactSourceChecksVerifyOnlySelectedLockedSources(t *testing.T) {
	inv := hostHBNTestInventory()
	inv.Hosts[0].Runtime.Hardware = &inventory.HardwareObservation{Cards: []inventory.CardHardware{{
		SerialNumber: inv.DPUs[0].SerialNumber,
		PCIAddress:   inv.Hosts[0].Adapters[0].PCIAddress,
		Domain:       "host-pci",
		PSID:         inventory.Observation{State: "observed", Values: []string{"MT_0000001774"}},
	}}}
	input := &config.Provisioning{Software: config.Software{
		OSISO: config.Artifact{URL: "https://artifacts.example/os.iso", SHA256: strings.Repeat("a", 64)},
		PLDM: map[string]config.Artifact{
			"MT_0000001774": {URL: "https://artifacts.example/fw.pkg", SHA256: strings.Repeat("b", 64)},
		},
	}}
	var got []artifactcheck.Artifact
	checks := hbnArtifactSourceChecks(context.Background(), input, &inv, artifactVerifierFunc(func(_ context.Context, artifacts []artifactcheck.Artifact) []artifactcheck.Result {
		got = append([]artifactcheck.Artifact(nil), artifacts...)
		results := make([]artifactcheck.Result, len(artifacts))
		for i, artifact := range artifacts {
			results[i] = artifactcheck.Result{Subject: artifact.Subject, State: "passed", ReasonCode: "ARTIFACT_SOURCE_VERIFIED"}
		}
		return results
	}))
	if len(got) != 2 || got[0].Subject != "osISO" || got[1].Subject != "pldm/MT_0000001774" {
		t.Fatalf("verified sources = %#v", got)
	}
	if len(checks) != 2 || checks[0].State != "passed" || checks[1].State != "passed" {
		t.Fatalf("source checks = %#v", checks)
	}
}

func TestHBNArtifactSourceChecksKeepUnknownPSIDScopedAndStillCheckISO(t *testing.T) {
	inv := hostHBNTestInventory()
	input := &config.Provisioning{Software: config.Software{
		OSISO: config.Artifact{URL: "https://artifacts.example/os.iso", SHA256: strings.Repeat("a", 64)},
		PLDM:  map[string]config.Artifact{"MT_0000001774": {URL: "https://artifacts.example/fw.pkg", SHA256: strings.Repeat("b", 64)}},
	}}
	calls := 0
	checks := hbnArtifactSourceChecks(context.Background(), input, &inv, artifactVerifierFunc(func(_ context.Context, artifacts []artifactcheck.Artifact) []artifactcheck.Result {
		calls++
		if len(artifacts) != 1 || artifacts[0].Subject != "osISO" {
			t.Fatalf("unknown PSID caused unsafe PLDM selection: %#v", artifacts)
		}
		return []artifactcheck.Result{{Subject: "osISO", State: "passed", ReasonCode: "ARTIFACT_SOURCE_VERIFIED"}}
	}))
	if calls != 1 || len(checks) != 2 || checkByCode(checks, "ARTIFACT_SOURCE_VERIFIED").State != "passed" || checkByCode(checks, "SELECTED_PSID_SCOPE_UNAVAILABLE").State != "unknown" {
		t.Fatalf("checks=%#v calls=%d", checks, calls)
	}
}

func TestHBNArtifactSourceChecksMissingVerifierAndInvalidResultFailClosed(t *testing.T) {
	inv := hostHBNTestInventory()
	inv.Hosts[0].Runtime.Hardware = &inventory.HardwareObservation{Cards: []inventory.CardHardware{{
		SerialNumber: inv.DPUs[0].SerialNumber,
		PCIAddress:   inv.Hosts[0].Adapters[0].PCIAddress,
		Domain:       "host-pci",
		PSID:         inventory.Observation{State: "observed", Values: []string{"MT_0000001774"}},
	}}}
	input := &config.Provisioning{Software: config.Software{OSISO: config.Artifact{URL: "https://artifacts.example/os.iso", SHA256: strings.Repeat("a", 64)}}}
	checks := hbnArtifactSourceChecks(context.Background(), input, &inv, nil)
	if len(checks) != 2 || checkByCode(checks, "ARTIFACT_VERIFIER_UNAVAILABLE").State != "unknown" || checkByCode(checks, "PLDM_ARTIFACT_MISSING").State != "unknown" {
		t.Fatalf("missing verifier checks = %#v", checks)
	}
	checks = hbnArtifactSourceChecks(context.Background(), input, &inv, artifactVerifierFunc(func(context.Context, []artifactcheck.Artifact) []artifactcheck.Result {
		return []artifactcheck.Result{{Subject: "wrong", State: "passed", ReasonCode: "ARTIFACT_SOURCE_VERIFIED"}}
	}))
	if checkByCode(checks, "ARTIFACT_RESULT_INVALID").State != "unknown" {
		t.Fatalf("invalid verifier result passed: %#v", checks)
	}
	checks = hbnArtifactSourceChecks(context.Background(), input, &inv, artifactVerifierFunc(func(_ context.Context, artifacts []artifactcheck.Artifact) []artifactcheck.Result {
		results := make([]artifactcheck.Result, len(artifacts))
		for i, artifact := range artifacts {
			results[i] = artifactcheck.Result{Subject: artifact.Subject, State: "failed", ReasonCode: "https://user:secret@artifact.invalid/?token=private"}
		}
		return results
	}))
	if check := checkByCode(checks, "ARTIFACT_RESULT_INVALID"); check.State != "unknown" || strings.Contains(check.ReasonCode+check.Message, "secret") || strings.Contains(check.ReasonCode+check.Message, "private") {
		t.Fatalf("unsafe verifier result escaped into inventory: %#v", check)
	}
}

func checkByCode(checks []inventory.Check, code string) inventory.Check {
	for _, check := range checks {
		if check.ReasonCode == code {
			return check
		}
	}
	return inventory.Check{}
}
