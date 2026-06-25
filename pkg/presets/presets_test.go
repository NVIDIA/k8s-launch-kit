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
gpuType: NVIDIA-H200
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

	topo, err := LoadPreset("PowerEdge-XE9680", "NVIDIA-H200")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo == nil {
		t.Fatal("expected topology, got nil")
	}
	if topo.MachineType != "PowerEdge-XE9680" {
		t.Errorf("expected MachineType=PowerEdge-XE9680, got %q", topo.MachineType)
	}
	if topo.GPUType != "NVIDIA-H200" {
		t.Errorf("expected GPUType=NVIDIA-H200, got %q", topo.GPUType)
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

	topo, err := LoadPreset("NonExistent-Machine", "NVIDIA-H200")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo != nil {
		t.Errorf("expected nil for missing preset, got %+v", topo)
	}
}

func TestLoadPreset_NoPresetsDir_FallsBackToEmbedded(t *testing.T) {
	// Without an on-disk presets/ in CWD or the install share dir, LoadPreset
	// falls back to the embedded preset tree baked into the binary. This is
	// the library-mode contract: callers get a working preset catalog with no
	// filesystem layout. The lookup is exact-match — picking a key that the
	// embedded tree ships ensures the fallback path is exercised.
	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	topo, err := LoadPreset("PowerEdge-XE9680", "NVIDIA-H200")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo == nil {
		t.Fatalf("expected embedded preset for (PowerEdge-XE9680, NVIDIA-H200), got nil")
	}
	if topo.MachineType != "PowerEdge-XE9680" || topo.GPUType != "NVIDIA-H200" {
		t.Errorf("unexpected preset returned: (%q, %q)", topo.MachineType, topo.GPUType)
	}
}

func TestLoadPreset_NoPresetsDir_UnknownKeyStillMissing(t *testing.T) {
	// The embedded-fallback path must still respect exact-match semantics:
	// a (machineType, gpuType) the embedded tree doesn't carry returns nil,
	// not a stray hit. Without this, the fallback would silently widen the
	// match set the way a glob would.
	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	topo, err := LoadPreset("definitely-not-a-real-machine", "definitely-not-a-real-gpu")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo != nil {
		t.Errorf("expected nil for an unknown (machineType, gpuType), got %+v", topo)
	}
}

func TestLoadPreset_InvalidYAML(t *testing.T) {
	// Invalid YAML is skipped at load time with a warning rather than
	// surfaced as a hard error — keeps the lookup robust across mixed
	// preset directories. The lookup just returns nil because no valid
	// preset matches.
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

	topo, err := LoadPreset("BadMachine", "NVIDIA-H200")
	if err != nil {
		t.Fatalf("expected nil error (invalid presets are skipped), got: %v", err)
	}
	if topo != nil {
		t.Errorf("expected nil topology for skipped invalid preset, got %+v", topo)
	}
}

