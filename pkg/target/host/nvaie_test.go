// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/connectivity"
	"github.com/nvidia/k8s-launch-kit/pkg/nvaie"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNVAIEValidateEarlyErrorRetainsRequestedCheck(t *testing.T) {
	for _, fromFlag := range []bool{true, false} {
		t.Run(map[bool]string{true: "flag", false: "config"}[fromFlag], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "report.html")
			request := ValidateRequest{Kubeconfig: "unused", DeploymentFiles: filepath.Join(dir, "missing"), ReportPath: path, OutputFormat: "json"}
			if fromFlag {
				request.NVAIERelease = Explicit[string]{Set: true, Value: "9.0"}
			} else {
				request.UserConfig = filepath.Join(dir, "config.yaml")
				require.NoError(t, os.WriteFile(request.UserConfig, []byte("nvaieRelease: '9.0'\n"), 0600))
			}
			require.Error(t, NewValidateRunner().Run(context.Background(), request))
			report, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Contains(t, string(report), "NVIDIA AI Enterprise 9.0")
			assert.Contains(t, string(report), "UNKNOWN")
			assert.Contains(t, string(report), "before live NVAIE")
		})
	}
}

func TestNVAIEValidateConnectivityOnlyKeepsChecksAndGatesAssessment(t *testing.T) {
	dir, path := writeConnectivityOnlyInputs(t, config.RoutingDestinationBased)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append([]byte("nvaieRelease: '9.0'\nclusterConfig:\n- workerNodes: [worker-a]\n  gpuType: stale-product\n"), data...), 0600))
	observed, connected := false, false
	runner := validateRunner{
		newKubeClient: func(string) (ctrlclient.Client, *rest.Config, error) { return nil, &rest.Config{}, nil },
		assessNVAIE: func(_ context.Context, _ *rest.Config, release string, workers []string, _ *nvaie.DOCAHostEvidence) *nvaie.Assessment {
			observed = true
			assert.Equal(t, []string{"worker-a"}, workers)
			return pendingNVAIE(release, "fresh live GPU product unavailable")
		},
		runConnectivityMatrix: func(context.Context, ctrlclient.Client, *rest.Config, ui.Output, *bundle.Bundle, connectivity.Options) (*connectivity.MatrixResult, error) {
			connected = true
			return &connectivity.MatrixResult{PingResults: []connectivity.PingResult{{Test: connectivity.PingTest{Kind: connectivity.Gateway}, OK: true}}, Summary: connectivity.MatrixSummary{TotalTests: 1, Passed: 1}}, nil
		},
	}
	request := ValidateRequest{UserConfig: path, Kubeconfig: "test", DeploymentFiles: dir, ReportPath: filepath.Join(t.TempDir(), "report.html"), Checks: Explicit[[]string]{Set: true, Value: []string{"gateway"}}, OutputFormat: "json"}
	err = runner.Run(context.Background(), request)
	require.Error(t, err)
	assert.Equal(t, apperrors.ExitDeployment, apperrors.ExitCodeFromError(err))
	assert.True(t, observed)
	assert.True(t, connected)
	report, err := os.ReadFile(request.ReportPath)
	require.NoError(t, err)
	assert.Contains(t, string(report), "fresh live GPU product unavailable")
	assert.Contains(t, string(report), "VALIDATION FAILED")
}

func TestNVAIEEffectiveAndRawConfigRejectConflicts(t *testing.T) {
	for _, effective := range []bool{true, false} {
		t.Run(map[bool]string{true: "effective", false: "raw"}[effective], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if effective {
				var err error
				path, err = config.WriteEffectiveConfig(dir, &config.LaunchKitConfig{NVAIERelease: "9.0", NetworkOperator: &config.NetworkOperatorConfig{SelectedRelease: "26.4"}})
				require.NoError(t, err)
			} else {
				require.NoError(t, os.WriteFile(path, []byte("nvaieRelease: '9.0'\nnetworkOperator: {selectedRelease: '26.4'}\n"), 0600))
			}
			_, _, err := LoadUserConfig(UserConfigInput{Explicit: path}, options.Options{})
			require.ErrorContains(t, err, "conflicts with explicit")
		})
	}
}

