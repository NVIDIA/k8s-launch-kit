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

func TestGeneratePersistsResolvedProfileToOriginalConfig(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	t.Chdir(filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..")))

	cfg, err := config.DefaultLaunchKitConfig()
	require.NoError(t, err)
	require.NotEmpty(t, cfg.ClusterConfig)
	cfg.Profile = &config.Profile{Deployment: "host_device"}
	cfg.ClusterConfig[0].LinkType = "Ethernet"
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

	launcher := New(options.Options{
		DeploymentType: "sriov",
		Multirail:      false,
		MultirailSet:   true,
	})
	launcher.ui = ui.NewSilent()
	launcher.plugins[networkoperatorplugin.PluginName] = &networkoperatorplugin.NetworkOperatorPlugin{}

	require.NoError(t, launcher.executeGeneration(configPath))

	got, err := config.LoadFullConfig(configPath, launcher.logger)
	require.NoError(t, err)
	require.NotNil(t, got.Profile)
	assert.Equal(t, "ethernet", got.Profile.Fabric, "hardware default must be persisted")
	assert.Equal(t, "sriov", got.Profile.Deployment, "CLI override must be persisted")
	assert.False(t, got.Profile.Multirail, "explicit CLI false must be persisted")
	assert.True(t, got.Profile.MultirailSet)

	updated, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Contains(t, string(updated), "# original config comment")
	assert.Contains(t, string(updated), "# hardware inventory comment")
	assert.Contains(t, string(updated), "# hardware detail comment")
	assert.Contains(t, string(updated), "# profile settings comment")
	var persisted config.LaunchKitConfig
	require.NoError(t, yaml.Unmarshal(updated, &persisted))
	require.Len(t, persisted.NvIpam.Subnets, 1)
	assert.Len(t, persisted.NvIpam.Subnets[0].Exclusions, 1,
		"computed reserve exclusions must not be written back as explicit input")

	info, err := os.Stat(configPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "in-place write must preserve file permissions")

	require.NoError(t, launcher.executeGeneration(configPath))
	secondUpdate, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, string(updated), string(secondUpdate), "repeated generation must produce stable config YAML")
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
		ForPreset:    "GB300-NVL-NVIDIA-GB300",
		NodeSelector: "nvidia.com/gpu.product=NVIDIA-GB300",
	})
	launcher.ui = ui.NewSilent()
	launcher.presetCatalog = catalog

	require.NoError(t, launcher.executeGeneration(configPath))

	got, err := config.LoadFullConfig(configPath, launcher.logger)
	require.NoError(t, err)
	require.NotNil(t, got.Profile)
	require.NotNil(t, got.Profile.SpectrumX)
	assert.Equal(t, "swplb", got.Profile.SpectrumX.MultiplaneMode)
	assert.Equal(t, 2, got.Profile.SpectrumX.NumberOfPlanes)
	require.Len(t, got.ClusterConfig, 1)
	assert.Equal(t, "source-inventory", got.ClusterConfig[0].Identifier)
	assert.Equal(t, "NVIDIA-H200", got.ClusterConfig[0].GPUType)
	require.Len(t, got.ClusterConfig[0].PFs, 1)
	assert.Equal(t, "a2dc", got.ClusterConfig[0].PFs[0].DeviceID)
}
