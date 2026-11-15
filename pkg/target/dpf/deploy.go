// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifactcheck"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifacts"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/catalog"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/switchprobe"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// DeployRequest selects an already generated DPF bundle and explicit site
// access. It has no Host-target kubeconfig or overwrite fallback.
type DeployRequest struct {
	UserConfig      string
	DeploymentFiles string
	Stdout          io.Writer
	Stderr          io.Writer
}

type deployDriver struct{ request DeployRequest }

func NewDeployAdapter(request DeployRequest) target.Driver { return deployDriver{request: request} }
func (d deployDriver) Descriptor() target.Descriptor       { return Descriptor() }

func (d deployDriver) Bind(inv target.Invocation) (target.Operation, error) {
	if inv.Phase != target.Deploy {
		return nil, &target.PhaseUnavailableError{Name: target.DPF, Phase: inv.Phase, Reason: Descriptor().Capability(inv.Phase).Reason}
	}
	if inv.Execution.DryRun {
		return nil, fmt.Errorf("DPF deploy does not support --dry-run; inspect the generated bundle and run discovery before deployment")
	}
	if d.request.UserConfig == "" || d.request.DeploymentFiles == "" {
		return nil, fmt.Errorf("DPF deploy requires --user-config and --deployment-files")
	}
	if inv.Execution.Timeout < 0 || inv.Execution.Timeout > 2*time.Hour {
		return nil, fmt.Errorf("DPF deployment timeout must be between zero (the two-hour default) and two hours")
	}
	r := d.request
	if r.Stdout == nil {
		r.Stdout = os.Stdout
	}
	if r.Stderr == nil {
		r.Stderr = os.Stderr
	}
	return target.OperationFunc(func(parent context.Context) error {
		ctx, stop := signal.NotifyContext(parent, terminationSignals()...)
		defer stop()
		timeout := inv.Execution.Timeout
		if timeout == 0 {
			timeout = 2 * time.Hour
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		progress := ui.NewSilent()
		if !inv.Output.Quiet {
			progress = ui.NewWithWriter(r.Stderr)
		}
		ctx = ui.WithOutput(ctx, progress)
		progress.Info("[RUNNING] Deployment: validating the retained bundle and site access")
		retained, err := bundle.Load(r.DeploymentFiles)
		if err != nil {
			return apperrors.NewValidationError("invalid DPF deployment bundle", err, "Regenerate the complete bundle and retain its metadata")
		}
		input, err := config.Load(r.UserConfig)
		if err != nil {
			return apperrors.NewValidationError("invalid DPF deployment configuration", err, "Supply the discovered site configuration")
		}
		access, err := config.Resolve(input)
		if err != nil {
			return apperrors.NewValidationError("invalid DPF deployment access", err, "Correct the site configuration and access references")
		}
		if err := validateManagement(access); err != nil {
			return err
		}
		rebooter, err := NewExternalRebooter(access, access.RequestTimeout, 15*time.Minute, 5*time.Second)
		if err != nil {
			return err
		}
		root, err := filepath.Abs(r.DeploymentFiles)
		if err != nil {
			return err
		}
		runDir, err := os.MkdirTemp(filepath.Dir(root), "l8k-dpf-deploy-")
		if err != nil {
			return fmt.Errorf("create DPF deployment report directory: %w", err)
		}
		progress.Info("[RUNNING] Deployment records: %s", runDir)
		result, runErr := deployment.Run(ctx, deployment.RunRequest{
			Bundle: retained, Access: access,
			ReportPath: filepath.Join(runDir, "deployment.jsonl"), ProbeJournalPath: filepath.Join(runDir, "probe-resources.json"),
			VerifyPrerequisites: freshDeploymentPrerequisites(retained, runDir), VerifyChartSource: verifyDeploymentChart,
			Rebooter:     rebooter,
			PollInterval: 5 * time.Second, RequestTimeout: access.RequestTimeout,
		})
		return writeDeploymentResult(r, inv.Output.Format, result, runErr)
	}), nil
}

func writeDeploymentResult(request DeployRequest, format string, result deployment.RunResult, runErr error) error {
	var failure *apperrors.StructuredError
	if runErr != nil {
		if result.State == "" {
			result.State = deployment.RunStateFailed
		}
		failure = apperrors.NewDeploymentError("DPF deployment stopped", runErr, "Inspect the saved report and current resource state before retrying")
		if result.ReportPath == "" {
			failure.Message = "DPF deployment stopped before a report could be created"
			failure.Suggestion = "Review the site, bundle and output permissions before retrying"
		}
	}
	if format == "json" {
		outcome := struct {
			deployment.RunResult
			Error *apperrors.StructuredError `json:"error,omitempty"`
		}{RunResult: result, Error: failure}
		if err := json.NewEncoder(request.Stdout).Encode(outcome); err != nil {
			fmt.Fprintln(request.Stderr, "cannot write complete DPF deployment result")
			return apperrors.MarkReported(apperrors.NewGeneralError("cannot write complete DPF deployment result", err))
		}
		if failure != nil {
			return apperrors.MarkReported(failure)
		}
		return nil
	}
	stage, report := result.LastCompletedStage, result.ReportPath
	if stage == "" {
		stage = "none"
	}
	if report == "" {
		report = "unavailable"
	}
	if _, err := fmt.Fprintf(request.Stdout, "DPF deployment: %s\nLast completed stage: %s\nReport: %s\n", result.State, stage, report); err != nil {
		return fmt.Errorf("write DPF deployment result: %w", err)
	}
	if failure != nil {
		return failure
	}
	return nil
}

func freshDeploymentPrerequisites(retained *bundle.Bundle, runDir string) deployment.PrerequisiteVerifier {
	return func(ctx context.Context, request config.Request, plan *generation.Plan) (deployment.PrerequisiteVerification, error) {
		store, err := artifacts.New(request.Input, filepath.Join(runDir, "discovered.yaml"), true)
		if err != nil {
			return deployment.PrerequisiteVerification{}, err
		}
		deps := discovery.Dependencies{
			Switches: switchprobe.Collector{}, SSH: sshprobe.Collector{}, HostLLDP: sshprobe.Collector{},
			BMC:            redfish.Collector{Qualifications: catalog.Hardware(), IncludeNetworkFunctionMACs: true, ProvisioningRelease: plan.Intent().Release},
			Probes:         probe.Runner{Assets: catalog.ProbeAssets(), Journal: probe.FileJournal(filepath.Join(store.RunDir, "resources.json"))},
			HBNPermissions: newHBNPermissionObserver().observe, Artifacts: artifactcheck.Verifier{}, HostNetworking: sshprobe.ObserveHostWorkloadNetwork,
		}
		report := discovery.Collect(ctx, request, store.RunID(), deps)
		publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err = store.Publish(publishCtx, &report)
		cancel()
		if err != nil {
			return deployment.PrerequisiteVerification{}, fmt.Errorf("save fresh deployment discovery: %w", err)
		}
		if ctx.Err() != nil {
			return deployment.PrerequisiteVerification{}, ctx.Err()
		}
		if report.Execution != "completed" || report.Discovery.Outcome != "complete" || len(report.Errors) > 0 || report.Readiness == nil {
			return deployment.PrerequisiteVerification{}, fmt.Errorf("fresh deployment discovery is incomplete; inspect %s", store.RunDir)
		}
		if report.Readiness.State != "ready" {
			return deployment.PrerequisiteVerification{Inventory: report.Inventory, Checks: report.Readiness.Checks}, fmt.Errorf("fresh HBN-BGP prerequisites are blocked; inspect %s", store.RunDir)
		}
		current := request.Input
		current.Site.Inventory = report.Inventory
		if err := verifyDeploymentPlan(current, retained, plan); err != nil {
			return deployment.PrerequisiteVerification{}, err
		}
		return deployment.PrerequisiteVerification{Inventory: report.Inventory, Checks: report.Readiness.Checks}, nil
	}
}

func verifyDeploymentChart(ctx context.Context, plan *generation.Plan) (deployment.PreflightCheck, error) {
	check := deployment.PreflightCheck{ID: "hbn-chart-source-matches-archive", State: "blocked", ReasonCode: "HBN_CHART_SOURCE_UNVERIFIED", Message: "Current HBN chart source bytes have not been verified."}
	if plan == nil {
		return check, fmt.Errorf("retained deployment plan is required")
	}
	selected := plan.Intent().Generation
	data, err := downloadGenerationChart(ctx, strings.TrimSuffix(selected.ChartRepository, "/")+"/charts/"+selected.ChartName+"-"+selected.ChartVersion+".tgz")
	if err != nil {
		return check, err
	}
	if !bytes.Equal(data, plan.HBNChart()) {
		return check, fmt.Errorf("current HBN chart source differs from the retained archive")
	}
	check.State, check.ReasonCode, check.Message = "passed", "HBN_CHART_SOURCE_VERIFIED", "Current HBN chart source bytes match the retained archive."
	return check, nil
}
