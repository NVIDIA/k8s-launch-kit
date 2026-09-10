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
	"os"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/connectivity"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
)

const userConnectivityDaemonSet = `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: user-connectivity-test
  namespace: default
spec:
  selector:
    matchLabels:
      app: user-connectivity-test
  template:
    metadata:
      labels:
        app: user-connectivity-test
    spec:
      containers:
        - name: test-container
          image: nvcr.io/nvidia/doca/doca:full-rt
        - name: netshoot
          image: nicolaka/netshoot:latest
`

func writeConnectivityOnlyInputs(t *testing.T, routing string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "60-example-daemonset.yaml"), []byte(userConnectivityDaemonSet), 0o600))
	configPath := filepath.Join(t.TempDir(), "cluster-config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`profile:
  routing: `+routing+`
validation:
  gpuDirect:
    enabled: false
`), 0o600))
	return dir, configPath
}

func TestValidateWritesPartialReportBeforeKubernetesClientExists(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "partial-report.html")
	runner := NewValidateRunner()

	err := runner.Run(context.Background(), ValidateRequest{
		Kubeconfig:      filepath.Join(t.TempDir(), "kubeconfig"),
		DeploymentFiles: filepath.Join(t.TempDir(), "missing-deployment"),
		ReportPath:      reportPath,
		OutputFormat:    "text",
		Version:         "test",
	})

	require.Error(t, err)
	assert.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(err))
	assert.FileExists(t, reportPath)
}

func TestListNodesForReportAcceptsNilClient(t *testing.T) {
	assert.Nil(t, listNodesForReport(context.Background(), nil))
}

func TestValidateDoesNotCreateMissingDeploymentDirectoryForDefaultReport(t *testing.T) {
	workingDir := t.TempDir()
	t.Chdir(workingDir)
	deploymentPath := filepath.Join(workingDir, "missing-deployment")

	err := NewValidateRunner().Run(context.Background(), ValidateRequest{
		Kubeconfig:      filepath.Join(workingDir, "missing-kubeconfig"),
		DeploymentFiles: deploymentPath,
		OutputFormat:    "text",
	})

	require.Error(t, err)
	assert.NoDirExists(t, deploymentPath)
}

func TestValidateRunsConnectivityOnlyFromExampleDaemonSet(t *testing.T) {
	deploymentDir, configPath := writeConnectivityOnlyInputs(t, config.RoutingSourceBased)
	clientCreated := false
	var captured connectivity.Options
	runner := validateRunner{
		newKubeClient: func(path string) (ctrlclient.Client, *rest.Config, error) {
			clientCreated = true
			assert.Equal(t, "test-kubeconfig", path)
			return nil, &rest.Config{}, nil
		},
		runConnectivityMatrix: func(
			_ context.Context,
			_ ctrlclient.Client,
			_ *rest.Config,
			_ ui.Output,
			opts connectivity.Options,
		) (*connectivity.MatrixResult, error) {
			captured = opts
			return &connectivity.MatrixResult{
				PingResults: []connectivity.PingResult{
					{Test: connectivity.PingTest{Kind: connectivity.ICMPSameRail}, OK: true},
					{Test: connectivity.PingTest{Kind: connectivity.RDMAPingSameRail}, OK: true},
					{Test: connectivity.PingTest{Kind: connectivity.RDMABwSameRail}, OK: true},
				},
				Summary: connectivity.MatrixSummary{TotalTests: 3, Passed: 3},
			}, nil
		},
	}

	err := runner.Run(context.Background(), ValidateRequest{
		Kubeconfig:      "test-kubeconfig",
		DeploymentFiles: deploymentDir,
		UserConfig:      configPath,
		ReportPath:      "-",
		OutputFormat:    "text",
	})

	require.NoError(t, err)
	assert.True(t, clientCreated)
	assert.Equal(t, deploymentDir, captured.ManifestDir)
	assert.Equal(t, connectivity.ModeStrict, captured.Mode)
	assert.Equal(t, config.RoutingSourceBased, captured.Routing)
	assert.Equal(t, []connectivity.Check{
		connectivity.CheckICMP,
		connectivity.CheckRPing,
		connectivity.CheckIBWriteBW,
	}, captured.Checks)
}

func TestValidateConnectivityOnlyHonorsCLIOverrides(t *testing.T) {
	deploymentDir, configPath := writeConnectivityOnlyInputs(t, config.RoutingDestinationBased)
	var captured connectivity.Options
	runner := validateRunner{
		newKubeClient: func(string) (ctrlclient.Client, *rest.Config, error) {
			return nil, &rest.Config{}, nil
		},
		runConnectivityMatrix: func(
			_ context.Context,
			_ ctrlclient.Client,
			_ *rest.Config,
			_ ui.Output,
			opts connectivity.Options,
		) (*connectivity.MatrixResult, error) {
			captured = opts
			return &connectivity.MatrixResult{
				PingResults: []connectivity.PingResult{
					{Test: connectivity.PingTest{Kind: connectivity.RDMAPingSameRail}, OK: true},
				},
				Summary: connectivity.MatrixSummary{TotalTests: 1, Passed: 1},
			}, nil
		},
	}

	err := runner.Run(context.Background(), ValidateRequest{
		Kubeconfig:      "test-kubeconfig",
		DeploymentFiles: deploymentDir,
		UserConfig:      configPath,
		Mode:            Explicit[string]{Value: config.ValidationModeFull, Set: true},
		Checks:          Explicit[[]string]{Value: []string{config.ValidationCheckRPing}, Set: true},
		ReportPath:      "-",
		OutputFormat:    "text",
	})

	require.NoError(t, err)
	assert.Equal(t, connectivity.ModeFull, captured.Mode)
	assert.Equal(t, config.RoutingDestinationBased, captured.Routing)
	assert.Equal(t, []connectivity.Check{connectivity.CheckRPing}, captured.Checks)
}

func TestValidateConnectivityRequiresUserOwnedConfigBeforeCreatingClient(t *testing.T) {
	deploymentDir, _ := writeConnectivityOnlyInputs(t, config.RoutingSourceBased)
	clientCreated := false
	runner := validateRunner{
		newKubeClient: func(string) (ctrlclient.Client, *rest.Config, error) {
			clientCreated = true
			return nil, nil, nil
		},
	}

	err := runner.Run(context.Background(), ValidateRequest{
		Kubeconfig:      "test-kubeconfig",
		DeploymentFiles: deploymentDir,
		ReportPath:      "-",
		OutputFormat:    "text",
	})

	require.Error(t, err)
	assert.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(err))
	assert.ErrorContains(t, err, "user-owned cluster-config.yaml")
	assert.False(t, clientCreated)
}

func TestValidateConnectivityRequiresExampleDaemonSetBeforeCreatingClient(t *testing.T) {
	deploymentDir := t.TempDir()
	_, configPath := writeConnectivityOnlyInputs(t, config.RoutingSourceBased)
	clientCreated := false
	runner := validateRunner{
		newKubeClient: func(string) (ctrlclient.Client, *rest.Config, error) {
			clientCreated = true
			return nil, nil, nil
		},
	}

	err := runner.Run(context.Background(), ValidateRequest{
		Kubeconfig:      "test-kubeconfig",
		DeploymentFiles: deploymentDir,
		UserConfig:      configPath,
		ReportPath:      "-",
		OutputFormat:    "text",
	})

	require.Error(t, err)
	assert.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(err))
	assert.ErrorContains(t, err, "no example test DaemonSet")
	assert.False(t, clientCreated)
}

func TestValidateConnectivityOnlyRequiresConclusiveMatrix(t *testing.T) {
	deploymentDir, configPath := writeConnectivityOnlyInputs(t, config.RoutingSourceBased)
	tests := map[string]*connectivity.MatrixResult{
		"skipped": {
			Skipped: &connectivity.MatrixSkip{Reason: "fewer than two schedulable test pods"},
		},
		"empty": {},
		"failed": {
			Summary: connectivity.MatrixSummary{TotalTests: 2, Passed: 1, Failed: 1},
		},
	}
	for name, result := range tests {
		t.Run(name, func(t *testing.T) {
			runner := validateRunner{
				newKubeClient: func(string) (ctrlclient.Client, *rest.Config, error) {
					return nil, &rest.Config{}, nil
				},
				runConnectivityMatrix: func(
					context.Context, ctrlclient.Client, *rest.Config, ui.Output, connectivity.Options,
				) (*connectivity.MatrixResult, error) {
					return result, nil
				},
			}

			err := runner.Run(context.Background(), ValidateRequest{
				Kubeconfig:      "test-kubeconfig",
				DeploymentFiles: deploymentDir,
				UserConfig:      configPath,
				ReportPath:      "-",
				OutputFormat:    "text",
			})

			require.Error(t, err)
			assert.Equal(t, apperrors.ExitDeployment, apperrors.ExitCodeFromError(err))
		})
	}
}

func TestConnectivityOnlyVerdictRequiresGatingResultForEverySelectedCheck(t *testing.T) {
	selectedChecks := []connectivity.Check{
		connectivity.CheckICMP,
		connectivity.CheckRPing,
		connectivity.CheckIBWriteBW,
		connectivity.CheckGPUDirectDMABuf,
	}
	matrix := &connectivity.MatrixResult{
		PingResults: []connectivity.PingResult{
			{Test: connectivity.PingTest{Kind: connectivity.ICMPSameRail}, OK: true},
			{Test: connectivity.PingTest{Kind: connectivity.RDMAPingSameRail}, OK: true},
			{Test: connectivity.PingTest{Kind: connectivity.RDMABwSameRail}, OK: true},
			{
				Test:        connectivity.PingTest{Kind: connectivity.GPUDirectDMABufCrossRail},
				OK:          true,
				Expectation: connectivity.ExpectObserve,
			},
		},
		Summary: connectivity.MatrixSummary{TotalTests: 4, Passed: 4},
	}

	verdict := connectivityOnlyVerdict(matrix, selectedChecks)

	assert.False(t, verdict.Pass)
	assert.Contains(t, verdict.Reasons,
		`selected connectivity check "gpudirect_dmabuf" did not produce any gating tests`)

	matrix.PingResults[3] = connectivity.PingResult{
		Test:        connectivity.PingTest{Kind: connectivity.GPUDirectDMABufCrossRail},
		OK:          true,
		Expectation: connectivity.ExpectForbidden,
	}

	assert.True(t, connectivityOnlyVerdict(matrix, selectedChecks).Pass)
}
