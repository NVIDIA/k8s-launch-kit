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

// Package assets resolves optional filesystem overrides for the runtime
// configuration embedded in the l8k binary.
package assets

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// DefaultConfigName is the user-facing filename for an on-disk override
	// of the default configuration embedded in pkg/config.
	DefaultConfigName = "l8k-config.yaml"
	// PresetsDirName is the directory containing topology preset directories.
	PresetsDirName = "presets"
)

// ConfigDir is the validated layout rooted at --config-dir. Empty paths mean
// that the corresponding asset was not provided and its embedded counterpart
// should be used.
type ConfigDir struct {
	Root              string
	DefaultConfigPath string
	PresetsDir        string
}

// ResolveConfigDir validates an explicit configuration directory and records
// the optional assets it contains. An empty root is valid and returns an empty
// ConfigDir so callers can preserve their legacy lookup chain.
//
// A supplied directory may override either the default config, the presets, or
// both. An existing directory with neither asset is valid and selects both
// embedded fallbacks; a nonexistent or mistyped root remains an error.
func ResolveConfigDir(root string) (ConfigDir, error) {
	if root == "" {
		return ConfigDir{}, nil
	}

	info, err := os.Stat(root)
	if err != nil {
		return ConfigDir{}, fmt.Errorf("config directory %q is not accessible: %w", root, err)
	}
	if !info.IsDir() {
		return ConfigDir{}, fmt.Errorf("config directory %q is not a directory", root)
	}

	resolved := ConfigDir{Root: root}
	configPath := filepath.Join(root, DefaultConfigName)
	if configInfo, statErr := os.Stat(configPath); statErr == nil {
		if configInfo.IsDir() {
			return ConfigDir{}, fmt.Errorf("default config override %q is a directory", configPath)
		}
		resolved.DefaultConfigPath = configPath
	} else if !os.IsNotExist(statErr) {
		return ConfigDir{}, fmt.Errorf("default config override %q is not accessible: %w", configPath, statErr)
	}

	presetsDir := filepath.Join(root, PresetsDirName)
	if presetsInfo, statErr := os.Stat(presetsDir); statErr == nil {
		if !presetsInfo.IsDir() {
			return ConfigDir{}, fmt.Errorf("presets override %q is not a directory", presetsDir)
		}
		resolved.PresetsDir = presetsDir
	} else if !os.IsNotExist(statErr) {
		return ConfigDir{}, fmt.Errorf("presets override %q is not accessible: %w", presetsDir, statErr)
	}

	return resolved, nil
}
