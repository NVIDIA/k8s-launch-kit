// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/catalog"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

type softwarePreparationFake struct {
	*softwareStageFake
	tenant *tenantClient
}

func (f *softwarePreparationFake) CheckManagementPreflight(ctx context.Context, req ManagementPreflightRequest) PreflightResult {
	return f.operator.CheckManagementPreflight(ctx, req)
}

func (f *softwarePreparationFake) checkDPFOperatorConfigPermissions(ctx context.Context, namespace string) []PreflightCheck {
	return f.operator.checkDPFOperatorConfigPermissions(ctx, namespace)
}

func (f *softwarePreparationFake) discoverResourceList(ctx context.Context, groupVersion string) (*metav1.APIResourceList, error) {
	return f.operator.discoverResourceList(ctx, groupVersion)
}

func (f *softwarePreparationFake) ensureOperatorPlan(ctx context.Context, plan *operatorPlan) error {
	return f.operator.ensureOperatorPlan(ctx, plan)
}

func (f *softwarePreparationFake) Get(ctx context.Context, wanted *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if wanted.GetKind() == "CustomResourceDefinition" {
		return f.operator.Get(ctx, wanted)
	}
	return f.softwareStageFake.Get(ctx, wanted)
}

func (f *softwarePreparationFake) checkTenantStorage(ctx context.Context, namespace string) PreflightCheck {
	return f.base.checkTenantStorage(ctx, namespace)
}

func (f *softwarePreparationFake) checkTenantNetwork(_ context.Context, _ bootstrap.Tenant, _ tenantNetworkRunner, _ string, _ time.Duration) ([]inventory.Check, error) {
	return []inventory.Check{{ID: "test.network", State: "passed", ReasonCode: "NETWORK_FIXTURE", Message: "fixture observation"}}, nil
}

func (f *softwarePreparationFake) connectTenant(ctx context.Context, _ *generation.Plan, uid types.UID, _ *Reconciler, _ *executionJournal, _, _ time.Duration) (*tenantClient, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.tenant.dpuClusterUID = string(uid)
	return f.tenant, nil
}

func TestRunSoftwarePreparationPassesOneRetainedBundleThroughStages(t *testing.T) {
	execution, _, selected, _, stageFake, _ := softwareStageFixture(t)
	retained := execution.retained
	base := stageFake.base
	operator := stageFake.operator
	prepared := &softwarePreparationFake{softwareStageFake: stageFake}

	// Seed an exact, controller-ready DPUCluster and tenant source matching the
	// retained plan. This tests stage composition and plan handoff, not a clean
	// cluster's controller installation lifecycle.
	expected, desired, err := tenantPlanInputs(execution.plan)
	if err != nil {
		t.Fatal(err)
	}
	current, err := execution.reconciler.markedDesired(desired)
	if err != nil {
		t.Fatal(err)
	}
	current.SetUID(types.UID("dc-uid"))
	current.SetResourceVersion("5")
	current.Object["status"] = runtime.DeepCopyJSONValue(stageFake.sources.sources.observed.DPUCluster.Object["status"])
	base.resources[tenantStageKey(current)] = current.DeepCopy()
	base.clusters = []unstructured.Unstructured{*current.DeepCopy()}
	if err := operatorBindingForExecution(operator, execution); err != nil {
		t.Fatal(err)
	}
	tenant, _ := tenantIdentityClientFixture(t, expected, stageFake.sources)
	tenant.source = prepared
	prepared.tenant = tenant
	base.tenant = tenant

	request := controlPlaneTestRequest(t)
	for i := range request.Targets {
		if request.Targets[i].Endpoint == "https://192.0.2.20" {
			request.Targets[i].Endpoint = "https://192.0.2.30"
		}
	}
	journal := testJournal(execution)
	probes := probe.Runner{Assets: catalog.ProbeAssets(), Journal: func([]probe.Receipt) error { return nil }}
	ctx := tenantStageContext(t)
	got, err := runSoftwarePreparation(ctx, prepared, retained, journal, probes, request, time.Millisecond, time.Second, 1<<20)
	if err != nil {
		t.Fatalf("runSoftwarePreparation() error = %v", err)
	}
	if got.BundleDigest != execution.plan.Digest() || got.ControlPlane.enrollment.BundleDigest != got.BundleDigest || len(got.ControlPlane.enrollment.Devices) != len(selected) || len(got.Software.Components) != 2 {
		t.Fatalf("preparation receipt does not preserve the selected plan and completed stage receipts: %#v", got)
	}
	if stageFake.createSoftware != 1 || stageFake.streamRequests.Load() != 2 || stageFake.tunnelCloses != 1 {
		t.Fatalf("software effect counts create=%d streams=%d close=%d", stageFake.createSoftware, stageFake.streamRequests.Load(), stageFake.tunnelCloses)
	}
}

func operatorBindingForExecution(fake *managementFake, execution *managementExecution) error {
	if fake == nil || execution == nil || execution.plan == nil || fake.bundleDigest != execution.plan.Digest() {
		return fmt.Errorf("management fake is not bound to the retained plan")
	}
	fake.binding = true
	return nil
}

var _ softwareStageManagementClient = (*softwarePreparationFake)(nil)
var _ managementRunnerClient = (*softwarePreparationFake)(nil)
