// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/connectivity"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/nvidia/k8s-launch-kit/pkg/presetmatch"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

func TestJUnitCatalogNamesAndPairResults(t *testing.T) {
	for _, fabric := range []string{"ethernet", "infiniband"} {
		t.Run(fabric, func(t *testing.T) {
			matrix := &connectivity.MatrixResult{StageDurations: map[connectivity.Check]time.Duration{connectivity.CheckRPing: 1250 * time.Millisecond}}
			for _, kind := range []connectivity.PingTestKind{connectivity.ICMPSameRail, connectivity.RDMAPingSameRail, connectivity.RDMABwSameRail, connectivity.GPUDirectDMABufSameRail} {
				matrix.PingResults = append(matrix.PingResults, connectivity.PingResult{
					Test: connectivity.PingTest{Kind: kind, SrcNode: "worker-a", DstNode: "worker-b", SrcPod: "pod-a", DstPod: "pod-b", SrcRail: "rail-0", DstRail: "rail-0", SrcIP: "192.0.2.1", DstIP: "192.0.2.2"},
					OK:   true, ObservedOK: true, Expectation: connectivity.ExpectRequired,
				})
			}
			// An unsuccessful observation is successful expected isolation.
			isolation := matrix.PingResults[1]
			isolation.Test.Kind = connectivity.RDMAPingCrossRail
			isolation.Test.DstRail = "rail-1"
			isolation.Expectation = connectivity.ExpectForbidden
			isolation.ObservedOK = false
			matrix.PingResults = append(matrix.PingResults, isolation)
			observation := isolation
			observation.Test.SrcRail = "rail-2"
			observation.Expectation = connectivity.ExpectObserve
			matrix.PingResults = append(matrix.PingResults, observation)
			failed := matrix.PingResults[1]
			failed.Test.SrcPod = "pod-c"
			failed.OK = false
			failed.Err = errors.New("timeout <waiting> & retry")
			failed.Stderr = "stderr & <details>"
			matrix.PingResults = append(matrix.PingResults, failed)
			input := junitInput{Configured: true, Enabled: true, Fabric: fabric, Data: connectivity.ReportData{Matrix: matrix}}
			path := filepath.Join(t.TempDir(), "nested", "junit.xml")
			require.NoError(t, writeJUnitReport(path, input))
			bytes, err := os.ReadFile(path)
			require.NoError(t, err)
			var doc junitDocument
			require.NoError(t, xml.Unmarshal(bytes, &doc))
			require.Len(t, doc.Suites, 4)
			expected := []string{"K8sEastWestNetworkICMPPing-", "K8sEastWestNetworkRDMAPing-", "K8sEastWestNetworkIBWriteBandwidth-", "K8sEastWestNetworkDMABufBandwidth-"}
			for i, name := range expected {
				assert.Equal(t, name+fabric, doc.Suites[i].Name)
			}
			suite := doc.Suites[1]
			assert.Equal(t, 4, suite.Tests)
			assert.Equal(t, 1, suite.Failures)
			assert.Zero(t, suite.Errors)
			assert.Zero(t, suite.Skipped)
			assert.Equal(t, "1.250", suite.Time)
			seen := map[string]bool{}
			for _, c := range suite.Cases {
				assert.False(t, seen[c.Name])
				seen[c.Name] = true
				assert.True(t, strings.HasPrefix(c.Name, suite.Name+"::"))
				if c.Failure != nil {
					assert.Equal(t, failed.Err.Error(), c.Failure.Message)
					assert.Equal(t, failed.Stderr, c.Err)
				}
			}
		})
	}
}

func TestJUnitMissingCoverageAndPartialRun(t *testing.T) {
	for _, runErr := range []error{nil, apperrors.NewExitStatus(4), errors.New("setup interrupted")} {
		input := junitInput{Configured: true, Enabled: true, Fabric: "ethernet", Checks: []connectivity.Check{connectivity.CheckRPing}, RunError: runErr}
		doc := buildJUnit(input)
		offset := 0
		if runErr != nil {
			offset = 1
			assert.Equal(t, "network/validation", doc.Suites[0].Name)
			assert.Equal(t, 1, doc.Suites[0].Failures+doc.Suites[0].Errors)
		}
		assert.Len(t, doc.Suites, 4+offset)
		for _, s := range doc.Suites[offset:] {
			assert.Equal(t, 1, s.Tests)
			assert.Equal(t, 1, s.Skipped)
		}
	}
	partial := buildJUnit(junitInput{Configured: true, Enabled: true, Fabric: "infiniband", RunError: errors.New("interrupted"), Data: connectivity.ReportData{Matrix: &connectivity.MatrixResult{PingResults: []connectivity.PingResult{{Test: connectivity.PingTest{Kind: connectivity.ICMPSameRail}, OK: true}}}}})
	require.Len(t, partial.Suites, 5)
	assert.Equal(t, 1, partial.Suites[0].Errors)
	assert.Equal(t, 1, partial.Suites[1].Tests)
	assert.Zero(t, partial.Suites[1].Skipped)
}

