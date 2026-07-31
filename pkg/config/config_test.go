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

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeneratedGroupIdentityIsBounded(t *testing.T) {
	const (
		machineType = "HPE-ProLiant-Compute-DL380-Gen12"
		gpuType     = "NVIDIA-AX800-Converged-Accelerator"
	)

	machineLabel := MachineLabelValue(machineType, gpuType)
	assert.Equal(t, "HPE-ProLiant-Compute-DL380-Gen1-0885f134", machineLabel)
	assert.LessOrEqual(t, len(machineLabel), MaxGeneratedIdentifierLength)

	identifier := SanitizeIdentifier(machineLabel)
	assert.Equal(t, "hpe-proliant-compute-dl380-gen1-0885f134", identifier)
	assert.LessOrEqual(t, len(identifier), MaxGeneratedIdentifierLength)

	otherIdentifier := SanitizeIdentifier(MachineLabelValue(machineType, gpuType+"-Variant"))
	assert.NotEqual(t, identifier, otherIdentifier,
		"long identities with a shared prefix must retain distinct hash suffixes")
}

func TestSanitizeIdentifierBoundsLongValues(t *testing.T) {
	input := "hpe-proliant-compute-dl380-gen12-nvidia-ax800-converge-0885f134"
	got := SanitizeIdentifier(input)

	assert.Len(t, got, MaxGeneratedIdentifierLength)
	assert.Equal(t, got, SanitizeIdentifier(input), "shortening must be deterministic")
}

func TestLoadFullConfig(t *testing.T) {
	logger := logr.Discard()

	t.Run("load valid config with separate MTU values", func(t *testing.T) {
		// Create a temporary config file
		tempDir := t.TempDir()
		configPath := filepath.Join(tempDir, "test-config.yaml")

		configContent := `networkOperator:
  version: v25.10.0
  componentVersion: network-operator-v25.10.0
  repository: nvcr.io/nvidia/mellanox
  namespace: nvidia-network-operator

sriov:
  ethernetMtu: 9000
  infinibandMtu: 4000
  numVfs: 8
  priority: 90
  resourceName: sriov_resource
  networkName: sriov_network

profile:
  fabric: ethernet
  deployment: sriov
  multirail: false
  ai: false
`
		err := os.WriteFile(configPath, []byte(configContent), 0644)
		require.NoError(t, err)

		// Load the config
		config, err := LoadFullConfig(configPath, logger)
		require.NoError(t, err)
		require.NotNil(t, config)

		// Verify network operator config
		assert.Equal(t, "v25.10.0", config.NetworkOperator.Version)
		assert.Equal(t, "network-operator-v25.10.0", config.NetworkOperator.ComponentVersion)
		assert.Equal(t, "nvcr.io/nvidia/mellanox", config.NetworkOperator.Repository)
		assert.Equal(t, "nvidia-network-operator", config.NetworkOperator.Namespace)

		// Verify SR-IOV config with separate MTU values
		require.NotNil(t, config.Sriov)
		assert.Equal(t, 9000, config.Sriov.EthernetMtu, "Ethernet MTU should be 9000")
		assert.Equal(t, 4000, config.Sriov.InfinibandMtu, "Infiniband MTU should be 4000")
		assert.Equal(t, 8, config.Sriov.NumVfs)
		assert.Equal(t, 90, config.Sriov.Priority)
		assert.Equal(t, "sriov_resource", config.Sriov.ResourceName)
		assert.Equal(t, "sriov_network", config.Sriov.NetworkName)

		// Verify profile config
		require.NotNil(t, config.Profile)
		assert.Equal(t, "ethernet", config.Profile.Fabric)
		assert.Equal(t, "sriov", config.Profile.Deployment)
		assert.False(t, config.Profile.Multirail)
		assert.Nil(t, config.Profile.SpectrumX)

		_, source, err := LoadFullConfigWithSource(configPath, logger)
		require.NoError(t, err)
		assert.Equal(t, configContent, string(source))

		require.NotNil(t, config.Validation)
		require.NotNil(t, config.Validation.Connectivity)
		assert.True(t, *config.Validation.Connectivity)
		assert.Equal(t, ValidationModeStrict, config.Validation.Mode)
		assert.Equal(t, []string{ValidationCheckICMP, ValidationCheckRPing, ValidationCheckIBWriteBW}, config.Validation.Checks)
		require.NotNil(t, config.Validation.RDMA)
		assert.Equal(t, DefaultValidationRPingIterations, config.Validation.RDMA.RPingIterations)
		assert.Equal(t, DefaultValidationIBWriteSize, config.Validation.RDMA.IBWriteSize)
		require.NotNil(t, config.Validation.RDMA.IBWriteMinBandwidthGbps)
		assert.Equal(t, float64(DefaultValidationIBWriteMinGbps), *config.Validation.RDMA.IBWriteMinBandwidthGbps)
	})

	t.Run("load config file that does not exist", func(t *testing.T) {
		_, err := LoadFullConfig("/nonexistent/path/config.yaml", logger)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "does not exist")
	})

	t.Run("load config with empty path returns embedded defaults", func(t *testing.T) {
		// Library-mode contract: an empty configPath yields the binary's
		// embedded default config, not an error. A caller without a
		// filesystem-resolved cluster-config still gets a populated
		// LaunchKitConfig with NetworkOperator + nv-ipam etc. defaults set.
		cfg, err := LoadFullConfig("", logger)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		require.NotNil(t, cfg.NetworkOperator, "embedded default must populate NetworkOperator")
		assert.NotEmpty(t, cfg.NetworkOperator.Version, "embedded default must carry a NetworkOperator version")
	})

	t.Run("load invalid YAML config", func(t *testing.T) {
		tempDir := t.TempDir()
		configPath := filepath.Join(tempDir, "invalid-config.yaml")

		invalidContent := `
networkOperator:
  version: v25.10.0
  this is not valid yaml content
    - broken indentation
`
		err := os.WriteFile(configPath, []byte(invalidContent), 0644)
		require.NoError(t, err)

		_, err = LoadFullConfig(configPath, logger)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse cluster config YAML")
	})
}

