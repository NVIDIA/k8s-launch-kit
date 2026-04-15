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
	"os"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

// intPtr returns a pointer to the given int value.
func intPtr(i int) *int { return &i }

// --- GetPresetsDir tests ---

func TestGetPresetsDir_CWD(t *testing.T) {
	// Create a temporary presets directory in CWD
	tmpDir := t.TempDir()
	presetsDir := filepath.Join(tmpDir, "presets")
	if err := os.Mkdir(presetsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Change to temp dir so CWD lookup finds ./presets
	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	dir, err := GetPresetsDir()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dir != "presets" {
		t.Errorf("expected 'presets', got %q", dir)
	}
}

func TestGetPresetsDir_NotFound(t *testing.T) {
	// Use a temp dir with no presets subdirectory
	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	dir, err := GetPresetsDir()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dir != "" {
		t.Errorf("expected empty string for missing presets dir, got %q", dir)
	}
}

func TestGetPresetsDir_SkipsFiles(t *testing.T) {
	// Create a file named 'presets' (not a directory) — should not match
	tmpDir := t.TempDir()
	presetsFile := filepath.Join(tmpDir, "presets")
	if err := os.WriteFile(presetsFile, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	dir, err := GetPresetsDir()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dir != "" {
		t.Errorf("expected empty string when 'presets' is a file, got %q", dir)
	}
}

// --- LoadPreset tests ---

func TestLoadPreset_Found(t *testing.T) {
	tmpDir := t.TempDir()
	machineDir := filepath.Join(tmpDir, "presets", "PowerEdge-XE9680")
	if err := os.MkdirAll(machineDir, 0o755); err != nil {
		t.Fatal(err)
	}

	topoYAML := `machineType: PowerEdge-XE9680
productType: NVIDIA-H200
nicModel: BlueField-3 SuperNIC
gpuInterconnect: NV18
numaNodes: 2
pfs:
  - deviceID: a2dc
    pciAddress: "0000:1a:00.0"
    traffic: east-west
    rail: 0
    numaNode: 0
    connectedGPU: GPU0
    gpuProximity: PIX
    psid: mt_0000001069
    partNumber: 0KK4NR
  - deviceID: a2dc
    pciAddress: "0000:3c:00.0"
    traffic: east-west
    rail: 1
    numaNode: 0
    connectedGPU: GPU1
    gpuProximity: PIX
    psid: mt_0000001069
    partNumber: 0KK4NR
`
	if err := os.WriteFile(filepath.Join(machineDir, "topology.yaml"), []byte(topoYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	topo, err := LoadPreset("PowerEdge-XE9680")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo == nil {
		t.Fatal("expected topology, got nil")
	}
	if topo.MachineType != "PowerEdge-XE9680" {
		t.Errorf("expected MachineType=PowerEdge-XE9680, got %q", topo.MachineType)
	}
	if topo.ProductType != "NVIDIA-H200" {
		t.Errorf("expected ProductType=NVIDIA-H200, got %q", topo.ProductType)
	}
	if topo.NicModel != "BlueField-3 SuperNIC" {
		t.Errorf("expected NicModel, got %q", topo.NicModel)
	}
	if topo.GPUInterconnect != "NV18" {
		t.Errorf("expected GPUInterconnect=NV18, got %q", topo.GPUInterconnect)
	}
	if topo.NumaNodes != 2 {
		t.Errorf("expected NumaNodes=2, got %d", topo.NumaNodes)
	}
	if len(topo.PFs) != 2 {
		t.Fatalf("expected 2 PFs, got %d", len(topo.PFs))
	}
	pf := topo.PFs[0]
	if pf.DeviceID != "a2dc" {
		t.Errorf("PF[0] DeviceID: expected a2dc, got %q", pf.DeviceID)
	}
	if pf.PciAddress != "0000:1a:00.0" {
		t.Errorf("PF[0] PciAddress: expected 0000:1a:00.0, got %q", pf.PciAddress)
	}
	if pf.Traffic != "east-west" {
		t.Errorf("PF[0] Traffic: expected east-west, got %q", pf.Traffic)
	}
	if pf.Rail == nil || *pf.Rail != 0 {
		t.Errorf("PF[0] Rail: expected 0, got %v", pf.Rail)
	}
	if pf.NumaNode == nil || *pf.NumaNode != 0 {
		t.Errorf("PF[0] NumaNode: expected 0, got %v", pf.NumaNode)
	}
	if pf.ConnectedGPU != "GPU0" {
		t.Errorf("PF[0] ConnectedGPU: expected GPU0, got %q", pf.ConnectedGPU)
	}
	if pf.GPUProximity != "PIX" {
		t.Errorf("PF[0] GPUProximity: expected PIX, got %q", pf.GPUProximity)
	}
}

func TestLoadPreset_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "presets"), 0o755); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	topo, err := LoadPreset("NonExistent-Machine")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo != nil {
		t.Errorf("expected nil for missing preset, got %+v", topo)
	}
}

func TestLoadPreset_NoPresetsDir(t *testing.T) {
	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	topo, err := LoadPreset("PowerEdge-XE9680")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo != nil {
		t.Errorf("expected nil when no presets dir, got %+v", topo)
	}
}

func TestLoadPreset_InvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	machineDir := filepath.Join(tmpDir, "presets", "BadMachine")
	if err := os.MkdirAll(machineDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(machineDir, "topology.yaml"), []byte("{{invalid yaml"), 0o644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	topo, err := LoadPreset("BadMachine")
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
	if topo != nil {
		t.Errorf("expected nil topology on error, got %+v", topo)
	}
}

func TestLoadPreset_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	machineDir := filepath.Join(tmpDir, "presets", "EmptyMachine")
	if err := os.MkdirAll(machineDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(machineDir, "topology.yaml"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	topo, err := LoadPreset("EmptyMachine")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty YAML unmarshals to zero-value struct — not nil
	if topo == nil {
		t.Fatal("expected non-nil topology for empty YAML")
	}
	if len(topo.PFs) != 0 {
		t.Errorf("expected 0 PFs, got %d", len(topo.PFs))
	}
}

func TestLoadPreset_DirectoryWithoutTopologyYAML(t *testing.T) {
	tmpDir := t.TempDir()
	machineDir := filepath.Join(tmpDir, "presets", "NoTopology")
	if err := os.MkdirAll(machineDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Directory exists but no topology.yaml inside

	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	topo, err := LoadPreset("NoTopology")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo != nil {
		t.Errorf("expected nil when topology.yaml is missing, got %+v", topo)
	}
}

// --- ListPresets tests ---

func TestListPresets(t *testing.T) {
	tmpDir := t.TempDir()
	presetsDir := filepath.Join(tmpDir, "presets")

	// Create 3 valid presets and 1 invalid (no topology.yaml)
	for _, name := range []string{"Alpha", "Beta", "Gamma"} {
		dir := filepath.Join(presetsDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "topology.yaml"), []byte("machineType: "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Dir without topology.yaml
	if err := os.MkdirAll(filepath.Join(presetsDir, "Invalid"), 0o755); err != nil {
		t.Fatal(err)
	}
	// File (not dir) in presets directory
	if err := os.WriteFile(filepath.Join(presetsDir, "README.md"), []byte("# presets"), 0o644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	names, err := ListPresets()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("expected 3 presets, got %d: %v", len(names), names)
	}
	expected := []string{"Alpha", "Beta", "Gamma"}
	for i, name := range names {
		if name != expected[i] {
			t.Errorf("names[%d]: expected %q, got %q", i, expected[i], name)
		}
	}
}

func TestListPresets_NoPresetsDir(t *testing.T) {
	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	names, err := ListPresets()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if names != nil {
		t.Errorf("expected nil for missing presets dir, got %v", names)
	}
}

// --- ValidatePreset tests ---

func TestValidatePreset_ExactMatch(t *testing.T) {
	preset := &Topology{
		PFs: []PresetPF{
			{DeviceID: "a2dc", PciAddress: "0000:1a:00.0"},
			{DeviceID: "a2dc", PciAddress: "0000:3c:00.0"},
			{DeviceID: "101f", PciAddress: "0000:5a:00.0"},
		},
	}
	discovered := []config.PFConfig{
		{DeviceID: "a2dc", PciAddress: "0000:1a:00.0"},
		{DeviceID: "a2dc", PciAddress: "0000:3c:00.0"},
		{DeviceID: "101f", PciAddress: "0000:5a:00.0"},
	}

	if err := ValidatePreset(preset, discovered); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

func TestValidatePreset_DifferentOrderStillMatches(t *testing.T) {
	preset := &Topology{
		PFs: []PresetPF{
			{DeviceID: "a2dc", PciAddress: "0000:3c:00.0"},
			{DeviceID: "a2dc", PciAddress: "0000:1a:00.0"},
		},
	}
	discovered := []config.PFConfig{
		{DeviceID: "a2dc", PciAddress: "0000:1a:00.0"},
		{DeviceID: "a2dc", PciAddress: "0000:3c:00.0"},
	}

	if err := ValidatePreset(preset, discovered); err != nil {
		t.Errorf("expected no error for different order, got: %v", err)
	}
}

func TestValidatePreset_PFCountMismatch(t *testing.T) {
	preset := &Topology{
		PFs: []PresetPF{
			{DeviceID: "a2dc", PciAddress: "0000:1a:00.0"},
			{DeviceID: "a2dc", PciAddress: "0000:3c:00.0"},
		},
	}
	discovered := []config.PFConfig{
		{DeviceID: "a2dc", PciAddress: "0000:1a:00.0"},
	}

	err := ValidatePreset(preset, discovered)
	if err == nil {
		t.Fatal("expected PF count mismatch error, got nil")
	}
	if !containsSubstring(err.Error(), "PF count mismatch") {
		t.Errorf("expected PF count mismatch in error, got: %v", err)
	}
}

func TestValidatePreset_PciAddressMismatch_PresetHasExtra(t *testing.T) {
	preset := &Topology{
		PFs: []PresetPF{
			{DeviceID: "a2dc", PciAddress: "0000:1a:00.0"},
			{DeviceID: "a2dc", PciAddress: "0000:ff:00.0"}, // not in discovered
		},
	}
	discovered := []config.PFConfig{
		{DeviceID: "a2dc", PciAddress: "0000:1a:00.0"},
		{DeviceID: "a2dc", PciAddress: "0000:3c:00.0"}, // not in preset
	}

	err := ValidatePreset(preset, discovered)
	if err == nil {
		t.Fatal("expected PCI address mismatch error, got nil")
	}
	if !containsSubstring(err.Error(), "PCI address mismatch") {
		t.Errorf("expected PCI address mismatch in error, got: %v", err)
	}
	if !containsSubstring(err.Error(), "0000:ff:00.0") {
		t.Errorf("expected missing preset address in error, got: %v", err)
	}
	if !containsSubstring(err.Error(), "0000:3c:00.0") {
		t.Errorf("expected missing discovered address in error, got: %v", err)
	}
}

func TestValidatePreset_DeviceIDMismatch(t *testing.T) {
	preset := &Topology{
		PFs: []PresetPF{
			{DeviceID: "a2dc", PciAddress: "0000:1a:00.0"},
			{DeviceID: "1023", PciAddress: "0000:3c:00.0"}, // different device ID
		},
	}
	discovered := []config.PFConfig{
		{DeviceID: "a2dc", PciAddress: "0000:1a:00.0"},
		{DeviceID: "101f", PciAddress: "0000:3c:00.0"}, // different device ID
	}

	err := ValidatePreset(preset, discovered)
	if err == nil {
		t.Fatal("expected device ID mismatch error, got nil")
	}
	if !containsSubstring(err.Error(), "device ID mismatch") {
		t.Errorf("expected device ID mismatch in error, got: %v", err)
	}
	if !containsSubstring(err.Error(), "0000:3c:00.0") {
		t.Errorf("expected PCI address in error, got: %v", err)
	}
}

func TestValidatePreset_PartNumberDifference_Passes(t *testing.T) {
	// Part number mismatch should NOT cause validation failure
	preset := &Topology{
		PFs: []PresetPF{
			{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", PartNumber: "DELL-PART-123"},
		},
	}
	discovered := []config.PFConfig{
		{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", PartNumber: "LENOVO-PART-456"},
	}

	if err := ValidatePreset(preset, discovered); err != nil {
		t.Errorf("expected no error for part number difference, got: %v", err)
	}
}

func TestValidatePreset_PSIDDifference_Passes(t *testing.T) {
	// PSID mismatch should NOT cause validation failure
	preset := &Topology{
		PFs: []PresetPF{
			{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", PSID: "mt_0000001069"},
		},
	}
	discovered := []config.PFConfig{
		{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", PSID: "mt_0000009999"},
	}

	if err := ValidatePreset(preset, discovered); err != nil {
		t.Errorf("expected no error for PSID difference, got: %v", err)
	}
}

func TestValidatePreset_EmptyPFs(t *testing.T) {
	preset := &Topology{PFs: []PresetPF{}}
	discovered := []config.PFConfig{}

	if err := ValidatePreset(preset, discovered); err != nil {
		t.Errorf("expected no error for empty PFs, got: %v", err)
	}
}

func TestValidatePreset_MultipleMismatches(t *testing.T) {
	// Multiple device ID mismatches at different PCI addresses
	preset := &Topology{
		PFs: []PresetPF{
			{DeviceID: "a2dc", PciAddress: "0000:1a:00.0"},
			{DeviceID: "a2dc", PciAddress: "0000:3c:00.0"},
		},
	}
	discovered := []config.PFConfig{
		{DeviceID: "101f", PciAddress: "0000:1a:00.0"},
		{DeviceID: "1023", PciAddress: "0000:3c:00.0"},
	}

	err := ValidatePreset(preset, discovered)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Should mention both addresses
	if !containsSubstring(err.Error(), "0000:1a:00.0") || !containsSubstring(err.Error(), "0000:3c:00.0") {
		t.Errorf("expected both mismatched addresses in error, got: %v", err)
	}
}

// --- ApplyPreset tests ---

func TestApplyPreset_OverridesTopologyFields(t *testing.T) {
	preset := &Topology{
		ProductType: "NVIDIA-H200",
		PFs: []PresetPF{
			{
				DeviceID:     "a2dc",
				PciAddress:   "0000:1a:00.0",
				Traffic:      "east-west",
				Rail:         intPtr(0),
				NumaNode:     intPtr(0),
				ConnectedGPU: "GPU0",
				GPUProximity: "PIX",
				PartNumber:   "PRESET-PART",
				PSID:         "preset-psid",
			},
			{
				DeviceID:     "a2dc",
				PciAddress:   "0000:9d:00.0",
				Traffic:      "north-south",
				NumaNode:     intPtr(1),
				ConnectedGPU: "",
				GPUProximity: "NODE",
				PartNumber:   "PRESET-NS-PART",
				PSID:         "preset-ns-psid",
			},
		},
	}

	group := &config.ClusterConfig{
		ProductType: "", // should be filled from preset
		PFs: []config.PFConfig{
			{
				DeviceID:         "a2dc",
				PciAddress:       "0000:1a:00.0",
				RdmaDevice:       "mlx5_0",
				NetworkInterface: "eth2",
				Traffic:          "north-south", // wrong — will be corrected by preset
				PSID:             "discovered-psid",
				PartNumber:       "discovered-part",
			},
			{
				DeviceID:         "a2dc",
				PciAddress:       "0000:9d:00.0",
				RdmaDevice:       "mlx5_4",
				NetworkInterface: "eth7",
				Traffic:          "east-west", // wrong — will be corrected
				Rail:             intPtr(3),   // wrong — will be corrected
				PSID:             "discovered-ns-psid",
				PartNumber:       "discovered-ns-part",
			},
		},
	}

	ApplyPreset(preset, group)

	// Verify preset-authoritative fields were overridden
	pf0 := group.PFs[0]
	if pf0.Traffic != "east-west" {
		t.Errorf("PF[0] Traffic: expected east-west, got %q", pf0.Traffic)
	}
	if pf0.Rail == nil || *pf0.Rail != 0 {
		t.Errorf("PF[0] Rail: expected 0, got %v", pf0.Rail)
	}
	if pf0.NumaNode == nil || *pf0.NumaNode != 0 {
		t.Errorf("PF[0] NumaNode: expected 0, got %v", pf0.NumaNode)
	}
	if pf0.ConnectedGPU != "GPU0" {
		t.Errorf("PF[0] ConnectedGPU: expected GPU0, got %q", pf0.ConnectedGPU)
	}
	if pf0.GPUProximity != "PIX" {
		t.Errorf("PF[0] GPUProximity: expected PIX, got %q", pf0.GPUProximity)
	}

	pf1 := group.PFs[1]
	if pf1.Traffic != "north-south" {
		t.Errorf("PF[1] Traffic: expected north-south, got %q", pf1.Traffic)
	}
	if pf1.Rail != nil {
		t.Errorf("PF[1] Rail: expected nil (north-south), got %v", pf1.Rail)
	}
	if pf1.NumaNode == nil || *pf1.NumaNode != 1 {
		t.Errorf("PF[1] NumaNode: expected 1, got %v", pf1.NumaNode)
	}
	if pf1.GPUProximity != "NODE" {
		t.Errorf("PF[1] GPUProximity: expected NODE, got %q", pf1.GPUProximity)
	}

	// Verify discovery-authoritative fields were preserved
	if pf0.RdmaDevice != "mlx5_0" {
		t.Errorf("PF[0] RdmaDevice: expected mlx5_0 (preserved), got %q", pf0.RdmaDevice)
	}
	if pf0.NetworkInterface != "eth2" {
		t.Errorf("PF[0] NetworkInterface: expected eth2 (preserved), got %q", pf0.NetworkInterface)
	}
	if pf0.PSID != "discovered-psid" {
		t.Errorf("PF[0] PSID: expected discovered-psid (preserved), got %q", pf0.PSID)
	}
	if pf0.PartNumber != "discovered-part" {
		t.Errorf("PF[0] PartNumber: expected discovered-part (preserved), got %q", pf0.PartNumber)
	}

	if pf1.RdmaDevice != "mlx5_4" {
		t.Errorf("PF[1] RdmaDevice: expected mlx5_4 (preserved), got %q", pf1.RdmaDevice)
	}
	if pf1.PSID != "discovered-ns-psid" {
		t.Errorf("PF[1] PSID: expected discovered-ns-psid (preserved), got %q", pf1.PSID)
	}
	if pf1.PartNumber != "discovered-ns-part" {
		t.Errorf("PF[1] PartNumber: expected discovered-ns-part (preserved), got %q", pf1.PartNumber)
	}

	// Verify product type was filled from preset
	if group.ProductType != "NVIDIA-H200" {
		t.Errorf("ProductType: expected NVIDIA-H200, got %q", group.ProductType)
	}

	// Verify preset applied flag
	if !group.PresetApplied {
		t.Error("expected PresetApplied=true")
	}
}

func TestApplyPreset_PreservesExistingProductType(t *testing.T) {
	preset := &Topology{
		ProductType: "NVIDIA-H200",
		PFs:         []PresetPF{},
	}
	group := &config.ClusterConfig{
		ProductType: "NVIDIA-H100-NVL", // already set — should NOT be overwritten
		PFs:         []config.PFConfig{},
	}

	ApplyPreset(preset, group)

	if group.ProductType != "NVIDIA-H100-NVL" {
		t.Errorf("ProductType: expected NVIDIA-H100-NVL (preserved), got %q", group.ProductType)
	}
}

func TestApplyPreset_UnmatchedPCIAddress(t *testing.T) {
	// PF in group has no match in preset — should be left unchanged
	preset := &Topology{
		PFs: []PresetPF{
			{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", Traffic: "east-west", Rail: intPtr(0)},
		},
	}
	group := &config.ClusterConfig{
		PFs: []config.PFConfig{
			{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", Traffic: "north-south"},
			{DeviceID: "101f", PciAddress: "0000:99:00.0", Traffic: "east-west", Rail: intPtr(5)},
		},
	}

	ApplyPreset(preset, group)

	// First PF should be updated
	if group.PFs[0].Traffic != "east-west" {
		t.Errorf("PF[0] Traffic: expected east-west, got %q", group.PFs[0].Traffic)
	}

	// Second PF should remain unchanged (no match in preset)
	if group.PFs[1].Traffic != "east-west" {
		t.Errorf("PF[1] Traffic: expected east-west (unchanged), got %q", group.PFs[1].Traffic)
	}
	if group.PFs[1].Rail == nil || *group.PFs[1].Rail != 5 {
		t.Errorf("PF[1] Rail: expected 5 (unchanged), got %v", group.PFs[1].Rail)
	}
}

func TestApplyPreset_LargePreset_PowerEdgeXE9680(t *testing.T) {
	// Test with a realistic preset matching the PowerEdge-XE9680 topology
	tmpDir := t.TempDir()
	presetsDir := filepath.Join(tmpDir, "presets")

	// Copy the actual preset file from the repo
	srcPath := filepath.Join("..", "..", "presets", "PowerEdge-XE9680", "topology.yaml")
	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Skipf("skipping: cannot read real preset file: %v", err)
	}

	machineDir := filepath.Join(presetsDir, "PowerEdge-XE9680")
	if err := os.MkdirAll(machineDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(machineDir, "topology.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	preset, err := LoadPreset("PowerEdge-XE9680")
	if err != nil {
		t.Fatalf("LoadPreset failed: %v", err)
	}
	if preset == nil {
		t.Fatal("expected preset, got nil")
	}

	// Build matching discovered PFs (same PCI addresses and device IDs)
	discovered := make([]config.PFConfig, len(preset.PFs))
	for i, pp := range preset.PFs {
		discovered[i] = config.PFConfig{
			DeviceID:         pp.DeviceID,
			PciAddress:       pp.PciAddress,
			RdmaDevice:       "mlx5_" + pp.PciAddress, // synthetic
			NetworkInterface: "eth" + pp.PciAddress,    // synthetic
			Traffic:          "east-west",              // deliberately wrong for some
			PSID:             "discovered-psid-" + pp.PciAddress,
			PartNumber:       "discovered-part-" + pp.PciAddress,
		}
	}

	// Validation should pass
	if err := ValidatePreset(preset, discovered); err != nil {
		t.Fatalf("validation failed: %v", err)
	}

	group := &config.ClusterConfig{
		PFs:         discovered,
		WorkerNodes: []string{"node-1", "node-2"},
	}
	ApplyPreset(preset, group)

	// Verify key topology assertions
	if !group.PresetApplied {
		t.Error("expected PresetApplied=true")
	}

	// Count east-west and north-south
	var ew, ns int
	for _, pf := range group.PFs {
		switch pf.Traffic {
		case "east-west":
			ew++
		case "north-south":
			ns++
		}
	}

	// PowerEdge-XE9680 has 8 east-west and 2 north-south PFs
	if ew != 8 {
		t.Errorf("expected 8 east-west PFs, got %d", ew)
	}
	if ns != 2 {
		t.Errorf("expected 2 north-south PFs, got %d", ns)
	}

	// Verify rail numbers are set for east-west, nil for north-south
	maxRail := -1
	for _, pf := range group.PFs {
		if pf.Traffic == "east-west" {
			if pf.Rail == nil {
				t.Errorf("east-west PF %s has nil rail", pf.PciAddress)
			} else if *pf.Rail > maxRail {
				maxRail = *pf.Rail
			}
		}
		if pf.Traffic == "north-south" && pf.Rail != nil {
			t.Errorf("north-south PF %s has rail=%d, expected nil", pf.PciAddress, *pf.Rail)
		}
	}
	if maxRail != 7 {
		t.Errorf("expected max rail=7, got %d", maxRail)
	}

	// Verify NUMA nodes are populated
	for _, pf := range group.PFs {
		if pf.NumaNode == nil {
			t.Errorf("PF %s has nil NumaNode after preset application", pf.PciAddress)
		}
	}

	// Verify discovery fields are preserved
	for _, pf := range group.PFs {
		if pf.PSID == "" || !containsSubstring(pf.PSID, "discovered-psid-") {
			t.Errorf("PF %s PSID was not preserved: %q", pf.PciAddress, pf.PSID)
		}
		if pf.PartNumber == "" || !containsSubstring(pf.PartNumber, "discovered-part-") {
			t.Errorf("PF %s PartNumber was not preserved: %q", pf.PciAddress, pf.PartNumber)
		}
	}
}

// helper
func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && contains(s, sub))
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
