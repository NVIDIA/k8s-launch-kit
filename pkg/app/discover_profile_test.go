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
	"github.com/nvidia/k8s-launch-kit/pkg/resolve"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type fakeProfileDiscoveryPlugin struct {
	networkoperatorplugin.NetworkOperatorPlugin
	groups       []config.ClusterConfig
	mutateConfig func(*config.LaunchKitConfig)
}

func (p *fakeProfileDiscoveryPlugin) DiscoverClusterConfig(
	_ context.Context,
	_ client.Client,
	cfg *config.LaunchKitConfig,
) error {
	if p.mutateConfig != nil {
		p.mutateConfig(cfg)
	}
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

func TestDiscoverFreshEmbeddedDefaultsHardwareReleaseAboveCanonical(t *testing.T) {
	t.Chdir(t.TempDir())
	outputConfig := filepath.Join(t.TempDir(), "cluster-config.yaml")
	launcher := newProfileDiscoveryLauncher(options.Options{
		SaveClusterConfig: outputConfig,
		ConfigDir:         t.TempDir(),
		SpectrumX:         true,
		SPCXVersion:       "RA2.1",
		TopologyScheme:    config.SpectrumXTopology2Tier,
	}, []config.ClusterConfig{{
		Identifier:  "group-a",
		GPUType:     "NVIDIA-H100",
		LinkType:    "Ethernet",
		WorkerNodes: []string{"worker-a"},
		PFs: []config.PFConfig{{
			DeviceID: "1023",
			Traffic:  "east-west",
		}},
	}})

	require.NoError(t, launcher.discoverClusterConfig())
	got, err := config.LoadFullConfig(outputConfig, launcher.logger)
	require.NoError(t, err)
	require.NotNil(t, got.NetworkOperator)
	assert.Equal(t, "26.1", got.NetworkOperator.SelectedRelease)
	require.NotNil(t, got.Profile)
	require.NotNil(t, got.Profile.SpectrumX)
	assert.Equal(t, "RA2.1", got.Profile.SpectrumX.SPCXVersion)
}

func TestDiscoverPreservesUserConfigOutsideClusterConfig(t *testing.T) {
	tmpDir := t.TempDir()
	inputConfig := filepath.Join(tmpDir, "input.yaml")
	outputConfig := filepath.Join(tmpDir, "output.yaml")
	partial := `networkOperator:
  selectedRelease: "26.4"
  version: user-version
  componentVersion: user-component-version
  repository: user.example.com/components
  operatorRepository: user.example.com/operator
  helmRepoURL: https://user.example.com/charts
  namespace: user-network-operator
  imagePullSecrets: [user-secret]
docaDriver:
  enable: true
  version: user-doca-version
networkNamespaces: [user-namespace]
validation:
  connectivity: false
  mode: quick
  checks: []
  gpuDirect:
    enabled: false
    gpuResourceType: nvidia.com/gpu
profile:
  deployment: host_device
  multirail: false
clusterConfig:
  - identifier: stale-group
`
	require.NoError(t, os.WriteFile(inputConfig, []byte(partial), 0o600))

	groups := []config.ClusterConfig{{Identifier: "group-a", LinkType: "Ethernet"}}
	launcher := newProfileDiscoveryLauncher(options.Options{
		UserConfig:        inputConfig,
		SaveClusterConfig: outputConfig,
	}, groups)
	launcher.plugins[networkoperatorplugin.PluginName] = &fakeProfileDiscoveryPlugin{
		groups: groups,
		mutateConfig: func(cfg *config.LaunchKitConfig) {
			cfg.NetworkOperator.Version = "discovery-version"
			cfg.DOCADriver.Enable = false
			cfg.NetworkNamespaces = []string{"discovery-namespace"}
			cfg.Validation.GPUDirect.Enabled = true
			cfg.Profile.Fabric = "ethernet"
		},
	}

	require.NoError(t, launcher.discoverClusterConfig())

	got, err := config.LoadFullConfig(outputConfig, launcher.logger)
	require.NoError(t, err)
	require.NotNil(t, got.NetworkOperator)
	assert.Equal(t, "26.4", got.NetworkOperator.SelectedRelease)
	assert.Equal(t, "user-version", got.NetworkOperator.Version)
	assert.Equal(t, "user-component-version", got.NetworkOperator.ComponentVersion)
	assert.Equal(t, "user.example.com/components", got.NetworkOperator.Repository)
	assert.Equal(t, "user.example.com/operator", got.NetworkOperator.OperatorRepository)
	assert.Equal(t, "https://user.example.com/charts", got.NetworkOperator.HelmRepoURL)
	assert.Equal(t, "user-network-operator", got.NetworkOperator.Namespace)
	assert.Equal(t, []string{"user-secret"}, got.NetworkOperator.ImagePullSecrets)
	require.NotNil(t, got.DOCADriver)
	assert.True(t, got.DOCADriver.Enable)
	assert.Equal(t, "user-doca-version", got.DOCADriver.Version)
	assert.Equal(t, []string{"user-namespace"}, got.NetworkNamespaces)
	require.NotNil(t, got.Validation)
	require.NotNil(t, got.Validation.Connectivity)
	assert.False(t, *got.Validation.Connectivity)
	assert.Empty(t, got.Validation.Checks)
	assert.False(t, got.Validation.GPUDirect.Enabled)
	require.NotNil(t, got.Profile)
	assert.Empty(t, got.Profile.Fabric)
	assert.Equal(t, "host_device", got.Profile.Deployment)
	assert.False(t, got.Profile.Multirail)
	assert.True(t, got.Profile.MultirailSet)
	require.Len(t, got.ClusterConfig, 1)
	assert.Equal(t, "group-a", got.ClusterConfig[0].Identifier)
}

func TestDiscoverPreservesUserFieldPresenceAcrossRefreshes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster-config.yaml")
	source := []byte(`networkOperator:
  selectedRelease: "" # custom coordinates
  version: custom
docaDriver:
  version: custom
nvIpam:
  offset: 0
profile:
  fabric: ethernet
validation:
  checks: []
extension:
  userOwned: keep-me
`)
	require.NoError(t, os.WriteFile(path, source, 0o600))
	before, err := config.DecodeInput(source, path)
	require.NoError(t, err)
	expected, err := resolve.Resolve(resolve.Request{Input: before, ApplyHardwareDefaults: true})
	require.NoError(t, err)
	groups := []config.ClusterConfig{{Identifier: "fresh", LinkType: "Ethernet"}}
	for range 2 {
		launcher := newProfileDiscoveryLauncher(options.Options{UserConfig: path}, groups)
		require.NoError(t, launcher.discoverClusterConfig())
		after, err := config.LoadInput(path, launcher.logger)
		require.NoError(t, err)
		for field := range before.Present {
			assert.True(t, after.Present.Has(field), "lost explicit field %s", field)
		}
		for _, field := range []string{"docaDriver.enable", "profile.deployment", "profile.multirail"} {
			assert.False(t, after.Present.Has(field), "invented explicit field %s", field)
		}
		actual, err := resolve.Resolve(resolve.Request{Input: after, ApplyHardwareDefaults: true})
		require.NoError(t, err)
		assert.Equal(t, expected.Config.NetworkOperator, actual.Config.NetworkOperator)
		assert.Equal(t, expected.Config.DOCADriver, actual.Config.DOCADriver)
		assert.Equal(t, expected.Config.Profile, actual.Config.Profile)
		assert.Equal(t, expected.Config.NvIpam, actual.Config.NvIpam)
		assert.Equal(t, groups[0].Identifier, actual.Config.ClusterConfig[0].Identifier)
		assert.Contains(t, string(after.SourceYAML), "# custom coordinates")
		assert.Contains(t, string(after.SourceYAML), "userOwned: keep-me")
	}
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
	inputConfig := filepath.Join(tmpDir, "l8k-config.yaml")
	outputConfig := filepath.Join(tmpDir, "output.yaml")
	require.NoError(t, os.WriteFile(inputConfig, []byte(profileDiscoveryConfigWithoutProfile), 0o600))
	t.Chdir(tmpDir)

	launcher := newProfileDiscoveryLauncher(options.Options{
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

func TestDiscoverDoesNotCreateMissingProfileForUserConfig(t *testing.T) {
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
	assert.Nil(t, got.Profile)
}

func TestDiscoverPreservesExplicitFalseFromReferenceConfig(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "l8k-config.yaml"), []byte(`docaDriver:
  enable: false
`), 0o600))
	t.Chdir(tmpDir)
	outputConfig := filepath.Join(tmpDir, "output.yaml")
	launcher := newProfileDiscoveryLauncher(options.Options{SaveClusterConfig: outputConfig},
		[]config.ClusterConfig{{Identifier: "group-a", LinkType: "Ethernet"}})

	require.NoError(t, launcher.discoverClusterConfig())
	got, err := config.LoadInput(outputConfig, launcher.logger)
	require.NoError(t, err)
	assert.False(t, got.Config.DOCADriver.Enable)
}

