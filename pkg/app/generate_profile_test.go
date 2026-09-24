// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/presets"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

func TestExecuteGenerationPreservesStructuredTemplateValidationError(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	t.Chdir(filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..")))

	cfg, err := config.LoadFullConfig(
		filepath.Join("pkg", "networkoperatorplugin", "testdata", "grouping", "mixed-same-type.yaml"),
		ctrllog.Log,
	)
	require.NoError(t, err)
	cfg.Profile = &config.Profile{
		Fabric:     "ethernet",
		Deployment: "sriov",
		Multirail:  true,
	}
	cfg.ClusterConfig[0].NetplanManaged = true

	raw, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	configPath := filepath.Join(t.TempDir(), "cluster-config.yaml")
	require.NoError(t, os.WriteFile(configPath, raw, 0o600))

	launcher := New(options.Options{})
	launcher.ui = ui.NewSilent()
	launcher.plugins[networkoperatorplugin.PluginName] = &networkoperatorplugin.NetworkOperatorPlugin{}

	err = launcher.executeGeneration(configPath)
	require.Error(t, err)
	var structured *apperrors.StructuredError
	require.True(t, errors.As(err, &structured))
	assert.Equal(t, apperrors.ExitValidation, structured.ExitCode)
	assert.Contains(t, structured.Message, "conflicting netplan configuration")
}

func TestGenerateLeavesSourceUntouchedAndWritesEffectiveConfig(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	t.Chdir(filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..")))

	cfg, err := config.DefaultLaunchKitConfig()
	require.NoError(t, err)
	cfg.ClusterConfig = []config.ClusterConfig{{
		Identifier:  "test-group",
		LinkType:    "Ethernet",
		WorkerNodes: []string{"worker-0"},
		Capabilities: &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{
			Sriov: true,
			Rdma:  true,
		}},
		PFs: []config.PFConfig{{
			DeviceID:         "1023",
			PciAddress:       "0000:05:00.0",
			RdmaDevice:       "mlx5_0",
			NetworkInterface: "net1",
			Traffic:          "east-west",
		}},
	}}
	cfg.Profile = &config.Profile{Fabric: "ethernet", Deployment: "host_device"}
	cfg.NvIpam.ReserveFirstIPs = 2
	cfg.NvIpam.Subnets = []config.NvIpamSubnetConfig{{
		Subnet:  "192.168.50.0/24",
		Gateway: "192.168.50.1",
		Exclusions: []config.NvIpamExclusion{{
			StartIP: "192.168.50.20",
			EndIP:   "192.168.50.21",
		}},
	}}

	raw, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	source := "# original config comment\n" + string(raw)
	source = strings.Replace(source, "clusterConfig:\n", "clusterConfig: # hardware inventory comment\n", 1)
	source = strings.Replace(source, "profile:\n", "profile:\n  # profile settings comment\n", 1)
	source = strings.Replace(source, "linkType: Ethernet", "linkType: Ethernet # hardware detail comment", 1)

	configPath := filepath.Join(t.TempDir(), "cluster-config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(source), 0o600))
	outputDir := filepath.Join(t.TempDir(), "deployment")

	launcher := New(options.Options{
		DeploymentType:      "sriov",
		Multirail:           false,
		MultirailSet:        true,
		SaveDeploymentFiles: outputDir,
	})
	launcher.ui = ui.NewSilent()
	launcher.plugins[networkoperatorplugin.PluginName] = &networkoperatorplugin.NetworkOperatorPlugin{}

	require.NoError(t, launcher.executeGeneration(configPath))

	updated, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, source, string(updated), "generate must not rewrite user input")

	got, err := config.LoadEffectiveConfig(config.EffectiveConfigPath(outputDir))
	require.NoError(t, err)
	require.NotNil(t, got.Profile)
	assert.Equal(t, "ethernet", got.Profile.Fabric)
	assert.Equal(t, "sriov", got.Profile.Deployment)
	assert.False(t, got.Profile.Multirail)
	assert.True(t, got.Profile.MultirailSet)

	info, err := os.Stat(configPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "in-place write must preserve file permissions")

	require.NoError(t, launcher.executeGeneration(configPath))
	secondUpdate, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, source, string(secondUpdate), "repeated generation must leave source YAML untouched")
}