func TestDefaultLaunchKitConfig(t *testing.T) {
	// The embedded default config powers library-mode discovery — Go callers
	// must get a populated LaunchKitConfig with no filesystem layout on the
	// host. The key invariant is that the canonical sections (NetworkOperator,
	// DOCADriver, NvIpam) come back populated; subsequent test logic then
	// relies on those being safe to read without nil-checks.
	cfg, err := DefaultLaunchKitConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.NetworkOperator, "DefaultLaunchKitConfig must populate NetworkOperator")
	assert.NotEmpty(t, cfg.NetworkOperator.Version, "default NetworkOperator must carry a version")
	require.NotNil(t, cfg.DOCADriver, "DefaultLaunchKitConfig must populate DOCADriver")
	require.NotNil(t, cfg.NvIpam, "DefaultLaunchKitConfig must populate NvIpam")
	require.NotNil(t, cfg.Validation, "DefaultLaunchKitConfig must populate Validation")
	assert.Equal(t, ValidationModeStrict, cfg.Validation.Mode)
	assert.Equal(t, []string{ValidationCheckICMP, ValidationCheckRPing, ValidationCheckIBWriteBW}, cfg.Validation.Checks)
	require.NotNil(t, cfg.Validation.RDMA)
	assert.Equal(t, DefaultValidationRPingIterations, cfg.Validation.RDMA.RPingIterations)
	assert.Equal(t, DefaultValidationIBWriteSize, cfg.Validation.RDMA.IBWriteSize)
	require.NotNil(t, cfg.Validation.RDMA.IBWriteMinBandwidthGbps)
	assert.Equal(t, float64(DefaultValidationIBWriteMinGbps), *cfg.Validation.RDMA.IBWriteMinBandwidthGbps)
	require.NotNil(t, cfg.SpectrumX)
	require.NotNil(t, cfg.SpectrumX.SinglePlane)
	assert.Equal(t, "eth_r%rail_id%", cfg.SpectrumX.SinglePlane.NetdevPrefix)
	assert.Equal(t, "roce_r%rail_id%", cfg.SpectrumX.SinglePlane.RdmaPrefix)
	require.NotNil(t, cfg.SpectrumX.HWPLB)
	assert.Equal(t, "eth_r%rail_id%_p%plane_id%", cfg.SpectrumX.HWPLB.NetdevPrefix)
	assert.Equal(t, "roce_r%rail_id%", cfg.SpectrumX.HWPLB.RdmaPrefix)
	require.NotNil(t, cfg.SpectrumX.SWPLB)
	assert.Equal(t, "eth_r%rail_id%_p%plane_id%", cfg.SpectrumX.SWPLB.NetdevPrefix)
	assert.Equal(t, "roce_r%rail_id%_p%plane_id%", cfg.SpectrumX.SWPLB.RdmaPrefix)
}

