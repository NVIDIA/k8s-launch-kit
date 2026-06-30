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

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

func TestNormalizeMaintenanceDefaults(t *testing.T) {
	config := &LaunchKitConfig{}
	require.NoError(t, NormalizeMaintenance(config))
	require.NotNil(t, config.Maintenance)

	assert.Equal(t, "4", config.Maintenance.MaxParallelOperations.String())
	assert.Equal(t, "4", config.Maintenance.MaxUnavailable.String())
	assert.Equal(t, int32(3600), *config.Maintenance.MaxNodeMaintenanceTimeSeconds)
	assert.Equal(t, 4, *config.Maintenance.MaxParallelUpgrades)
}

func TestNormalizeMaintenancePartialPreservesExplicitZero(t *testing.T) {
	zeroSeconds := int32(0)
	zeroUpgrades := 0
	config := &LaunchKitConfig{
		Maintenance: &MaintenanceConfig{
			MaxUnavailable:                IntOrPercentFromInt32(0),
			MaxNodeMaintenanceTimeSeconds: &zeroSeconds,
			MaxParallelUpgrades:           &zeroUpgrades,
		},
	}

	require.NoError(t, NormalizeMaintenance(config))
	assert.Equal(t, "4", config.Maintenance.MaxParallelOperations.String())
	assert.Equal(t, "0", config.Maintenance.MaxUnavailable.String())
	assert.Zero(t, *config.Maintenance.MaxNodeMaintenanceTimeSeconds)
	assert.Zero(t, *config.Maintenance.MaxParallelUpgrades)
}

func TestDefaultMaintenanceConfigReturnsFreshCopy(t *testing.T) {
	first := DefaultMaintenanceConfig()
	second := DefaultMaintenanceConfig()

	first.MaxUnavailable = IntOrPercentFromInt32(0)
	*first.MaxNodeMaintenanceTimeSeconds = 1
	*first.MaxParallelUpgrades = 1

	assert.Equal(t, "4", second.MaxUnavailable.String())
	assert.Equal(t, int32(3600), *second.MaxNodeMaintenanceTimeSeconds)
	assert.Equal(t, 4, *second.MaxParallelUpgrades)
}

func TestLoadFullConfigMaintenanceDefaultsAndExplicitZeros(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	contents := `networkOperator:
  version: v26.1.0
  componentVersion: network-operator-v26.1.0
  repository: nvcr.io/nvidia/mellanox
  namespace: nvidia-network-operator
maintenance:
  maxUnavailable: 0
  maxNodeMaintenanceTimeSeconds: 0
  maxParallelUpgrades: 0
`
	require.NoError(t, os.WriteFile(configPath, []byte(contents), 0o600))

	config, err := LoadFullConfig(configPath, logr.Discard())
	require.NoError(t, err)
	require.NotNil(t, config.Maintenance)
	assert.Equal(t, "4", config.Maintenance.MaxParallelOperations.String())
	assert.Equal(t, "0", config.Maintenance.MaxUnavailable.String())
	assert.Zero(t, *config.Maintenance.MaxNodeMaintenanceTimeSeconds)
	assert.Zero(t, *config.Maintenance.MaxParallelUpgrades)
}

func TestMaintenanceIntOrPercentYAMLRoundTrip(t *testing.T) {
	zeroUpgrades := 0
	config := &LaunchKitConfig{
		Maintenance: &MaintenanceConfig{
			MaxParallelOperations:         IntOrPercentFromString("25%"),
			MaxUnavailable:                IntOrPercentFromInt32(0),
			MaxNodeMaintenanceTimeSeconds: int32Pointer(1200),
			MaxParallelUpgrades:           &zeroUpgrades,
		},
	}
	require.NoError(t, NormalizeMaintenance(config))

	marshaled, err := yaml.Marshal(config)
	require.NoError(t, err)
	output := string(marshaled)
	assert.Contains(t, output, "maxParallelOperations: 25%")
	assert.Contains(t, output, "maxUnavailable: 0")
	assert.NotContains(t, output, "intVal:")
	assert.NotContains(t, output, "strVal:")

	var reloaded LaunchKitConfig
	require.NoError(t, yaml.Unmarshal(marshaled, &reloaded))
	require.NoError(t, NormalizeMaintenance(&reloaded))
	assert.Equal(t, "25%", reloaded.Maintenance.MaxParallelOperations.String())
	assert.Equal(t, "0", reloaded.Maintenance.MaxUnavailable.String())
	assert.Equal(t, int32(1200), *reloaded.Maintenance.MaxNodeMaintenanceTimeSeconds)
	assert.Zero(t, *reloaded.Maintenance.MaxParallelUpgrades)
}

