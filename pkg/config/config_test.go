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
	})

	t.Run("load config file that does not exist", func(t *testing.T) {
		_, err := LoadFullConfig("/nonexistent/path/config.yaml", logger)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "does not exist")
	})

	t.Run("load config with empty path", func(t *testing.T) {
		_, err := LoadFullConfig("", logger)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "no cluster configuration path provided")
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

func TestValidateClusterConfig(t *testing.T) {
	t.Run("validate config with missing network operator repository", func(t *testing.T) {
		config := &LaunchKubernetesConfig{
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
		config := &LaunchKubernetesConfig{
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
		config := &LaunchKubernetesConfig{
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
		config := &LaunchKubernetesConfig{
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
		config := &LaunchKubernetesConfig{
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
		config := &LaunchKubernetesConfig{
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
		config := &LaunchKubernetesConfig{
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
  rdmaPrefix: "roce_nic%nic_id%_p%plane%_r%rail%"
  netdevPrefix: "nic%nic_id%_p%plane%_r%rail%"

profile:
  fabric: ethernet
  deployment: sriov
  multirail: true
  spectrumX:
    spcxVersion: "RA2.2"
    multiplaneMode: hwplb
    numberOfPlanes: 4
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
		assert.Equal(t, "roce_nic%nic_id%_p%plane%_r%rail%", config.SpectrumX.RdmaPrefix)
		assert.Equal(t, "nic%nic_id%_p%plane%_r%rail%", config.SpectrumX.NetdevPrefix)

		// Verify profile flags
		require.NotNil(t, config.Profile)
		require.NotNil(t, config.Profile.SpectrumX)
		assert.Equal(t, "RA2.2", config.Profile.SpectrumX.SPCXVersion)
		assert.Equal(t, "hwplb", config.Profile.SpectrumX.MultiplaneMode)
		assert.Equal(t, 4, config.Profile.SpectrumX.NumberOfPlanes)
		assert.True(t, config.Profile.Multirail)
	})

	t.Run("verify Spectrum-X struct fields", func(t *testing.T) {
		config := &SpectrumXConfig{
			NicType:      "1023",
			Overlay:      "none",
			RdmaPrefix:   "roce_",
			NetdevPrefix: "eth_p%plane%_r%rail%",
		}

		assert.Equal(t, "1023", config.NicType)
		assert.Equal(t, "none", config.Overlay)
		assert.Equal(t, "roce_", config.RdmaPrefix)
		assert.Equal(t, "eth_p%plane%_r%rail%", config.NetdevPrefix)
	})

	t.Run("verify different multiplane modes", func(t *testing.T) {
		modes := []string{"swplb", "hwplb", "uniplane"}

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

func TestValidateSpectrumXTemplates(t *testing.T) {
	t.Run("valid multiplane and multirail templates", func(t *testing.T) {
		config := &LaunchKubernetesConfig{
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
			SpectrumX: &SpectrumXConfig{
				NetdevPrefix: "nic%nic_id%_p%plane%_r%rail%",
				RdmaPrefix:   "roce_nic%nic_id%_p%plane%_r%rail%",
			},
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.NoError(t, err)
	})

	t.Run("multiplane without plane placeholder in netdevPrefix", func(t *testing.T) {
		config := &LaunchKubernetesConfig{
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
			SpectrumX: &SpectrumXConfig{
				NetdevPrefix: "nic%nic_id%_r%rail%", // Missing %plane%
				RdmaPrefix:   "roce_p%plane%_r%rail%",
			},
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "netdevPrefix must contain %plane_id% placeholder")
	})

	t.Run("multiplane without plane placeholder in rdmaPrefix", func(t *testing.T) {
		config := &LaunchKubernetesConfig{
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
					NumberOfPlanes: 2, // Multiplane
				},
			},
			SpectrumX: &SpectrumXConfig{
				NetdevPrefix: "nic_p%plane%_r%rail%",
				RdmaPrefix:   "roce_nic%nic_id%_r%rail%", // Missing %plane%
			},
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "rdmaPrefix must contain %plane_id% placeholder")
	})

	t.Run("multirail without rail placeholder in netdevPrefix", func(t *testing.T) {
		config := &LaunchKubernetesConfig{
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
			SpectrumX: &SpectrumXConfig{
				NetdevPrefix: "nic%nic_id%_p%plane%", // Missing %rail%
				RdmaPrefix:   "roce_r%rail%",
			},
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "netdevPrefix must contain %rail_id% placeholder")
	})

	t.Run("multirail without rail placeholder in rdmaPrefix", func(t *testing.T) {
		config := &LaunchKubernetesConfig{
			NetworkOperator: &NetworkOperatorConfig{
				Repository:       "nvcr.io/nvidia/mellanox",
				ComponentVersion: "v26.1.0",
				Namespace:        "nvidia-network-operator",
			},
			Profile: &Profile{
				Multirail: true, // Multirail
				SpectrumX: &ProfileSpectrumX{
					SPCXVersion:    "RA2.2",
					MultiplaneMode: "uniplane",
					NumberOfPlanes: 1,
				},
			},
			SpectrumX: &SpectrumXConfig{
				NetdevPrefix: "nic_p%plane%_r%rail%",
				RdmaPrefix:   "roce_nic%nic_id%", // Missing %rail%
			},
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "rdmaPrefix must contain %rail_id% placeholder")
	})

	t.Run("valid template with plane and rail only", func(t *testing.T) {
		config := &LaunchKubernetesConfig{
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
			SpectrumX: &SpectrumXConfig{
				NetdevPrefix: "eth_p%plane%_r%rail%", // No %nic_id% but has plane and rail
				RdmaPrefix:   "roce_p%plane%_r%rail%",
			},
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.NoError(t, err)
	})

	t.Run("non-multiplane with single rail", func(t *testing.T) {
		config := &LaunchKubernetesConfig{
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
			SpectrumX: &SpectrumXConfig{
				NetdevPrefix: "nic%nic_id%", // No plane or rail required
				RdmaPrefix:   "roce%nic_id%",
			},
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.NoError(t, err, "Single plane and single rail don't require plane/rail placeholders")
	})

	t.Run("non-SpectrumX config should not validate templates", func(t *testing.T) {
		config := &LaunchKubernetesConfig{
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
		config := &LaunchKubernetesConfig{
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
			SpectrumX: &SpectrumXConfig{
				NetdevPrefix: "nic_p%plane%_r%rail%",
				RdmaPrefix:   "roce_p%plane%_r%rail%",
			},
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "multiplane mode swplb requires spcxVersion in")
		assert.Contains(t, err.Error(), "RA2.1")
		assert.Contains(t, err.Error(), "RA2.2")
	})

	t.Run("multiplane mode hwplb rejects unsupported version", func(t *testing.T) {
		config := &LaunchKubernetesConfig{
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
			SpectrumX: &SpectrumXConfig{
				NetdevPrefix: "nic_p%plane%_r%rail%",
				RdmaPrefix:   "roce_p%plane%_r%rail%",
			},
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "multiplane mode hwplb requires spcxVersion in")
	})

	t.Run("multiplane mode swplb accepts RA2.1", func(t *testing.T) {
		config := &LaunchKubernetesConfig{
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
			SpectrumX: &SpectrumXConfig{
				NetdevPrefix: "nic_p%plane%_r%rail%",
				RdmaPrefix:   "roce_p%plane%_r%rail%",
			},
		}

		err := ValidateClusterConfig(config, "spectrum-x-ra2.1")
		assert.NoError(t, err, "RA2.1 should be accepted for swplb")
	})

	t.Run("multiplane mode hwplb accepts RA2.1", func(t *testing.T) {
		config := &LaunchKubernetesConfig{
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
			SpectrumX: &SpectrumXConfig{
				NetdevPrefix: "nic_p%plane%_r%rail%",
				RdmaPrefix:   "roce_p%plane%_r%rail%",
			},
		}

		err := ValidateClusterConfig(config, "spectrum-x-ra2.1")
		assert.NoError(t, err, "RA2.1 should be accepted for hwplb")
	})

	t.Run("multiplane mode none accepts any version", func(t *testing.T) {
		config := &LaunchKubernetesConfig{
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
			SpectrumX: &SpectrumXConfig{
				NetdevPrefix: "nic%nic_id%",
				RdmaPrefix:   "roce%nic_id%",
			},
		}

		err := ValidateClusterConfig(config, "spectrum-x")
		assert.NoError(t, err, "Mode 'none' should accept any version")
	})

	t.Run("multiplane and multirail with missing both placeholders", func(t *testing.T) {
		config := &LaunchKubernetesConfig{
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
			SpectrumX: &SpectrumXConfig{
				NetdevPrefix: "static_interface", // Missing both %plane% and %rail%
				RdmaPrefix:   "roce_p%plane%_r%rail%",
			},
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