func TestValidationConfig(t *testing.T) {
	t.Run("explicit empty checks stays empty", func(t *testing.T) {
		cfg := NormalizeValidationConfig(&ValidationConfig{Checks: []string{}})
		require.NotNil(t, cfg)
		assert.Empty(t, cfg.Checks)
	})

	t.Run("normalizes duplicates and blanks", func(t *testing.T) {
		checks := NormalizeValidationChecks([]string{" icmp ", " rping ", "", "ib_write_bw", "rping"})
		assert.Equal(t, []string{ValidationCheckICMP, ValidationCheckRPing, ValidationCheckIBWriteBW}, checks)
	})

	t.Run("rejects unknown checks", func(t *testing.T) {
		cfg := NormalizeValidationConfig(&ValidationConfig{Checks: []string{"rping", "tcp"}})
		err := ValidateValidationConfig(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported check")
	})

	t.Run("rejects unknown mode", func(t *testing.T) {
		cfg := NormalizeValidationConfig(&ValidationConfig{Mode: "deep"})
		err := ValidateValidationConfig(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "validation.mode")
	})

	t.Run("preserves explicit zero minimum bandwidth", func(t *testing.T) {
		zero := 0.0
		cfg := NormalizeValidationConfig(&ValidationConfig{RDMA: &ValidationRDMAConfig{IBWriteMinBandwidthGbps: &zero}})
		require.NotNil(t, cfg.RDMA.IBWriteMinBandwidthGbps)
		assert.Equal(t, 0.0, *cfg.RDMA.IBWriteMinBandwidthGbps)
	})
}

func TestDefaultLaunchKitConfig_FreshCopy(t *testing.T) {
	// Each call to DefaultLaunchKitConfig must return an independently
	// mutable copy — a library caller mutating one copy must not affect a
	// later caller. Mutating the version field on one copy and reading the
	// other catches the obvious shared-pointer bug.
	a, err := DefaultLaunchKitConfig()
	require.NoError(t, err)
	originalVersion := a.NetworkOperator.Version
	a.NetworkOperator.Version = "mutated-by-test"

	b, err := DefaultLaunchKitConfig()
	require.NoError(t, err)
	assert.Equal(t, originalVersion, b.NetworkOperator.Version,
		"second DefaultLaunchKitConfig() saw mutation made to the first — copies are not independent")
}

func TestDefaultConfigYAMLReturnsFreshCopy(t *testing.T) {
	a := DefaultConfigYAML()
	require.NotEmpty(t, a)
	a[0] = 'X'

	b := DefaultConfigYAML()
	require.NotEmpty(t, b)
	assert.NotEqual(t, byte('X'), b[0])
}

func TestNormalizeSpectrumXProfileConfigFromFullConfigMap(t *testing.T) {
	spcx := &ProfileSpectrumX{
		Profile: `apiVersion: v1
kind: ConfigMap
metadata:
  name: site-ra23-profile
  namespace: ignored
data:
  profile: |
    useSoftwareCCAlgorithm: true
    docaCCVersion: "example"
`,
	}

	require.NoError(t, NormalizeSpectrumXProfileConfig(spcx))
	assert.Equal(t, "site-ra23-profile", spcx.ConfigMapName)
	assert.Contains(t, spcx.Profile, "useSoftwareCCAlgorithm: true")
	assert.Contains(t, spcx.Profile, `docaCCVersion: "example"`)
	assert.NotContains(t, spcx.Profile, "kind: ConfigMap")
}

func TestNormalizeSpectrumXProfileConfigPreservesRawProfile(t *testing.T) {
	spcx := &ProfileSpectrumX{
		ConfigMapName: "site-ra23-profile",
		Profile: `useSoftwareCCAlgorithm: true
docaCCVersion: "example"
`,
	}

	require.NoError(t, NormalizeSpectrumXProfileConfig(spcx))
	assert.Equal(t, "site-ra23-profile", spcx.ConfigMapName)
	assert.Contains(t, spcx.Profile, `docaCCVersion: "example"`)
}

func TestNormalizeSpectrumXProfileConfigDoesNotMisclassifyRawDataProfile(t *testing.T) {
	raw := `data:
  profile: raw profile field owned by Spectrum-X
`
	spcx := &ProfileSpectrumX{
		ConfigMapName: "site-ra23-profile",
		Profile:       raw,
	}

	require.NoError(t, NormalizeSpectrumXProfileConfig(spcx))
	assert.Equal(t, "site-ra23-profile", spcx.ConfigMapName)
	assert.Equal(t, raw, spcx.Profile)
}

func TestValidateClusterConfig(t *testing.T) {
	t.Run("validate config with missing network operator repository", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Version:          "v25.10.0",
				ComponentVersion: "network-operator-v25.10.0",
				Repository:       "", // Missing
				Namespace:        "nvidia-network-operator",
			},
		}

		err := ValidateClusterConfig(config, "sriov-rdma")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "networkOperator.repository is required")
	})

	t.Run("validate config with missing network operator component version", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Version:          "v25.10.0",
				ComponentVersion: "", // Missing
				Repository:       "nvcr.io/nvidia/mellanox",
				Namespace:        "nvidia-network-operator",
			},
		}

		err := ValidateClusterConfig(config, "sriov-rdma")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "networkOperator.componentVersion is required")
	})

	t.Run("validate config with missing network operator namespace", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Version:          "v25.10.0",
				ComponentVersion: "network-operator-v25.10.0",
				Repository:       "nvcr.io/nvidia/mellanox",
				Namespace:        "", // Missing
			},
		}

		err := ValidateClusterConfig(config, "sriov-rdma")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "networkOperator.namespace is required")
	})

	t.Run("validate sriov profile with missing resource name", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Version:          "v25.10.0",
				ComponentVersion: "network-operator-v25.10.0",
				Repository:       "nvcr.io/nvidia/mellanox",
				Namespace:        "nvidia-network-operator",
			},
			Sriov: &SriovConfig{
				EthernetMtu:   9000,
				InfinibandMtu: 4000,
				NumVfs:        8,
				Priority:      90,
				ResourceName:  "", // Missing
				NetworkName:   "sriov_network",
			},
		}

		err := ValidateClusterConfig(config, "sriov-rdma")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "sriov.resourceName is required")
	})

	t.Run("validate sriov profile with missing network name", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Version:          "v25.10.0",
				ComponentVersion: "network-operator-v25.10.0",
				Repository:       "nvcr.io/nvidia/mellanox",
				Namespace:        "nvidia-network-operator",
			},
			Sriov: &SriovConfig{
				EthernetMtu:   9000,
				InfinibandMtu: 4000,
				NumVfs:        8,
				Priority:      90,
				ResourceName:  "sriov_resource",
				NetworkName:   "", // Missing
			},
		}

		err := ValidateClusterConfig(config, "sriov-rdma")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "sriov.networkName is required")
	})

	t.Run("validate hostdev profile with missing resource name", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Version:          "v25.10.0",
				ComponentVersion: "network-operator-v25.10.0",
				Repository:       "nvcr.io/nvidia/mellanox",
				Namespace:        "nvidia-network-operator",
			},
			Hostdev: &HostdevConfig{
				ResourceName: "", // Missing
				NetworkName:  "hostdev-network",
			},
		}

		err := ValidateClusterConfig(config, "host-device-rdma")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "hostdev.resourceName is required")
	})

	t.Run("validate valid sriov config", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Version:          "v25.10.0",
				ComponentVersion: "network-operator-v25.10.0",
				Repository:       "nvcr.io/nvidia/mellanox",
				Namespace:        "nvidia-network-operator",
			},
			Sriov: &SriovConfig{
				EthernetMtu:   9000,
				InfinibandMtu: 4000,
				NumVfs:        8,
				Priority:      90,
				ResourceName:  "sriov_resource",
				NetworkName:   "sriov_network",
			},
		}

		err := ValidateClusterConfig(config, "sriov-rdma")
		assert.NoError(t, err)
	})
}

func TestSriovConfig(t *testing.T) {
	t.Run("verify separate MTU fields in struct", func(t *testing.T) {
		config := &SriovConfig{
			EthernetMtu:   9000,
			InfinibandMtu: 4000,
			NumVfs:        8,
			Priority:      90,
			ResourceName:  "sriov_resource",
			NetworkName:   "sriov_network",
		}

		assert.Equal(t, 9000, config.EthernetMtu, "Ethernet MTU should be 9000")
		assert.Equal(t, 4000, config.InfinibandMtu, "Infiniband MTU should be 4000")
		assert.Equal(t, 8, config.NumVfs)
		assert.Equal(t, 90, config.Priority)
		assert.Equal(t, "sriov_resource", config.ResourceName)
		assert.Equal(t, "sriov_network", config.NetworkName)
	})

	t.Run("verify different MTU values for different fabrics", func(t *testing.T) {
		ethernetConfig := &SriovConfig{
			EthernetMtu:   9000,
			InfinibandMtu: 4000,
		}

		infinibandConfig := &SriovConfig{
			EthernetMtu:   9000,
			InfinibandMtu: 4000,
		}

		// Verify that we can access different MTU values for different use cases
		assert.NotEqual(t, ethernetConfig.EthernetMtu, infinibandConfig.InfinibandMtu,
			"Ethernet and Infiniband MTU values should be different")
		assert.Equal(t, 9000, ethernetConfig.EthernetMtu)
		assert.Equal(t, 4000, infinibandConfig.InfinibandMtu)
	})
}

