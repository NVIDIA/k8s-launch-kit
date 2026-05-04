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
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

func TestSynthesizeClusterConfig_FieldMapping(t *testing.T) {
	rail0 := 0
	numa0 := 0
	preset := &Topology{
		MachineType: "PowerEdge-XE9680",
		GPUType:     "NVIDIA-H200",
		Capabilities: &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{
			Sriov: true, Rdma: true, Ib: false,
		}},
		PFs: []PresetPF{
			{
				DeviceID: "a2dc", PciAddress: "0000:1a:00.0",
				RdmaDevice: "rocep26s0f0", NetworkInterface: "eth2",
				Traffic: "east-west", Rail: &rail0, NumaNode: &numa0,
				ConnectedGPU: "GPU0", GPUProximity: "PIX",
				PSID: "mt_0000001069", PartNumber: "0KK4NR",
			},
			{
				DeviceID: "a2dc", PciAddress: "0000:9d:00.0",
				RdmaDevice: "rocep157s0f0", NetworkInterface: "eth7",
				Traffic: "north-south", Rail: nil, NumaNode: &numa0,
				ConnectedGPU: "GPU4", GPUProximity: "PIX",
				PSID: "mt_0000000884", PartNumber: "0HFWRM",
			},
		},
	}
	selector := map[string]string{"nvidia.com/gpu.product": "NVIDIA-H200"}

	cc, err := SynthesizeClusterConfig("PowerEdge-XE9680-H200", preset, selector)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Identifier comes from the directory name (so multi-variant presets
	// produce distinct NicNodePolicy names).
	if cc.Identifier != "PowerEdge-XE9680-H200" {
		t.Errorf("Identifier: expected directory name, got %q", cc.Identifier)
	}
	if cc.MachineType != "PowerEdge-XE9680" {
		t.Errorf("MachineType: expected PowerEdge-XE9680, got %q", cc.MachineType)
	}
	if cc.GPUType != "NVIDIA-H200" {
		t.Errorf("GPUType: expected NVIDIA-H200, got %q", cc.GPUType)
	}
	if !cc.PresetApplied {
		t.Error("PresetApplied should be true")
	}
	if cc.WorkerNodes != nil {
		t.Errorf("WorkerNodes should be nil (selector targets nodes at apply time), got %v", cc.WorkerNodes)
	}
	if got := cc.NodeSelector["nvidia.com/gpu.product"]; got != "NVIDIA-H200" {
		t.Errorf("NodeSelector: expected nvidia.com/gpu.product=NVIDIA-H200, got %q", got)
	}

	// PFs flow through with all fields, including RdmaDevice and
	// NetworkInterface (preset is authoritative in --for mode since
	// there is no live discovery to override).
	if len(cc.PFs) != 2 {
		t.Fatalf("expected 2 PFs, got %d", len(cc.PFs))
	}
	pf0 := cc.PFs[0]
	if pf0.PciAddress != "0000:1a:00.0" || pf0.RdmaDevice != "rocep26s0f0" ||
		pf0.NetworkInterface != "eth2" || pf0.Traffic != "east-west" ||
		pf0.Rail == nil || *pf0.Rail != 0 || pf0.ConnectedGPU != "GPU0" ||
		pf0.PSID != "mt_0000001069" || pf0.PartNumber != "0KK4NR" {
		t.Errorf("PF[0] field mapping wrong: %+v", pf0)
	}
	pf1 := cc.PFs[1]
	if pf1.Traffic != "north-south" || pf1.Rail != nil {
		t.Errorf("PF[1] traffic/rail wrong: %+v", pf1)
	}
}

func TestSynthesizeClusterConfig_CapabilitiesPassthrough(t *testing.T) {
	preset := &Topology{
		MachineType: "M", GPUType: "G",
		Capabilities: &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{
			Sriov: true, Rdma: true, Ib: false,
		}},
	}
	cc, err := SynthesizeClusterConfig("dir", preset, map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cc.Capabilities == nil || cc.Capabilities.Nodes == nil {
		t.Fatal("Capabilities not propagated")
	}
	if !cc.Capabilities.Nodes.Sriov || !cc.Capabilities.Nodes.Rdma || cc.Capabilities.Nodes.Ib {
		t.Errorf("Capabilities mismatch: got %+v", *cc.Capabilities.Nodes)
	}
}

func TestSynthesizeClusterConfig_NoCapabilities_ReturnsError(t *testing.T) {
	preset := &Topology{MachineType: "M", GPUType: "G"}
	_, err := SynthesizeClusterConfig("dir", preset, map[string]string{"k": "v"})
	if err == nil {
		t.Fatal("expected error for preset without capabilities, got nil")
	}
	if !containsSubstring(err.Error(), "capabilities") {
		t.Errorf("error should mention capabilities, got: %v", err)
	}
}

func TestSynthesizeClusterConfig_NilPreset(t *testing.T) {
	_, err := SynthesizeClusterConfig("dir", nil, map[string]string{"k": "v"})
	if err == nil {
		t.Fatal("expected error for nil preset, got nil")
	}
}