func TestLoadPreset_EmptyFile(t *testing.T) {
	// Empty YAML unmarshals to a zero-value Topology, which has empty
	// machineType and gpuType — both required fields. The preset is
	// rejected at load time, so LoadPreset returns nil.
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

	topo, err := LoadPreset("EmptyMachine", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo != nil {
		t.Errorf("expected nil for empty preset (missing required fields), got %+v", topo)
	}
}

func TestLoadPreset_ExactGPUTypeMatch(t *testing.T) {
	// Two presets share the same machineType but declare different
	// gpuTypes — exact-match lookup should return the right one for each
	// gpuType, and nil when neither matches (no fallback).
	tmpDir := t.TempDir()
	presetsDir := filepath.Join(tmpDir, "presets")

	writePreset := func(dir, machine, gpu string) {
		full := filepath.Join(presetsDir, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "machineType: " + machine + "\ngpuType: " + gpu + "\n"
		if err := os.WriteFile(filepath.Join(full, "topology.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writePreset("PowerEdge-XE9680-H200", "PowerEdge-XE9680", "NVIDIA-H200")
	writePreset("PowerEdge-XE9680-B200", "PowerEdge-XE9680", "NVIDIA-B200")

	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	cases := []struct {
		machine, gpu string
		expectFound  bool
	}{
		{"PowerEdge-XE9680", "NVIDIA-H200", true},
		{"PowerEdge-XE9680", "NVIDIA-B200", true},
		{"PowerEdge-XE9680", "NVIDIA-A100", false}, // exact-match only — no fallback
		{"Other-Machine", "NVIDIA-H200", false},
	}
	for _, tc := range cases {
		topo, err := LoadPreset(tc.machine, tc.gpu)
		if err != nil {
			t.Errorf("LoadPreset(%q, %q): unexpected error: %v", tc.machine, tc.gpu, err)
			continue
		}
		if tc.expectFound && topo == nil {
			t.Errorf("LoadPreset(%q, %q): expected match, got nil", tc.machine, tc.gpu)
		}
		if !tc.expectFound && topo != nil {
			t.Errorf("LoadPreset(%q, %q): expected nil, got %+v", tc.machine, tc.gpu, topo)
		}
		if topo != nil && topo.GPUType != tc.gpu {
			t.Errorf("LoadPreset(%q, %q): wrong gpuType in result: %q", tc.machine, tc.gpu, topo.GPUType)
		}
	}
}

func TestLoadPreset_RejectsMissingGPUType(t *testing.T) {
	// A preset directory whose topology.yaml has empty gpuType is
	// silently dropped from the lookup pool — neither LoadPreset nor
	// ListPresets should surface it.
	tmpDir := t.TempDir()
	presetsDir := filepath.Join(tmpDir, "presets")

	mustMkdir := func(name string) string {
		full := filepath.Join(presetsDir, name)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		return full
	}
	// Valid: machineType + gpuType both set
	if err := os.WriteFile(filepath.Join(mustMkdir("Valid"), "topology.yaml"),
		[]byte("machineType: M\ngpuType: NVIDIA-H200\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Invalid: missing gpuType
	if err := os.WriteFile(filepath.Join(mustMkdir("NoGPU"), "topology.yaml"),
		[]byte("machineType: M\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Invalid: missing machineType
	if err := os.WriteFile(filepath.Join(mustMkdir("NoMachine"), "topology.yaml"),
		[]byte("gpuType: NVIDIA-H200\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	names, err := ListPresets()
	if err != nil {
		t.Fatalf("ListPresets failed: %v", err)
	}
	if len(names) != 1 || names[0] != "Valid" {
		t.Errorf("ListPresets: expected [Valid], got %v", names)
	}

	// Lookup must return nil for the invalid pair (NoGPU declared
	// machineType=M with empty gpuType, but it was rejected at load).
	topo, err := LoadPreset("M", "")
	if err != nil {
		t.Fatalf("LoadPreset(M, \"\") unexpected error: %v", err)
	}
	if topo != nil {
		t.Errorf("LoadPreset(M, \"\"): expected nil (NoGPU was rejected), got %+v", topo)
	}
}

func TestLoadPresetByDir_Found(t *testing.T) {
	tmpDir := t.TempDir()
	presetsDir := filepath.Join(tmpDir, "presets", "MyPreset")
	if err := os.MkdirAll(presetsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presetsDir, "topology.yaml"),
		[]byte("machineType: M\ngpuType: NVIDIA-H200\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	topo, err := LoadPresetByDir("MyPreset")
	if err != nil {
		t.Fatalf("LoadPresetByDir failed: %v", err)
	}
	if topo == nil || topo.MachineType != "M" || topo.GPUType != "NVIDIA-H200" {
		t.Errorf("LoadPresetByDir returned wrong topology: %+v", topo)
	}
}

func TestLoadPresetByDir_Unknown(t *testing.T) {
	tmpDir := t.TempDir()
	presetsDir := filepath.Join(tmpDir, "presets", "Foo")
	if err := os.MkdirAll(presetsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(presetsDir, "topology.yaml"),
		[]byte("machineType: M\ngpuType: NVIDIA-H200\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	topo, err := LoadPresetByDir("Bar")
	if err == nil {
		t.Fatal("expected error for unknown preset, got nil")
	}
	if topo != nil {
		t.Errorf("expected nil topology on unknown preset, got %+v", topo)
	}
	if !containsSubstring(err.Error(), "Bar") {
		t.Errorf("error should mention requested name 'Bar', got: %v", err)
	}
	if !containsSubstring(err.Error(), "Foo") {
		t.Errorf("error should list available preset 'Foo', got: %v", err)
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

	topo, err := LoadPreset("NoTopology", "NVIDIA-H200")
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
		if err := os.WriteFile(filepath.Join(dir, "topology.yaml"), []byte("machineType: "+name+"\ngpuType: NVIDIA-H200\n"), 0o644); err != nil {
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

func TestListPresets_NoPresetsDir_FallsBackToEmbedded(t *testing.T) {
	// Without an on-disk presets/ directory, ListPresets falls back to the
	// embedded preset tree and returns every directory it carries. This is
	// the library-mode contract that lets a Go caller enumerate available
	// presets without any host filesystem layout. The exact list is what
	// happens to ship in the binary at build time; we assert non-empty plus
	// presence of one well-known canonical preset rather than coupling the
	// test to the full ship list.
	tmpDir := t.TempDir()
	origDir, _ := os.Getwd()
	defer func() { _ = os.Chdir(origDir) }()
	_ = os.Chdir(tmpDir)

	names, err := ListPresets()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("expected at least one embedded preset, got empty list")
	}
	found := false
	for _, n := range names {
		if n == "PowerEdge-XE9680-H200" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected canonical preset PowerEdge-XE9680-H200 in embedded list, got %v", names)
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

	deviations := ValidatePreset(preset, discovered)
	if len(deviations) != 0 {
		t.Errorf("expected no deviations, got: %v", deviations)
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

	deviations := ValidatePreset(preset, discovered)
	if len(deviations) != 0 {
		t.Errorf("expected no deviations, got: %v", deviations)
	}
}

func TestValidatePreset_PFCountMismatch_RecordedAsDeviation(t *testing.T) {
	preset := &Topology{
		PFs: []PresetPF{
			{DeviceID: "a2dc", PciAddress: "0000:1a:00.0"},
			{DeviceID: "a2dc", PciAddress: "0000:3c:00.0"},
		},
	}
	discovered := []config.PFConfig{
		{DeviceID: "a2dc", PciAddress: "0000:1a:00.0"},
	}

	// PF count mismatch is a soft deviation now — the preset is still
	// applied on a best-effort basis. Expect a pfCount entry plus the
	// pciAddress entry for the address present in the preset but not
	// discovered.
	deviations := ValidatePreset(preset, discovered)
	var sawPFCount bool
	for _, d := range deviations {
		if d.Field == "pfCount" {
			sawPFCount = true
			if d.Expected != "2" || d.Got != "1" {
				t.Errorf("expected pfCount expected=2 got=1, got: %+v", d)
			}
		}
	}
	if !sawPFCount {
		t.Errorf("expected a pfCount deviation, got: %v", deviations)
	}
}

func TestValidatePreset_PciAddressMismatch_RecordedAsDeviations(t *testing.T) {
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

	deviations := ValidatePreset(preset, discovered)
	if len(deviations) != 2 {
		t.Fatalf("expected 2 PCI deviations, got %d: %v", len(deviations), deviations)
	}
	// Soft check: both addresses surface somewhere in the entries.
	all := ""
	for _, d := range deviations {
		all += d.Field + " " + d.Expected + " " + d.Got + " "
	}
	if !containsSubstring(all, "0000:ff:00.0") || !containsSubstring(all, "0000:3c:00.0") {
		t.Errorf("expected both drifted addresses in deviations, got: %v", deviations)
	}
}

func TestValidatePreset_DeviceIDMismatch_RecordedAsDeviation(t *testing.T) {
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

	deviations := ValidatePreset(preset, discovered)
	if len(deviations) != 1 {
		t.Fatalf("expected 1 device-ID deviation, got %d: %v", len(deviations), deviations)
	}
	d := deviations[0]
	if d.Field != "deviceID" {
		t.Errorf("expected field=deviceID, got %s", d.Field)
	}
	if !containsSubstring(d.Expected+" "+d.Got, "0000:3c:00.0") {
		t.Errorf("expected PCI address in deviation expected/got, got: %+v", d)
	}
}

func TestValidatePreset_PartNumberDifference_Passes(t *testing.T) {
	// Part number mismatch should NOT cause a deviation
	preset := &Topology{
		PFs: []PresetPF{
			{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", PartNumber: "DELL-PART-123"},
		},
	}
	discovered := []config.PFConfig{
		{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", PartNumber: "LENOVO-PART-456"},
	}

	deviations := ValidatePreset(preset, discovered)
	if len(deviations) != 0 {
		t.Errorf("part numbers should not produce deviations, got: %v", deviations)
	}
}

func TestValidatePreset_PSIDDifference_Passes(t *testing.T) {
	// PSID mismatch should NOT cause a deviation
	preset := &Topology{
		PFs: []PresetPF{
			{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", PSID: "mt_0000001069"},
		},
	}
	discovered := []config.PFConfig{
		{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", PSID: "mt_0000009999"},
	}

	deviations := ValidatePreset(preset, discovered)
	if len(deviations) != 0 {
		t.Errorf("PSIDs should not produce deviations, got: %v", deviations)
	}
}

func TestValidatePreset_EmptyPFs(t *testing.T) {
	preset := &Topology{PFs: []PresetPF{}}
	discovered := []config.PFConfig{}

	deviations := ValidatePreset(preset, discovered)
	if len(deviations) != 0 {
		t.Errorf("expected no deviations, got: %v", deviations)
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

	deviations := ValidatePreset(preset, discovered)
	if len(deviations) != 2 {
		t.Fatalf("expected 2 device-ID deviations, got %d: %v", len(deviations), deviations)
	}
	all := ""
	for _, d := range deviations {
		all += d.Expected + " " + d.Got + " "
	}
	if !containsSubstring(all, "0000:1a:00.0") || !containsSubstring(all, "0000:3c:00.0") {
		t.Errorf("expected both drifted addresses in deviations, got: %v", deviations)
	}
}

// --- ApplyPreset tests ---

func TestApplyPreset_OverridesTopologyFields(t *testing.T) {
	preset := &Topology{
		GPUType: "NVIDIA-H200",
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
		GPUType: "", // should be filled from preset
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

	if !ApplyPreset(preset, group) {
		t.Fatal("expected ApplyPreset to apply on a full match")
	}

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

	// GPUType is no longer filled in from the preset — it's a matching
	// key, so by the time ApplyPreset runs, the discovered group's
	// GPUType already equals the preset's. Here we exercise that the
	// caller passed an empty GPUType and ApplyPreset does not write
	// over it (since GPUType fill-in was deliberately removed).
	if group.GPUType != "" {
		t.Errorf("GPUType: expected unchanged empty (no fill-in), got %q", group.GPUType)
	}

	// Verify preset applied flag
	if !group.PresetApplied {
		t.Error("expected PresetApplied=true")
	}
}

func TestApplyPreset_PreservesExistingGPUType(t *testing.T) {
	preset := &Topology{
		GPUType: "NVIDIA-H200",
		PFs:     []PresetPF{},
	}
	group := &config.ClusterConfig{
		GPUType: "NVIDIA-H100-NVL", // already set — should NOT be overwritten
		PFs:     []config.PFConfig{},
	}

	applied := ApplyPreset(preset, group)

	// Both PF slices are empty, so the bijection check passes vacuously:
	// the preset "applies" (there are no PFs to mutate) and PresetApplied is
	// set. Pin this degenerate zero-PF behaviour explicitly.
	if !applied {
		t.Error("expected ApplyPreset to return true for the empty-PF (vacuous) bijection")
	}
	if !group.PresetApplied {
		t.Error("expected PresetApplied=true for the empty-PF (vacuous) bijection")
	}

	if group.GPUType != "NVIDIA-H100-NVL" {
		t.Errorf("GPUType: expected NVIDIA-H100-NVL (preserved), got %q", group.GPUType)
	}
}

func TestApplyPreset_PartialMatch_NoOp(t *testing.T) {
	// Group has a PF (0000:99:00.0) absent from the preset — a partial
	// match. ApplyPreset is all-or-nothing: it must change nothing and
	// return false, so the live-discovered classification is preserved
	// intact rather than half-overwritten (half-overwriting would leave the
	// overlapping PF reclassified while the rest keep live rails, producing
	// gaps/duplicate rail indices).
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

	if ApplyPreset(preset, group) {
		t.Error("expected ApplyPreset to return false on a partial match")
	}
	if group.PresetApplied {
		t.Error("expected PresetApplied=false on a partial match")
	}

	// The overlapping PF must NOT have been touched.
	if group.PFs[0].Traffic != "north-south" {
		t.Errorf("PF[0] Traffic: expected north-south (untouched), got %q", group.PFs[0].Traffic)
	}
	if group.PFs[0].Rail != nil {
		t.Errorf("PF[0] Rail: expected nil (untouched), got %v", group.PFs[0].Rail)
	}
	// The unmatched PF must be unchanged too.
	if group.PFs[1].Traffic != "east-west" {
		t.Errorf("PF[1] Traffic: expected east-west (untouched), got %q", group.PFs[1].Traffic)
	}
	if group.PFs[1].Rail == nil || *group.PFs[1].Rail != 5 {
		t.Errorf("PF[1] Rail: expected 5 (untouched), got %v", group.PFs[1].Rail)
	}
}

func TestApplyPreset_GroupSubsetOfPreset_NoOp(t *testing.T) {
	// Group has fewer PFs than the preset (preset describes a device the
	// hardware lacks). Applying would leave a rail gap where the missing PF
	// would have been, so ApplyPreset must no-op and return false.
	preset := &Topology{
		PFs: []PresetPF{
			{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", Traffic: "east-west", Rail: intPtr(0)},
			{DeviceID: "a2dc", PciAddress: "0000:3c:00.0", Traffic: "east-west", Rail: intPtr(1)},
		},
	}
	group := &config.ClusterConfig{
		PFs: []config.PFConfig{
			{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", Traffic: "east-west", Rail: intPtr(0)},
		},
	}

	if ApplyPreset(preset, group) {
		t.Error("expected ApplyPreset to return false when group is a subset of the preset")
	}
	if group.PresetApplied {
		t.Error("expected PresetApplied=false when group is a subset of the preset")
	}
}

func TestApplyPreset_PreservesOutOfOrderRails(t *testing.T) {
	// Presets may number rails out of PCI-address order — real example:
	// PowerEdge-XE7745 assigns rail 0 to a PCI-domain-0001 device. A full
	// match must copy those rail values VERBATIM, never renumber them into
	// PCI order.
	preset := &Topology{
		PFs: []PresetPF{
			{PciAddress: "0000:44:00.0", Traffic: "east-west", Rail: intPtr(1)},
			{PciAddress: "0001:05:00.0", Traffic: "east-west", Rail: intPtr(0)},
		},
	}
	group := &config.ClusterConfig{
		PFs: []config.PFConfig{
			{PciAddress: "0000:44:00.0", Traffic: "east-west"},
			{PciAddress: "0001:05:00.0", Traffic: "east-west"},
		},
	}

	if !ApplyPreset(preset, group) {
		t.Fatal("expected ApplyPreset to apply on a full match")
	}
	if group.PFs[0].Rail == nil || *group.PFs[0].Rail != 1 {
		t.Errorf("PF[0] (0000:44:00.0) Rail: expected 1 (verbatim), got %v", group.PFs[0].Rail)
	}
	if group.PFs[1].Rail == nil || *group.PFs[1].Rail != 0 {
		t.Errorf("PF[1] (0001:05:00.0) Rail: expected 0 (verbatim), got %v", group.PFs[1].Rail)
	}
}

func TestApplyPreset_LargePreset_PowerEdgeXE9680(t *testing.T) {
	// Test with a realistic preset matching the PowerEdge-XE9680 topology
	tmpDir := t.TempDir()
	presetsDir := filepath.Join(tmpDir, "presets")

	// Copy the actual preset file from the package-local data tree (which is
	// also the source of the embedded FS). Path is relative to this test
	// file's directory: pkg/presets/data/PowerEdge-XE9680-H200/topology.yaml.
	srcPath := filepath.Join("data", "PowerEdge-XE9680-H200", "topology.yaml")
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

	preset, err := LoadPreset("PowerEdge-XE9680", "NVIDIA-H200")
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
			NetworkInterface: "eth" + pp.PciAddress,   // synthetic
			Traffic:          "east-west",             // deliberately wrong for some
			PSID:             "discovered-psid-" + pp.PciAddress,
			PartNumber:       "discovered-part-" + pp.PciAddress,
		}
	}

	// Validation should pass with no deviations
	if deviations := ValidatePreset(preset, discovered); len(deviations) != 0 {
		t.Fatalf("expected no deviations, got: %v", deviations)
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
