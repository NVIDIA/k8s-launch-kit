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
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"gopkg.in/yaml.v2"
)

// LaunchKubernetesConfig represents the l8k-config.yaml structure
type LaunchKubernetesConfig struct {
	NetworkOperator *NetworkOperatorConfig `yaml:"networkOperator,omitempty"`
	DOCADriver      *DOCADriverConfig      `yaml:"docaDriver,omitempty"`
	NvIpam          *NvIpamConfig          `yaml:"nvIpam,omitempty"`
	Sriov           *SriovConfig           `yaml:"sriov,omitempty"`
	Hostdev         *HostdevConfig         `yaml:"hostdev,omitempty"`
	RdmaShared      *RdmaSharedConfig      `yaml:"rdmaShared,omitempty"`
	Ipoib           *IpoibConfig           `yaml:"ipoib,omitempty"`
	Macvlan         *MacvlanConfig         `yaml:"macvlan,omitempty"`
	SpectrumX       *SpectrumXConfig       `yaml:"spectrumX,omitempty"`
	Profile         *Profile               `yaml:"profile,omitempty"`
	ClusterConfig   []ClusterConfig        `yaml:"clusterConfig,omitempty"`
}

type NetworkOperatorConfig struct {
	Version          string `yaml:"version"`
	ComponentVersion string `yaml:"componentVersion"`
	Repository       string `yaml:"repository"`
	Namespace        string `yaml:"namespace"`
}

type DOCADriverConfig struct {
	Enable               bool   `yaml:"enable"`
	Version              string `yaml:"version"`
	UnloadStorageModules bool   `yaml:"unloadStorageModules"`
	EnableNFSRDMA        bool   `yaml:"enableNFSRDMA"`
}

type NvIpamConfig struct {
	PoolName       string               `yaml:"poolName"`
	Subnets        []NvIpamSubnetConfig `yaml:"subnets,omitempty"`
	StartingSubnet string               `yaml:"startingSubnet,omitempty"`
	Mask           int                  `yaml:"mask,omitempty"`
	Offset         int                  `yaml:"offset,omitempty"`
}

type NvIpamSubnetConfig struct {
	Subnet  string `yaml:"subnet"`
	Gateway string `yaml:"gateway"`
}

type SriovConfig struct {
	EthernetMtu   int    `yaml:"ethernetMtu"`
	InfinibandMtu int    `yaml:"infinibandMtu"`
	NumVfs        int    `yaml:"numVfs"`
	Priority      int    `yaml:"priority"`
	ResourceName  string `yaml:"resourceName"`
	NetworkName   string `yaml:"networkName"`
}

type SpectrumXConfig struct {
	NicType      string `yaml:"nicType"`      // "1023" for ConnectX-8, "a2dc" for BlueField-3 SuperNIC
	Overlay      string `yaml:"overlay"`      // "none"
	RdmaPrefix   string `yaml:"rdmaPrefix"`   // e.g., "roce_nic%nic_id%_p%plane%_r%rail%"
	NetdevPrefix string `yaml:"netdevPrefix"` // e.g., "nic%nic_id%_p%plane%_r%rail%"
}

type HostdevConfig struct {
	ResourceName string `yaml:"resourceName"`
	NetworkName  string `yaml:"networkName"`
}

type RdmaSharedConfig struct {
	ResourceName string `yaml:"resourceName"`
	HcaMax       int    `yaml:"hcaMax"`
}

type IpoibConfig struct {
	NetworkName string `yaml:"networkName"`
}

type MacvlanConfig struct {
	NetworkName string `yaml:"networkName"`
}

type Profile struct {
	Fabric     string           `yaml:"fabric"`
	Deployment string           `yaml:"deployment"`
	Multirail  bool             `yaml:"multirail"`
	SpectrumX  *ProfileSpectrumX `yaml:"spectrumX,omitempty"`
	Ai         bool             `yaml:"ai"`
}

type ProfileSpectrumX struct {
	Enable         bool   `yaml:"enable"`         // must be true for Spectrum-X profiles to match
	SPCXVersion    string `yaml:"spcxVersion"`    // e.g., "RA2.1"
	MultiplaneMode string `yaml:"multiplaneMode"` // swplb, hwplb, uniplane
	NumberOfPlanes int    `yaml:"numberOfPlanes"` // 2 or 4
}

