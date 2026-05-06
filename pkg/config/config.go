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
	SpectrumX                *SpectrumXConfig                `yaml:"spectrumX,omitempty"`
	NicConfigurationOperator *NicConfigurationOperatorConfig `yaml:"nicConfigurationOperator,omitempty"`
	PodNamespace             string                          `yaml:"podNamespace,omitempty"`
	Workload                 *WorkloadConfig                 `yaml:"workload,omitempty"`
	Profile         *Profile               `yaml:"profile,omitempty"`
	ClusterConfig   []ClusterConfig        `yaml:"clusterConfig,omitempty"`
}

type NetworkOperatorConfig struct {
	Version          string   `yaml:"version"`
	ComponentVersion string   `yaml:"componentVersion"`
	// SelectedRelease is the catalog key (MAJOR.MINOR, e.g. "26.4") chosen via
	// --network-operator-release. Empty means "no release pinned"; templates
	// treat that as "latest" so existing configs render the newest gates by
	// default. When non-empty, ApplyOptionsToConfig has already populated
	// Version/ComponentVersion/Repository and DOCADriver.Version from the
	// embedded catalog.
	SelectedRelease  string   `yaml:"selectedRelease,omitempty"`
	Repository       string   `yaml:"repository"`
	Namespace        string   `yaml:"namespace"`
	ImagePullSecrets []string `yaml:"imagePullSecrets,omitempty"`
}

type DOCADriverConfig struct {
	Enable                      bool `yaml:"enable"`
	Version                     string `yaml:"version"`
	UnloadStorageModules        bool `yaml:"unloadStorageModules"`
	EnableNFSRDMA               bool `yaml:"enableNFSRDMA"`
	UnloadThirdPartyRDMAModules bool `yaml:"unloadThirdPartyRDMAModules"`
	// SkipPreflightChecks controls the init container's module dependency check.
	// When false (l8k default), the check runs and any blocking dependency fails
	// the init container, preventing MOFED load. When true, the check is skipped
	// entirely and init succeeds immediately. The init container binary's own
	// default (`envDefault:"true"`) is overridden here because l8k is opinionated:
	// a deployment tool should surface hardware-compat issues early rather than
	// letting a broken MOFED reload happen silently downstream.
	SkipPreflightChecks bool `yaml:"skipPreflightChecks"`
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
	NicType      string `yaml:"nicType"`      // "1023" for ConnectX-8, "1025" for ConnectX-9, "a2dc" for BlueField-3 SuperNIC
	Overlay      string `yaml:"overlay"`      // "none"
	RdmaPrefix   string `yaml:"rdmaPrefix"`   // e.g., "roce_p%plane_id%_r%rail_id%"
	NetdevPrefix string `yaml:"netdevPrefix"` // e.g., "eth_p%plane_id%_r%rail_id%"
}

