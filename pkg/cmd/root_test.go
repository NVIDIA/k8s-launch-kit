// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplySpectrumXDefaults_SetsImpliedValues(t *testing.T) {
	opts := &options.Options{
		SpectrumX: true,
	}
	err := applySpectrumXDefaults(opts)
	require.NoError(t, err)
	assert.Equal(t, "ethernet", opts.Fabric)
	assert.Equal(t, "sriov", opts.DeploymentType)
	assert.True(t, opts.Multirail)
}

func TestApplySpectrumXDefaults_NoOpWhenDisabled(t *testing.T) {
	opts := &options.Options{
		SpectrumX: false,
	}
	err := applySpectrumXDefaults(opts)
	require.NoError(t, err)
	assert.Empty(t, opts.Fabric)
	assert.Empty(t, opts.DeploymentType)
	assert.False(t, opts.Multirail)
}

func TestApplySpectrumXDefaults_AcceptsMatchingFabric(t *testing.T) {
	opts := &options.Options{
		SpectrumX: true,
		Fabric:    "ethernet",
	}
	err := applySpectrumXDefaults(opts)
	require.NoError(t, err)
	assert.Equal(t, "ethernet", opts.Fabric)
}

func TestApplySpectrumXDefaults_ErrorOnConflictingFabric(t *testing.T) {
	opts := &options.Options{
		SpectrumX: true,
		Fabric:    "infiniband",
	}
	err := applySpectrumXDefaults(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ethernet")
	assert.Contains(t, err.Error(), "infiniband")
}

func TestApplySpectrumXDefaults_ErrorOnConflictingDeploymentType(t *testing.T) {
	opts := &options.Options{
		SpectrumX:      true,
		DeploymentType: "host_device",
	}
	err := applySpectrumXDefaults(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sriov")
	assert.Contains(t, err.Error(), "host_device")
}

func TestValidateConfig_DeployWithUserConfigPassesWithoutProfileFlags(t *testing.T) {
	opts := options.Options{
		UserConfig:     "some-config.yaml",
		Deploy:         true,
		Kubeconfig:     "/path/to/kubeconfig",
		EnabledPlugins: []string{"network-operator"},
		OutputFormat:   "text",
	}
	err := validateConfig(&opts)
	assert.NoError(t, err)
}

func TestValidateConfig_DeployWithoutAnyProfileSourceFails(t *testing.T) {
	opts := options.Options{
		DiscoverClusterConfig: true,
		Deploy:                true,
		Kubeconfig:            "/path/to/kubeconfig",
		EnabledPlugins:        []string{"network-operator"},
		OutputFormat:          "text",
	}
	err := validateConfig(&opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--deploy requires")
}

func TestValidateConfig_InvalidOutputFormat(t *testing.T) {
	opts := options.Options{
		UserConfig:     "config.yaml",
		EnabledPlugins: []string{"network-operator"},
		OutputFormat:   "xml",
	}
	err := validateConfig(&opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--output must be one of")
}

func TestValidateConfig_ValidOutputFormats(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		opts := options.Options{
			UserConfig:     "config.yaml",
			EnabledPlugins: []string{"network-operator"},
			OutputFormat:   format,
		}
		err := validateConfig(&opts)
		assert.NoError(t, err, "format %q should be valid", format)
	}
}

func TestValidateConfig_DryRunRequiresDeploy(t *testing.T) {
	opts := options.Options{
		UserConfig:     "config.yaml",
		EnabledPlugins: []string{"network-operator"},
		OutputFormat:   "text",
		DryRun:         true,
		Deploy:         false,
	}
	err := validateConfig(&opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--dry-run requires --deploy")
}

func TestValidateConfig_DryRunWithDeployPasses(t *testing.T) {
	opts := options.Options{
		UserConfig:     "config.yaml",
		EnabledPlugins: []string{"network-operator"},
		OutputFormat:   "text",
		DryRun:         true,
		Deploy:         true,
		Kubeconfig:     "/path/to/kubeconfig",
		Fabric:         "ethernet",
		DeploymentType: "sriov",
		SaveDeploymentFiles: "/tmp/test",
	}
	err := validateConfig(&opts)
	assert.NoError(t, err)
}
