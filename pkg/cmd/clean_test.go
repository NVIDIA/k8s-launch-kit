// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

func preserveCleanFlagState(t *testing.T) {
	t.Helper()
	oldNamespace := networkOperatorNamespace
	oldUserConfig := userConfig
	oldDeploymentFiles := deploymentFiles
	oldConfigDir := configDir
	oldKeepHelmChart := keepHelmChart
	t.Cleanup(func() {
		networkOperatorNamespace = oldNamespace
		userConfig = oldUserConfig
		deploymentFiles = oldDeploymentFiles
		configDir = oldConfigDir
		keepHelmChart = oldKeepHelmChart
	})
}

func TestCleanCommandFlags(t *testing.T) {
	for _, name := range []string{
		"kubeconfig",
		"user-config",
		"network-operator-namespace",
		"keep-helm-chart",
	} {
		assert.NotNil(t, cleanCmd.Flags().Lookup(name), "missing --%s", name)
	}
	assert.Equal(t, "false", cleanCmd.Flags().Lookup("keep-helm-chart").DefValue)
}

func TestResolveCleanSettings(t *testing.T) {
	t.Run("explicit namespace wins while config retention still applies", func(t *testing.T) {
		preserveCleanFlagState(t)
		path := filepath.Join(t.TempDir(), "cluster-config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(`networkOperator:
  namespace: config-namespace
  skipHelmChart: true
`), 0o600))
		networkOperatorNamespace = "explicit-namespace"
		userConfig = path

		settings, err := resolveCleanSettings(false)
		require.NoError(t, err)
		assert.Equal(t, "explicit-namespace", settings.Namespace)
		assert.Equal(t, "--network-operator-namespace", settings.NamespaceSource)
		assert.True(t, settings.KeepHelmChart)
		assert.Contains(t, settings.HelmRetentionSource, "networkOperator.skipHelmChart")
		assert.Contains(t, settings.HelmRetentionSource, path)
	})

	t.Run("reads namespace and retention from user config", func(t *testing.T) {
		preserveCleanFlagState(t)
		path := filepath.Join(t.TempDir(), "cluster-config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(`networkOperator:
  namespace: operator-system
  skipHelmChart: true
unrelatedSetting:
  oldField: still-ignored
`), 0o600))
		networkOperatorNamespace = ""
		userConfig = path

		settings, err := resolveCleanSettings(false)
		require.NoError(t, err)
		assert.Equal(t, "operator-system", settings.Namespace)
		assert.Equal(t, path, settings.NamespaceSource)
		assert.True(t, settings.KeepHelmChart)
		assert.Contains(t, settings.HelmRetentionSource, path)
	})

	t.Run("explicit keep flag retains without config", func(t *testing.T) {
		preserveCleanFlagState(t)
		t.Chdir(t.TempDir())
		networkOperatorNamespace = ""
		userConfig = ""
		deploymentFiles = ""
		configDir = ""

		settings, err := resolveCleanSettings(true)
		require.NoError(t, err)
		assert.True(t, settings.KeepHelmChart)
		assert.Equal(t, "--keep-helm-chart", settings.HelmRetentionSource)
	})

	t.Run("false config setting leaves Helm uninstall enabled", func(t *testing.T) {
		preserveCleanFlagState(t)
		path := filepath.Join(t.TempDir(), "cluster-config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(`networkOperator:
  skipHelmChart: false
`), 0o600))
		networkOperatorNamespace = ""
		userConfig = path

		settings, err := resolveCleanSettings(false)
		require.NoError(t, err)
		assert.False(t, settings.KeepHelmChart)
		assert.Empty(t, settings.HelmRetentionSource)
	})

	t.Run("reports malformed user config", func(t *testing.T) {
		preserveCleanFlagState(t)
		path := filepath.Join(t.TempDir(), "cluster-config.yaml")
		require.NoError(t, os.WriteFile(path, []byte("networkOperator: [\n"), 0o600))
		networkOperatorNamespace = ""
		userConfig = path

		_, err := resolveCleanSettings(false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse")
		assert.Contains(t, err.Error(), path)
	})

	t.Run("reads an explicit config directory override", func(t *testing.T) {
		preserveCleanFlagState(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "l8k-config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(`networkOperator:
  namespace: config-dir-operator
  skipHelmChart: true
`), 0o600))
		networkOperatorNamespace = ""
		userConfig = ""
		deploymentFiles = ""
		configDir = dir

		settings, err := resolveCleanSettings(false)
		require.NoError(t, err)
		assert.Equal(t, "config-dir-operator", settings.Namespace)
		assert.Equal(t, path, settings.NamespaceSource)
		assert.True(t, settings.KeepHelmChart)
	})

	t.Run("uses the standard default without trusted config", func(t *testing.T) {
		preserveCleanFlagState(t)
		t.Chdir(t.TempDir())
		networkOperatorNamespace = ""
		userConfig = ""
		deploymentFiles = ""
		configDir = ""

		settings, err := resolveCleanSettings(false)
		require.NoError(t, err)
		assert.Equal(t, defaultOperatorNamespace, settings.Namespace)
		assert.Equal(t, "default", settings.NamespaceSource)
		assert.False(t, settings.KeepHelmChart)
		assert.Empty(t, settings.HelmRetentionSource)
	})
}

func TestFinalizeCleanJSON(t *testing.T) {
	var stdout bytes.Buffer
	jsonOutput := ui.NewJSON(&stdout, &bytes.Buffer{})

	finalizeCleanJSON(jsonOutput, "operator-system", 7, false, true)

	var result ui.JSONResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	assert.True(t, result.Success)
	assert.Equal(t, "clean", result.Phase)
	require.NotNil(t, result.Cleanup)
	assert.Equal(t, "operator-system", result.Cleanup.Namespace)
	assert.Equal(t, 7, result.Cleanup.CustomResourcesDeleted)
	assert.False(t, result.Cleanup.HelmReleaseRemoved)
	assert.True(t, result.Cleanup.KeepHelmChart)
}
