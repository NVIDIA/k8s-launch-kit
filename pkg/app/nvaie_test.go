// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/nvaie"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

func TestNVAIEDiscoveryPreservesSourceAndReportsFreshProducts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	source := "# preserved comment\nnvaieRelease: '9.0'\nprofile: {fabric: ethernet, deployment: host_device}\nvalidation: {connectivity: false}\nextension: {custom: keep-me}\n"
	require.NoError(t, os.WriteFile(path, []byte(source), 0600))
	launcher := newProfileDiscoveryLauncher(options.Options{UserConfig: path}, []config.ClusterConfig{{Identifier: "new", GPUType: "NVIDIA H100 PCIe", WorkerNodes: []string{"worker-a"}}})
	calls := 0
	launcher.assessNVAIEFunc = func(_ context.Context, _ *rest.Config, release string, workers []string, products map[string]string, _ *nvaie.DOCAHostEvidence) *nvaie.Assessment {
		calls++
		assert.Equal(t, "9.0", release)
		assert.Equal(t, []string{"worker-a"}, workers)
		assert.Equal(t, map[string]string{"worker-a": "NVIDIA H100 PCIe"}, products)
		return &nvaie.Assessment{Release: release, Checks: []nvaie.Check{{Name: "components", Status: nvaie.Unknown, Required: true, Detail: "not installed yet"}}}
	}
	require.NoError(t, launcher.discoverClusterConfig(), "discovery assessment is informational")
	require.Equal(t, 1, calls)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "# preserved comment")
	assert.Contains(t, string(data), "extension: {custom: keep-me}")
	input, err := config.DecodeInput(data, path)
	require.NoError(t, err)
	assert.Equal(t, "9.0", input.Config.NVAIERelease)
	assert.Equal(t, "26.7", input.Config.NetworkOperator.SelectedRelease)
	assert.Equal(t, "v26.7.1", input.Config.NetworkOperator.Version)
	payload, err := json.Marshal(launcher.result)
	require.NoError(t, err)
	assert.Contains(t, string(payload), `"nvaie":{"release":"9.0"`)
	assert.NotContains(t, string(data), "not installed yet", "observations must not become persisted config authority")
}

func TestNVAIEDiscoveryConflictPreventsPluginMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	source := "nvaieRelease: '9.0'\nnetworkOperator: {selectedRelease: '26.4'}\n"
	require.NoError(t, os.WriteFile(path, []byte(source), 0600))
	launcher := newProfileDiscoveryLauncher(options.Options{UserConfig: path}, nil)
	require.ErrorContains(t, launcher.discoverClusterConfig(), "conflicts with explicit")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, source, string(after))
}

func TestNVAIEFreshDiscoveryReferenceReleaseIsDefault(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.WriteFile("l8k-config.yaml", []byte("networkOperator: {selectedRelease: '26.10'}\n"), 0600))
	launcher := newProfileDiscoveryLauncher(options.Options{NVAIERelease: "9.0"}, []config.ClusterConfig{{Identifier: "new", LinkType: "Ethernet"}})
	launcher.assessNVAIEFunc = func(_ context.Context, _ *rest.Config, release string, _ []string, _ map[string]string, _ *nvaie.DOCAHostEvidence) *nvaie.Assessment {
		return &nvaie.Assessment{Release: release}
	}
	require.NoError(t, launcher.discoverClusterConfig())
	input, err := config.LoadInput("cluster-config.yaml", launcher.logger)
	require.NoError(t, err)
	assert.Equal(t, "26.7", input.Config.NetworkOperator.SelectedRelease)
}

func TestNVAIEDiscoveryRetainsInstalledHostEvidenceRegardlessOfDriverIntent(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "container", false: "host"}[enabled], func(t *testing.T) {
			cfg := &config.LaunchKitConfig{NVAIERelease: "9.0", DOCADriver: &config.DOCADriverConfig{Enable: enabled}, ClusterConfig: []config.ClusterConfig{{WorkerNodes: []string{"render-only"}}},
				DiscoveredDOCAHostScope: &config.DOCAHostScope{WorkerNodes: []string{"worker-a", "worker-b"}, MembershipKnown: false},
				DiscoveredDOCAHost:      []config.DOCAHostObservation{{Node: "worker-a", Status: config.DOCAHostInstalled, Version: "3.5.0-041000"}, {Node: "outside", Status: config.DOCAHostInstalled, Version: "3.6.0"}},
			}
			launcher := &Launcher{assessNVAIEFunc: func(_ context.Context, _ *rest.Config, _ string, _ []string, _ map[string]string, evidence *nvaie.DOCAHostEvidence) *nvaie.Assessment {
				require.NotNil(t, evidence)
				assert.True(t, evidence.Fresh)
				assert.False(t, evidence.MembershipKnown)
				require.Len(t, evidence.Observations, 2)
				assert.Equal(t, "worker-a", evidence.Observations[0].Node)
				assert.Equal(t, "3.5.0-041000", evidence.Observations[0].Version)
				assert.Equal(t, config.DOCAHostUnknown, evidence.Observations[1].Status)
				return &nvaie.Assessment{Release: "9.0"}
			}}
			launcher.assessNVAIE(context.Background(), cfg)
		})
	}
}

func TestNVAIEDiscoveryKeepsContainerPathWithoutInstalledHostEvidence(t *testing.T) {
	cfg := &config.LaunchKitConfig{DOCADriver: &config.DOCADriverConfig{Enable: true}, ClusterConfig: []config.ClusterConfig{{WorkerNodes: []string{"worker-a"}}}, DiscoveredDOCAHost: []config.DOCAHostObservation{
		{Node: "worker-a", Status: config.DOCAHostNotInstalled},
		{Node: "outside", Status: config.DOCAHostInstalled, Version: "3.6.0"},
	}}
	assert.Nil(t, discoveryDOCAHost(cfg), "packages outside selected discovery scope cannot select host evidence")
	cfg.DOCADriver.Enable = false
	evidence := discoveryDOCAHost(cfg)
	require.NotNil(t, evidence)
	assert.Equal(t, config.DOCAHostNotInstalled, evidence.Observations[0].Status)
}