type NicConfigurationOperatorConfig struct {
	DeployNicInterfaceNameTemplate bool   `yaml:"deployNicInterfaceNameTemplate"`
	RdmaPrefix                     string `yaml:"rdmaPrefix"`   // e.g., "rdma_r%rail_id%"
	NetdevPrefix                   string `yaml:"netdevPrefix"` // e.g., "eth_r%rail_id%"
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

type WorkloadConfig struct {
	Manifest string `yaml:"manifest,omitempty"`
}

type Profile struct {
	Fabric     string            `yaml:"fabric"`
	Deployment string            `yaml:"deployment"`
	Multirail  bool              `yaml:"multirail"`
	SpectrumX  *ProfileSpectrumX `yaml:"spectrumX,omitempty"`
}

type ProfileSpectrumX struct {
	Enable         bool   `yaml:"enable"`         // must be true for Spectrum-X profiles to match
	SPCXVersion    string `yaml:"spcxVersion"`    // e.g., "RA2.2"
	MultiplaneMode string `yaml:"multiplaneMode"` // swplb, hwplb, uniplane
	NumberOfPlanes int    `yaml:"numberOfPlanes"` // 2 or 4
}

type ClusterConfig struct {
	Identifier           string               `yaml:"identifier"`
	MachineType          string               `yaml:"machineType,omitempty"`
	GPUType          string               `yaml:"gpuType,omitempty"`
	PresetApplied        bool                 `yaml:"presetApplied,omitempty"`
	// PresetDeviation lists discrepancies between the matched preset and
	// the cluster's actually-discovered hardware. When non-empty, the
	// preset was applied (so rail/NUMA topology fields are populated) but
	// the cluster differs from the certified configuration. l8k re-warns
	// every time the config is loaded.
	PresetDeviation []PresetDeviationEntry `yaml:"presetDeviation,omitempty"`
	Capabilities         *ClusterCapabilities `yaml:"capabilities"`
	PFs                  []PFConfig           `yaml:"pfs"`
	WorkerNodes          []string             `yaml:"workerNodes"`
	NodeSelector         map[string]string    `yaml:"nodeSelector,omitempty"`
	ThirdPartyRDMAModules []string            `yaml:"thirdPartyRDMAModules,omitempty"`
	StorageModules        []string            `yaml:"storageModules,omitempty"`
	RailPciAddresses     [][]string           `yaml:"-"` // Transient: per-rail merged PCI addresses (not serialized)
}

// PresetDeviationEntry records a single field-level discrepancy between a
// preset and the cluster's actually-discovered hardware. Field is one of
// "pciAddress", "deviceID", or "pfCount".
type PresetDeviationEntry struct {
	Field    string `yaml:"field"`
	Expected string `yaml:"expected,omitempty"`
	Got      string `yaml:"got,omitempty"`
	Detail   string `yaml:"detail,omitempty"`
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
	// Topology fields (populated from presets when available)
	NumaNode     *int   `yaml:"numaNode,omitempty"`
	ConnectedGPU string `yaml:"connectedGPU,omitempty"`
	GPUProximity string `yaml:"gpuProximity,omitempty"`
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

	emitPresetDeviationWarnings(&config, logger)

	return &config, nil
}

// emitPresetDeviationWarnings logs a warning for every group whose config
// records preset deviations. Designed to fire on every load — operators
// running against hardware that differs from the matched preset are
// reminded each run.
func emitPresetDeviationWarnings(cfg *LaunchKubernetesConfig, logger logr.Logger) {
	for _, g := range cfg.ClusterConfig {
		if len(g.PresetDeviation) == 0 {
			continue
		}
		logger.Info(
			"WARNING: cluster differs from the matched preset — manifests are still produced, but the deployment is not certified",
			"group", g.Identifier,
			"machineType", g.MachineType,
			"gpuType", g.GPUType,
			"deviationCount", len(g.PresetDeviation),
		)
		for _, d := range g.PresetDeviation {
			logger.Info("  preset deviation",
				"group", g.Identifier,
				"field", d.Field,
				"expected", d.Expected,
				"got", d.Got,
				"detail", d.Detail,
			)
		}
	}
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

// SupportedSPCXVersions lists the Spectrum-X RA versions for which l8k can
// emit non-`none` multiplane configurations. RA2.1 ships on Network Operator
// 26.1; RA2.2 on 26.4+. Order is preserved in error messages.
var SupportedSPCXVersions = []string{"RA2.1", "RA2.2"}

// SupportedMultiplaneModes lists the Spectrum-X multiplane modes the CLI
// accepts. `none` and `uniplane` collapse to one plane; `swplb` and `hwplb`
// require numberOfPlanes > 1.
var SupportedMultiplaneModes = []string{"none", "swplb", "hwplb", "uniplane"}

// SupportedNumberOfPlanes lists the values numberOfPlanes can take.
var SupportedNumberOfPlanes = []int{1, 2, 4}

// validateSpectrumXTemplates validates that Spectrum-X templates have required placeholders
func validateSpectrumXTemplates(config *LaunchKubernetesConfig) error {
	netdevPrefix := config.SpectrumX.NetdevPrefix
	rdmaPrefix := config.SpectrumX.RdmaPrefix

	// Non-`none` multiplane modes (swplb, hwplb, uniplane) require a supported
	// RA version.
	if config.Profile.SpectrumX.MultiplaneMode != "none" && config.Profile.SpectrumX.MultiplaneMode != "" {
		got := config.Profile.SpectrumX.SPCXVersion
		supported := false
		for _, v := range SupportedSPCXVersions {
			if got == v {
				supported = true
				break
			}
		}
		if !supported {
			return fmt.Errorf("multiplane mode %s requires spcxVersion in %v, got %q",
				config.Profile.SpectrumX.MultiplaneMode, SupportedSPCXVersions, got)
		}
	}

	isMultiplane := config.Profile.SpectrumX.NumberOfPlanes > 1
	isMultirail := config.Profile.Multirail
	
	// Check netdevPrefix (accept both %plane%/%rail% and %plane_id%/%rail_id%)
	hasPlaneInNetdev := containsPlaceholder(netdevPrefix, "%plane%") || containsPlaceholder(netdevPrefix, "%plane_id%")
	hasRailInNetdev := containsPlaceholder(netdevPrefix, "%rail%") || containsPlaceholder(netdevPrefix, "%rail_id%")

	if isMultiplane && !hasPlaneInNetdev {
		return fmt.Errorf("spectrumX.netdevPrefix must contain %%plane_id%% placeholder when numberOfPlanes > 1 (multiplane mode)")
	}

	if isMultirail && !hasRailInNetdev {
		return fmt.Errorf("spectrumX.netdevPrefix must contain %%rail_id%% placeholder when multirail is enabled")
	}

	// Check rdmaPrefix (same rules)
	hasPlaneInRdma := containsPlaceholder(rdmaPrefix, "%plane%") || containsPlaceholder(rdmaPrefix, "%plane_id%")
	hasRailInRdma := containsPlaceholder(rdmaPrefix, "%rail%") || containsPlaceholder(rdmaPrefix, "%rail_id%")

	if isMultiplane && !hasPlaneInRdma {
		return fmt.Errorf("spectrumX.rdmaPrefix must contain %%plane_id%% placeholder when numberOfPlanes > 1 (multiplane mode)")
	}

	if isMultirail && !hasRailInRdma {
		return fmt.Errorf("spectrumX.rdmaPrefix must contain %%rail_id%% placeholder when multirail is enabled")
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