func TestSpectrumXConfig(t *testing.T) {
	t.Run("load config with Spectrum-X parameters", func(t *testing.T) {
		tempDir := t.TempDir()
		configPath := filepath.Join(tempDir, "spectrum-x-config.yaml")

		configContent := `networkOperator:
  version: v26.1.0
  componentVersion: network-operator-v26.1.0
  repository: nvcr.io/nvidia/mellanox
  namespace: nvidia-network-operator

sriov:
  ethernetMtu: 9216
  infinibandMtu: 4000
  numVfs: 8
  priority: 90
  resourceName: rail-1
  networkName: rail-1

spectrumX:
  nicType: "1023"
  overlay: "none"
  singlePlane:
    netdevPrefix: "nic%nic_id%_r%rail%"
    rdmaPrefix: "roce_nic%nic_id%_r%rail%"
  hwplb:
    netdevPrefix: "nic%nic_id%_p%plane%_r%rail%"
    rdmaPrefix: "roce_nic%nic_id%_r%rail%"
  swplb:
    netdevPrefix: "nic%nic_id%_p%plane%_r%rail%"
    rdmaPrefix: "roce_nic%nic_id%_p%plane%_r%rail%"

profile:
  fabric: ethernet
  deployment: sriov
  multirail: true
  spectrumX:
    spcxVersion: "RA2.2"
    multiplaneMode: hwplb
    numberOfPlanes: 4
    useDRA: true
  ai: true
`
		err := os.WriteFile(configPath, []byte(configContent), 0644)
		require.NoError(t, err)

		logger := logr.Discard()
		config, err := LoadFullConfig(configPath, logger)
		require.NoError(t, err)
		require.NotNil(t, config)

		// Verify Spectrum-X config
		require.NotNil(t, config.SpectrumX)
		assert.Equal(t, "1023", config.SpectrumX.NicType)
		assert.Equal(t, "none", config.SpectrumX.Overlay)
		require.NotNil(t, config.SpectrumX.SinglePlane)
		assert.Equal(t, "roce_nic%nic_id%_r%rail%", config.SpectrumX.SinglePlane.RdmaPrefix)
		assert.Equal(t, "nic%nic_id%_r%rail%", config.SpectrumX.SinglePlane.NetdevPrefix)
		require.NotNil(t, config.SpectrumX.HWPLB)
		assert.Equal(t, "roce_nic%nic_id%_r%rail%", config.SpectrumX.HWPLB.RdmaPrefix)
		assert.Equal(t, "nic%nic_id%_p%plane%_r%rail%", config.SpectrumX.HWPLB.NetdevPrefix)
		require.NotNil(t, config.SpectrumX.SWPLB)
		assert.Equal(t, "roce_nic%nic_id%_p%plane%_r%rail%", config.SpectrumX.SWPLB.RdmaPrefix)
		assert.Equal(t, "nic%nic_id%_p%plane%_r%rail%", config.SpectrumX.SWPLB.NetdevPrefix)

		// Verify profile flags
		require.NotNil(t, config.Profile)
		require.NotNil(t, config.Profile.SpectrumX)
		assert.Equal(t, "RA2.2", config.Profile.SpectrumX.SPCXVersion)
		assert.Equal(t, "hwplb", config.Profile.SpectrumX.MultiplaneMode)
		assert.Equal(t, 4, config.Profile.SpectrumX.NumberOfPlanes)
		assert.True(t, config.Profile.SpectrumX.UseDRA)
		assert.True(t, config.Profile.Multirail)
	})

	t.Run("legacy file without mode blocks uses embedded mode defaults", func(t *testing.T) {
		configPath := filepath.Join(t.TempDir(), "legacy-spectrum-x-config.yaml")
		configContent := `networkOperator:
  version: v26.4.0
  componentVersion: network-operator-v26.4.0
  repository: nvcr.io/nvidia/mellanox
  namespace: network-operator

spectrumX:
  nicType: "1023"
  overlay: "none"
  rdmaPrefix: "legacy-rdma"
  netdevPrefix: "legacy-netdev"

profile:
  fabric: ethernet
  deployment: sriov
  multirail: true
  spectrumX:
    spcxVersion: "RA2.2"
    multiplaneMode: hwplb
    numberOfPlanes: 2
`
		require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0644))

		loaded, err := LoadFullConfig(configPath, logr.Discard())
		require.NoError(t, err)
		require.NotNil(t, loaded.SpectrumX)
		assert.Nil(t, loaded.SpectrumX.HWPLB)

		rdmaPrefix, netdevPrefix := SpectrumXInterfaceNamePrefixes(loaded.SpectrumX, "hwplb")
		assert.Equal(t, "roce_r%rail_id%", rdmaPrefix)
		assert.Equal(t, "eth_r%rail_id%_p%plane_id%", netdevPrefix)
	})

	t.Run("verify Spectrum-X struct fields", func(t *testing.T) {
		config := &SpectrumXConfig{
			NicType: "1023",
			Overlay: "none",
			HWPLB: &SpectrumXInterfaceNamePrefixConfig{
				RdmaPrefix:   "roce_",
				NetdevPrefix: "eth_p%plane%_r%rail%",
			},
		}

		assert.Equal(t, "1023", config.NicType)
		assert.Equal(t, "none", config.Overlay)
		require.NotNil(t, config.HWPLB)
		assert.Equal(t, "roce_", config.HWPLB.RdmaPrefix)
		assert.Equal(t, "eth_p%plane%_r%rail%", config.HWPLB.NetdevPrefix)
	})

	t.Run("verify different multiplane modes", func(t *testing.T) {
		modes := []string{"none", "swplb", "hwplb"}

		for _, mode := range modes {
			profileSpectrumX := &ProfileSpectrumX{
				MultiplaneMode: mode,
			}
			assert.Contains(t, modes, profileSpectrumX.MultiplaneMode,
				"Multiplane mode should be one of the supported values")
		}
	})

	t.Run("verify NIC types", func(t *testing.T) {
		connectX8 := &SpectrumXConfig{
			NicType: "1023",
		}
		blueField3 := &SpectrumXConfig{
			NicType: "a2dc",
		}

		assert.Equal(t, "1023", connectX8.NicType, "ConnectX-8 NIC type")
		assert.Equal(t, "a2dc", blueField3.NicType, "BlueField-3 SuperNIC type")
	})
}

