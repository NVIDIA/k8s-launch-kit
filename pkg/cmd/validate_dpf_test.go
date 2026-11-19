// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
)

func TestDPFValidateFlagScopes(t *testing.T) {
	for _, name := range []string{"deployment-files", "user-config", "connectivity", "connectivity-timeout", "wait", "report-path"} {
		actual, err := flagTargetScope(validateCmd.Flags().Lookup(name))
		require.NoError(t, err)
		assert.Equal(t, []target.Name{target.Host, target.DPF}, actual, name)
	}
	actual, err := flagTargetScope(validateCmd.Flags().Lookup("inventory"))
	require.NoError(t, err)
	assert.Equal(t, []target.Name{target.DPF}, actual)
	actual, err = flagTargetScope(validateCmd.Flags().Lookup("quiet"))
	require.NoError(t, err)
	assert.Equal(t, []target.Name{target.DPF}, actual)
	for _, name := range []string{"flavor", "kubeconfig", "keep", "junit-path", "validation-mode", "validation-checks", "network-operator-namespace", "skip-network-operator-helm", "rdma-rping-iterations", "rdma-ib-write-size", "rdma-ib-write-min-bandwidth-gbps"} {
		actual, err := flagTargetScope(validateCmd.Flags().Lookup(name))
		require.NoError(t, err)
		assert.Equal(t, []target.Name{target.Host}, actual, name)
	}
}

func TestDPFValidateRequestUsesExplicitInputPathsAndPresence(t *testing.T) {
	originalInventory, originalBundle, originalConfig := dpfInventoryPath, deploymentFiles, userConfig
	originalWait, originalReport := validateWait, validateReportPath
	t.Cleanup(func() {
		dpfInventoryPath, deploymentFiles, userConfig = originalInventory, originalBundle, originalConfig
		validateWait, validateReportPath = originalWait, originalReport
		resetValidateRequestGlobals(t)
	})
	resetValidateRequestGlobals(t)
	command := &cobra.Command{}
	command.Flags().StringVar(&dpfInventoryPath, "inventory", "", "")
	command.Flags().StringVar(&deploymentFiles, "deployment-files", DefaultDeploymentDir, "")
	command.Flags().StringVar(&userConfig, "user-config", "ambient-host-config.yaml", "")
	command.Flags().BoolVar(&validateConnectivity, "connectivity", true, "")
	command.Flags().DurationVar(&validateConnectivityTimeout, "connectivity-timeout", 0, "")

	request := newDPFValidateRequest(command)
	assert.Equal(t, validation.ModeFull, request.Mode)
	assert.Empty(t, request.DeploymentFiles)
	assert.Empty(t, request.UserConfig)
	assert.False(t, request.ConnectivityTimeoutSet)
	require.NoError(t, command.ParseFlags([]string{"--inventory", "inventory.yaml", "--deployment-files", "bundle", "--user-config", "access.yaml", "--connectivity=false", "--connectivity-timeout=0"}))
	validateWait, validateReportPath = 3*time.Minute, "report.html"
	request = newDPFValidateRequest(command)
	assert.Equal(t, "inventory.yaml", request.Inventory)
	assert.Equal(t, "bundle", request.DeploymentFiles)
	assert.Equal(t, "access.yaml", request.UserConfig)
	assert.Equal(t, validation.ModeResources, request.Mode)
	assert.True(t, request.ConnectivityTimeoutSet)
	assert.Zero(t, request.ConnectivityTimeout)
	assert.Equal(t, 3*time.Minute, request.Wait)
	assert.Equal(t, "report.html", request.ReportPath)
}

