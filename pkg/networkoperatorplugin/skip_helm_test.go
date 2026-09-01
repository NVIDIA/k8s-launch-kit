// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

func TestApplyOptionsToConfigSkipNetworkOperatorHelm(t *testing.T) {
	tests := []struct {
		name     string
		initial  bool
		opts     options.Options
		expected bool
	}{
		{
			name:     "omitted flag preserves config",
			initial:  true,
			expected: true,
		},
		{
			name: "explicit true enables skip",
			opts: options.Options{
				SkipNetworkOperatorHelm:    true,
				SkipNetworkOperatorHelmSet: true,
			},
			expected: true,
		},
		{
			name:    "explicit false disables config skip",
			initial: true,
			opts: options.Options{
				SkipNetworkOperatorHelm:    false,
				SkipNetworkOperatorHelmSet: true,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.LaunchKitConfig{
				NetworkOperator: &config.NetworkOperatorConfig{SkipHelmChart: tt.initial},
			}
			require.NoError(t, (&NetworkOperatorPlugin{}).ApplyOptionsToConfig(tt.opts, cfg))
			assert.Equal(t, tt.expected, cfg.NetworkOperator.SkipHelmChart)
		})
	}
}

func TestConfigFlagMappings(t *testing.T) {
	mappings := ConfigFlagMappings()
	require.NotEmpty(t, mappings)

	seen := map[string]struct{}{}
	for _, mapping := range mappings {
		assert.NotEmpty(t, mapping.FlagName)
		assert.NotEmpty(t, mapping.ConfigPaths)
		_, duplicate := seen[mapping.FlagName]
		assert.Falsef(t, duplicate, "duplicate mapping for --%s", mapping.FlagName)
		seen[mapping.FlagName] = struct{}{}
		for _, configPath := range mapping.ConfigPaths {
			assert.NotEmpty(t, configPath)
		}
	}

	assert.Equal(t, []string{"networkOperator.skipHelmChart"},
		ConfigPathsForFlag("--skip-network-operator-helm"))
	assert.Equal(t, []string{"networkOperator.skipHelmChart"},
		ConfigPathsForFlag("skip-network-operator-helm"))
	assert.Nil(t, ConfigPathsForFlag("not-config-backed"))
}

func TestRenderForScopeSkipsHelmValues(t *testing.T) {
	templatePath := filepath.Join(t.TempDir(), helmValuesTemplateName)
	require.NoError(t, os.WriteFile(templatePath, []byte("nfd:\n  enabled: true\n"), 0o600))

	rendered, err := renderForScope(templatePath, &config.LaunchKitConfig{
		NetworkOperator: &config.NetworkOperatorConfig{SkipHelmChart: true},
	}, nil, nil)

	require.NoError(t, err)
	assert.Empty(t, rendered)
}

func TestGenerateProfileSkipsHelmValuesButKeepsManifests(t *testing.T) {
	cfg, err := config.LoadFullConfig(
		filepath.Join("testdata", "grouping", "mixed-same-type.yaml"),
		logr.Discard(),
	)
	require.NoError(t, err)
	require.NotNil(t, cfg.NetworkOperator)
	cfg.NetworkOperator.SkipHelmChart = true
	cfg.Profile = &config.Profile{
		Fabric:     "ethernet",
		Deployment: "sriov",
		Multirail:  true,
	}

	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma"), cfg)

	require.NoError(t, err)
	assert.NotEmpty(t, rendered)
	assert.NotContains(t, rendered, helmValuesOutputName)
	assert.Contains(t, rendered, "10-nicclusterpolicy.yaml")
}

func TestRunHelmInstallPhaseSkipsBeforeReadingValues(t *testing.T) {
	err := runHelmInstallPhase(context.Background(), filepath.Join(t.TempDir(), "missing"), DeployOptions{
		SkipHelmChart: true,
	}, ui.NewSilent())

	require.NoError(t, err)
}

func TestBuildPreflightInputsSkipsOnlyHelmChecks(t *testing.T) {
	in, err := buildPreflightInputs(nil, t.TempDir(), DeployOptions{
		SkipHelmChart: true,
		NetworkOperator: &config.NetworkOperatorConfig{
			Version:          "v26.7.0",
			ComponentVersion: "network-operator-v26.7.0",
		},
		DOCAVersion: "doca-26.7",
	})

	require.NoError(t, err)
	assert.True(t, in.SkipHelmChecks)
	assert.Empty(t, in.GeneratedValuesYAML)
	assert.Equal(t, "network-operator-v26.7.0", in.ExpectedComponentVersion)
	assert.Equal(t, "doca-26.7", in.ExpectedDOCAVersion)
}
