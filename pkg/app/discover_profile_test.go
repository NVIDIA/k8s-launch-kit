// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type fakeProfileDiscoveryPlugin struct {
	networkoperatorplugin.NetworkOperatorPlugin
	groups []config.ClusterConfig
}

func (p *fakeProfileDiscoveryPlugin) DiscoverClusterConfig(
	_ context.Context,
	_ client.Client,
	cfg *config.LaunchKitConfig,
) error {
	cfg.ClusterConfig = append([]config.ClusterConfig(nil), p.groups...)
	return nil
}

const profileDiscoveryBaseConfig = `networkOperator:
  version: v26.4.0
  componentVersion: network-operator-v26.4.0
  repository: nvcr.io/nvidia/mellanox
  namespace: nvidia-network-operator
docaDriver:
  enable: true
profile:
  # user-selected fabric
  fabric: ethernet
  deployment: host_device
  multirail: true
`

const profileDiscoveryConfigWithoutProfile = `networkOperator:
  version: v26.4.0
  componentVersion: network-operator-v26.4.0
  selectedRelease: "26.4"
  repository: nvcr.io/nvidia/mellanox
  namespace: nvidia-network-operator
docaDriver:
  enable: true
`

func newProfileDiscoveryLauncher(opts options.Options, groups []config.ClusterConfig) *Launcher {
	launcher := New(opts)
	launcher.ui = ui.NewSilent()
	launcher.plugins[networkoperatorplugin.PluginName] = &fakeProfileDiscoveryPlugin{groups: groups}
	return launcher
}

func TestDiscoverFreshConfigIgnoresReferenceProfile(t *testing.T) {
	tmpDir := t.TempDir()
	referenceConfig := filepath.Join(tmpDir, "l8k-config.yaml")
	outputConfig := filepath.Join(tmpDir, "cluster-config.yaml")
	require.NoError(t, os.WriteFile(referenceConfig, []byte(profileDiscoveryBaseConfig), 0o600))
	t.Chdir(tmpDir)

	launcher := newProfileDiscoveryLauncher(options.Options{
		SaveClusterConfig: outputConfig,
	}, []config.ClusterConfig{{Identifier: "group-a", LinkType: "InfiniBand"}})

	require.NoError(t, launcher.discoverClusterConfig())

	got, err := config.LoadFullConfig(outputConfig, launcher.logger)
	require.NoError(t, err)
	require.NotNil(t, got.Profile)
	assert.Equal(t, "infiniband", got.Profile.Fabric)
	assert.Equal(t, "sriov", got.Profile.Deployment)
	assert.True(t, got.Profile.Multirail)
	assert.True(t, got.Profile.MultirailSet)
	assert.Equal(t, "infiniband", launcher.result.Profile["fabric"])
	assert.Equal(t, "sriov", launcher.result.Profile["deployment"])

	data, err := os.ReadFile(outputConfig)
	require.NoError(t, err)
	assert.Contains(t, string(data), "# user-selected fabric",
		"profile documentation comments should survive write-back")
	assert.Contains(t, string(data), "networkNamespaces:")
	assert.NotContains(t, string(data), "podNamespace:")
	assert.NotContains(t, string(data), "currentNetworkNamespace:")
}

func TestDiscoverPreservesPartialUserProfileAndFillsMissingSettings(t *testing.T) {
	tmpDir := t.TempDir()
	inputConfig := filepath.Join(tmpDir, "input.yaml")
	outputConfig := filepath.Join(tmpDir, "output.yaml")
	partial := `networkOperator:
  version: v26.4.0
  componentVersion: network-operator-v26.4.0
  repository: nvcr.io/nvidia/mellanox
  namespace: nvidia-network-operator
profile:
  deployment: host_device
  multirail: false
`
	require.NoError(t, os.WriteFile(inputConfig, []byte(partial), 0o600))

	launcher := newProfileDiscoveryLauncher(options.Options{
		UserConfig:        inputConfig,
		SaveClusterConfig: outputConfig,
	}, []config.ClusterConfig{{Identifier: "group-a", LinkType: "Ethernet"}})

	require.NoError(t, launcher.discoverClusterConfig())

	got, err := config.LoadFullConfig(outputConfig, launcher.logger)
	require.NoError(t, err)
	require.NotNil(t, got.Profile)
	assert.Equal(t, "ethernet", got.Profile.Fabric)
	assert.Equal(t, "host_device", got.Profile.Deployment)
	assert.False(t, got.Profile.Multirail)
	assert.True(t, got.Profile.MultirailSet)
}