func TestDPFValidateCLIInputsAndScopes(t *testing.T) {
	for _, flags := range [][]string{
		{}, {"--inventory", "/missing/inventory"}, {"--deployment-files", "/missing/bundle"},
	} {
		args := append([]string{"validate", "--target", "dpf", "--output", "json", "--quiet"}, flags...)
		output, code := runCLIHelper(t, args...)
		assert.Equal(t, 2, code, output)
		assert.Contains(t, output, "DPF validate requires --inventory dpf-discovered.yaml and --deployment-files ./hbn-bgp")
	}
	for _, flag := range []string{"--keep=false", "--skip-network-operator-helm=false", "--kubeconfig=missing", "--flavor=k8s", "--junit-path=report.xml", "--validation-mode=full", "--validation-checks=icmp", "--rdma-rping-iterations=1"} {
		output, code := runCLIHelper(t, "validate", "--target", "dpf", flag, "--output", "json", "--quiet")
		assert.Equal(t, 2, code, output)
		assert.Contains(t, output, "targets: host", flag)
		assert.NotContains(t, output, "DPF validate requires", flag)
	}
	for _, flags := range [][]string{
		{"--connectivity=false", "--connectivity-timeout=0"},
		{"--connectivity=false", "--connectivity-timeout=1m"},
		{"--wait=-1s"}, {"--connectivity-timeout=-1s"},
	} {
		args := append([]string{"validate", "--target", "dpf", "--inventory", "missing", "--deployment-files", "missing", "--output", "json", "--quiet"}, flags...)
		output, code := runCLIHelper(t, args...)
		assert.Equal(t, 2, code, output)
		assert.Contains(t, output, "invalid target invocation")
		assert.NotContains(t, output, `"checks"`)
	}
}

func TestDPFValidatePreparationEmitsOneCanonicalResultWithoutHostFallback(t *testing.T) {
	for _, flags := range [][]string{{}, {"--connectivity=false"}, {"--connectivity-timeout=0"}} {
		args := append([]string{"validate", "--target", "dpf", "--inventory", "/missing/inventory.yaml", "--deployment-files", "/missing/bundle", "--report-path=-", "--output", "json", "--quiet"}, flags...)
		output, code := runCLIHelper(t, args...)
		assert.Equal(t, 2, code, output)
		var result validation.Result
		require.NoError(t, json.Unmarshal([]byte(output), &result), output)
		assert.Equal(t, validation.FatalInput, result.Fatal)
		assert.Equal(t, validation.VerdictIncomplete, result.Verdict)
		assert.Equal(t, validation.ReportDisabled, result.Report.Status)
		require.Len(t, result.Checks, 1)
		assert.Equal(t, validation.AreaPreparation, result.Checks[0].Area)
		assert.NotEmpty(t, result.Checks[0].Outcome.Action)
		assert.NotContains(t, output, "network-operator")
		assert.NotContains(t, output, "kubeconfig")
		assert.NotContains(t, output, "does not implement phase")
		assert.NotContains(t, output, "no such file")
		if len(flags) > 0 && flags[0] == "--connectivity=false" {
			assert.Equal(t, validation.ModeResources, result.Mode)
		} else {
			assert.Equal(t, validation.ModeFull, result.Mode)
		}
	}
}

func TestDPFValidateSchemaSharesFlagsAndKeepsPipelineUnavailable(t *testing.T) {
	output, code := runCLIHelper(t, "schema")
	require.Zero(t, code)
	var s schema
	require.NoError(t, json.Unmarshal([]byte(output), &s))
	for _, name := range []string{"--connectivity", "--connectivity-timeout", "--wait", "--report-path", "--deployment-files", "--user-config"} {
		assert.Equal(t, []string{"host", "dpf"}, s.Flags[name].Targets, name)
	}
	assert.Equal(t, []string{"host"}, s.Flags["--validation-mode"].Targets)
	assert.Contains(t, s.Flags["--quiet"].Description, "With validate, supported only for --target dpf")
	for _, descriptor := range s.Targets {
		if descriptor.Name == "dpf" {
			assert.True(t, descriptor.Phases["validate"].Available)
			assert.False(t, descriptor.Phases["pipeline"].Available)
		}
	}
}

func TestValidateQuietIsRejectedForHost(t *testing.T) {
	for _, flags := range [][]string{{"--quiet"}, {"--target", "host", "--quiet=false"}} {
		args := append([]string{"validate", "--output", "json"}, flags...)
		output, code := runCLIHelper(t, args...)
		assert.Equal(t, 2, code, output)
		assert.Contains(t, output, "--quiet (targets: dpf)")
		assert.NotContains(t, output, "deployment files directory")
	}
}
