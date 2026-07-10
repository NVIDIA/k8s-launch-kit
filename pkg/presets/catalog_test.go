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
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/assets"
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
