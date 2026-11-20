// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementaccess"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// ValidationRequest selects retained desired state and explicit current access.
// Validation does not acquire deployment authority or change the retained bundle.
type ValidationRequest struct {
	Inventory, UserConfig, DeploymentFiles string
	Mode                                   validation.Mode
	Wait, ConnectivityTimeout              time.Duration
	ConnectivityTimeoutSet                 bool
	ReportPath, Version                    string
	Progress                               validation.Progress
}

type validationObserverFactory func(*bundle.Bundle, *generation.Plan, config.Request, []config.AccessIssue, validation.Mode) (validation.Plan, validation.Observer, error)

// Validate prepares one immutable observation plan, evaluates current state and
// publishes a self-contained report, including partial and canceled results.
func Validate(ctx context.Context, request ValidationRequest) validation.Result {
	return validate(ctx, request, deployment.NewValidationObserver)
}

func validate(ctx context.Context, request ValidationRequest, newObserver validationObserverFactory) (result validation.Result) {
	metadata := validation.ReportMetadata{Version: request.Version, GeneratedAt: time.Now()}
	var destination *validation.ReportDestination
	fail := func(subject, summary, action string, cause error) validation.Result {
		category := validation.FatalInput
		if errors.Is(cause, context.Canceled) {
			category = validation.FatalCanceled
		} else if errors.Is(cause, context.DeadlineExceeded) {
			category = validation.FatalTimeout
		}
		failed := validation.PreparationFailure(request.Mode, category, cause)
		failed.Checks[0].Subject = subject
		failed.Checks[0].Outcome.Summary = summary
		failed.Checks[0].Outcome.Action = action
		return failed
	}
	defer func() {
		if destination == nil {
			result.Report = validation.ReportDisposition{Status: validation.ReportUnavailable}
			return
		}
		defer destination.Close()
		disposition, err := destination.Publish(func(w io.Writer) error {
			return validation.RenderHTML(w, result, metadata)
		})
		result.Report = disposition
		if err != nil {
			result.Cause = errors.Join(result.Cause, err)
			result.Checks = append(result.Checks, validation.CheckResult{
				CheckSpec: validation.CheckSpec{ID: "report/publication", Area: validation.AreaPreparation, Subject: "HTML report publication"},
				Outcome: validation.Outcome{ID: "report/publication", State: validation.Unverified, Reason: validation.ReasonUnavailable,
					Summary: "The HTML report could not be published; validation results remain available in the command output.",
					Action:  "Choose a stable writable report location outside the deployment bundle and its input files."},
			})
		}
	}()

	// The private default and disabled destinations need no credential reads.
	// Reserve them early so input failures and cancellation can still be reported.
	if request.ReportPath == "" || request.ReportPath == "-" {
		var err error
		destination, err = validation.PrepareReportDestination(request.DeploymentFiles, request.ReportPath, nil)
		if err != nil {
			return fail("HTML report destination", "The report destination could not be prepared.", "Use an accessible deployment bundle parent, an explicit report path, or --report-path - to disable HTML.", err)
		}
	}
	if ctx == nil || newObserver == nil || request.Inventory == "" || request.DeploymentFiles == "" {
		return fail("Validation inputs", "Explicit inventory, retained deployment bundle and validation context are required.", "Supply --inventory dpf-discovered.yaml and --deployment-files ./hbn-bgp.", fmt.Errorf("incomplete validation request"))
	}
	if err := ctx.Err(); err != nil {
		return fail("Validation preparation", "Validation stopped before reading its inputs.", "Run validation again when ready.", err)
	}
	if request.Mode != validation.ModeFull && request.Mode != validation.ModeResources || request.Wait < 0 {
		return fail("Validation options", "The validation mode or wait duration is invalid.", "Use full validation or --connectivity=false, and a nonnegative --wait duration.", fmt.Errorf("invalid validation options"))
	}
	if _, err := validation.RuntimeBudget(1, request.ConnectivityTimeout, request.ConnectivityTimeoutSet, request.Mode); err != nil {
		return fail("Validation options", "The connectivity timeout is invalid for the selected mode.", "Use a positive --connectivity-timeout only with full validation, or omit it for an automatic budget.", err)
	}
	progress := ui.FromContext(ctx)
	progress.Info("[RUNNING] Validation preparation: reading inventory and retained deployment settings")
	input, err := config.LoadInputs(request.Inventory, request.UserConfig)
	if err != nil {
		return fail("Inventory and overrides", "The inventory or optional overrides could not be read as valid DPF inputs.", "Check the input paths and YAML fields; use discovery output and only supported overrides.", err)
	}
	if destination == nil {
		protected, err := validationProtectedFiles(input)
		if err != nil {
			return fail("HTML report destination", "The explicit report path cannot be checked against every input and access file.", "Restore readable inventory and management kubeconfig inputs, or use the private default report location or --report-path -.", err)
		}
		destination, err = validation.PrepareReportDestination(request.DeploymentFiles, request.ReportPath, protected)
		if err != nil {
			return fail("HTML report destination", "The explicit report destination is unavailable or could overwrite a protected input.", "Choose a regular file in an existing directory outside the deployment bundle and all inventory, credential and trust files.", err)
		}
	}
	retained, err := bundle.Load(request.DeploymentFiles)
	if err != nil {
		return fail("Retained deployment bundle", "The generated deployment bundle could not be read or its integrity checks failed.", "Supply the complete unchanged directory produced by l8k generate for this deployment.", err)
	}
	plan, err := generation.LoadPlan(retained)
	if err != nil {
		return fail("Retained deployment plan", "The bundle does not contain a supported complete deployment plan.", "Use the complete generated HBN-BGP bundle for this deployment.", err)
	}
	metadata.Release, metadata.UseCase = plan.Intent().Release, retained.Index().Profile
	input, err = WithRetainedConfiguration(input, plan)
	if err != nil {
		return fail("Retained configuration", "The current inventory or overrides cannot resolve the retained deployment settings.", "Use this deployment's inventory and matching overrides; validation does not change desired settings.", err)
	}
	input, err = WithRetainedUseCase(input, retained)
	if err != nil {
		return fail("Retained use case", "The current inputs do not match the bundle's supported HBN-BGP selection.", "Use the inventory and overrides associated with this deployment.", err)
	}
	input, err = WithRetainedProvisioning(input, plan)
	if err != nil {
		return fail("Retained provisioning settings", "The retained provisioning settings cannot be resolved from these inputs.", "Use the complete unchanged generated bundle and its matching inventory.", err)
	}
	if err := VerifyDeploymentPlan(input, retained, plan); err != nil {
		return fail("Retained configuration", "Current inputs change the retained desired resources or physical selection.", "Use the matching inventory and overrides, or generate and deploy the intended change before validating it.", err)
	}
	if err := ctx.Err(); err != nil {
		return fail("Validation preparation", "Validation stopped before opening observation sources.", "Run validation again when ready.", err)
	}
	access, unavailable, err := config.ResolveForValidation(input)
	if err != nil {
		return fail("Explicit access configuration", "The explicit management, BMC, host or switch access declarations are invalid.", "Check credential references, endpoint declarations and trust policy in the inventory.", err)
	}
	compiled, observer, err := newObserver(retained, plan, access, unavailable, request.Mode)
	if err != nil {
		return fail("Observation plan", "The current inputs cannot safely identify the retained deployment and its observation scope.", "Check retained identities and endpoint trust settings; restore the matching inventory before validating.", err)
	}
	runner, err := validation.NewRunner(compiled, observer, validation.Options{
		Wait: request.Wait, ConnectivityTimeout: request.ConnectivityTimeout, ConnectivityTimeoutSet: request.ConnectivityTimeoutSet, Progress: request.Progress,
	})
	if err != nil {
		return fail("Validation options", "The selected validation scope or timeout cannot cover the required checks.", "Omit --connectivity-timeout for an automatic budget, or increase it to cover every selected host pair.", err)
	}
	budget, _ := validation.RuntimeBudget(len(compiled.RuntimeScope().Hosts), request.ConnectivityTimeout, request.ConnectivityTimeoutSet, request.Mode)
	progress.Info("DPF %s · %s · %s validation", metadata.Release, metadata.UseCase, request.Mode)
	if request.Mode == validation.ModeFull {
		progress.Info("Runtime checks have a total budget of %s; resource reconciliation wait is %s.", budget, request.Wait)
	} else {
		progress.Info("Resource-only scope excludes physical, switch, BGP, route and traffic checks.")
	}
	// Native collectors also serve discovery, whose optional-detail messages
	// do not describe this validation report. Keep the captured typed validation
	// progress callback, and silence only their inherited informational UI.
	return runner.Run(ui.WithOutput(ctx, ui.NewSilent()))
}

func validationProtectedFiles(input config.Input) ([]string, error) {
	files, err := config.ReferencedFiles(input)
	if err != nil {
		return nil, err
	}
	kubeconfig := input.Site.Management.KubeconfigRef.File
	if kubeconfig == "" {
		return files, nil
	}
	if !filepath.IsAbs(kubeconfig) {
		kubeconfig = filepath.Join(filepath.Dir(input.Path), kubeconfig)
	}
	references, err := managementaccess.ReferencedFiles(kubeconfig)
	if err != nil {
		return nil, err
	}
	return append(files, references...), nil
}
