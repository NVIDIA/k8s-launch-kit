// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

// Package host implements the Launch Kit lifecycle for Kubernetes host
// networking managed by NVIDIA Network Operator.
package host

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
)

// Explicit preserves the difference between an omitted CLI flag and an
// explicitly supplied zero value.
type Explicit[T any] struct {
	Value T
	Set   bool
}

// LauncherRequest is the immutable Host request used by discover, generate,
// and the legacy root pipeline.
type LauncherRequest struct {
	Options options.Options
}

// DeployRequest contains the Host-owned inputs for standalone deployment.
type DeployRequest struct {
	Kubeconfig        string
	DeploymentFiles   string
	UserConfig        string
	ConfigDir         string
	OperatorNamespace string
	OverwriteExisting bool
	OutputFormat      string
	Quiet             bool
	AutoApprove       bool
	DryRun            bool
	Timeout           time.Duration
}

// ValidateRequest contains the Host-owned inputs for standalone validation.
type ValidateRequest struct {
	Kubeconfig        string
	DeploymentFiles   string
	UserConfig        string
	ConfigDir         string
	OperatorNamespace string
	Connectivity      Explicit[bool]
	Keep              bool
	ConnectivityTime  time.Duration
	Mode              Explicit[string]
	Checks            Explicit[[]string]
	RDMAPIterations   Explicit[int]
	RDMAIBWriteSize   Explicit[int]
	RDMAMinBandwidth  Explicit[float64]
	Wait              time.Duration
	ReportPath        string
	Version           string
	OutputFormat      string
	Quiet             bool
	AutoApprove       bool
}

// LauncherRunner executes one app.Launcher-backed Host phase.
type LauncherRunner interface {
	Run(context.Context, target.Phase, options.Options) error
}

// LauncherRunnerFunc adapts a function to LauncherRunner.
type LauncherRunnerFunc func(context.Context, target.Phase, options.Options) error

func (f LauncherRunnerFunc) Run(ctx context.Context, phase target.Phase, opts options.Options) error {
	if f == nil {
		return fmt.Errorf("host launcher runner must not be nil")
	}
	return f(ctx, phase, opts)
}

// DeployRunner executes a standalone Host deployment.
type DeployRunner interface {
	Run(context.Context, DeployRequest) error
}

// DeployRunnerFunc adapts a function to DeployRunner.
type DeployRunnerFunc func(context.Context, DeployRequest) error

func (f DeployRunnerFunc) Run(ctx context.Context, request DeployRequest) error {
	if f == nil {
		return fmt.Errorf("host deploy runner must not be nil")
	}
	return f(ctx, request)
}

// ValidateRunner executes a standalone Host validation.
type ValidateRunner interface {
	Run(context.Context, ValidateRequest) error
}

// ValidateRunnerFunc adapts a function to ValidateRunner.
type ValidateRunnerFunc func(context.Context, ValidateRequest) error

func (f ValidateRunnerFunc) Run(ctx context.Context, request ValidateRequest) error {
	if f == nil {
		return fmt.Errorf("host validate runner must not be nil")
	}
	return f(ctx, request)
}

type phaseDriver struct {
	phase target.Phase
	bind  func(target.Invocation) (target.Operation, error)
}

func (d phaseDriver) Descriptor() target.Descriptor { return Descriptor() }

func (d phaseDriver) Bind(invocation target.Invocation) (target.Operation, error) {
	if invocation.Phase != d.phase {
		return nil, fmt.Errorf("host adapter was bound for phase %q, not %q", d.phase, invocation.Phase)
	}
	if d.bind == nil {
		return nil, fmt.Errorf("host adapter for phase %q has no binder", d.phase)
	}
	return d.bind(invocation)
}

// Descriptor is the single source of Host target capabilities.
func Descriptor() target.Descriptor {
	available := target.Capability{Available: true}
	return target.Descriptor{
		Name:        target.Host,
		Description: "Kubernetes host networking managed through NVIDIA Network Operator",
		Phases: map[target.Phase]target.Capability{
			target.Discover: available,
			target.Generate: available,
			target.Deploy:   available,
			target.Validate: available,
			target.Pipeline: available,
		},
	}
}

// NewDiscoverAdapter binds an immutable Host discovery request.
func NewDiscoverAdapter(request LauncherRequest, runner LauncherRunner) target.Driver {
	return newLauncherAdapter(target.Discover, request, runner)
}

// NewGenerateAdapter binds an immutable Host generation request.
func NewGenerateAdapter(request LauncherRequest, runner LauncherRunner) target.Driver {
	return newLauncherAdapter(target.Generate, request, runner)
}

// NewPipelineAdapter binds an immutable legacy Host pipeline request.
func NewPipelineAdapter(request LauncherRequest, runner LauncherRunner) target.Driver {
	return newLauncherAdapter(target.Pipeline, request, runner)
}

