// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
)

// These partial workflow compositions support stage tests. They are not native
// entrypoints and do not establish end-to-end Run or adapter coverage.

// runManagement validates the complete retained bundle before any management
// API call, then executes only the management bootstrap portion of its fixed
// workflow. Later tenant, enrollment, workload and DPUDeployment stages remain
// unexecuted and are not reported as ready.
func runManagement(ctx context.Context, client managementRunnerClient, retained *bundle.Bundle,
	journal *executionJournal, pollInterval time.Duration,
) (managementBootstrapReceipt, error) {
	if ctx == nil || client == nil || journal == nil || pollInterval <= 0 {
		return managementBootstrapReceipt{}, fmt.Errorf("management runner requires context, client, report and positive polling interval")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return managementBootstrapReceipt{}, fmt.Errorf("management runner requires an execution deadline")
	}
	if err := ctx.Err(); err != nil {
		return managementBootstrapReceipt{}, err
	}
	execution, err := loadManagementExecution(retained, time.Until(deadline(ctx)))
	if err != nil {
		return managementBootstrapReceipt{}, err
	}
	return runPreparedManagement(ctx, client, execution, journal, pollInterval)
}

// runControlPlane executes the retained management, tenant and selected-device
// enrollment stages using one prepared plan. Its receipt does not establish
// workload or DPUDeployment readiness.
func runControlPlane(ctx context.Context, client controlPlaneTestClient, retained *bundle.Bundle, journal *executionJournal,
	probes probe.Runner, request config.Request, interval, requestTimeout time.Duration,
) (enrollmentStageReceipt, error) {
	prepared, err := prepareControlPlaneRun(ctx, client, retained, journal, probes, request, interval, requestTimeout)
	if err != nil {
		return enrollmentStageReceipt{}, err
	}
	receipts, err := runPreparedControlPlane(ctx, client, client, client, client, prepared, probes, journal, interval, requestTimeout)
	return receipts.enrollment, err
}

func prepareControlPlaneRun(ctx context.Context, client controlPlaneTestClient, retained *bundle.Bundle, journal *executionJournal,
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
	execution, err := loadManagementExecution(retained, time.Until(deadline(ctx)))
	if err != nil {
		return nil, err
	}
	return prepareControlPlaneWithExecution(ctx, client, execution, journal, request, interval, requestTimeout)
}

// softwarePreparationReceipt reports only staged cache verification. It does
// not report workload or DPUDeployment readiness.
type softwarePreparationReceipt struct {
	BundleDigest string
	ControlPlane controlPlaneReceipts
	Software     softwareVerificationReceipt
}

// runSoftwarePreparation owns the retained execution across control-plane and
// software staging. It stops after verifying selected software bytes; it does
// not create the remaining workload resources or DPUDeployment.
func runSoftwarePreparation(ctx context.Context, client workloadTestClient, retained *bundle.Bundle,
	journal *executionJournal, probes probe.Runner, request config.Request, interval, requestTimeout time.Duration, maxBytes int64,
) (softwarePreparationReceipt, error) {
	if ctx == nil || client == nil || journal == nil || interval <= 0 || requestTimeout < time.Second || requestTimeout > 2*time.Minute || maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return softwarePreparationReceipt{}, fmt.Errorf("software preparation requires a bounded context, management client and durable report")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return softwarePreparationReceipt{}, fmt.Errorf("software preparation requires an execution deadline")
	}
	prepared, err := prepareControlPlaneRun(ctx, client, retained, journal, probes, request, interval, requestTimeout)
	if err != nil {
		return softwarePreparationReceipt{}, err
	}
	receipts, err := runPreparedControlPlane(ctx, client, client, client, client, prepared, probes, journal, interval, requestTimeout)
	if err != nil {
		return softwarePreparationReceipt{BundleDigest: prepared.execution.plan.Digest(), ControlPlane: receipts}, err
	}
	software, err := runSoftwareStage(ctx, client, prepared.execution.verificationInputs(), receipts, prepared.selected, prepared.password, journal,
		interval, requestTimeout, maxBytes)
	return softwarePreparationReceipt{BundleDigest: prepared.execution.plan.Digest(), ControlPlane: receipts, Software: software}, err
}

// runWorkloadPreparation validates one retained execution, checks the chart
// source and all required workload permissions before effects, then carries the
// same execution through management, tenant, enrollment, cached-software and
// workload-prerequisite stages. It deliberately stops before DPUDeployment.
func runWorkloadPreparation(ctx context.Context, client workloadTestClient, retained *bundle.Bundle,
	journal *executionJournal, probes probe.Runner, request config.Request, interval, requestTimeout time.Duration,
	maxBytes int64, verifyChartSource workloadChartSourceVerifier,
) (workloadPreparationReceipt, error) {
	if ctx == nil || client == nil || journal == nil || verifyChartSource == nil || interval <= 0 ||
		requestTimeout < time.Second || requestTimeout > 2*time.Minute || maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return workloadPreparationReceipt{}, fmt.Errorf("workload preparation requires a bound client, retained plan, chart verifier, durable report and bounded controls")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return workloadPreparationReceipt{}, fmt.Errorf("workload preparation requires an execution deadline")
	}
	prepared, err := prepareControlPlaneRun(ctx, client, retained, journal, probes, request, interval, requestTimeout)
	if err != nil {
		return workloadPreparationReceipt{}, err
	}
	objects, err := checkWorkloadPreparationInputs(ctx, client, prepared, journal, verifyChartSource)
	if err != nil {
		return workloadPreparationReceipt{}, err
	}
	execution := prepared.execution
	controlPlane, err := runPreparedControlPlane(ctx, client, client, client, client, prepared, probes, journal, interval, requestTimeout)
	if err != nil {
		return workloadPreparationReceipt{BundleDigest: execution.plan.Digest(), ControlPlane: controlPlane}, err
	}
	return runPreparedWorkload(ctx, client, client, prepared, controlPlane, journal, interval, requestTimeout, maxBytes, verifyChartSource, objects)
}

type controlPlaneTestClient interface {
	managementRunnerClient
	tenantStageManagementClient
	enrollmentManagementClient
	tenantNetworkChecker
}
type workloadTestClient interface {
	controlPlaneTestClient
	softwareStageManagementClient
	workloadStageClient
	workloadPermissionChecker
}

// loadManagementExecution exercises the untrusted-bundle constructor for stage
// tests. Run callers instead supply a Plan explicitly and get fresh execution.
func loadManagementExecution(retained *bundle.Bundle, timeout time.Duration) (*managementExecution, error) {
	plan, err := generation.LoadPlan(retained)
	if err != nil {
		return nil, fmt.Errorf("validate retained DPF plan before management access: %w", err)
	}
	return prepareManagementExecution(plan, timeout)
}