func TestMaintenanceValuesAreTemplateFriendly(t *testing.T) {
	zeroUpgrades := 0
	config := &LaunchKitConfig{
		Maintenance: &MaintenanceConfig{
			MaxParallelOperations:         IntOrPercentFromInt32(4),
			MaxUnavailable:                IntOrPercentFromString("25%"),
			MaxNodeMaintenanceTimeSeconds: int32Pointer(3600),
			MaxParallelUpgrades:           &zeroUpgrades,
		},
	}
	require.NoError(t, NormalizeMaintenance(config))

	tmpl, err := template.New("maintenance").Parse(
		`{{.Maintenance.MaxParallelOperations.String}}|{{.Maintenance.MaxUnavailable.String}}|` +
			`{{.Maintenance.MaxNodeMaintenanceTimeSeconds}}|{{.Maintenance.MaxParallelUpgrades}}`)
	require.NoError(t, err)
	var output bytes.Buffer
	require.NoError(t, tmpl.Execute(&output, config))
	assert.Equal(t, "4|25%|3600|0", output.String())
}

func TestMaintenanceCommentsSurviveConfigRoundTrip(t *testing.T) {
	config, err := DefaultLaunchKitConfig()
	require.NoError(t, err)

	marshaled, err := MarshalConfigWithComments(config, defaultConfigYAML, "")
	require.NoError(t, err)
	output := string(marshaled)
	assert.Contains(t, output, "# Global concurrency; positive integer or 1%-100%.")
	assert.Contains(t, output, "# Global or legacy SR-IOV limit; non-negative integer or 1%-100%.")

	var reloaded LaunchKitConfig
	require.NoError(t, yaml.Unmarshal(marshaled, &reloaded))
	require.NoError(t, NormalizeMaintenance(&reloaded))
	assert.Equal(t, "4", reloaded.Maintenance.MaxParallelOperations.String())
	assert.Equal(t, "4", reloaded.Maintenance.MaxUnavailable.String())
}

func TestNormalizeMaintenanceValidation(t *testing.T) {
	tests := []struct {
		name       string
		yaml       string
		errorMatch string
	}{
		{
			name:       "maxParallelOperations zero",
			yaml:       "maxParallelOperations: 0",
			errorMatch: "maintenance.maxParallelOperations must be > 0",
		},
		{
			name:       "maxParallelOperations negative",
			yaml:       "maxParallelOperations: -1",
			errorMatch: "maintenance.maxParallelOperations must be > 0",
		},
		{
			name:       "maxUnavailable negative",
			yaml:       "maxUnavailable: -1",
			errorMatch: "maintenance.maxUnavailable must be >= 0",
		},
		{
			name:       "percentage zero",
			yaml:       "maxUnavailable: 0%",
			errorMatch: "percentage must be between 1% and 100%",
		},
		{
			name:       "percentage over one hundred",
			yaml:       "maxParallelOperations: 101%",
			errorMatch: "percentage must be between 1% and 100%",
		},
		{
			name:       "string is not percentage",
			yaml:       `maxUnavailable: "4"`,
			errorMatch: "string value must be a percentage",
		},
		{
			name:       "negative cleanup timeout",
			yaml:       "maxNodeMaintenanceTimeSeconds: -1",
			errorMatch: "maintenance.maxNodeMaintenanceTimeSeconds must be >= 0",
		},
		{
			name:       "negative parallel upgrades",
			yaml:       "maxParallelUpgrades: -1",
			errorMatch: "maintenance.maxParallelUpgrades must be >= 0",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var config LaunchKitConfig
			yamlDocument := "maintenance:\n  " + strings.ReplaceAll(test.yaml, "\n", "\n  ") + "\n"
			require.NoError(t, yaml.Unmarshal([]byte(yamlDocument), &config))
			err := NormalizeMaintenance(&config)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.errorMatch)
		})
	}
}

func TestIntOrPercentRejectsNonScalarNumber(t *testing.T) {
	var config LaunchKitConfig
	err := yaml.Unmarshal([]byte("maintenance:\n  maxUnavailable: 1.5\n"), &config)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be an integer or percentage string")
}

func int32Pointer(value int32) *int32 {
	return &value
}
