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

package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/assets"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/resolve"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// DefaultShareDir is the default installation directory for l8k assets.
const DefaultShareDir = "/usr/local/share/l8k"

// ResolveShareDir returns the share directory, checking binary-relative path for custom prefix.
func ResolveShareDir() string {
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "..", "share", "l8k")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return DefaultShareDir
}

// DefaultConfigPath resolves the l8k-config.yaml path using a lookup chain:
// 1. ./l8k-config.yaml (CWD — container/repo root)
// 2. <share-dir>/l8k-config.yaml (installed)
// Returns empty string if neither found.
func DefaultConfigPath() string {
	candidates := []string{
		"l8k-config.yaml",
		filepath.Join(ResolveShareDir(), "l8k-config.yaml"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func defaultConfigSource(configPath, configRoot string) string {
	if configPath != "" {
		return configPath
	}
	if configRoot != "" {
		return fmt.Sprintf("embedded (--config-dir %q has no %s)", configRoot, assets.DefaultConfigName)
	}
	return "embedded"
}

// discoverClusterConfig handles cluster configuration discovery
func (l *Launcher) discoverClusterConfig() error {
	l.ui.Info("Discovering cluster capabilities")
	l.logger.Info("Discovering cluster configuration")

	// Resolve config path
	var configPath string
	if l.options.UserConfig != "" {
		configPath = l.options.UserConfig
		l.ui.Info("Using base configuration: %s", configPath)
	} else if l.options.ConfigDir != "" {
		configPath = l.configAssets.DefaultConfigPath
	} else {
		configPath = DefaultConfigPath()
	}

	baseConfig, srcConfigYAML, err := config.LoadFullConfigWithSource(configPath, l.logger)
	if err != nil {
		source := defaultConfigSource(configPath, l.options.ConfigDir)
		return apperrors.NewValidationError(
			fmt.Sprintf("cannot load config: %s", source), err,
			fmt.Sprintf("Check that %s is valid YAML", source))
	}

	if l.options.UserConfig == "" {
		source := defaultConfigSource(configPath, l.options.ConfigDir)
		l.ui.Info("Using default configuration: %s", source)
	}

	// Discovery plugins need a mutable configuration for bootstrap inputs and
	// discovered output. Keep the explicitly supplied user config on a separate
	// object: after discovery, only clusterConfig is copied back to it.
	discoveryConfig := baseConfig
	if l.options.UserConfig != "" {
		discoveryConfig, err = config.LoadFullConfig(configPath, l.logger)
		if err != nil {
			return fmt.Errorf("failed to load discovery working config: %w", err)
		}
	}

	// Apply config release selection and CLI flags to the working copy so the
	// discovery bootstrap itself uses the effective image and credentials.
	if err := networkoperatorplugin.ApplyCLIConfigOverrides(l.options, discoveryConfig); err != nil {
		return apperrors.NewValidationError(err.Error(), err, "Run 'l8k --help' for supported releases")
	}

	discoveryConfig.ClusterConfig = nil
	// The bundled reference config contains an example profile. A fresh
	// discovery must resolve the profile from the discovered hardware instead
	// of treating that example as user intent. An explicit --user-config is
	// restored unchanged after discovery instead.
	if l.options.UserConfig == "" {
		discoveryConfig.Profile = nil
	}

	ctx := ui.WithOutput(l.context, l.ui)
	for _, plugin := range l.plugins {
		err := plugin.DiscoverClusterConfig(ctx, l.kubeClient, discoveryConfig)
		if err != nil {
			return fmt.Errorf("failed to discover cluster config: %w", err)
		}
	}

	finalConfig := discoveryConfig
	if l.options.UserConfig == "" {
		if err := l.resolveProfileSettings(discoveryConfig); err != nil {
			return err
		}
	} else {
		baseConfig.ClusterConfig = discoveryConfig.ClusterConfig
		if err := networkoperatorplugin.ApplyExplicitCLIConfigOverrides(l.options, baseConfig); err != nil {
			return apperrors.NewValidationError(err.Error(), err, "Run 'l8k --help' for supported releases")
		}
		if err := resolve.ValidateResolvedConfig(baseConfig); err != nil {
			return apperrors.NewValidationError(err.Error(), nil,
				"Adjust the conflicting flags or fields in cluster-config.yaml.")
		}
		l.recordResolvedProfile(baseConfig)
		finalConfig = baseConfig
	}

	// Compute effective save path:
	// 1. --save-cluster-config if explicitly provided
	// 2. --user-config path (rewrite in place) if provided
	// 3. Default path as fallback
	savePath := l.options.SaveClusterConfig
	if savePath == "" {
		if l.options.UserConfig != "" {
			savePath = l.options.UserConfig
		} else {
			savePath = "cluster-config.yaml"
		}
	}
	l.options.SaveClusterConfig = savePath

	// Marshal and save merged config to disk, preserving the documentation
	// comments from the source config for the user's reference.
	banner := "Generated by `l8k discover`. Hardware-derived profile defaults and explicit CLI overrides\n" +
		"are persisted below; edit values as needed, then run `l8k generate`."
	if l.options.UserConfig != "" {
		banner = "Generated by `l8k discover`. Only clusterConfig and explicit CLI overrides were updated;\n" +
			"all other values came from --user-config."
	}
	data, err := config.MarshalConfigWithComments(finalConfig, srcConfigYAML, banner)
	if err != nil {
		return fmt.Errorf("failed to marshal discovered config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(savePath), 0755); err != nil {
		return fmt.Errorf("failed to create output directory %s: %w", filepath.Dir(savePath), err)
	}
	if err := os.WriteFile(savePath, data, 0644); err != nil {
		l.ui.Error("Failed to save configuration: %v", err)
		return fmt.Errorf("failed to write discovered config to %s: %w", savePath, err)
	}

	l.ui.Success("Configuration saved: %s", savePath)
	l.logger.Info("Discovered cluster config saved", "path", savePath)

	warnThirdPartyRDMAModules(finalConfig, "discover", l.ui)
	warnStorageModules(finalConfig, "discover", l.ui)

	return nil
}