func newLauncherAdapter(phase target.Phase, request LauncherRequest, runner LauncherRunner) target.Driver {
	snapshot := LauncherRequest{Options: cloneOptions(request.Options)}
	return phaseDriver{phase: phase, bind: func(invocation target.Invocation) (target.Operation, error) {
		if runner == nil {
			return nil, fmt.Errorf("host %s runner must not be nil", phase)
		}
		opts := cloneOptions(snapshot.Options)
		applyCommonOptions(&opts, invocation)
		if err := validateLauncherOptions(phase, &opts); err != nil {
			return nil, err
		}
		return target.OperationFunc(func(ctx context.Context) error {
			return runner.Run(ctx, phase, cloneOptions(opts))
		}), nil
	}}
}

// NewDeployAdapter binds an immutable Host deployment request.
func NewDeployAdapter(request DeployRequest, runner DeployRunner) target.Driver {
	snapshot := request
	return phaseDriver{phase: target.Deploy, bind: func(invocation target.Invocation) (target.Operation, error) {
		if runner == nil {
			return nil, fmt.Errorf("host deploy runner must not be nil")
		}
		bound := snapshot
		bound.OutputFormat = invocation.Output.Format
		bound.Quiet = invocation.Output.Quiet
		bound.AutoApprove = invocation.Output.AutoApprove
		bound.DryRun = invocation.Execution.DryRun
		bound.Timeout = invocation.Execution.Timeout
		return target.OperationFunc(func(ctx context.Context) error {
			return runner.Run(ctx, bound)
		}), nil
	}}
}

// NewValidateAdapter binds an immutable Host validation request.
func NewValidateAdapter(request ValidateRequest, runner ValidateRunner) target.Driver {
	snapshot := cloneValidateRequest(request)
	return phaseDriver{phase: target.Validate, bind: func(invocation target.Invocation) (target.Operation, error) {
		if runner == nil {
			return nil, fmt.Errorf("host validate runner must not be nil")
		}
		bound := cloneValidateRequest(snapshot)
		bound.OutputFormat = invocation.Output.Format
		bound.Quiet = invocation.Output.Quiet
		bound.AutoApprove = invocation.Output.AutoApprove
		if err := validateExplicitValidationOptions(bound); err != nil {
			return nil, apperrors.NewValidationError(
				"invalid validation configuration",
				err,
				"Use --validation-mode quick|full|strict, supported validation checks, positive RDMA values, and non-negative validation timeouts",
			)
		}
		return target.OperationFunc(func(ctx context.Context) error {
			return runner.Run(ctx, cloneValidateRequest(bound))
		}), nil
	}}
}

func applyCommonOptions(opts *options.Options, invocation target.Invocation) {
	opts.OutputFormat = invocation.Output.Format
	opts.Quiet = invocation.Output.Quiet
	opts.Yes = invocation.Output.AutoApprove
	opts.DryRun = invocation.Execution.DryRun
	opts.DeployTimeout = invocation.Execution.Timeout
}

func cloneOptions(in options.Options) options.Options {
	out := in
	out.ImagePullSecrets = append([]string(nil), in.ImagePullSecrets...)
	out.NetworkNamespaces = append([]string(nil), in.NetworkNamespaces...)
	out.EnabledPlugins = append([]string(nil), in.EnabledPlugins...)
	out.Groups = append([]string(nil), in.Groups...)
	if in.EnableDocaDriver != nil {
		value := *in.EnableDocaDriver
		out.EnableDocaDriver = &value
	}
	return out
}

func cloneValidateRequest(in ValidateRequest) ValidateRequest {
	out := in
	out.Checks.Value = append([]string(nil), in.Checks.Value...)
	return out
}

func validateExplicitValidationOptions(request ValidateRequest) error {
	if request.Mode.Set {
		mode := strings.TrimSpace(request.Mode.Value)
		if mode == "" {
			return fmt.Errorf("--validation-mode must not be empty")
		}
	}
	if request.RDMAPIterations.Set && request.RDMAPIterations.Value <= 0 {
		return fmt.Errorf("--rdma-rping-iterations must be greater than 0")
	}
	if request.RDMAIBWriteSize.Set && request.RDMAIBWriteSize.Value <= 0 {
		return fmt.Errorf("--rdma-ib-write-size must be greater than 0")
	}
	if request.RDMAMinBandwidth.Set && request.RDMAMinBandwidth.Value < 0 {
		return fmt.Errorf("--rdma-ib-write-min-bandwidth-gbps must be greater than or equal to 0")
	}
	if request.ConnectivityTime < 0 {
		return fmt.Errorf("--connectivity-timeout must be greater than or equal to 0")
	}
	if request.Wait < 0 {
		return fmt.Errorf("--wait must be greater than or equal to 0")
	}
	validation := config.DefaultValidationConfig()
	return applyValidationOverrides(request, validation)
}
