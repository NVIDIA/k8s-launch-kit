// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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

package presetmatch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/presets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatchGroup_SkippedWhenMachineOrGPUMissing(t *testing.T) {
	t.Run("both empty", func(t *testing.T) {
		r := MatchGroup(config.ClusterConfig{Identifier: "g"})
		assert.Equal(t, StatusSkipped, r.Status)
		assert.Contains(t, r.Reason, "machineType and gpuType not discovered")
	})
	t.Run("machineType only", func(t *testing.T) {
		r := MatchGroup(config.ClusterConfig{Identifier: "g", MachineType: "vendor-a-h200"})
		assert.Equal(t, StatusSkipped, r.Status)
		assert.Contains(t, r.Reason, "gpuType not discovered")
	})
	t.Run("gpuType only", func(t *testing.T) {
		r := MatchGroup(config.ClusterConfig{Identifier: "g", GPUType: "h200"})
		assert.Equal(t, StatusSkipped, r.Status)
		assert.Contains(t, r.Reason, "machineType not discovered")
	})
}

// The preset catalog can be absent in the test environment (it's
// installed under /usr/local/share/l8k or similar). When that's the
// case LoadPreset returns "not found" for any pair — exercising the
// StatusNotFound path. The other Status outcomes need real preset
// fixtures and are covered by the higher-level integration tests in
// pkg/presets and pkg/networkoperatorplugin.
func TestMatchGroup_NotFoundWhenNoCatalog(t *testing.T) {
	r := MatchGroup(config.ClusterConfig{
		Identifier:  "g",
		MachineType: "fictional-machine-no-such-preset",
		GPUType:     "h200",
	})
	// Either StatusNotFound (catalog exists but no match) or
	// StatusNotFound (no catalog at all) — both produce the same
	// status code, so a single assertion is enough.
	assert.Equal(t, StatusNotFound, r.Status)
	assert.NotEmpty(t, r.Reason)
}

func TestMatchAll(t *testing.T) {
	cfg := &config.LaunchKitConfig{
		ClusterConfig: []config.ClusterConfig{
			{Identifier: "a"},
			{Identifier: "b", MachineType: "vendor-x", GPUType: "h200"},
		},
	}
	results := MatchAll(cfg)
	assert.Len(t, results, 2)
	assert.Equal(t, "a", results[0].Group)
	assert.Equal(t, StatusSkipped, results[0].Status)
	assert.Equal(t, "b", results[1].Group)
	// StatusNotFound when no preset catalog; never panics either way.
	assert.NotEqual(t, StatusSkipped, results[1].Status)
}

func TestMatchAllWithCatalogUsesSelectedSource(t *testing.T) {
	root := t.TempDir()
	presetDir := filepath.Join(root, "machine-a-gpu-model-x")
	require.NoError(t, os.MkdirAll(presetDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(presetDir, "topology.yaml"), []byte(`machineType: machine-a
gpuType: gpu-model-x
manufacturer: vendor-a
pfs:
  - deviceID: "1021"
    pciAddress: "0000:01:00.0"
`), 0o644))

	catalog, err := presets.NewCatalogFromDir(root)
	require.NoError(t, err)
	cfg := &config.LaunchKitConfig{ClusterConfig: []config.ClusterConfig{{
		Identifier:  "group-a",
		MachineType: "machine-a",
		GPUType:     "gpu-model-x",
		PFs: []config.PFConfig{{
			DeviceID:   "1021",
			PciAddress: "0000:01:00.0",
		}},
	}}}

	results := MatchAllWithCatalog(cfg, catalog)
	require.Len(t, results, 1)
	assert.Equal(t, StatusMatch, results[0].Status)
	assert.Equal(t, "vendor-a", results[0].Manufacturer)
}

func TestAnyMatched(t *testing.T) {
	assert.False(t, AnyMatched(nil))
	assert.False(t, AnyMatched([]Result{{Status: StatusSkipped}, {Status: StatusNotFound}}))
	assert.True(t, AnyMatched([]Result{{Status: StatusMatch}}))
	assert.True(t, AnyMatched([]Result{{Status: StatusDeviation}}))
	assert.True(t, AnyMatched([]Result{{Status: StatusSkipped}, {Status: StatusMatch}}))
}
