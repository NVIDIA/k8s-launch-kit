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
	"strings"
	"sync"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/app"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// ValidateRequest selects retained DPF inputs and the terminal output streams.
type ValidateRequest struct {
	app.ValidationRequest
	Stdout io.Writer
	Stderr io.Writer
}

// ValidateRunner executes standalone validation without lifecycle authority.
type ValidateRunner func(context.Context, app.ValidationRequest) validation.Result

func NewValidateRunner() ValidateRunner { return app.Validate }

type validateDriver struct {
	request ValidateRequest
	runner  ValidateRunner
}

func NewValidateAdapter(request ValidateRequest, runner ValidateRunner) target.Driver {
	return validateDriver{request: request, runner: runner}
}

func (d validateDriver) Descriptor() target.Descriptor { return Descriptor() }

func (d validateDriver) Bind(inv target.Invocation) (target.Operation, error) {
	if inv.Phase != target.Validate {
		return nil, &target.PhaseUnavailableError{Name: target.DPF, Phase: inv.Phase, Reason: Descriptor().Capability(inv.Phase).Reason}
	}
	if d.runner == nil {
		return nil, fmt.Errorf("DPF validation runner is required")
	}
	r := d.request
	if strings.TrimSpace(r.Inventory) == "" || strings.TrimSpace(r.DeploymentFiles) == "" {
		return nil, fmt.Errorf("DPF validate requires --inventory dpf-discovered.yaml and --deployment-files ./hbn-bgp; --user-config is optional")
	}
	if inv.Execution.DryRun {
		return nil, fmt.Errorf("DPF validate does not support --dry-run; validation performs observations and bounded diagnostic probes")
	}
	if r.Mode == "" {
		r.Mode = validation.ModeFull
	}
	if r.Mode != validation.ModeFull && r.Mode != validation.ModeResources {
		return nil, fmt.Errorf("DPF validation mode must be full or resources")
	}
	if r.Wait < 0 || r.ConnectivityTimeout < 0 {
		return nil, fmt.Errorf("--wait and --connectivity-timeout must be greater than or equal to zero")
	}
	if r.Mode == validation.ModeResources && r.ConnectivityTimeoutSet {
		return nil, fmt.Errorf("--connectivity-timeout cannot be used with --connectivity=false, including zero")
	}
	if r.Stdout == nil {
		r.Stdout = os.Stdout
	}
	if r.Stderr == nil {
		r.Stderr = os.Stderr
	}
	return target.OperationFunc(func(parent context.Context) error {
		ctx, stop := signal.NotifyContext(parent, terminationSignals()...)
		defer stop()
		output := ui.NewSilent()
		if !inv.Output.Quiet {
			output = ui.NewWithWriter(r.Stderr)
		}
		ctx = ui.WithOutput(ctx, output)
		output.Info("DPF validation: %s", validationModeLabel(r.Mode))
		request := r.ValidationRequest
		request.Progress = validationProgress(output)
		result := d.runner(ctx, request)
		if err := writeValidationResult(r.Stdout, inv.Output.Format, result); err != nil {
			result.OutputFailed = true
			// Neither transport errors nor writer errors are safe presentation text.
			_, _ = fmt.Fprintln(r.Stderr, "Cannot write complete DPF validation result. Inspect the HTML report if it was published.")
		}
		return apperrors.NewExitStatus(result.ExitCode())
	}), nil
}

func validationModeLabel(mode validation.Mode) string {
	if mode == validation.ModeResources {
		return "resource checks only (physical, host network, switch, BGP, route and traffic probes excluded)"
	}
	return "full resource and runtime checks"
}

func validationProgress(output ui.Output) validation.Progress {
	var mu sync.Mutex
	groups := map[string]bool{}
	return func(event validation.ProgressEvent) {
		mu.Lock()
		defer mu.Unlock()
		group := validationCheckGroup(event.Check)
		if !groups[group] {
			groups[group] = true
			output.Info("Checking %s", group)
		}
		state := "RUNNING"
		if event.Phase == validation.Completed {
			state = validationStateLabel(event.Outcome.State)
		}
		output.Info("%s: %s", state, event.Check.Subject)
	}
}

