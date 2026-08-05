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
	t.Cleanup(func() {
		networkOperatorNamespace = oldNamespace
		userConfig = oldUserConfig
		deploymentFiles = oldDeploymentFiles
		configDir = oldConfigDir
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

func TestConfiguredCleanNamespace(t *testing.T) {
	t.Run("explicit flag wins", func(t *testing.T) {
		preserveCleanFlagState(t)
		networkOperatorNamespace = "explicit-namespace"
		userConfig = filepath.Join(t.TempDir(), "does-not-exist.yaml")

		namespace, source, err := configuredCleanNamespace()
		require.NoError(t, err)
		assert.Equal(t, "explicit-namespace", namespace)
		assert.Equal(t, "--network-operator-namespace", source)
	})

	t.Run("reads only namespace from user config", func(t *testing.T) {
		preserveCleanFlagState(t)
		path := filepath.Join(t.TempDir(), "cluster-config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(`networkOperator:
  namespace: operator-system
unrelatedSetting:
  oldField: still-ignored
`), 0o600))
		networkOperatorNamespace = ""
		userConfig = path

		namespace, source, err := configuredCleanNamespace()
		require.NoError(t, err)
		assert.Equal(t, "operator-system", namespace)
		assert.Equal(t, path, source)
	})

	t.Run("reports malformed user config", func(t *testing.T) {
		preserveCleanFlagState(t)
		path := filepath.Join(t.TempDir(), "cluster-config.yaml")
		require.NoError(t, os.WriteFile(path, []byte("networkOperator: [\n"), 0o600))
		networkOperatorNamespace = ""
		userConfig = path

		_, source, err := configuredCleanNamespace()
		require.Error(t, err)
		assert.Equal(t, path, source)
		assert.Contains(t, err.Error(), "parse")
	})

	t.Run("reads an explicit config directory override", func(t *testing.T) {
		preserveCleanFlagState(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "l8k-config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(`networkOperator:
  namespace: config-dir-operator
`), 0o600))
		networkOperatorNamespace = ""
		userConfig = ""
		deploymentFiles = ""
		configDir = dir

		namespace, source, err := configuredCleanNamespace()
		require.NoError(t, err)
		assert.Equal(t, "config-dir-operator", namespace)
		assert.Equal(t, path, source)
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
