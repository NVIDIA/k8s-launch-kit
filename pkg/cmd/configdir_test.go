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

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/assets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserConfigPathForConfigDir(t *testing.T) {
	originalUserConfig := userConfig
	originalDeploymentFiles := deploymentFiles
	originalWD, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() {
		userConfig = originalUserConfig
		deploymentFiles = originalDeploymentFiles
		require.NoError(t, os.Chdir(originalWD))
	})
	userConfig = ""
	deploymentFiles = ""

	workingDir := t.TempDir()
	require.NoError(t, os.Chdir(workingDir))
	configRoot := t.TempDir()
	overridePath := filepath.Join(configRoot, assets.DefaultConfigName)
	require.NoError(t, os.WriteFile(overridePath, []byte("networkOperator: {}\n"), 0o644))

	got, err := userConfigPathFor(configRoot)
	require.NoError(t, err)
	assert.Equal(t, overridePath, got)

	clusterPath := filepath.Join(workingDir, "cluster-config.yaml")
	require.NoError(t, os.WriteFile(clusterPath, []byte("clusterConfig: []\n"), 0o644))
	got, err = userConfigPathFor(configRoot)
	require.NoError(t, err)
	assert.Equal(t, defaultUserConfigPath, got, "a discovered cluster config must outrank default overrides")
}

func TestUserConfigPathForGenerateDefersConfigDirResolution(t *testing.T) {
	originalUserConfig := userConfig
	originalDeploymentFiles := deploymentFiles
	originalWD, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() {
		userConfig = originalUserConfig
		deploymentFiles = originalDeploymentFiles
		require.NoError(t, os.Chdir(originalWD))
	})
	userConfig = ""
	deploymentFiles = ""

	workingDir := t.TempDir()
	require.NoError(t, os.Chdir(workingDir))
	missingConfigRoot := filepath.Join(t.TempDir(), "missing")

	assert.Empty(t, userConfigPathForGenerate(missingConfigRoot),
		"generate must leave config-dir validation and fallback to the launcher")

	require.NoError(t, os.WriteFile(defaultUserConfigPath, []byte("clusterConfig: []\n"), 0o644))
	assert.Equal(t, defaultUserConfigPath, userConfigPathForGenerate(missingConfigRoot),
		"a discovered cluster config must still outrank config-dir defaults")
}

func TestPresetCatalogForConfigDirFallsBackPerAsset(t *testing.T) {
	configRoot := t.TempDir()
	catalog, resolved, err := presetCatalogForConfigDir(configRoot)
	require.NoError(t, err)
	assert.Equal(t, "embedded", catalog.Source())
	assert.Empty(t, resolved.DefaultConfigPath)
	assert.Empty(t, resolved.PresetsDir)

	require.NoError(t, os.WriteFile(
		filepath.Join(configRoot, assets.DefaultConfigName),
		[]byte("networkOperator: {}\n"),
		0o644,
	))

	catalog, resolved, err = presetCatalogForConfigDir(configRoot)
	require.NoError(t, err)
	assert.Equal(t, "embedded", catalog.Source())
	assert.Empty(t, resolved.PresetsDir)

	presetsDir := filepath.Join(configRoot, assets.PresetsDirName)
	require.NoError(t, os.Mkdir(presetsDir, 0o755))
	catalog, resolved, err = presetCatalogForConfigDir(configRoot)
	require.NoError(t, err)
	assert.Equal(t, presetsDir, catalog.Source())
	assert.Equal(t, presetsDir, resolved.PresetsDir)
}

func TestConfigDirFlagIsPersistent(t *testing.T) {
	flag := rootCmd.PersistentFlags().Lookup("config-dir")
	require.NotNil(t, flag)
	assert.Equal(t, "string", flag.Value.Type())
}
