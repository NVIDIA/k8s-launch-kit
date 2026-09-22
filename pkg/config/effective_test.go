// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEffectiveConfigRoundTrip(t *testing.T) {
	root := t.TempDir()
	cfg := &LaunchKitConfig{
		NetworkOperator:   &NetworkOperatorConfig{SelectedRelease: "26.7"},
		Profile:           &Profile{Fabric: "ethernet", Multirail: false, MultirailSet: true},
		NetworkNamespaces: []string{},
		Validation:        &ValidationConfig{Checks: []string{}},
	}
	path, err := WriteEffectiveConfig(root, cfg)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, EffectiveConfigRelativePath), path)

	loaded, err := LoadEffectiveConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "26.7", loaded.NetworkOperator.SelectedRelease)
	assert.False(t, loaded.Profile.Multirail)
	assert.True(t, loaded.Profile.MultirailSet)
	assert.NotNil(t, loaded.NetworkNamespaces)
	assert.Empty(t, loaded.NetworkNamespaces)
	require.NotNil(t, loaded.Validation)
	assert.NotNil(t, loaded.Validation.Checks)
	assert.Empty(t, loaded.Validation.Checks)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "kind: ResolvedConfig")
	assert.Contains(t, string(data), "validation.checks")
}