func TestNVAIEValidationCohortErrorNeverSoftSkips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("nvaieRelease: '9.0'\nnetworkOperator: {selectedRelease: '26.4'}\nvalidation: {connectivity: false}\n"), 0600))
	called := false
	runner := validateRunner{newKubeClient: func(string) (ctrlclient.Client, *rest.Config, error) { called = true; return nil, nil, nil }}
	err := runner.Run(context.Background(), ValidateRequest{UserConfig: path, Kubeconfig: "test", DeploymentFiles: t.TempDir(), ReportPath: "-", OutputFormat: "json"})
	require.ErrorContains(t, err, "NVAIE configuration resolution failed")
	assert.False(t, called)
}

func TestNVAIEInProgressCannotReturnSuccess(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "nvaie"}[enabled], func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "policy.yaml"), []byte("apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata: {name: policy}\nspec: {}\n"), 0600))
			path := filepath.Join(t.TempDir(), "config.yaml")
			source := "validation: {connectivity: false}\nnetworkOperator: {skipHelmChart: true}\n"
			if enabled {
				source = "nvaieRelease: '9.0'\n" + source
			}
			require.NoError(t, os.WriteFile(path, []byte(source), 0600))
			live := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "mellanox.com/v1alpha1", "kind": "NicClusterPolicy", "metadata": map[string]interface{}{"name": "policy"}, "spec": map[string]interface{}{"nvIpam": map[string]interface{}{"version": "network-operator-v26.7.1"}}, "status": map[string]interface{}{"state": "notReady"}}}
			c := fake.NewClientBuilder().WithObjects(live).Build()
			observed := false
			runner := validateRunner{newKubeClient: func(string) (ctrlclient.Client, *rest.Config, error) { return c, nil, nil }, assessNVAIE: func(_ context.Context, _ *rest.Config, release string, _ []string, _ *nvaie.DOCAHostEvidence) *nvaie.Assessment {
				observed = true
				return pendingNVAIE(release, "GPU missing")
			}}
			err := runner.Run(context.Background(), ValidateRequest{UserConfig: path, Kubeconfig: "test", DeploymentFiles: dir, ReportPath: "-", OutputFormat: "json"})
			if enabled {
				require.Error(t, err)
				assert.Equal(t, apperrors.ExitDeployment, apperrors.ExitCodeFromError(err))
				assert.True(t, observed)
			} else {
				require.NoError(t, err)
				assert.False(t, observed)
			}
		})
	}
}

func TestNVAIEEarlyErrorRetainsEffectiveBundleIntent(t *testing.T) {
	dir := t.TempDir()
	path, err := config.WriteEffectiveConfig(dir, &config.LaunchKitConfig{NVAIERelease: "9.0"})
	require.NoError(t, err)
	result := requestedNVAIE(ValidateRequest{UserConfig: path})
	require.NotNil(t, result)
	assert.Equal(t, "9.0", result.Release)
	assert.False(t, result.Passed())
}

func TestNVAIEReportWriteFailureIsReturned(t *testing.T) {
	dir := t.TempDir()
	obstruction := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(obstruction, []byte("occupied"), 0600))
	err := NewValidateRunner().Run(context.Background(), ValidateRequest{Kubeconfig: "unused", DeploymentFiles: filepath.Join(dir, "missing"), ReportPath: filepath.Join(obstruction, "report.html"), NVAIERelease: Explicit[string]{Set: true, Value: "9.0"}, OutputFormat: "json"})
	require.ErrorContains(t, err, "failed to write NVAIE validation report")
}

