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

package assets

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveConfigDir(t *testing.T) {
	t.Run("empty root preserves default resolution", func(t *testing.T) {
		got, err := ResolveConfigDir("")
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("resolves both supported assets", func(t *testing.T) {
		root := t.TempDir()
		configPath := filepath.Join(root, DefaultConfigName)
		require.NoError(t, os.WriteFile(configPath, []byte("networkOperator: {}\n"), 0o644))
		presetsDir := filepath.Join(root, PresetsDirName)
		require.NoError(t, os.Mkdir(presetsDir, 0o755))

		got, err := ResolveConfigDir(root)
		require.NoError(t, err)
		assert.Equal(t, root, got.Root)
		assert.Equal(t, configPath, got.DefaultConfigPath)
		assert.Equal(t, presetsDir, got.PresetsDir)
	})

	t.Run("allows a partial override", func(t *testing.T) {
		root := t.TempDir()
		presetsDir := filepath.Join(root, PresetsDirName)
		require.NoError(t, os.Mkdir(presetsDir, 0o755))

		got, err := ResolveConfigDir(root)
		require.NoError(t, err)
		assert.Empty(t, got.DefaultConfigPath)
		assert.Equal(t, presetsDir, got.PresetsDir)
	})

	t.Run("allows an empty directory", func(t *testing.T) {
		root := t.TempDir()
		got, err := ResolveConfigDir(root)
		require.NoError(t, err)
		assert.Equal(t, root, got.Root)
		assert.Empty(t, got.DefaultConfigPath)
		assert.Empty(t, got.PresetsDir)
	})

	t.Run("rejects wrong path types", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, PresetsDirName), []byte("not a directory"), 0o644))

		_, err := ResolveConfigDir(root)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "presets override")
	})
}
