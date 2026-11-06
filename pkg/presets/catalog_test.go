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

package presets

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/assets"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeCatalogPreset(t *testing.T, root, name, manufacturer string) {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	body := "machineType: machine-a\n" +
		"gpuType: gpu-model-x\n" +
		"manufacturer: " + manufacturer + "\n" +
		"pfs: []\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "topology.yaml"), []byte(body), 0o644))
}

func TestEmbeddedCatalog(t *testing.T) {
	catalog, err := EmbeddedCatalog()
	require.NoError(t, err)
	assert.Equal(t, "embedded", catalog.Source())

	topology, err := catalog.LoadPreset("PowerEdge-XE9680", "NVIDIA-H200")
	require.NoError(t, err)
	require.NotNil(t, topology)
	assert.Equal(t, "PowerEdge-XE9680", topology.MachineType)
}

func TestEmbeddedENETPreset(t *testing.T) {
	catalog, err := EmbeddedCatalog()
	require.NoError(t, err)
	topology, err := catalog.LoadPreset("MGX-ETL256-Vera", "NVIDIA-Graphics-Device")
	require.NoError(t, err)
	require.NotNil(t, topology)
	require.Len(t, topology.PFs, 16)
	require.NotNil(t, topology.Capabilities)
	require.NotNil(t, topology.Capabilities.Nodes)
	assert.True(t, topology.Capabilities.Nodes.Sriov)
	assert.True(t, topology.Capabilities.Nodes.Rdma)
	assert.False(t, topology.Capabilities.Nodes.Ib, "this preset covers Ethernet")

	for nic, domain := range []int{1, 3, 4, 6} {
		for function := range 4 {
			rail := nic*4 + function
			pf := topology.PFs[rail]
			assert.Equal(t, "1025", pf.DeviceID)
			assert.Equal(t, fmt.Sprintf("%04x:03:00.%d", domain, function), pf.PciAddress)
			assert.Equal(t, fmt.Sprintf("enP%dp3s0f%dnp%d", domain, function, function), pf.NetworkInterface)
			rdmaIndex := rail
			if domain == 6 {
				rdmaIndex++ // mlx5_12 belongs to the excluded BlueField-4.
			}
			assert.Equal(t, fmt.Sprintf("mlx5_%d", rdmaIndex), pf.RdmaDevice)
			assert.Equal(t, "east-west", pf.Traffic)
			require.NotNil(t, pf.Rail)
			assert.Equal(t, rail, *pf.Rail)
			require.NotNil(t, pf.NumaNode)
			assert.Zero(t, *pf.NumaNode)
			assert.Equal(t, fmt.Sprintf("GPU%d", nic*2+function%2), pf.ConnectedGPU)
			assert.Equal(t, "PXB", pf.GPUProximity)
		}
	}

	group, err := SynthesizeClusterConfig("enet", topology, map[string]string{"example.com/node-group": "enet"})
	require.NoError(t, err)
	assert.Empty(t, ValidatePreset(topology, group.PFs))
	assert.True(t, ApplyPreset(topology, &group))

	// An exposed BF4 is a different managed inventory. Never partially apply
	// this zero-trust variant or turn the extra north-south PF into a rail.
	group.PFs = append(group.PFs, config.PFConfig{
		DeviceID: "a2df", PciAddress: "0005:05:00.0", Traffic: "north-south",
	})
	assert.True(t, HasTopologyDeviation(ValidatePreset(topology, group.PFs)))
	assert.False(t, ApplyPreset(topology, &group))
	assert.Equal(t, "north-south", group.PFs[16].Traffic)
	assert.Nil(t, group.PFs[16].Rail)

	// Default discovery retains one PF per NIC; the sixteen-rail preset must
	// require the explicit --collapse-nic-rails=false inventory.
	collapsed := []config.PFConfig{group.PFs[0], group.PFs[4], group.PFs[8], group.PFs[12]}
	assert.True(t, HasTopologyDeviation(ValidatePreset(topology, collapsed)))
}

func TestCatalogForConfigDir(t *testing.T) {
	t.Run("no explicit root preserves default lookup", func(t *testing.T) {
		expected, err := DefaultCatalog()
		require.NoError(t, err)

		catalog, err := CatalogForConfigDir(assets.ConfigDir{})
		require.NoError(t, err)
		assert.Equal(t, expected.Source(), catalog.Source())
	})

	t.Run("explicit root without presets uses embedded", func(t *testing.T) {
		catalog, err := CatalogForConfigDir(assets.ConfigDir{Root: t.TempDir()})
		require.NoError(t, err)
		assert.Equal(t, "embedded", catalog.Source())
	})

	t.Run("explicit presets directory is authoritative", func(t *testing.T) {
		root := t.TempDir()
		presetsDir := filepath.Join(root, assets.PresetsDirName)
		require.NoError(t, os.Mkdir(presetsDir, 0o755))

		catalog, err := CatalogForConfigDir(assets.ConfigDir{
			Root:       root,
			PresetsDir: presetsDir,
		})
		require.NoError(t, err)
		assert.Equal(t, presetsDir, catalog.Source())
	})
}

func TestDiskCatalogIsAuthoritative(t *testing.T) {
	root := t.TempDir()
	writeCatalogPreset(t, root, "custom", "vendor-a")

	catalog, err := NewCatalogFromDir(root)
	require.NoError(t, err)
	assert.Equal(t, root, catalog.Source())

	names, err := catalog.ListPresets()
	require.NoError(t, err)
	assert.Equal(t, []string{"custom"}, names)

	topology, err := catalog.LoadPreset("machine-a", "gpu-model-x")
	require.NoError(t, err)
	require.NotNil(t, topology)
	assert.Equal(t, "vendor-a", topology.Manufacturer)

	embeddedOnly, err := catalog.LoadPreset("PowerEdge-XE9680", "NVIDIA-H200")
	require.NoError(t, err)
	assert.Nil(t, embeddedOnly, "a disk catalog must not merge missing entries from embedded presets")
}

func TestCatalogsAreIndependent(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	writeCatalogPreset(t, rootA, "custom", "vendor-a")
	writeCatalogPreset(t, rootB, "custom", "vendor-b")

	catalogA, err := NewCatalogFromDir(rootA)
	require.NoError(t, err)
	catalogB, err := NewCatalogFromDir(rootB)
	require.NoError(t, err)

	var wg sync.WaitGroup
	manufacturers := make(chan string, 2)
	for _, catalog := range []*Catalog{catalogA, catalogB} {
		wg.Add(1)
		go func(c *Catalog) {
			defer wg.Done()
			topology, loadErr := c.LoadPreset("machine-a", "gpu-model-x")
			if loadErr == nil && topology != nil {
				manufacturers <- topology.Manufacturer
			}
		}(catalog)
	}
	wg.Wait()
	close(manufacturers)

	got := map[string]bool{}
	for manufacturer := range manufacturers {
		got[manufacturer] = true
	}
	assert.Equal(t, map[string]bool{"vendor-a": true, "vendor-b": true}, got)
}