func TestSpectrumXInterfaceNamePrefixes(t *testing.T) {
	defaults, err := DefaultLaunchKitConfig()
	require.NoError(t, err)
	require.NotNil(t, defaults.SpectrumX)

	testCases := []struct {
		mode           string
		wantRdmaPrefix string
		wantNetPrefix  string
	}{
		{
			mode:           "hwplb",
			wantRdmaPrefix: "roce_r%rail_id%",
			wantNetPrefix:  "eth_r%rail_id%_p%plane_id%",
		},
		{
			mode:           "swplb",
			wantRdmaPrefix: "roce_r%rail_id%_p%plane_id%",
			wantNetPrefix:  "eth_r%rail_id%_p%plane_id%",
		},
		{
			mode:           "none",
			wantRdmaPrefix: "roce_r%rail_id%",
			wantNetPrefix:  "eth_r%rail_id%",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.mode, func(t *testing.T) {
			rdmaPrefix, netdevPrefix := SpectrumXInterfaceNamePrefixes(defaults.SpectrumX, tc.mode)
			assert.Equal(t, tc.wantRdmaPrefix, rdmaPrefix)
			assert.Equal(t, tc.wantNetPrefix, netdevPrefix)
		})
	}

	t.Run("uses embedded defaults when the selected block is absent", func(t *testing.T) {
		rdmaPrefix, netdevPrefix := SpectrumXInterfaceNamePrefixes(&SpectrumXConfig{}, "hwplb")
		assert.Equal(t, defaults.SpectrumX.HWPLB.RdmaPrefix, rdmaPrefix)
		assert.Equal(t, defaults.SpectrumX.HWPLB.NetdevPrefix, netdevPrefix)
	})

	t.Run("preserves a partial per-mode override", func(t *testing.T) {
		settings := &SpectrumXConfig{
			HWPLB: &SpectrumXInterfaceNamePrefixConfig{
				RdmaPrefix: "custom-rdma-r%rail_id%",
			},
		}

		rdmaPrefix, netdevPrefix := SpectrumXInterfaceNamePrefixes(settings, "hwplb")
		assert.Equal(t, settings.HWPLB.RdmaPrefix, rdmaPrefix)
		assert.Equal(t, defaults.SpectrumX.HWPLB.NetdevPrefix, netdevPrefix)
	})

	t.Run("selects explicit overrides from each mode block", func(t *testing.T) {
		settings := &SpectrumXConfig{
			SinglePlane: &SpectrumXInterfaceNamePrefixConfig{
				RdmaPrefix:   "single-rdma-r%rail_id%",
				NetdevPrefix: "single-net-r%rail_id%",
			},
			HWPLB: &SpectrumXInterfaceNamePrefixConfig{
				RdmaPrefix:   "hw-rdma-r%rail_id%",
				NetdevPrefix: "hw-net-r%rail_id%-p%plane_id%",
			},
			SWPLB: &SpectrumXInterfaceNamePrefixConfig{
				RdmaPrefix:   "sw-rdma-r%rail_id%-p%plane_id%",
				NetdevPrefix: "sw-net-r%rail_id%-p%plane_id%",
			},
		}

		for _, tc := range []struct {
			mode string
			want *SpectrumXInterfaceNamePrefixConfig
		}{
			{mode: "none", want: settings.SinglePlane},
			{mode: "hwplb", want: settings.HWPLB},
			{mode: "swplb", want: settings.SWPLB},
		} {
			t.Run(tc.mode, func(t *testing.T) {
				rdmaPrefix, netdevPrefix := SpectrumXInterfaceNamePrefixes(settings, tc.mode)
				assert.Equal(t, tc.want.RdmaPrefix, rdmaPrefix)
				assert.Equal(t, tc.want.NetdevPrefix, netdevPrefix)
			})
		}
	})
}

func spectrumXConfigWithPrefixes(mode, netdevPrefix, rdmaPrefix string) *SpectrumXConfig {
	prefixes := &SpectrumXInterfaceNamePrefixConfig{
		NetdevPrefix: netdevPrefix,
		RdmaPrefix:   rdmaPrefix,
	}
	settings := &SpectrumXConfig{}
	switch mode {
	case "hwplb":
		settings.HWPLB = prefixes
	case "swplb":
		settings.SWPLB = prefixes
	default:
		settings.SinglePlane = prefixes
	}
	return settings
}