func TestNVAIERejectedCLIReleaseRetainsIntentAcrossReports(t *testing.T) {
	for _, effective := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw", true: "effective"}[effective], func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			if effective {
				var err error
				configPath, err = config.WriteEffectiveConfig(dir, &config.LaunchKitConfig{NVAIERelease: "9.0", NetworkOperator: &config.NetworkOperatorConfig{SelectedRelease: "26.7"}})
				require.NoError(t, err)
			} else {
				require.NoError(t, os.WriteFile(configPath, []byte("nvaieRelease: '9.0'\nnetworkOperator: {selectedRelease: '26.7'}\n"), 0600))
			}
			stdoutPath := filepath.Join(dir, "stdout.json")
			stdout, err := os.Create(stdoutPath)
			require.NoError(t, err)
			previousStdout := os.Stdout
			os.Stdout = stdout
			defer func() { os.Stdout = previousStdout; _ = stdout.Close() }()
			called := false
			runner := validateRunner{newKubeClient: func(string) (ctrlclient.Client, *rest.Config, error) { called = true; return nil, nil, nil }}
			request := ValidateRequest{UserConfig: configPath, Kubeconfig: "unused", DeploymentFiles: t.TempDir(), ReportPath: filepath.Join(dir, "report.html"), JUnitPath: filepath.Join(dir, "report.xml"), NVAIERelease: Explicit[string]{Set: true, Value: "8.0"}, OutputFormat: "json"}
			runErr := runner.Run(context.Background(), request)
			os.Stdout = previousStdout
			require.NoError(t, stdout.Close())
			require.ErrorContains(t, runErr, `unsupported nvaieRelease "8.0"`)
			assert.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(runErr))
			assert.False(t, called, "unsupported CLI release must fail before Kubernetes access")

			output, err := os.ReadFile(stdoutPath)
			require.NoError(t, err)
			decoder := json.NewDecoder(bytes.NewReader(output))
			var assessment *nvaie.Assessment
			for {
				var document map[string]json.RawMessage
				if err := decoder.Decode(&document); err == io.EOF {
					break
				} else {
					require.NoError(t, err)
				}
				if data, exists := document["nvaie"]; exists {
					require.NoError(t, json.Unmarshal(data, &assessment))
				}
			}
			require.NotNil(t, assessment)
			assert.Equal(t, "8.0", assessment.Release)
			require.Len(t, assessment.Checks, 1)
			assert.Contains(t, assessment.Checks[0].Detail, `unsupported nvaieRelease "8.0"`)

			report, err := os.ReadFile(request.ReportPath)
			require.NoError(t, err)
			assert.Contains(t, string(report), "NVIDIA AI Enterprise 8.0 compliance")
			assert.NotContains(t, string(report), "NVIDIA AI Enterprise 9.0 compliance")
			assert.Contains(t, string(report), "unsupported nvaieRelease")

			junitData, err := os.ReadFile(request.JUnitPath)
			require.NoError(t, err)
			var junit junitDocument
			require.NoError(t, xml.Unmarshal(junitData, &junit))
			foundAssessment, foundExecution := false, false
			for _, suite := range junit.Suites {
				for _, testCase := range suite.Cases {
					if testCase.Name == "NVAIECompatibility" {
						foundAssessment = true
						require.NotNil(t, testCase.Failure)
						var evidence nvaie.Assessment
						require.NoError(t, json.Unmarshal([]byte(testCase.Out), &evidence))
						assert.Equal(t, "8.0", evidence.Release)
						assert.Contains(t, evidence.Checks[0].Detail, `unsupported nvaieRelease "8.0"`)
					}
					if testCase.Name == "ValidationExecution" {
						foundExecution = true
						require.NotNil(t, testCase.Error)
						assert.Contains(t, testCase.Error.Message, `unsupported nvaieRelease "8.0"`)
					}
				}
			}
			assert.True(t, foundAssessment)
			assert.True(t, foundExecution)
		})
	}
}