func TestJUnitFabricRequiresExplicitKnownValue(t *testing.T) {
	for _, cfg := range []*config.LaunchKitConfig{nil, {}, {Profile: &config.Profile{Fabric: "unknown"}}} {
		_, err := junitFabric(cfg)
		require.Error(t, err)
	}
	fabric, err := junitFabric(&config.LaunchKitConfig{Profile: &config.Profile{Fabric: "InfiniBand"}})
	require.NoError(t, err)
	assert.Equal(t, "infiniband", fabric)
}

func TestValidateJUnitRunner(t *testing.T) {
	for _, outcome := range []string{"pass", "partial-error", "write-error", "cluster-write-error", "deployment-write-error"} {
		t.Run(outcome, func(t *testing.T) {
			dir, cfgPath := writeConnectivityOnlyInputs(t, config.RoutingSourceBased)
			cfg, err := os.ReadFile(cfgPath)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(cfgPath, []byte(strings.Replace(string(cfg), "profile:", "profile:\n  fabric: ethernet", 1)), 0o600))
			path := filepath.Join(t.TempDir(), "report.xml")
			if strings.HasSuffix(outcome, "write-error") {
				require.NoError(t, os.Mkdir(path, 0o700))
			}
			runner := validateRunner{
				newKubeClient: func(string) (ctrlclient.Client, *rest.Config, error) { return nil, &rest.Config{}, nil },
				runConnectivityMatrix: func(context.Context, ctrlclient.Client, *rest.Config, ui.Output, *bundle.Bundle, connectivity.Options) (*connectivity.MatrixResult, error) {
					m := &connectivity.MatrixResult{PingResults: []connectivity.PingResult{
						{Test: connectivity.PingTest{Kind: connectivity.ICMPSameRail}, OK: true},
						{Test: connectivity.PingTest{Kind: connectivity.RDMAPingSameRail}, OK: true},
						{Test: connectivity.PingTest{Kind: connectivity.RDMABwSameRail}, OK: true},
					}, Summary: connectivity.MatrixSummary{TotalTests: 3, Passed: 3}}
					if outcome == "deployment-write-error" {
						m.PingResults[0].OK = false
						m.Summary.Failed = 1
						m.Summary.Passed = 2
					}
					if outcome == "partial-error" || outcome == "cluster-write-error" {
						return m, errors.New("matrix interrupted")
					}
					return m, nil
				},
			}
			err = runner.Run(context.Background(), ValidateRequest{Kubeconfig: "test-kubeconfig", DeploymentFiles: dir, UserConfig: cfgPath, ReportPath: "-", JUnitPath: path, OutputFormat: "json"})
			if outcome == "pass" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if strings.HasSuffix(outcome, "write-error") {
				assert.Contains(t, err.Error(), "failed to write JUnit report")
				switch outcome {
				case "write-error":
					assert.Contains(t, err.Error(), "failed to write JUnit report")
					assert.Equal(t, apperrors.ExitGeneral, apperrors.ExitCodeFromError(err))
				case "cluster-write-error":
					assert.Contains(t, err.Error(), "matrix interrupted")
					assert.Equal(t, apperrors.ExitCluster, apperrors.ExitCodeFromError(err))
				case "deployment-write-error":
					assert.Equal(t, apperrors.ExitDeployment, apperrors.ExitCodeFromError(err))
				}
				leftovers, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), ".l8k-junit-*"))
				require.NoError(t, globErr)
				assert.Empty(t, leftovers)
				return
			}
			bytes, err := os.ReadFile(path)
			require.NoError(t, err)
			var doc junitDocument
			require.NoError(t, xml.Unmarshal(bytes, &doc))
			if outcome == "partial-error" {
				assert.Equal(t, 1, doc.Suites[0].Errors)
			}
			assert.Contains(t, string(bytes), "K8sEastWestNetworkRDMAPing-ethernet::")
		})
	}
}