func TestDiscoverCLIOverridesPersistAndRemainStableOnRerun(t *testing.T) {
	tmpDir := t.TempDir()
	inputConfig := filepath.Join(tmpDir, "input.yaml")
	firstOutput := filepath.Join(tmpDir, "first.yaml")
	secondOutput := filepath.Join(tmpDir, "second.yaml")
	require.NoError(t, os.WriteFile(inputConfig, []byte(profileDiscoveryBaseConfig), 0o600))
	groups := []config.ClusterConfig{{Identifier: "group-a", LinkType: "Ethernet"}}

	first := newProfileDiscoveryLauncher(options.Options{
		UserConfig:        inputConfig,
		SaveClusterConfig: firstOutput,
		Fabric:            "infiniband",
		DeploymentType:    "rdma_shared",
		Multirail:         false,
		MultirailSet:      true,
	}, groups)
	require.NoError(t, first.discoverClusterConfig())

	second := newProfileDiscoveryLauncher(options.Options{
		UserConfig:        firstOutput,
		SaveClusterConfig: secondOutput,
	}, groups)
	require.NoError(t, second.discoverClusterConfig())

	got, err := config.LoadFullConfig(secondOutput, second.logger)
	require.NoError(t, err)
	require.NotNil(t, got.Profile)
	assert.Equal(t, "infiniband", got.Profile.Fabric)
	assert.Equal(t, "rdma_shared", got.Profile.Deployment)
	assert.False(t, got.Profile.Multirail)
	assert.True(t, got.Profile.MultirailSet)
}

func TestDiscoverPersistsSpectrumXHardwareDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	inputConfig := filepath.Join(tmpDir, "input.yaml")
	outputConfig := filepath.Join(tmpDir, "output.yaml")
	require.NoError(t, os.WriteFile(inputConfig, []byte(profileDiscoveryConfigWithoutProfile), 0o600))

	launcher := newProfileDiscoveryLauncher(options.Options{
		UserConfig:        inputConfig,
		SaveClusterConfig: outputConfig,
		SpectrumX:         true,
		SPCXVersion:       "RA2.2",
		TopologyScheme:    config.SpectrumXTopology2Tier,
	}, []config.ClusterConfig{{
		Identifier: "group-a",
		LinkType:   "Ethernet",
		PFs: []config.PFConfig{{
			DeviceID: "1023",
			Traffic:  "east-west",
		}},
	}})

	require.NoError(t, launcher.discoverClusterConfig())

	got, err := config.LoadFullConfig(outputConfig, launcher.logger)
	require.NoError(t, err)
	require.NotNil(t, got.Profile)
	require.NotNil(t, got.Profile.SpectrumX)
	assert.Equal(t, "ethernet", got.Profile.Fabric)
	assert.Equal(t, "sriov", got.Profile.Deployment)
	assert.True(t, got.Profile.Multirail)
	assert.True(t, got.Profile.SpectrumX.Enable)
	assert.Equal(t, "RA2.2", got.Profile.SpectrumX.SPCXVersion)
	assert.Equal(t, "swplb", got.Profile.SpectrumX.MultiplaneMode)
	assert.Equal(t, 2, got.Profile.SpectrumX.NumberOfPlanes)
}

func TestDiscoverPersistsPartialProfileWhenFabricCannotBeResolved(t *testing.T) {
	tmpDir := t.TempDir()
	inputConfig := filepath.Join(tmpDir, "input.yaml")
	outputConfig := filepath.Join(tmpDir, "output.yaml")
	require.NoError(t, os.WriteFile(inputConfig, []byte(profileDiscoveryConfigWithoutProfile), 0o600))

	launcher := newProfileDiscoveryLauncher(options.Options{
		UserConfig:        inputConfig,
		SaveClusterConfig: outputConfig,
	}, []config.ClusterConfig{{Identifier: "group-a"}})

	require.NoError(t, launcher.discoverClusterConfig())

	got, err := config.LoadFullConfig(outputConfig, launcher.logger)
	require.NoError(t, err)
	require.NotNil(t, got.Profile)
	assert.Empty(t, got.Profile.Fabric)
	assert.Equal(t, "sriov", got.Profile.Deployment)
	assert.True(t, got.Profile.Multirail)
}