func TestNVAIEHostEvidenceBranchUsesSavedDiscoveryScope(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "container", false: "host"}[enabled], func(t *testing.T) {
			dir, path := writeConnectivityOnlyInputs(t, config.RoutingDestinationBased)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			source := "nvaieRelease: '9.0'\nclusterConfig:\n- workerNodes: [render-only]\ndiscoveredDocaHostScope:\n  workerNodes: [worker-a, worker-b]\n  membershipKnown: false\ndiscoveredDocaHost:\n- {node: worker-a, status: installed, version: '3.5.0-041000'}\n- {node: outside, status: installed, version: '3.6.0'}\ndocaDriver:\n  enable: " + map[bool]string{true: "true", false: "false"}[enabled] + "\n"
			require.NoError(t, os.WriteFile(path, append([]byte(source), data...), 0600))
			observed := false
			runner := validateRunner{
				newKubeClient: func(string) (ctrlclient.Client, *rest.Config, error) { return nil, &rest.Config{}, nil },
				assessNVAIE: func(_ context.Context, _ *rest.Config, release string, _ []string, evidence *nvaie.DOCAHostEvidence) *nvaie.Assessment {
					observed = true
					require.NotNil(t, evidence)
					assert.False(t, evidence.Fresh)
					assert.False(t, evidence.MembershipKnown)
					require.Len(t, evidence.Observations, 2)
					assert.Equal(t, "worker-a", evidence.Observations[0].Node)
					assert.Equal(t, "worker-b", evidence.Observations[1].Node)
					assert.Equal(t, "unknown", evidence.Observations[1].Status)
					return pendingNVAIE(release, "test observation")
				},
				runConnectivityMatrix: func(context.Context, ctrlclient.Client, *rest.Config, ui.Output, *bundle.Bundle, connectivity.Options) (*connectivity.MatrixResult, error) {
					return &connectivity.MatrixResult{}, nil
				},
			}
			_ = runner.Run(context.Background(), ValidateRequest{UserConfig: path, Kubeconfig: "test", DeploymentFiles: dir, ReportPath: "-", OutputFormat: "json"})
			assert.True(t, observed)
		})
	}
}

