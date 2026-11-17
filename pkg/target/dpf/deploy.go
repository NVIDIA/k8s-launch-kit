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

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/app"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// DeployRequest selects an already generated DPF bundle and explicit site
// access. It has no Host-target kubeconfig or overwrite fallback.
type DeployRequest struct {
	Inventory string
	Resume    string
	ResumeSet bool

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
	if d.request.Inventory == "" || d.request.DeploymentFiles == "" {
		return nil, fmt.Errorf("DPF deploy requires --inventory dpf-discovered.yaml and --deployment-files ./hbn-bgp; --user-config is optional")
	}
	if inv.Execution.Timeout < 0 || inv.Execution.Timeout > hbn.MaxNativeDeploymentDuration {
		return nil, fmt.Errorf("DPF deployment timeout must be between zero (the two-hour default) and two hours")
	}
	var resume *deployment.OperationRef
	if d.request.ResumeSet || d.request.Resume != "" {
		var err error
		resume, err = deployment.ParseOperationRef(d.request.Resume)
		if err != nil {
			return nil, err
		}
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
			timeout = hbn.MaxNativeDeploymentDuration
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
		plan, err := generation.LoadPlan(retained)
		if err != nil {
			return apperrors.NewValidationError("invalid retained DPF deployment plan", err, "Regenerate and review the bundle before opening access")
		}
		input, err := config.LoadInputs(r.Inventory, r.UserConfig)
		if err != nil {
			return apperrors.NewValidationError("invalid DPF deployment configuration", err, "Supply the discovery output with --inventory dpf-discovered.yaml")
		}
		input, err = app.WithRetainedConfiguration(input, plan)
		if err != nil {
			return apperrors.NewValidationError("cannot restore DPF deployment settings", err, "Regenerate and review the bundle")
		}
		input, err = app.WithRetainedUseCase(input, retained)
		if err != nil {
			return apperrors.NewValidationError("DPF deployment selection conflicts with the bundle", err, "Use the generated selection or regenerate and review the bundle")
		}
		input, err = app.WithRetainedProvisioning(input, plan)
		if err != nil {
			return apperrors.NewValidationError("cannot resolve retained DPF provisioning", err, "Use the validated bundle's supported recipe")
		}
		access, err := config.Resolve(input)
		if err != nil {
			return apperrors.NewValidationError("invalid DPF deployment access", err, "Correct the site configuration and access references")
		}
		if err := validateManagement(access); err != nil {
			return err
		}
		rebooter, err := NewExternalRebooter(access, access.RequestTimeout, maxExternalRebootWait, 5*time.Second)
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
			Bundle: retained, Access: access, Resume: resume,
			ReportPath: filepath.Join(runDir, "deployment.jsonl"), ProbeJournalPath: filepath.Join(runDir, "probe-resources.json"),
			VerifyPrerequisites: app.DeploymentPrerequisites(retained, runDir, resume != nil), VerifyChartSource: verifyDeploymentChart,
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
		// A stopped DPF run can have partial effects that require inspection
		// before a caller decides whether an explicit resume is safe.
		failure.Transient = false
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
	if result.Operation != nil {
		if _, err := fmt.Fprintf(request.Stdout, "Operation: %s\n", result.Operation.String()); err != nil {
			return err
		}
	}
	if failure != nil {
		return failure
	}
	return nil
}

func verifyDeploymentChart(ctx context.Context, plan *generation.Plan) (deployment.PreflightCheck, error) {
	check := deployment.PreflightCheck{ID: "hbn-chart-source-matches-archive", State: "blocked", ReasonCode: "HBN_CHART_SOURCE_UNVERIFIED", Message: "Current HBN chart source bytes have not been verified."}
	if plan == nil {
		return check, fmt.Errorf("retained deployment plan is required")
	}
	selected := plan.Intent().Generation
	data, err := app.DownloadChart(ctx, strings.TrimSuffix(selected.ChartRepository, "/")+"/charts/"+selected.ChartName+"-"+selected.ChartVersion+".tgz")
	if err != nil {
		return check, err
	}
	if !bytes.Equal(data, plan.HBNChart()) {
		return check, fmt.Errorf("current HBN chart source differs from the retained archive")
	}
	check.State, check.ReasonCode, check.Message = "passed", "HBN_CHART_SOURCE_VERIFIED", "Current HBN chart source bytes match the retained archive."
	return check, nil
}