func TestResolveSpectrumXTopologyFile(t *testing.T) {
	t.Run("config relative path resolves from config directory", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "configs", "cluster-config.yaml")
		cfg := &config.LaunchKitConfig{
			Profile: &config.Profile{SpectrumX: &config.ProfileSpectrumX{
				TopologyFile: "../topology.json",
			}},
		}

		resolveSpectrumXTopologyFile(configPath, cfg)

		assert.Equal(t,
			filepath.Join(filepath.Dir(configPath), "../topology.json"),
			cfg.Profile.SpectrumX.ResolvedTopologyFile)
	})

	t.Run("pre-resolved CLI path is preserved", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "configs", "cluster-config.yaml")
		resolvedCLIPath := filepath.Join(t.TempDir(), "topology.json")
		cfg := &config.LaunchKitConfig{
			Profile: &config.Profile{SpectrumX: &config.ProfileSpectrumX{
				TopologyFile:         "topology.json",
				ResolvedTopologyFile: resolvedCLIPath,
			}},
		}

		resolveSpectrumXTopologyFile(configPath, cfg)

		assert.Equal(t, resolvedCLIPath, cfg.Profile.SpectrumX.ResolvedTopologyFile)
	})
}

func TestGenerateUsesPresetHardwareForDefaultsWithoutPersistingPresetInventory(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cluster-config.yaml")
	source := `networkOperator:
  selectedRelease: "26.4"
profile:
  multirail: true
  spectrumX:
    enable: true
    spcxVersion: RA2.2
    topologyType: 2-tier
clusterConfig:
  - identifier: source-inventory
    machineType: source-machine
    gpuType: NVIDIA-H200
    linkType: Ethernet
    pfs:
      - deviceID: a2dc
        traffic: east-west
`
	require.NoError(t, os.WriteFile(configPath, []byte(source), 0o600))

	catalog, err := presets.EmbeddedCatalog()
	require.NoError(t, err)
	launcher := New(options.Options{
		ForPreset:           "GB300-NVL-NVIDIA-GB300",
		NodeSelector:        "nvidia.com/gpu.product=NVIDIA-GB300",
		SaveDeploymentFiles: filepath.Join(t.TempDir(), "deployment"),
	})
	launcher.ui = ui.NewSilent()
	launcher.presetCatalog = catalog

	require.NoError(t, launcher.executeGeneration(configPath))

	unchanged, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, source, string(unchanged))

	got, err := config.LoadEffectiveConfig(config.EffectiveConfigPath(launcher.options.SaveDeploymentFiles))
	require.NoError(t, err)
	require.NotNil(t, got.Profile)
	require.NotNil(t, got.Profile.SpectrumX)
	assert.Equal(t, "swplb", got.Profile.SpectrumX.MultiplaneMode)
	assert.Equal(t, 2, got.Profile.SpectrumX.NumberOfPlanes)
	require.Len(t, got.ClusterConfig, 1)
	assert.NotEqual(t, "source-inventory", got.ClusterConfig[0].Identifier)
	assert.Equal(t, "NVIDIA-GB300", got.ClusterConfig[0].GPUType)
}

func TestExecuteGenerationRejectsDuplicateClusterConfigIdentifiers(t *testing.T) {
	// Two groups sharing an identifier must be rejected in the actual
	// generation path (issue #234): the per-source output filenames are
	// derived from the identifier, so the second group would silently
	// overwrite the first and only its NICs would be configured.
	source := `networkOperator:
  version: v25.10.0
  componentVersion: network-operator-v25.10.0
  repository: nvcr.io/nvidia/mellanox
  namespace: nvidia-network-operator
profile:
  fabric: ethernet
  deployment: sriov
  multirail: true
clusterConfig:
  - identifier: dgx-h100
    machineType: dgx-h100
    gpuType: NVIDIA-H100
    linkType: Ethernet
    workerNodes:
      - node-a
    pfs:
      - deviceID: a2dc
        traffic: east-west
  - identifier: dgx-h100
    machineType: dgx-h100
    gpuType: NVIDIA-H100
    linkType: Ethernet
    workerNodes:
      - node-b
    pfs:
      - deviceID: a2dc
        traffic: east-west
`
	configPath := filepath.Join(t.TempDir(), "cluster-config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(source), 0o600))

	outputDir := filepath.Join(t.TempDir(), "manifests")
	launcher := New(options.Options{SaveDeploymentFiles: outputDir})
	launcher.ui = ui.NewSilent()
	launcher.plugins[networkoperatorplugin.PluginName] = &networkoperatorplugin.NetworkOperatorPlugin{}

	err := launcher.executeGeneration(configPath)
	require.Error(t, err)
	var structured *apperrors.StructuredError
	require.True(t, errors.As(err, &structured))
	assert.Equal(t, apperrors.ExitValidation, structured.ExitCode)
	assert.Contains(t, structured.Message, `duplicate identifier "dgx-h100"`)

	// Generation must fail before producing any manifests.
	_, statErr := os.Stat(outputDir)
	assert.True(t, os.IsNotExist(statErr), "no deployment files should be written when validation fails")
}
