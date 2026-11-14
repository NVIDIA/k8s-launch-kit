// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/catalog"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
)

func TestControlPlaneRejectsIncompleteExecutionBeforeManagementAccess(t *testing.T) {
	generated, err := generation.Generate(preflightGenerationInputs(t))
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
			_, err := runControlPlane(ctx, &APIClient{}, generated, journal, probes, time.Second, time.Second)
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
	generated, err := generation.Generate(input)
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
	_, err = runControlPlane(ctx, client, generated, journal, probes, time.Second, time.Second)
	if err == nil || !strings.Contains(err.Error(), "TENANT_SITE_ALLOCATION_MISSING") {
		t.Fatalf("expected missing allocation to block before unconfigured API transport, got %v", err)
	}
	if !strings.Contains(file.String(), `"state":"unknown"`) || strings.Contains(file.String(), `"state":"failed"`) {
		t.Fatalf("missing declaration was not retained as unknown: %s", file.String())
	}
}
