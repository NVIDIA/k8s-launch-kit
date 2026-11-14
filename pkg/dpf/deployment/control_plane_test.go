// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/catalog"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
)

func TestControlPlaneRejectsIncompleteExecutionBeforeManagementAccess(t *testing.T) {
	generated, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*probe.Runner, *executionJournal)
		want   string
	}{
		{name: "unqualified probe", change: func(r *probe.Runner, _ *executionJournal) { r.Assets.Qualified = false }, want: "qualification"},
		{name: "missing resource journal", change: func(r *probe.Runner, _ *executionJournal) { r.Journal = nil }, want: "journal"},
		{name: "unbound report", change: func(_ *probe.Runner, j *executionJournal) { j.digest = "other" }, want: "retained bundle"},
		{name: "management binding mismatch", change: func(_ *probe.Runner, _ *executionJournal) {}, want: "binding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			file := &managementMemoryFile{}
			journal := &executionJournal{file: file, digest: generated.Index().BundleDigest}
			probes := probe.Runner{Assets: catalog.ProbeAssets(), Journal: func([]probe.Receipt) error {
				t.Fatal("probe effects began before input validation")
				return nil
			}}
			tc.change(&probes, journal)
			// An unbound client has no transport: any attempted API access would be
			// invalid. These input failures must be detected before reaching it.
			_, err := runControlPlane(ctx, &APIClient{}, generated, journal, probes, config.Request{}, time.Second, time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want failure containing %q", err, tc.want)
			}
			if file.Len() != 0 {
				t.Fatalf("management execution started before validation: %s", file.String())
			}
		})
	}
}

func TestControlPlaneRequiresSiteAllocationBeforeManagementEffects(t *testing.T) {
	input := preflightGenerationInputs(t)
	generated, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := generation.LoadPlan(generated)
	if err != nil {
		t.Fatal(err)
	}
	intent := plan.Intent()
	client := &APIClient{expected: intent.Management, version: intent.KubernetesVersion}
	file := &managementMemoryFile{}
	journal := &executionJournal{file: file, digest: plan.Digest()}
	probes := probe.Runner{Assets: catalog.ProbeAssets(), Journal: func([]probe.Receipt) error {
		t.Fatal("probe effects began without a site allocation declaration")
		return nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	request := controlPlaneTestRequest(t)
	_, err = runControlPlane(ctx, client, generated, journal, probes, request, time.Second, time.Second)
	if err == nil || !strings.Contains(err.Error(), "TENANT_REQUIRED_CHECKS_BLOCKED") || !strings.Contains(file.String(), "TENANT_SITE_ALLOCATION_MISSING") {
		t.Fatalf("expected missing allocation to block before unconfigured API transport, got %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(file.String()), "\n") {
		var event executionEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Operation == "site-allocation" && event.State != "unknown" {
			t.Fatalf("missing declaration was not retained as unknown: %s", line)
		}
	}
}

func controlPlaneTestRequest(t *testing.T) config.Request {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "password"), []byte("enrollment-test-password"), 0o600); err != nil {
		t.Fatal(err)
	}
	mode := "management"
	input := config.Input{Path: filepath.Join(dir, "site.yaml")}
	input.Site.Management.KubeconfigRef.File = "kubeconfig"
	input.Site.Credentials = map[string]config.Credential{"bmc": {Username: "admin", PasswordRef: config.PasswordRef{File: strPtr("password")}}}
	input.Site.Discovery.Probes.Mode = &mode
	input.Site.Discovery.DPUs = []config.Endpoint{{BMCEndpoint: "https://192.0.2.20", CredentialRef: "bmc"}}
	input.Site.Discovery.Hosts = []config.Endpoint{{BMCEndpoint: "https://192.0.2.21", CredentialRef: "bmc"}}
	request, err := config.Resolve(input)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func strPtr(value string) *string { return &value }
