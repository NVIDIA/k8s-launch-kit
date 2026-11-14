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
	if ctx == nil || client == nil || journal == nil || interval <= 0 || requestTimeout < time.Second || requestTimeout > 2*time.Minute {
		return enrollmentStageReceipt{}, fmt.Errorf("control-plane execution requires a client, report and bounded polling/request controls")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return enrollmentStageReceipt{}, fmt.Errorf("control-plane execution requires a deadline")
	}
	if err := ctx.Err(); err != nil {
		return enrollmentStageReceipt{}, err
	}
	if err := probes.Assets.Validate(); err != nil {
		return enrollmentStageReceipt{}, err
	}
	if probes.Journal == nil {
		return enrollmentStageReceipt{}, fmt.Errorf("control-plane execution requires a durable probe resource journal")
	}
	execution, err := prepareManagementExecution(retained, time.Until(deadline(ctx)))
	if err != nil {
		return enrollmentStageReceipt{}, err
	}
	if journal.digest != execution.plan.Digest() {
		return enrollmentStageReceipt{}, fmt.Errorf("control-plane report must identify the retained bundle")
	}
	intent := execution.plan.Intent()
	if !client.matchesBinding(intent.Management, intent.KubernetesVersion) {
		return enrollmentStageReceipt{}, fmt.Errorf("management client binding differs from the retained plan")
	}
	selected, password, err := bindEnrollmentBMCs(intent.Devices, request.Targets)
	if err != nil {
		return enrollmentStageReceipt{}, err
	}
	if _, err := selectedEnrollmentDiscoveries(execution, selected); err != nil {
		return enrollmentStageReceipt{}, err
	}
	if err := journalTenantChecks(journal, "site-allocation", managementnetwork.CheckTenantAllocation(intent.Bootstrap.Tenant)); err != nil {
		return enrollmentStageReceipt{}, err
	}
	// DPU controllers may act as soon as the operator and configuration start.
	// Verify physical BMC identity and the complete existing enrollment scope
	// before either management effect can activate unrelated devices.
	if err := client.verifyEnrollmentBMCs(ctx, selected, requestTimeout, journal); err != nil {
		return enrollmentStageReceipt{}, err
	}
	if err := guardEnrollmentScope(ctx, client, execution, selected); err != nil {
		return enrollmentStageReceipt{}, enrollmentStageFailure(journal, "ENROLLMENT_SCOPE_UNSAFE", err.Error(), nil)
	}
	managementReceipt, err := runPreparedManagement(ctx, client, execution, journal, interval)
	if err != nil {
		return enrollmentStageReceipt{}, err
	}
	runID := "tenant-" + rand.Text()
	observeNetwork := func(ctx context.Context, tenant bootstrap.Tenant) ([]inventory.Check, error) {
		return client.checkTenantNetwork(ctx, tenant, probes, runID, requestTimeout)
	}
	tenantReceipt, err := runTenantStage(ctx, client, execution, managementReceipt, journal, interval, requestTimeout, observeNetwork)
	if err != nil {
		return enrollmentStageReceipt{}, err
	}
	return runEnrollmentStage(ctx, client, execution, managementReceipt, tenantReceipt, selected, password, journal, interval, requestTimeout)
}
