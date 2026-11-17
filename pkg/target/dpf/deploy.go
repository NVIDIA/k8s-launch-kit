// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/app"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
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
		result, err := app.Deploy(ctx, app.DeploymentRequest{
			Inventory: r.Inventory, UserConfig: r.UserConfig, DeploymentFiles: r.DeploymentFiles, Resume: resume,
		})
		if result == nil {
			return preparationError(err)
		}
		return writeDeploymentResult(r, inv.Output.Format, *result, err)
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
