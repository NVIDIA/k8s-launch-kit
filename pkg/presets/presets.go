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

package presets

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"gopkg.in/yaml.v2"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Topology represents a predefined cluster topology preset loaded from a
// topology.yaml file. It is intentionally separate from config.ClusterConfig
// to carry additional topology metadata (NUMA, GPU affinity) without
// polluting the core config.
type Topology struct {
	MachineType     string     `yaml:"machineType"`
	Manufacturer    string     `yaml:"manufacturer,omitempty"`
	ProductType     string     `yaml:"productType,omitempty"`
	NicModel        string     `yaml:"nicModel,omitempty"`
	GPUInterconnect string     `yaml:"gpuInterconnect,omitempty"`
	NumaNodes       int        `yaml:"numaNodes,omitempty"`
	PFs             []PresetPF `yaml:"pfs"`
}

// PresetPF describes a single physical function in a topology preset.
// It is a superset of config.PFConfig with additional topology fields.
type PresetPF struct {
	DeviceID         string `yaml:"deviceID"`
	PciAddress       string `yaml:"pciAddress"`
	RdmaDevice       string `yaml:"rdmaDevice"`
	NetworkInterface string `yaml:"networkInterface"`
	Traffic          string `yaml:"traffic"`
	Rail             *int   `yaml:"rail,omitempty"`
	NumaNode         *int   `yaml:"numaNode,omitempty"`
	ConnectedGPU     string `yaml:"connectedGPU,omitempty"`
	GPUProximity     string `yaml:"gpuProximity,omitempty"`
	PSID             string `yaml:"psid,omitempty"`
	PartNumber       string `yaml:"partNumber,omitempty"`
}

// GetPresetsDir resolves the presets directory using a lookup chain:
// 1. ./presets (CWD — container/repo root)
// 2. /usr/local/share/l8k/presets (default install)
// 3. <binary-dir>/../share/l8k/presets (custom prefix install)
//
// Returns ("", nil) if no presets directory is found — presets are optional.
func GetPresetsDir() (string, error) {
	candidates := []string{
		"presets",
		"/usr/local/share/l8k/presets",
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", "share", "l8k", "presets"))
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			return p, nil
		}
	}
	return "", nil
}

// LoadPreset loads a topology preset for the given machine type.
// Returns (nil, nil) if no presets directory exists or no preset matches
// the machine type. Returns an error only on parse failure.
func LoadPreset(machineType string) (*Topology, error) {
	dir, err := GetPresetsDir()
	if err != nil {
		return nil, err
	}
	if dir == "" {
		return nil, nil
	}

	path := filepath.Join(dir, machineType, "topology.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read preset %s: %w", path, err)
	}

	var t Topology
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("failed to parse preset %s: %w", path, err)
	}

	return &t, nil
}

// ListPresets returns the names (machine types) of all available presets
// in the presets directory. Returns nil if no presets directory exists.
func ListPresets() ([]string, error) {
	dir, err := GetPresetsDir()
	if err != nil {
		return nil, err
	}
	if dir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read presets directory %s: %w", dir, err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Only include directories that contain a topology.yaml
		topoPath := filepath.Join(dir, e.Name(), "topology.yaml")
		if _, err := os.Stat(topoPath); err == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// ApplyPreset enriches a discovered ClusterConfig group with data from a
// validated preset. It matches PFs by PCI address and overrides topology-
// derived fields (traffic, rail, NUMA, GPU affinity) while preserving
// live-state fields (RDMA device, network interface, PSID, part number).
func ApplyPreset(preset *Topology, group *config.ClusterConfig) {
	// Build lookup map from preset PFs keyed by PCI address
	presetMap := make(map[string]*PresetPF, len(preset.PFs))
	for i := range preset.PFs {
		presetMap[preset.PFs[i].PciAddress] = &preset.PFs[i]
	}

	for i := range group.PFs {
		pf := &group.PFs[i]
		pp, ok := presetMap[pf.PciAddress]
		if !ok {
			continue
		}

		// Override topology-derived fields (preset is authoritative)
		pf.Traffic = pp.Traffic
		pf.Rail = pp.Rail
		pf.NumaNode = pp.NumaNode
		pf.ConnectedGPU = pp.ConnectedGPU
		pf.GPUProximity = pp.GPUProximity

		// Log part number / PSID mismatches as warnings but keep discovered values
		if pp.PartNumber != "" && pf.PartNumber != "" && pp.PartNumber != pf.PartNumber {
			log.Log.V(1).Info("Preset part number differs from discovered — using discovered value",
				"pciAddress", pf.PciAddress, "preset", pp.PartNumber, "discovered", pf.PartNumber)
		}
		if pp.PSID != "" && pf.PSID != "" && pp.PSID != pf.PSID {
			log.Log.V(1).Info("Preset PSID differs from discovered — using discovered value",
				"pciAddress", pf.PciAddress, "preset", pp.PSID, "discovered", pf.PSID)
		}
	}

	// Fill in product type from preset if discovery didn't find one
	if group.ProductType == "" && preset.ProductType != "" {
		group.ProductType = preset.ProductType
	}

	group.PresetApplied = true
}
