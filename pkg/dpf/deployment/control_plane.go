// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementnetwork"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
)

// runControlPlane executes the retained management, tenant and selected-device
// enrollment stages using one prepared plan. Its receipt does not establish
// workload or DPUDeployment readiness.
func runControlPlane(ctx context.Context, client enrollmentManagementClient, retained *bundle.Bundle, journal *executionJournal,
	probes probe.Runner, request config.Request, interval, requestTimeout time.Duration,
) (enrollmentStageReceipt, error) {
	prepared, err := prepareControlPlaneRun(ctx, client, retained, journal, probes, request, interval, requestTimeout)
	if err != nil {
		return enrollmentStageReceipt{}, err
	}
	receipts, err := runPreparedControlPlane(ctx, client, prepared, probes, journal, interval, requestTimeout)
	return receipts.enrollment, err
}

type preparedControlPlaneRun struct {
	execution *managementExecution
	selected  []enrollmentBMC
	password  string
}

type controlPlaneReceipts struct {
	management managementBootstrapReceipt
	tenant     tenantStageReceipt
	enrollment enrollmentStageReceipt
}

func prepareControlPlaneRun(ctx context.Context, client enrollmentManagementClient, retained *bundle.Bundle, journal *executionJournal,
	probes probe.Runner, request config.Request, interval, requestTimeout time.Duration,
) (*preparedControlPlaneRun, error) {
	if ctx == nil || client == nil || journal == nil || interval <= 0 || requestTimeout < time.Second || requestTimeout > 2*time.Minute {
		return nil, fmt.Errorf("control-plane execution requires a client, report and bounded polling/request controls")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return nil, fmt.Errorf("control-plane execution requires a deadline")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := probes.Assets.Validate(); err != nil {
		return nil, err
	}
	if probes.Journal == nil {
		return nil, fmt.Errorf("control-plane execution requires a durable probe resource journal")
	}
	execution, err := prepareManagementExecution(retained, time.Until(deadline(ctx)))
	if err != nil {
		return nil, err
	}
	if journal.digest != execution.plan.Digest() {
		return nil, fmt.Errorf("control-plane report must identify the retained bundle")
	}
	intent := execution.plan.Intent()
	if !client.matchesBinding(intent.Management, intent.KubernetesVersion) {
		return nil, fmt.Errorf("management client binding differs from the retained plan")
	}
	selected, password, err := bindEnrollmentBMCs(intent.Devices, request.Targets)
	if err != nil {
		return nil, err
	}
	if _, err := selectedEnrollmentDiscoveries(execution, selected); err != nil {
		return nil, err
	}
	if err := journalTenantChecks(journal, "site-allocation", managementnetwork.CheckTenantAllocation(intent.Bootstrap.Tenant)); err != nil {
		return nil, err
	}
	// DPU controllers may act as soon as the operator and configuration start.
	// Verify physical BMC identity and the complete existing enrollment scope
	// before either management effect can activate unrelated devices.
	if err := client.verifyEnrollmentBMCs(ctx, selected, requestTimeout, journal); err != nil {
		return nil, err
	}
	if err := guardEnrollmentScope(ctx, client, execution, selected); err != nil {
		return nil, enrollmentStageFailure(journal, "ENROLLMENT_SCOPE_UNSAFE", err.Error(), nil)
	}
	return &preparedControlPlaneRun{execution: execution, selected: selected, password: password}, nil
}

func runPreparedControlPlane(ctx context.Context, client enrollmentManagementClient, prepared *preparedControlPlaneRun,
	probes probe.Runner, journal *executionJournal, interval, requestTimeout time.Duration,
) (controlPlaneReceipts, error) {
	if ctx == nil || client == nil || prepared == nil || prepared.execution == nil || prepared.execution.plan == nil || journal == nil || interval <= 0 ||
		requestTimeout < time.Second || requestTimeout > 2*time.Minute || len(prepared.selected) == 0 || prepared.password == "" {
		return controlPlaneReceipts{}, fmt.Errorf("prepared control-plane execution requires retained intent, selected credentials and bounded controls")
	}
	if err := ctx.Err(); err != nil {
		return controlPlaneReceipts{}, err
	}
	execution := prepared.execution
	managementReceipt, err := runPreparedManagement(ctx, client, execution, journal, interval)
	if err != nil {
		return controlPlaneReceipts{}, err
	}
	runID := "tenant-" + rand.Text()
	observeNetwork := func(ctx context.Context, tenant bootstrap.Tenant) ([]inventory.Check, error) {
		return client.checkTenantNetwork(ctx, tenant, probes, runID, requestTimeout)
	}
	tenantReceipt, err := runTenantStage(ctx, client, execution, managementReceipt, journal, interval, requestTimeout, observeNetwork)
	if err != nil {
		return controlPlaneReceipts{}, err
	}
	enrollmentReceipt, err := runEnrollmentStage(ctx, client, execution, managementReceipt, tenantReceipt, prepared.selected, prepared.password, journal, interval, requestTimeout)
	if err != nil {
		return controlPlaneReceipts{management: managementReceipt, tenant: tenantReceipt, enrollment: enrollmentReceipt}, err
	}
	return controlPlaneReceipts{management: managementReceipt, tenant: tenantReceipt, enrollment: enrollmentReceipt}, nil
}
