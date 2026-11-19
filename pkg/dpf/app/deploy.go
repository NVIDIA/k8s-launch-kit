// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"fmt"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/chartfetch"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// DeploymentRequest selects retained desired artifacts and current explicit access.
type DeploymentRequest struct {
	Inventory       string
	UserConfig      string
	DeploymentFiles string
	Resume          *deployment.OperationRef
}

// Deploy prepares the native execution service. A nil result means preparation
// failed before deployment.Run; it must not be presented as an execution result.
// The deployment package retains all lifecycle ordering and effect fences.
func Deploy(ctx context.Context, r DeploymentRequest) (*deployment.RunResult, error) {
	progress := ui.FromContext(ctx)
	progress.Info("[RUNNING] Deployment: validating the retained bundle and site access")
	retained, err := bundle.Load(r.DeploymentFiles)
	if err != nil {
		return nil, &PreparationError{Stage: DeploymentBundle, Err: err}
	}
	plan, err := generation.LoadPlan(retained)
	if err != nil {
		return nil, &PreparationError{Stage: DeploymentPlan, Err: err}
	}
	input, err := config.LoadInputs(r.Inventory, r.UserConfig)
	if err != nil {
		return nil, &PreparationError{Stage: DeploymentConfiguration, Err: err}
	}
	input, err = WithRetainedConfiguration(input, plan)
	if err != nil {
		return nil, &PreparationError{Stage: DeploymentSettings, Err: err}
	}
	input, err = WithRetainedUseCase(input, retained)
	if err != nil {
		return nil, &PreparationError{Stage: DeploymentSelection, Err: err}
	}
	input, err = WithRetainedProvisioning(input, plan)
	if err != nil {
		return nil, &PreparationError{Stage: DeploymentProvisioning, Err: err}
	}
	access, err := config.Resolve(input)
	if err != nil {
		return nil, &PreparationError{Stage: DeploymentAccess, Err: err}
	}
	if err := validateManagement(access); err != nil {
		return nil, err
	}
	rebooter, err := NewExternalRebooter(access, access.RequestTimeout, maxExternalRebootWait, 5*time.Second)
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(r.DeploymentFiles)
	if err != nil {
		return nil, err
	}
	runDir, err := os.MkdirTemp(filepath.Dir(root), "l8k-dpf-deploy-")
	if err != nil {
		return nil, fmt.Errorf("create DPF deployment report directory: %w", err)
	}
	progress.Info("[RUNNING] Deployment records: %s", runDir)
	result, runErr := deployment.Run(ctx, deployment.RunRequest{
		Bundle: retained, Access: access, Resume: r.Resume,
		ReportPath: filepath.Join(runDir, "deployment.jsonl"), ProbeJournalPath: filepath.Join(runDir, "probe-resources.json"),
		VerifyPrerequisites: DeploymentPrerequisites(retained, runDir, r.Resume != nil), VerifyChartSource: verifyDeploymentChart,
		Rebooter:     rebooter,
		PollInterval: 5 * time.Second, RequestTimeout: access.RequestTimeout,
	})
	return &result, runErr
}

func verifyDeploymentChart(ctx context.Context, plan *generation.Plan) (deployment.PreflightCheck, error) {
	check := deployment.PreflightCheck{ID: "hbn-chart-source-matches-archive", State: "blocked", ReasonCode: "HBN_CHART_SOURCE_UNVERIFIED", Message: "Current HBN chart source bytes have not been verified."}
	if plan == nil {
		return check, fmt.Errorf("retained deployment plan is required")
	}
	selected := plan.Intent().Generation
	data, err := (chartfetch.Downloader{}).Fetch(ctx, strings.TrimSuffix(selected.ChartRepository, "/")+"/charts/"+selected.ChartName+"-"+selected.ChartVersion+".tgz")
	if err != nil {
		return check, err
	}
	if !bytes.Equal(data, plan.HBNChart()) {
		return check, fmt.Errorf("current HBN chart source differs from the retained archive")
	}
	check.State, check.ReasonCode, check.Message = "passed", "HBN_CHART_SOURCE_VERIFIED", "Current HBN chart source bytes match the retained archive."
	return check, nil
}