func TestDiscoverAppliesCLIOverrideBeforeNormalization(t *testing.T) {
	tmpDir := t.TempDir()
	inputConfig := filepath.Join(tmpDir, "input.yaml")
	outputConfig := filepath.Join(tmpDir, "output.yaml")
	require.NoError(t, os.WriteFile(inputConfig, []byte(`profile:
  fabric: ethernet
  deployment: sriov
  multirail: true
  spectrumX:
    enable: true
    spcxVersion: RA2.2
    multiplaneMode: none
    numberOfPlanes: 1
    topologyType: invalid-old-value
networkOperator:
  selectedRelease: "26.4"
`), 0o600))
	launcher := newProfileDiscoveryLauncher(options.Options{
		UserConfig:        inputConfig,
		SaveClusterConfig: outputConfig,
		TopologyScheme:    config.SpectrumXTopology2Tier,
	}, []config.ClusterConfig{{Identifier: "group-a", LinkType: "Ethernet"}})

	require.NoError(t, launcher.discoverClusterConfig())
	got, err := config.LoadInput(outputConfig, launcher.logger)
	require.NoError(t, err)
	assert.Equal(t, config.SpectrumXTopology2Tier, got.Config.Profile.SpectrumX.TopologyType)
}

func TestDiscoverAppliesCLIProfileOverridesWhenUserProfileIsMissing(t *testing.T) {
	tmpDir := t.TempDir()
	inputConfig := filepath.Join(tmpDir, "input.yaml")
	outputConfig := filepath.Join(tmpDir, "output.yaml")
	require.NoError(t, os.WriteFile(inputConfig, []byte(profileDiscoveryConfigWithoutProfile), 0o600))

	launcher := newProfileDiscoveryLauncher(options.Options{
		UserConfig:        inputConfig,
		SaveClusterConfig: outputConfig,
		Fabric:            "infiniband",
		DeploymentType:    "rdma_shared",
		Multirail:         false,
		MultirailSet:      true,
	}, []config.ClusterConfig{{Identifier: "group-a", LinkType: "Ethernet"}})

	require.NoError(t, launcher.discoverClusterConfig())

	got, err := config.LoadFullConfig(outputConfig, launcher.logger)
	require.NoError(t, err)
	require.NotNil(t, got.Profile)
	assert.Equal(t, "infiniband", got.Profile.Fabric)
	assert.Equal(t, "rdma_shared", got.Profile.Deployment)
	assert.False(t, got.Profile.Multirail)
	assert.True(t, got.Profile.MultirailSet)
	assert.Empty(t, got.Profile.Routing)
}