// A saved compatibility failure is a result, not a prerequisite for live checks.
func TestDOCAHostMismatchContinuesIndependentValidation(t *testing.T) {
	for _, nvaieEnabled := range []bool{false, true} {
		for _, mode := range []string{"static", "full", "connectivity-only", "in-progress", "client-error"} {
			t.Run(map[bool]string{false: "ordinary", true: "nvaie"}[nvaieEnabled]+"/"+mode, func(t *testing.T) {
				dir, path := writeConnectivityOnlyInputs(t, config.RoutingDestinationBased)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				data = bytes.Replace(data, []byte("profile:\n"), []byte("profile:\n  fabric: ethernet\n"), 1)
				source := "networkOperator: {selectedRelease: '26.7', skipHelmChart: true}\nclusterConfig:\n- workerNodes: [worker-a]\ndocaDriver: {enable: true}\ndiscoveredDocaHost:\n- {node: worker-a, status: installed, version: '3.6.0-070000'}\n"
				if nvaieEnabled {
					source = "nvaieRelease: '9.0'\n" + source
				}
				require.NoError(t, os.WriteFile(path, append([]byte(source), data...), 0600))
				var objects []ctrlclient.Object
				if mode != "connectivity-only" {
					manifest := "apiVersion: v1\nkind: Namespace\nmetadata: {name: validation-target}\n"
					live := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]interface{}{"name": "validation-target"}}}
					if mode == "in-progress" {
						manifest = "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata: {name: policy}\nspec: {}\n"
						live = &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "mellanox.com/v1alpha1", "kind": "NicClusterPolicy", "metadata": map[string]interface{}{"name": "policy"}, "spec": map[string]interface{}{}, "status": map[string]interface{}{"state": "notReady"}}}
					}
					require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0600))
					objects = append(objects, live)
				}
				clientCreated, observed, connected := false, false, false
				runner := validateRunner{
					newKubeClient: func(string) (ctrlclient.Client, *rest.Config, error) {
						clientCreated = true
						if mode == "client-error" {
							return nil, nil, errors.New("test cluster unavailable")
						}
						return fake.NewClientBuilder().WithObjects(objects...).Build(), nil, nil
					},
					assessNVAIE: func(ctx context.Context, _ *rest.Config, release string, workers []string, evidence *nvaie.DOCAHostEvidence) *nvaie.Assessment {
						observed = true
						require.NotNil(t, evidence)
						require.Len(t, evidence.Observations, 1)
						assert.Equal(t, "3.6.0-070000", evidence.Observations[0].Version)
						result := nvaie.Assess(ctx, k8sfake.NewClientset(), release, workers, nil, evidence)
						require.Len(t, result.Components, 7, "mismatched DOCA must not suppress other components")
						return result
					},
					runConnectivityMatrix: func(context.Context, ctrlclient.Client, *rest.Config, ui.Output, *bundle.Bundle, connectivity.Options) (*connectivity.MatrixResult, error) {
						connected = true
						return &connectivity.MatrixResult{PingResults: []connectivity.PingResult{{Test: connectivity.PingTest{Kind: connectivity.Gateway}, OK: true}}, Summary: connectivity.MatrixSummary{TotalTests: 1, Passed: 1}}, nil
					},
				}
				reportDir := t.TempDir()
				request := ValidateRequest{UserConfig: path, Kubeconfig: "test", DeploymentFiles: dir, ReportPath: filepath.Join(reportDir, "report.html"), JUnitPath: filepath.Join(reportDir, "report.xml"), Checks: Explicit[[]string]{Set: true, Value: []string{"gateway"}}, OutputFormat: "json"}
				if mode != "connectivity-only" && mode != "full" {
					request.Connectivity = Explicit[bool]{Set: true, Value: false}
				}
				outputPath := filepath.Join(reportDir, "stdout.jsonl")
				stdout, err := os.Create(outputPath)
				require.NoError(t, err)
				previousStdout := os.Stdout
				os.Stdout = stdout
				t.Cleanup(func() { os.Stdout = previousStdout; _ = stdout.Close() })
				runErr := runner.Run(context.Background(), request)
				os.Stdout = previousStdout
				require.NoError(t, stdout.Close())
				require.Error(t, runErr)
				expectedExit := apperrors.ExitDeployment
				if mode == "client-error" {
					expectedExit = apperrors.ExitCluster
				}
				assert.Equal(t, expectedExit, apperrors.ExitCodeFromError(runErr))
				assert.True(t, clientCreated)
				assert.Equal(t, nvaieEnabled && mode != "client-error", observed)
				assert.Equal(t, mode == "connectivity-only" || mode == "full", connected)
				html, err := os.ReadFile(request.ReportPath)
				require.NoError(t, err)
				assert.Contains(t, string(html), "VALIDATION FAILED")
				assert.Contains(t, string(html), "does not match Network Operator 26.7 release SBOM DOCA 3.5.0")
				if nvaieEnabled {
					assert.Contains(t, string(html), "3.6.0 (discovery snapshot)")
					if mode != "client-error" {
						for _, name := range []string{nvaie.NetworkOperator, nvaie.GPUOperator, "NIM Operator", "Run:ai", nvaie.DOCADriver, nvaie.GPUDriver, nvaie.ContainerToolkit} {
							assert.Contains(t, string(html), "<td>"+name)
						}
						assert.NotContains(t, string(html), "Live NVAIE observations have not completed")
					}
				}
				output, err := os.ReadFile(outputPath)
				require.NoError(t, err)
				assert.Contains(t, string(output), `"docaHostIssues"`)
				assert.Contains(t, string(output), "does not match Network Operator 26.7 release SBOM DOCA 3.5.0")
				if mode == "static" || mode == "full" || mode == "in-progress" {
					assert.Contains(t, string(output), `"manifests"`)
				}
				junitData, err := os.ReadFile(request.JUnitPath)
				require.NoError(t, err)
				var junit junitDocument
				require.NoError(t, xml.Unmarshal(junitData, &junit))
				found := false
				for _, suite := range junit.Suites {
					for _, testCase := range suite.Cases {
						if testCase.Name == "DOCAHostCompatibility" {
							found = true
							require.NotNil(t, testCase.Failure)
							assert.Contains(t, testCase.Failure.Message, "does not match")
						}
					}
				}
				assert.True(t, found, "JUnit must retain the compatibility failure even without NVAIE")
			})
		}
	}
}