func validationCheckGroup(check validation.CheckSpec) string {
	if strings.HasPrefix(string(check.ID), "runtime/") {
		return "runtime connectivity"
	}
	switch check.Area {
	case validation.AreaPreparation:
		return "validation inputs"
	case validation.AreaManagement:
		return "management cluster"
	case validation.AreaOperator, validation.AreaDefinitions:
		return "DPF Operator and resource definitions"
	case validation.AreaSystem:
		return "system components and dependencies"
	case validation.AreaTenant:
		return "tenant cluster"
	case validation.AreaDevices, validation.AreaSoftware:
		return "selected DPUs and software"
	case validation.AreaHBN, validation.AreaWorkloads:
		return "HBN resources and workloads"
	case validation.AreaPhysical:
		return "physical topology and host networking"
	default:
		return "deployment state"
	}
}

func validationStateLabel(state validation.State) string {
	switch state {
	case validation.Passed:
		return "PASS"
	case validation.Failed:
		return "FAIL"
	case validation.Pending:
		return "PENDING"
	case validation.Skipped:
		return "SKIPPED"
	default:
		return "UNVERIFIED"
	}
}

func writeValidationResult(w io.Writer, format string, result validation.Result) error {
	var data []byte
	if format == "json" {
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		data = append(encoded, '\n')
	} else {
		data = validationText(result)
	}
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

func validationText(result validation.Result) []byte {
	var b bytes.Buffer
	verdict := "VALIDATION INCOMPLETE"
	switch result.Verdict {
	case validation.VerdictPassed:
		verdict = "VALIDATION PASSED"
	case validation.VerdictResourcesPassed:
		verdict = "RESOURCE CHECKS PASSED"
	case validation.VerdictFailed:
		verdict = "VALIDATION FAILED"
	}
	fmt.Fprintf(&b, "DPF %s\nScope: %s\n", verdict, validationModeLabel(result.Mode))
	counts := map[validation.State]int{}
	for _, check := range result.Checks {
		counts[check.Outcome.State]++
	}
	fmt.Fprintf(&b, "Checks: %d passed, %d failed, %d pending, %d unverified, %d skipped\n", counts[validation.Passed], counts[validation.Failed], counts[validation.Pending], counts[validation.Unverified], counts[validation.Skipped])
	if result.RuntimeBudget > 0 {
		fmt.Fprintf(&b, "Runtime budget: %s\n", result.RuntimeBudget)
	}
	if len(result.Scope.Hosts) > 0 {
		hosts := make([]string, len(result.Scope.Hosts))
		for i, host := range result.Scope.Hosts {
			hosts[i] = "host attached to DPU " + string(host)
		}
		fmt.Fprintf(&b, "Selected hosts: %s\n", strings.Join(hosts, "; "))
	}
	for _, check := range result.Checks {
		if check.Outcome.State == validation.Passed || check.Outcome.State == validation.Skipped {
			continue
		}
		fmt.Fprintf(&b, "%s: %s", validationStateLabel(check.Outcome.State), check.Subject)
		if check.Outcome.Summary != "" {
			fmt.Fprintf(&b, " — %s", check.Outcome.Summary)
		}
		fmt.Fprintln(&b)
		if check.Outcome.Action != "" {
			fmt.Fprintf(&b, "  Next: %s\n", check.Outcome.Action)
		}
	}
	if result.Report.Status == validation.ReportPublished && result.Report.Path != "" {
		fmt.Fprintf(&b, "Report: %s\n", result.Report.Path)
	} else {
		fmt.Fprintf(&b, "Report: %s\n", result.Report.Status)
	}
	return b.Bytes()
}