func TestValidateSpectrumXTemplates(t *testing.T) {
	t.Run("selected prefix block inherits embedded defaults", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: true,
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "RA2.2",
					MultiplaneMode: "hwplb",
					NumberOfPlanes: 2,
				},
			},
			SpectrumX: &SpectrumXConfig{},
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.NoError(t, err)
	})

	t.Run("valid multiplane and multirail templates", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: true,
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "RA2.2",
					MultiplaneMode: "swplb",
					NumberOfPlanes: 4,
				},
			},
			SpectrumX: spectrumXConfigWithPrefixes(
				"swplb",
				"nic%nic_id%_p%plane%_r%rail%",
				"roce_nic%nic_id%_p%plane%_r%rail%",
			),
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.NoError(t, err)
	})

	t.Run("multiplane without plane placeholder in netdevPrefix", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: true,
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "RA2.2",
					MultiplaneMode: "swplb",
					NumberOfPlanes: 4, // Multiplane
				},
			},
			SpectrumX: spectrumXConfigWithPrefixes(
				"swplb",
				"nic%nic_id%_r%rail%", // Missing %plane%
				"roce_p%plane%_r%rail%",
			),
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "netdevPrefix must contain %plane_id% placeholder")
	})

	t.Run("swplb without plane placeholder in rdmaPrefix", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: true,
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "RA2.2",
					MultiplaneMode: "swplb",
					NumberOfPlanes: 2, // Multiplane
				},
			},
			SpectrumX: spectrumXConfigWithPrefixes(
				"swplb",
				"nic_p%plane%_r%rail%",
				"roce_nic%nic_id%_r%rail%", // Missing %plane%
			),
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "rdmaPrefix must contain %plane_id% placeholder when multiplaneMode is swplb")
	})

	t.Run("hwplb accepts rail-only rdmaPrefix", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: true,
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "RA2.2",
					MultiplaneMode: "hwplb",
					NumberOfPlanes: 2,
				},
			},
			SpectrumX: spectrumXConfigWithPrefixes(
				"hwplb",
				"eth_r%rail_id%_p%plane_id%",
				"roce_r%rail_id%",
			),
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.NoError(t, err)
	})

	t.Run("multirail without rail placeholder in netdevPrefix", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: true, // Multirail
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "RA2.2",
					MultiplaneMode: "swplb",
					NumberOfPlanes: 1,
				},
			},
			SpectrumX: spectrumXConfigWithPrefixes(
				"swplb",
				"nic%nic_id%_p%plane%", // Missing %rail%
				"roce_r%rail%",
			),
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "netdevPrefix must contain %rail_id% placeholder")
	})

	t.Run("multirail without rail placeholder in rdmaPrefix", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: true, // Multirail
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "RA2.2",
					MultiplaneMode: "none",
					NumberOfPlanes: 1,
				},
			},
			SpectrumX: spectrumXConfigWithPrefixes(
				"none",
				"nic_p%plane%_r%rail%",
				"roce_nic%nic_id%", // Missing %rail%
			),
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "rdmaPrefix must contain %rail_id% placeholder")
	})

	t.Run("valid template with plane and rail only", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: true,
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "RA2.2",
					MultiplaneMode: "swplb",
					NumberOfPlanes: 4,
				},
			},
			SpectrumX: spectrumXConfigWithPrefixes(
				"swplb",
				"eth_p%plane%_r%rail%", // No %nic_id% but has plane and rail
				"roce_p%plane%_r%rail%",
			),
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.NoError(t, err)
	})

	t.Run("non-multiplane with single rail", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: false, // Single rail
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "RA2.2",
					MultiplaneMode: "none",
					NumberOfPlanes: 1, // Single plane
				},
			},
			SpectrumX: spectrumXConfigWithPrefixes(
				"none",
				"nic%nic_id%", // No plane or rail required
				"roce%nic_id%",
			),
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.NoError(t, err, "Single plane and single rail don't require plane/rail placeholders")
	})

	t.Run("non-SpectrumX config should not validate templates", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: true,
				SpectrumX: nil, // No Spectrum-X
			},
			Sriov: &SriovConfig{
				ResourceName: "sriov_resource",
				NetworkName:  "sriov_network",
			},
		}

		err := ValidateClusterConfig(config, "sriov-rdma")
		assert.NoError(t, err, "Non-Spectrum-X profiles should not validate template placeholders")
	})

	t.Run("multiplane mode swplb rejects unsupported version", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: true,
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "unsupported",
					MultiplaneMode: "swplb",
					NumberOfPlanes: 4,
				},
			},
			SpectrumX: spectrumXConfigWithPrefixes(
				"swplb",
				"nic_p%plane%_r%rail%",
				"roce_p%plane%_r%rail%",
			),
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "multiplane mode swplb requires spcxVersion in")
		assert.Contains(t, err.Error(), "RA2.1")
		assert.Contains(t, err.Error(), "RA2.2")
	})

	t.Run("multiplane mode hwplb rejects unsupported version", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: true,
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "unsupported",
					MultiplaneMode: "hwplb",
					NumberOfPlanes: 4,
				},
			},
			SpectrumX: spectrumXConfigWithPrefixes(
				"hwplb",
				"nic_p%plane%_r%rail%",
				"roce_p%plane%_r%rail%",
			),
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "multiplane mode hwplb requires spcxVersion in")
	})

	t.Run("multiplane mode swplb accepts RA2.1", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: true,
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "RA2.1",
					MultiplaneMode: "swplb",
					NumberOfPlanes: 2,
				},
			},
			SpectrumX: spectrumXConfigWithPrefixes(
				"swplb",
				"nic_p%plane%_r%rail%",
				"roce_p%plane%_r%rail%",
			),
		}

		err := ValidateClusterConfig(config, "spectrum-x-ra2.1")
		assert.NoError(t, err, "RA2.1 should be accepted for swplb")
	})

	t.Run("multiplane mode hwplb accepts RA2.1", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: true,
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "RA2.1",
					MultiplaneMode: "hwplb",
					NumberOfPlanes: 2,
				},
			},
			SpectrumX: spectrumXConfigWithPrefixes(
				"hwplb",
				"nic_p%plane%_r%rail%",
				"roce_p%plane%_r%rail%",
			),
		}

		err := ValidateClusterConfig(config, "spectrum-x-ra2.1")
		assert.NoError(t, err, "RA2.1 should be accepted for hwplb")
	})

	t.Run("multiplane mode none accepts any version", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: false,
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "unsupported", // mode "none" doesn't validate version
					MultiplaneMode: "none",
					NumberOfPlanes: 1,
				},
			},
			SpectrumX: spectrumXConfigWithPrefixes(
				"none",
				"nic%nic_id%",
				"roce%nic_id%",
			),
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.NoError(t, err, "Mode 'none' should accept any version")
	})

	t.Run("multiplane and multirail with missing both placeholders", func(t *testing.T) {
		config := &LaunchKitConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: true,
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "RA2.2",
					MultiplaneMode: "swplb",
					NumberOfPlanes: 4,
				},
			},
			SpectrumX: spectrumXConfigWithPrefixes(
				"swplb",
				"static_interface", // Missing both %plane% and %rail%
				"roce_p%plane%_r%rail%",
			),
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.Error(t, err)
		// Should fail on the first check (plane)
		assert.Contains(t, err.Error(), "netdevPrefix must contain %plane_id% placeholder")
	})
}