func TestDiscoverPersistsSpectrumXConfigMapOverride(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "cluster-config.yaml")
	profilePath := filepath.Join(dir, "profile.yaml")
	require.NoError(t, os.WriteFile(inputPath, []byte(`networkOperator:
  selectedRelease: "26.7"
profile:
  multirail: true
  spectrumX:
    enable: true
    spcxVersion: RA2.3
    multiplaneMode: none
    numberOfPlanes: 1
    topologyType: 2-tier
`), 0o600))
	require.NoError(t, os.WriteFile(profilePath, []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: site-ra23-profile
data:
  profile: |
    useSoftwareCCAlgorithm: true
`), 0o600))
	launcher := newProfileDiscoveryLauncher(options.Options{
		UserConfig:        inputPath,
		SaveClusterConfig: inputPath,
		SpectrumXConfig:   profilePath,
	}, []config.ClusterConfig{{Identifier: "group-a", LinkType: "Ethernet"}})
	require.NoError(t, launcher.discoverClusterConfig())
	got, err := config.LoadInput(inputPath, launcher.logger)
	require.NoError(t, err)
	assert.Equal(t, "site-ra23-profile", got.Config.Profile.SpectrumX.ConfigMapName)
	assert.Equal(t, "useSoftwareCCAlgorithm: true\n", got.Config.Profile.SpectrumX.Profile)
	assert.False(t, got.Present.Has("networkOperator.version"))
	assert.False(t, got.Present.Has("profile.deployment"))
}