type ClusterConfig struct {
	Identifier    string              `yaml:"identifier"`
	MachineType   string              `yaml:"machineType,omitempty"`
	ProductType   string              `yaml:"productType,omitempty"`
	LabelSelector map[string]string   `yaml:"labelSelector,omitempty"`
	Capabilities  *ClusterCapabilities `yaml:"capabilities"`
	PFs           []PFConfig           `yaml:"pfs"`
	WorkerNodes   []string             `yaml:"workerNodes"`
	NodeSelector  map[string]string    `yaml:"nodeSelector,omitempty"`
}

type ClusterCapabilities struct {
	Nodes *NodesCapabilities `yaml:"nodes"`
}

type NodesCapabilities struct {
	Sriov bool `yaml:"sriov"`
	Rdma  bool `yaml:"rdma"`
	Ib    bool `yaml:"ib"`
}

type PFConfig struct {
	DeviceID         string `yaml:"deviceID"`
	RdmaDevice       string `yaml:"rdmaDevice"`
	PciAddress       string `yaml:"pciAddress"`
	NetworkInterface string `yaml:"networkInterface"`
	Traffic          string `yaml:"traffic"`
	Rail             *int   `yaml:"rail,omitempty"`
	PSID             string `yaml:"psid,omitempty"`
	PartNumber       string `yaml:"partNumber,omitempty"`
}

// AggregateCapabilities computes the union of capabilities across all cluster config groups.
// If any group has a capability, the aggregate has it.
func AggregateCapabilities(groups []ClusterConfig) *ClusterCapabilities {
	result := &ClusterCapabilities{Nodes: &NodesCapabilities{}}
	for _, g := range groups {
		if g.Capabilities != nil && g.Capabilities.Nodes != nil {
			result.Nodes.Sriov = result.Nodes.Sriov || g.Capabilities.Nodes.Sriov
			result.Nodes.Rdma = result.Nodes.Rdma || g.Capabilities.Nodes.Rdma
			result.Nodes.Ib = result.Nodes.Ib || g.Capabilities.Nodes.Ib
		}
	}
	return result
}

// LoadFullConfig loads and parses the cluster configuration from the specified path
func LoadFullConfig(configPath string, logger logr.Logger) (*LaunchKubernetesConfig, error) {
	if configPath == "" {
		return nil, fmt.Errorf("no cluster configuration path provided")
	}

	logger.Info("Loading cluster configuration", "path", configPath)

	// Check if config file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("cluster config file does not exist: %s", configPath)
	}

	// Read the configuration file
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read cluster config file %s: %w", configPath, err)
	}

	// Parse the YAML configuration
	var config LaunchKubernetesConfig
	if err := yaml.Unmarshal(configData, &config); err != nil {
		return nil, fmt.Errorf("failed to parse cluster config YAML %s: %w", configPath, err)
	}

	logger.Info("Cluster configuration loaded successfully",
		"networkOperatorVersion", config.NetworkOperator.Version,
		"namespace", config.NetworkOperator.Namespace)

	return &config, nil
}

// ValidateClusterConfig validates that essential fields are present in the cluster config
func ValidateClusterConfig(config *LaunchKubernetesConfig, profile string) error {
	if config.NetworkOperator.Repository == "" {
		return fmt.Errorf("networkOperator.repository is required")
	}

	if config.NetworkOperator.ComponentVersion == "" {
		return fmt.Errorf("networkOperator.componentVersion is required")
	}

	if config.NetworkOperator.Namespace == "" {
		return fmt.Errorf("networkOperator.namespace is required")
	}

	// Validate Spectrum-X specific requirements
	if config.Profile != nil && config.Profile.SpectrumX != nil && config.SpectrumX != nil {
		if err := validateSpectrumXTemplates(config); err != nil {
			return err
		}
	}

	// Validate profile-specific requirements based on the selected profile
	if profile == "host-device-rdma" || profile == "hostdevice" {
		if config.Hostdev.ResourceName == "" {
			return fmt.Errorf("hostdev.resourceName is required for hostdevice profiles")
		}
		if config.Hostdev.NetworkName == "" {
			return fmt.Errorf("hostdev.networkName is required for hostdevice profiles")
		}
	}

	if profile == "sriov-rdma" || profile == "sriov-ib-rdma" {
		if config.Sriov.ResourceName == "" {
			return fmt.Errorf("sriov.resourceName is required for SR-IOV profiles")
		}
		if config.Sriov.NetworkName == "" {
			return fmt.Errorf("sriov.networkName is required for SR-IOV profiles")
		}
	}

	return nil
}

