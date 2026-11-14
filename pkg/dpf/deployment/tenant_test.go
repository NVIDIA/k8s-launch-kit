// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"testing"
	"time"
)

func TestTenantPlanUsesIndependentReleaseVersionAndImmutableIntent(t *testing.T) {
	input := preflightGenerationInputs(t)
	generated, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := generation.LoadPlan(generated)
	if err != nil {
		t.Fatal(err)
	}
	expected, desired, err := tenantPlanInputs(plan)
	if err != nil {
		t.Fatal(err)
	}
	if expected.KubernetesVersion != "v1.35.6" || expected.KubernetesVersion == plan.Intent().KubernetesVersion {
		t.Fatal("tenant version was substituted with management version")
	}
	if expected.Name != input.Operator.Bootstrap.Tenant.Name || expected.PodCIDR != input.Operator.Bootstrap.PodCIDR || desired.GetKind() != "DPUCluster" {
		t.Fatal("retained tenant inputs differ")
	}
	expected.Labels["unwanted"] = "mutation"
	expected.NodeSelector["unwanted"] = "mutation"
	desired.SetName("unwanted")
	again, object, err := tenantPlanInputs(plan)
	if err != nil {
		t.Fatal(err)
	}
	if again.Labels["unwanted"] != "" || again.NodeSelector["unwanted"] != "" || object.GetName() == "unwanted" {
		t.Fatal("tenant plan inputs alias retained intent")
	}
	if _, _, err := tenantPlanInputs(nil); err == nil {
		t.Fatal("nil tenant plan accepted")
	}
}

func TestConnectRetainedTenantRejectsMissingInputsBeforeAccess(t *testing.T) {
	if _, err := connectRetainedTenant(context.Background(), nil, nil, "", nil, nil, time.Second, time.Second); err == nil {
		t.Fatal("incomplete tenant handoff accepted")
	}
}