func TestGenerateSubnets(t *testing.T) {
	t.Run("basic sequential /24 subnets", func(t *testing.T) {
		subnets, err := GenerateSubnets("192.168.2.0", 24, 1, 4)
		require.NoError(t, err)
		require.Len(t, subnets, 4)

		assert.Equal(t, "192.168.2.0/24", subnets[0].Subnet)
		assert.Equal(t, "192.168.2.1", subnets[0].Gateway)
		assert.Equal(t, "192.168.3.0/24", subnets[1].Subnet)
		assert.Equal(t, "192.168.3.1", subnets[1].Gateway)
		assert.Equal(t, "192.168.4.0/24", subnets[2].Subnet)
		assert.Equal(t, "192.168.4.1", subnets[2].Gateway)
		assert.Equal(t, "192.168.5.0/24", subnets[3].Subnet)
		assert.Equal(t, "192.168.5.1", subnets[3].Gateway)
	})

	t.Run("offset of 2 skips every other subnet", func(t *testing.T) {
		subnets, err := GenerateSubnets("10.0.0.0", 24, 2, 3)
		require.NoError(t, err)
		require.Len(t, subnets, 3)

		assert.Equal(t, "10.0.0.0/24", subnets[0].Subnet)
		assert.Equal(t, "10.0.0.1", subnets[0].Gateway)
		assert.Equal(t, "10.0.2.0/24", subnets[1].Subnet)
		assert.Equal(t, "10.0.2.1", subnets[1].Gateway)
		assert.Equal(t, "10.0.4.0/24", subnets[2].Subnet)
		assert.Equal(t, "10.0.4.1", subnets[2].Gateway)
	})

	t.Run("/16 subnets", func(t *testing.T) {
		subnets, err := GenerateSubnets("10.0.0.0", 16, 1, 3)
		require.NoError(t, err)
		require.Len(t, subnets, 3)

		assert.Equal(t, "10.0.0.0/16", subnets[0].Subnet)
		assert.Equal(t, "10.0.0.1", subnets[0].Gateway)
		assert.Equal(t, "10.1.0.0/16", subnets[1].Subnet)
		assert.Equal(t, "10.1.0.1", subnets[1].Gateway)
		assert.Equal(t, "10.2.0.0/16", subnets[2].Subnet)
		assert.Equal(t, "10.2.0.1", subnets[2].Gateway)
	})

	t.Run("single subnet", func(t *testing.T) {
		subnets, err := GenerateSubnets("172.16.0.0", 24, 1, 1)
		require.NoError(t, err)
		require.Len(t, subnets, 1)

		assert.Equal(t, "172.16.0.0/24", subnets[0].Subnet)
		assert.Equal(t, "172.16.0.1", subnets[0].Gateway)
	})

	t.Run("reject misaligned starting address", func(t *testing.T) {
		_, err := GenerateSubnets("192.168.2.5", 24, 1, 4)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not aligned")
	})

	t.Run("reject invalid mask 0", func(t *testing.T) {
		_, err := GenerateSubnets("192.168.0.0", 0, 1, 1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "mask must be between 1 and 30")
	})

	t.Run("reject invalid mask 31", func(t *testing.T) {
		_, err := GenerateSubnets("192.168.0.0", 31, 1, 1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "mask must be between 1 and 30")
	})

	t.Run("reject invalid mask 32", func(t *testing.T) {
		_, err := GenerateSubnets("192.168.0.0", 32, 1, 1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "mask must be between 1 and 30")
	})

	t.Run("reject offset 0", func(t *testing.T) {
		_, err := GenerateSubnets("192.168.0.0", 24, 0, 4)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "offset must be >= 1")
	})

	t.Run("reject count 0", func(t *testing.T) {
		_, err := GenerateSubnets("192.168.0.0", 24, 1, 0)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "count must be >= 1")
	})

	t.Run("reject invalid IP address", func(t *testing.T) {
		_, err := GenerateSubnets("not-an-ip", 24, 1, 1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid starting subnet IP")
	})

	t.Run("reject IPv6 address", func(t *testing.T) {
		_, err := GenerateSubnets("::1", 24, 1, 1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "must be an IPv4 address")
	})

	t.Run("reject overflow", func(t *testing.T) {
		// 255.255.254.0/24 with count=3 → third subnet would be 256.0.0.0
		_, err := GenerateSubnets("255.255.254.0", 24, 1, 3)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "overflow")
	})

	t.Run("boundary - last valid /24 subnet", func(t *testing.T) {
		subnets, err := GenerateSubnets("255.255.255.0", 24, 1, 1)
		require.NoError(t, err)
		require.Len(t, subnets, 1)
		assert.Equal(t, "255.255.255.0/24", subnets[0].Subnet)
		assert.Equal(t, "255.255.255.1", subnets[0].Gateway)
	})
}

func TestReservedExclusions(t *testing.T) {
	t.Run("user case: /24 reserve 10 low and 6 high", func(t *testing.T) {
		ex, err := reservedExclusions("192.168.0.0/24", 10, 6)
		require.NoError(t, err)
		require.Len(t, ex, 2)
		assert.Equal(t, NvIpamExclusion{StartIP: "192.168.0.0", EndIP: "192.168.0.9"}, ex[0])
		assert.Equal(t, NvIpamExclusion{StartIP: "192.168.0.250", EndIP: "192.168.0.255"}, ex[1])
	})

	t.Run("non-zero base subnet keeps ranges relative to its own network", func(t *testing.T) {
		ex, err := reservedExclusions("192.168.5.0/24", 10, 6)
		require.NoError(t, err)
		require.Len(t, ex, 2)
		assert.Equal(t, NvIpamExclusion{StartIP: "192.168.5.0", EndIP: "192.168.5.9"}, ex[0])
		assert.Equal(t, NvIpamExclusion{StartIP: "192.168.5.250", EndIP: "192.168.5.255"}, ex[1])
	})

	t.Run("/16 subnet", func(t *testing.T) {
		ex, err := reservedExclusions("10.1.0.0/16", 10, 6)
		require.NoError(t, err)
		require.Len(t, ex, 2)
		assert.Equal(t, NvIpamExclusion{StartIP: "10.1.0.0", EndIP: "10.1.0.9"}, ex[0])
		assert.Equal(t, NvIpamExclusion{StartIP: "10.1.255.250", EndIP: "10.1.255.255"}, ex[1])
	})

	t.Run("reserve first only", func(t *testing.T) {
		ex, err := reservedExclusions("192.168.0.0/24", 5, 0)
		require.NoError(t, err)
		require.Len(t, ex, 1)
		assert.Equal(t, NvIpamExclusion{StartIP: "192.168.0.0", EndIP: "192.168.0.4"}, ex[0])
	})

	t.Run("reserve last only", func(t *testing.T) {
		ex, err := reservedExclusions("192.168.0.0/24", 0, 5)
		require.NoError(t, err)
		require.Len(t, ex, 1)
		assert.Equal(t, NvIpamExclusion{StartIP: "192.168.0.251", EndIP: "192.168.0.255"}, ex[0])
	})

	t.Run("both zero returns nil", func(t *testing.T) {
		ex, err := reservedExclusions("192.168.0.0/24", 0, 0)
		require.NoError(t, err)
		assert.Nil(t, ex)
	})

	t.Run("reserve exceeding block size errors", func(t *testing.T) {
		_, err := reservedExclusions("192.168.0.0/24", 200, 200)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no usable addresses")
	})

	t.Run("invalid CIDR errors", func(t *testing.T) {
		_, err := reservedExclusions("not-a-cidr", 1, 1)
		require.Error(t, err)
	})
}