// validateSpectrumXTemplates validates that Spectrum-X templates have required placeholders
func validateSpectrumXTemplates(config *LaunchKubernetesConfig) error {
	netdevPrefix := config.SpectrumX.NetdevPrefix
	rdmaPrefix := config.SpectrumX.RdmaPrefix
	
	// Multiplane modes (swplb, hwplb, uniplane) require RA2.1
	if config.Profile.SpectrumX.MultiplaneMode != "none" && config.Profile.SpectrumX.MultiplaneMode != "" {
		if config.Profile.SpectrumX.SPCXVersion != "RA2.1" {
			return fmt.Errorf("multiplane mode %s requires spcxVersion RA2.1, got %s",
				config.Profile.SpectrumX.MultiplaneMode, config.Profile.SpectrumX.SPCXVersion)
		}
	}

	isMultiplane := config.Profile.SpectrumX.NumberOfPlanes > 1
	isMultirail := config.Profile.Multirail
	
	// Check netdevPrefix
	hasPlaneInNetdev := containsPlaceholder(netdevPrefix, "%plane%")
	hasRailInNetdev := containsPlaceholder(netdevPrefix, "%rail%")
	
	if isMultiplane && !hasPlaneInNetdev {
		return fmt.Errorf("spectrumX.netdevPrefix must contain %%plane%% placeholder when numberOfPlanes > 1 (multiplane mode)")
	}
	
	if isMultirail && !hasRailInNetdev {
		return fmt.Errorf("spectrumX.netdevPrefix must contain %%rail%% placeholder when multirail is enabled")
	}
	
	// Check rdmaPrefix (same rules)
	hasPlaneInRdma := containsPlaceholder(rdmaPrefix, "%plane%")
	hasRailInRdma := containsPlaceholder(rdmaPrefix, "%rail%")
	
	if isMultiplane && !hasPlaneInRdma {
		return fmt.Errorf("spectrumX.rdmaPrefix must contain %%plane%% placeholder when numberOfPlanes > 1 (multiplane mode)")
	}
	
	if isMultirail && !hasRailInRdma {
		return fmt.Errorf("spectrumX.rdmaPrefix must contain %%rail%% placeholder when multirail is enabled")
	}
	
	return nil
}

// containsPlaceholder checks if a string contains a specific placeholder
func containsPlaceholder(s, placeholder string) bool {
	return len(s) > 0 && len(placeholder) > 0 &&
		len(s) >= len(placeholder) &&
		strings.Contains(s, placeholder)
}

// GenerateSubnets creates a list of subnet configurations by incrementing from a
// starting network address. Each subsequent subnet is offset by `offset` subnet-sized
// blocks. The gateway for each subnet is the first usable address (network + 1).
func GenerateSubnets(startingSubnet string, mask, offset, count int) ([]NvIpamSubnetConfig, error) {
	if count < 1 {
		return nil, fmt.Errorf("subnet count must be >= 1, got %d", count)
	}
	if mask < 1 || mask > 30 {
		return nil, fmt.Errorf("mask must be between 1 and 30, got %d", mask)
	}
	if offset < 1 {
		return nil, fmt.Errorf("offset must be >= 1, got %d", offset)
	}

	ip := net.ParseIP(startingSubnet)
	if ip == nil {
		return nil, fmt.Errorf("invalid starting subnet IP: %q", startingSubnet)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("starting subnet must be an IPv4 address, got %q", startingSubnet)
	}

	baseIP := binary.BigEndian.Uint32(ip4)
	blockSize := uint32(1) << uint(32-mask)
	hostMask := blockSize - 1

	// Verify the starting address is properly aligned (host bits must be zero)
	if baseIP&hostMask != 0 {
		return nil, fmt.Errorf("starting subnet %s is not aligned for /%d (host bits are not zero)", startingSubnet, mask)
	}

	// Check that the last subnet won't overflow the IPv4 address space
	lastIndex := uint64(count-1) * uint64(offset)
	lastAddr := uint64(baseIP) + lastIndex*uint64(blockSize)
	if lastAddr > math.MaxUint32 {
		return nil, fmt.Errorf("subnet generation would overflow IPv4 address space: starting %s/%d, offset %d, count %d",
			startingSubnet, mask, offset, count)
	}

	subnets := make([]NvIpamSubnetConfig, count)
	for i := 0; i < count; i++ {
		networkAddr := baseIP + uint32(i*offset)*blockSize
		subnetIP := make(net.IP, 4)
		binary.BigEndian.PutUint32(subnetIP, networkAddr)
		gatewayIP := make(net.IP, 4)
		binary.BigEndian.PutUint32(gatewayIP, networkAddr+1)

		subnets[i] = NvIpamSubnetConfig{
			Subnet:  fmt.Sprintf("%s/%d", subnetIP.String(), mask),
			Gateway: gatewayIP.String(),
		}
	}

	return subnets, nil
}
