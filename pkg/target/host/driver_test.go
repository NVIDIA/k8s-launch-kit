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

package host

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
)

func TestDescriptorDeclaresHostLifecycle(t *testing.T) {
	descriptor := Descriptor()
	assert.Equal(t, target.Host, descriptor.Name)
	for _, phase := range append(target.PublicPhases(), target.Pipeline) {
		assert.Truef(t, descriptor.Capability(phase).Available, "phase %s", phase)
	}
}

func TestLauncherAdapterSnapshotsRequestAndAppliesCommonPolicy(t *testing.T) {
	enableDriver := true
	request := LauncherRequest{Options: options.Options{
		Groups:              []string{"machine-a"},
		ImagePullSecrets:    []string{"registry-a"},
		NetworkNamespaces:   []string{"workloads"},
		EnabledPlugins:      []string{"network-operator"},
		EnableDocaDriver:    &enableDriver,
		OutputFormat:        "stale",
		Yes:                 false,
		Quiet:               false,
		DryRun:              false,
		DeployTimeout:       0,
		SaveDeploymentFiles: "./deployment",
	}}

	var captured options.Options
	driver := NewGenerateAdapter(request, LauncherRunnerFunc(
		func(_ context.Context, phase target.Phase, opts options.Options) error {
			assert.Equal(t, target.Generate, phase)
			captured = opts
			return nil
		},
	))

	request.Options.Groups[0] = "mutated"
	request.Options.ImagePullSecrets[0] = "mutated"
	request.Options.NetworkNamespaces[0] = "mutated"
	request.Options.EnabledPlugins[0] = "mutated"
	*request.Options.EnableDocaDriver = false

	invocation := target.Invocation{
		Target: target.Host,
		Phase:  target.Generate,
		Output: target.OutputOptions{
			Format:      "json",
			Quiet:       true,
			AutoApprove: true,
		},
		Execution: target.ExecutionOptions{
			DryRun:  true,
			Timeout: 2 * time.Minute,
		},
	}
	operation, err := driver.Bind(invocation)
	require.NoError(t, err)
	require.NoError(t, operation.Run(context.Background()))

	assert.Equal(t, []string{"machine-a"}, captured.Groups)
	assert.Equal(t, []string{"registry-a"}, captured.ImagePullSecrets)
	assert.Equal(t, []string{"workloads"}, captured.NetworkNamespaces)
	assert.Equal(t, []string{"network-operator"}, captured.EnabledPlugins)
	require.NotNil(t, captured.EnableDocaDriver)
	assert.True(t, *captured.EnableDocaDriver)
	assert.Equal(t, "json", captured.OutputFormat)
	assert.True(t, captured.Quiet)
	assert.True(t, captured.Yes)
	assert.True(t, captured.DryRun)
	assert.Equal(t, 2*time.Minute, captured.DeployTimeout)
}

func TestValidateAdapterPreservesExplicitFalseAndSnapshotsSlices(t *testing.T) {
	request := ValidateRequest{
		Connectivity: Explicit[bool]{Value: false, Set: true},
		Checks:       Explicit[[]string]{Value: []string{"icmp"}, Set: true},
	}
	var captured ValidateRequest
	driver := NewValidateAdapter(request, ValidateRunnerFunc(
		func(_ context.Context, actual ValidateRequest) error {
			captured = actual
			return nil
		},
	))
	request.Checks.Value[0] = "rping"

	operation, err := driver.Bind(target.Invocation{
		Target: target.Host,
		Phase:  target.Validate,
		Output: target.OutputOptions{Format: "text"},
	})
	require.NoError(t, err)
	require.NoError(t, operation.Run(context.Background()))

	assert.True(t, captured.Connectivity.Set)
	assert.False(t, captured.Connectivity.Value)
	assert.Equal(t, []string{"icmp"}, captured.Checks.Value)
}

func TestDeployAdapterForwardsContextAndCommonExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var captured DeployRequest
	driver := NewDeployAdapter(DeployRequest{Kubeconfig: "raw"}, DeployRunnerFunc(
		func(actualContext context.Context, request DeployRequest) error {
			captured = request
			return actualContext.Err()
		},
	))
	operation, err := driver.Bind(target.Invocation{
		Target:    target.Host,
		Phase:     target.Deploy,
		Output:    target.OutputOptions{Format: "json", Quiet: true, AutoApprove: true},
		Execution: target.ExecutionOptions{DryRun: true, Timeout: time.Minute},
	})
	require.NoError(t, err)
	assert.ErrorIs(t, operation.Run(ctx), context.Canceled)
	assert.Equal(t, "raw", captured.Kubeconfig)
	assert.Equal(t, "json", captured.OutputFormat)
	assert.True(t, captured.Quiet)
	assert.True(t, captured.AutoApprove)
	assert.True(t, captured.DryRun)
	assert.Equal(t, time.Minute, captured.Timeout)
}

func TestAdaptersRejectWrongPhaseAndNilRunners(t *testing.T) {
	driver := NewDiscoverAdapter(LauncherRequest{}, LauncherRunnerFunc(
		func(context.Context, target.Phase, options.Options) error { return nil },
	))
	operation, err := driver.Bind(target.Invocation{Target: target.Host, Phase: target.Generate})
	assert.Nil(t, operation)
	assert.ErrorContains(t, err, `bound for phase "discover"`)

	nilDriver := NewDeployAdapter(DeployRequest{}, nil)
	operation, err = nilDriver.Bind(target.Invocation{Target: target.Host, Phase: target.Deploy})
	assert.Nil(t, operation)
	assert.ErrorContains(t, err, "runner must not be nil")
}

func TestValidateAdapterRejectsInvalidExplicitOptionsAtBind(t *testing.T) {
	tests := []struct {
		name    string
		request ValidateRequest
		message string
	}{
		{
			name:    "mode",
			request: ValidateRequest{Mode: Explicit[string]{Value: "invalid", Set: true}},
			message: "validation.mode",
		},
		{
			name:    "checks",
			request: ValidateRequest{Checks: Explicit[[]string]{Value: []string{"invalid"}, Set: true}},
			message: "validation.checks",
		},
		{
			name:    "rping iterations",
			request: ValidateRequest{RDMAPIterations: Explicit[int]{Value: 0, Set: true}},
			message: "must be greater than 0",
		},
		{
			name:    "ib write size",
			request: ValidateRequest{RDMAIBWriteSize: Explicit[int]{Value: 0, Set: true}},
			message: "must be greater than 0",
		},
		{
			name:    "minimum bandwidth",
			request: ValidateRequest{RDMAMinBandwidth: Explicit[float64]{Value: -1, Set: true}},
			message: "greater than or equal to 0",
		},
		{
			name:    "connectivity timeout",
			request: ValidateRequest{ConnectivityTime: -time.Second},
			message: "connectivity-timeout",
		},
		{
			name:    "wait timeout",
			request: ValidateRequest{Wait: -time.Second},
			message: "--wait",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			driver := NewValidateAdapter(test.request, ValidateRunnerFunc(
				func(context.Context, ValidateRequest) error {
					t.Fatal("invalid request must not run")
					return nil
				},
			))

			operation, err := driver.Bind(target.Invocation{
				Target: target.Host,
				Phase:  target.Validate,
				Output: target.OutputOptions{Format: "text"},
			})
			assert.Nil(t, operation)
			assert.ErrorContains(t, err, test.message)
		})
	}
}