func TestApplyReservedExclusions(t *testing.T) {
	t.Run("prepends reserve before explicit exclusions", func(t *testing.T) {
		subnets := []NvIpamSubnetConfig{{
			Subnet:  "192.168.0.0/24",
			Gateway: "192.168.0.1",
			Exclusions: []NvIpamExclusion{
				{StartIP: "192.168.0.100", EndIP: "192.168.0.100"},
			},
		}}
		require.NoError(t, ApplyReservedExclusions(subnets, 10, 6))
		require.Len(t, subnets[0].Exclusions, 3)
		assert.Equal(t, NvIpamExclusion{StartIP: "192.168.0.0", EndIP: "192.168.0.9"}, subnets[0].Exclusions[0])
		assert.Equal(t, NvIpamExclusion{StartIP: "192.168.0.250", EndIP: "192.168.0.255"}, subnets[0].Exclusions[1])
		assert.Equal(t, NvIpamExclusion{StartIP: "192.168.0.100", EndIP: "192.168.0.100"}, subnets[0].Exclusions[2])
	})

	t.Run("no-op when both counts zero, explicit preserved", func(t *testing.T) {
		subnets := []NvIpamSubnetConfig{{
			Subnet:     "192.168.0.0/24",
			Exclusions: []NvIpamExclusion{{StartIP: "192.168.0.5", EndIP: "192.168.0.5"}},
		}}
		require.NoError(t, ApplyReservedExclusions(subnets, 0, 0))
		require.Len(t, subnets[0].Exclusions, 1)
		assert.Equal(t, NvIpamExclusion{StartIP: "192.168.0.5", EndIP: "192.168.0.5"}, subnets[0].Exclusions[0])
	})

	t.Run("applies across every subnet in the slice", func(t *testing.T) {
		subnets := []NvIpamSubnetConfig{
			{Subnet: "192.168.0.0/24"},
			{Subnet: "192.168.1.0/24"},
		}
		require.NoError(t, ApplyReservedExclusions(subnets, 10, 6))
		require.Len(t, subnets[0].Exclusions, 2)
		require.Len(t, subnets[1].Exclusions, 2)
		assert.Equal(t, "192.168.1.250", subnets[1].Exclusions[1].StartIP)
		assert.Equal(t, "192.168.1.255", subnets[1].Exclusions[1].EndIP)
	})
}

func TestValidateNvIpam(t *testing.T) {
	t.Run("nil is valid", func(t *testing.T) {
		assert.NoError(t, validateNvIpam(nil))
	})

	t.Run("negative reserveFirstIPs", func(t *testing.T) {
		err := validateNvIpam(&NvIpamConfig{ReserveFirstIPs: -1})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reserveFirstIPs must be >= 0")
	})

	t.Run("negative reserveLastIPs", func(t *testing.T) {
		err := validateNvIpam(&NvIpamConfig{ReserveLastIPs: -1})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reserveLastIPs must be >= 0")
	})

	t.Run("oversized reserve against manual subnet", func(t *testing.T) {
		err := validateNvIpam(&NvIpamConfig{
			ReserveFirstIPs: 200,
			ReserveLastIPs:  200,
			Subnets:         []NvIpamSubnetConfig{{Subnet: "192.168.0.0/24"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no usable addresses")
	})

	t.Run("oversized reserve against auto-gen subnet caught at load time", func(t *testing.T) {
		err := validateNvIpam(&NvIpamConfig{
			StartingSubnet:  "192.168.0.0",
			Mask:            24,
			ReserveFirstIPs: 300,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no usable addresses")
	})

	t.Run("invalid manual subnet CIDR", func(t *testing.T) {
		err := validateNvIpam(&NvIpamConfig{
			Subnets: []NvIpamSubnetConfig{{Subnet: "not-a-cidr"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not valid CIDR")
	})

	t.Run("explicit exclusion outside its subnet", func(t *testing.T) {
		err := validateNvIpam(&NvIpamConfig{
			Subnets: []NvIpamSubnetConfig{{
				Subnet:     "192.168.0.0/24",
				Exclusions: []NvIpamExclusion{{StartIP: "192.168.5.2", EndIP: "192.168.5.3"}},
			}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside subnet")
	})

	t.Run("invalid explicit startIP", func(t *testing.T) {
		err := validateNvIpam(&NvIpamConfig{
			Subnets: []NvIpamSubnetConfig{{
				Subnet:     "192.168.0.0/24",
				Exclusions: []NvIpamExclusion{{StartIP: "nope", EndIP: "192.168.0.9"}},
			}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid IPv4 startIP")
	})

	t.Run("inverted explicit range", func(t *testing.T) {
		err := validateNvIpam(&NvIpamConfig{
			Subnets: []NvIpamSubnetConfig{{
				Subnet:     "192.168.0.0/24",
				Exclusions: []NvIpamExclusion{{StartIP: "192.168.0.9", EndIP: "192.168.0.1"}},
			}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be <= endIP")
	})

	t.Run("valid reserve + explicit", func(t *testing.T) {
		err := validateNvIpam(&NvIpamConfig{
			ReserveFirstIPs: 10,
			ReserveLastIPs:  6,
			Subnets: []NvIpamSubnetConfig{{
				Subnet:     "192.168.0.0/24",
				Exclusions: []NvIpamExclusion{{StartIP: "192.168.0.2", EndIP: "192.168.0.3"}},
			}},
		})
		assert.NoError(t, err)
	})
}