func TestJUnitStaticCheckOutcomes(t *testing.T) {
	doc := buildJUnit(junitInput{Configured: true, Enabled: false, Duration: 2 * time.Second, Data: connectivity.ReportData{
		Release:        &networkoperatorplugin.VersionCheck{Skipped: true, Reason: "Externally managed operator"},
		ComponentCheck: &networkoperatorplugin.ComponentVersionCheck{AllMatch: false, Reason: "Wrong version"},
		HelmValues:     &networkoperatorplugin.HelmValuesCheck{AllMatch: true},
		Manifests: []networkoperatorplugin.ValidationResult{
			{Kind: "NicClusterPolicy", Name: "ready", State: crstate.StateSuccess},
			{Kind: "NicNodePolicy", Name: "pending", State: crstate.StateInProgress, Reason: "Reconciling"},
			{Kind: "SriovNetwork", Name: "missing", State: crstate.StateNotDeployed, Reason: "Not found"},
		},
	}})
	require.Len(t, doc.Suites, 1)
	suite := doc.Suites[0]
	assert.Equal(t, 7, suite.Tests)
	assert.Equal(t, 2, suite.Failures)
	assert.Equal(t, 3, suite.Skipped)
	assert.Zero(t, suite.Errors)
	assert.Equal(t, "2.000", suite.Time)
	assert.Equal(t, "Externally managed operator", suite.Cases[0].Skipped.Message)
}

func TestValidateJUnitMissingFabricRetainsError(t *testing.T) {
	dir, cfgPath := writeConnectivityOnlyInputs(t, config.RoutingSourceBased)
	path := filepath.Join(t.TempDir(), "report.xml")
	// No client hooks: fabric validation must happen before contacting Kubernetes.
	runner := validateRunner{}
	err := runner.Run(context.Background(), ValidateRequest{Kubeconfig: "test-kubeconfig", DeploymentFiles: dir, UserConfig: cfgPath, ReportPath: "-", JUnitPath: path, OutputFormat: "json"})
	require.ErrorContains(t, err, "profile.fabric")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc junitDocument
	require.NoError(t, xml.Unmarshal(data, &doc))
	require.Len(t, doc.Suites, 1)
	assert.Equal(t, 1, doc.Suites[0].Errors)
}

func TestJUnitCoverageFailureIndependentOfOtherFailures(t *testing.T) {
	for _, required := range []bool{true, false} {
		input := junitInput{RequireCoverage: required, Checks: []connectivity.Check{connectivity.CheckICMP, connectivity.CheckRPing}, RunError: apperrors.NewExitStatus(4), Data: connectivity.ReportData{
			ComponentCheck: &networkoperatorplugin.ComponentVersionCheck{AllMatch: false, Reason: "Version mismatch"},
			Matrix: &connectivity.MatrixResult{PingResults: []connectivity.PingResult{
				{Test: connectivity.PingTest{Kind: connectivity.ICMPSameRail}, OK: true, Expectation: connectivity.ExpectRequired},
				{Test: connectivity.PingTest{Kind: connectivity.RDMAPingCrossRail}, OK: true, Expectation: connectivity.ExpectObserve},
			}, Summary: connectivity.MatrixSummary{TotalTests: 2, Passed: 2}},
		}}
		doc := buildJUnit(input)
		var coverage *junitCase
		for i := range doc.Suites[0].Cases {
			if doc.Suites[0].Cases[i].Name == "ConnectivityCoverage" {
				coverage = &doc.Suites[0].Cases[i]
			}
		}
		if required {
			require.NotNil(t, coverage)
			require.NotNil(t, coverage.Failure)
			assert.Contains(t, coverage.Failure.Message, `"rping" did not produce any gating tests`)
			assert.Equal(t, 2, doc.Suites[0].Failures)
		} else {
			assert.Nil(t, coverage)
			assert.Equal(t, 1, doc.Suites[0].Failures)
		}
	}
}

func TestJUnitPresetOutcomes(t *testing.T) {
	doc := buildJUnit(junitInput{Data: connectivity.ReportData{PresetMatches: []presetmatch.Result{
		{Group: "matching", Status: presetmatch.StatusMatch},
		{Group: "deviating", Status: presetmatch.StatusDeviation, Reason: "PF count differs"},
		{Group: "unknown", Status: presetmatch.StatusNotFound, Reason: "No preset"},
		{Group: "skipped", Status: presetmatch.StatusSkipped, Reason: "Missing hardware identity"},
	}}})
	require.Len(t, doc.Suites, 1)
	suite := doc.Suites[0]
	assert.Equal(t, 4, suite.Tests)
	assert.Equal(t, 1, suite.Failures)
	assert.Equal(t, 2, suite.Skipped)
	assert.Equal(t, "TopologyPresets::matching", suite.Cases[0].Name)
	assert.Nil(t, suite.Cases[0].Failure)
	assert.Nil(t, suite.Cases[0].Skipped)
	assert.Equal(t, "PF count differs", suite.Cases[1].Failure.Message)
	assert.Equal(t, "No preset", suite.Cases[2].Skipped.Message)
	assert.Equal(t, "Missing hardware identity", suite.Cases[3].Skipped.Message)
}
