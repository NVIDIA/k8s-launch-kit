// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package dpf binds the independent DPU-plane discovery CLI to typed services.
package dpf

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/app"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Descriptor advertises implemented phases; deployment remains independently gated.
func Descriptor() target.Descriptor {
	phases := map[target.Phase]target.Capability{}
	for _, p := range append(target.PublicPhases(), target.Pipeline) {
		phases[p] = target.Capability{Available: false, Reason: "DPF validation and the composite pipeline are not implemented; use the standalone discover, generate and deploy commands"}
	}
	phases[target.Deploy] = target.Capability{Available: true, Reason: "HBN-BGP provisioning and runtime validation for the explicitly selected hosts and DPUs"}
	phases[target.Generate] = target.Capability{Available: true, Reason: "HBN-BGP artifact generation; deployment readiness requires fresh checks"}
	phases[target.Discover] = target.Capability{Available: true, Reason: "inventory discovery and management-network diagnostics; deployment eligibility is evaluated by the selected use case"}
	return target.Descriptor{Name: target.DPF, Description: "Independent DPF discovery and HBN-BGP generation and deployment", Phases: phases}
}

type Request struct {
	Inventory      string
	UserConfig     string
	UseCase        *string
	OutputPath     string
	ExplicitOutput bool
	Stdout         io.Writer
	Stderr         io.Writer
}
type driver struct{ request Request }

func NewDiscoverAdapter(r Request) target.Driver { return driver{request: r} }
func (d driver) Descriptor() target.Descriptor   { return Descriptor() }
func (d driver) Bind(inv target.Invocation) (target.Operation, error) {
	if inv.Phase != target.Discover {
		return nil, &target.PhaseUnavailableError{Name: target.DPF, Phase: inv.Phase, Reason: Descriptor().Capability(inv.Phase).Reason}
	}
	if inv.Execution.DryRun {
		return nil, fmt.Errorf("DPF discovery does not support --dry-run; inventory mode is the default, and management network probes require explicit management mode and runtime-qualified diagnostic assets")
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
		progress := ui.NewSilent()
		if !inv.Output.Quiet {
			progress = ui.NewWithWriter(r.Stderr)
		}
		ctx = ui.WithOutput(ctx, progress)
		logger := log.FromContext(ctx).WithName("dpf-discovery")
		ctx = log.IntoContext(ctx, logger)
		result := app.Discover(ctx, app.DiscoveryRequest{
			Inventory: r.Inventory, UserConfig: r.UserConfig, UseCase: r.UseCase,
			OutputPath: r.OutputPath, ExplicitOutput: r.ExplicitOutput,
		})
		report, input := result.Report, result.Input
		if inv.Output.Format == "json" {
			if err := writeJSONReport(r.Stdout, report); err != nil {
				logger.Error(err, "Discovery result output failed")
				fmt.Fprintln(r.Stderr, "cannot write complete DPF discovery result:", err)
				return apperrors.MarkReported(apperrors.NewGeneralError("cannot write complete DPF discovery result", nil))
			}
		} else {
			if err := textReport(r.Stdout, report, input); err != nil {
				fmt.Fprintln(r.Stderr, "cannot write DPF discovery result")
				return apperrors.MarkReported(apperrors.NewGeneralError("cannot write DPF discovery result", nil))
			}
		}
		return apperrors.NewExitStatus(discovery.ExitCode(report))
	}), nil
}
