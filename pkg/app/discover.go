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
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"gopkg.in/yaml.v2"
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

// discoverClusterConfig handles cluster configuration discovery
func (l *Launcher) discoverClusterConfig() error {
	l.ui.Info("Discovering cluster capabilities")
	l.logger.Info("Discovering cluster configuration")

	// Resolve config path
	var configPath string
	if l.options.UserConfig != "" {
		configPath = l.options.UserConfig
		l.ui.Info("Using base configuration: %s", configPath)
	} else {
		configPath = DefaultConfigPath()
	}

	if configPath == "" {
		return apperrors.NewValidationError(
			"no configuration file found",
			fmt.Errorf("checked ./l8k-config.yaml and /usr/local/share/l8k/l8k-config.yaml"),
			"Provide --user-config <path> or run 'scripts/install.sh' to install the default config")
	}

	defaults, err := config.LoadFullConfig(configPath, l.logger)
	if err != nil {
		return apperrors.NewValidationError(
			fmt.Sprintf("cannot load config: %s", configPath), err,
			fmt.Sprintf("Check that %s exists and is valid YAML", configPath))
	}

	if l.options.UserConfig == "" {
		l.ui.Info("Using default configuration: %s", configPath)
	}

	// Override namespace from CLI flag if provided
	if l.options.NetworkOperatorNamespace != "" {
		defaults.NetworkOperator.Namespace = l.options.NetworkOperatorNamespace
		l.logger.Info("Using CLI override for network operator namespace", "namespace", l.options.NetworkOperatorNamespace)
	}

	// Override image pull secrets from CLI flag if provided
	if len(l.options.ImagePullSecrets) > 0 {
		defaults.NetworkOperator.ImagePullSecrets = l.options.ImagePullSecrets
		l.logger.Info("Using CLI override for image pull secrets", "secrets", l.options.ImagePullSecrets)
	}

	defaults.ClusterConfig = nil
	defaults.Profile = nil

	ctx := ui.WithOutput(context.Background(), l.ui)
	for _, plugin := range l.plugins {
		err := plugin.DiscoverClusterConfig(ctx, l.kubeClient, defaults)
		if err != nil {
			return fmt.Errorf("failed to discover cluster config: %w", err)
		}
	}

	discoveredConfig := *defaults

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

	// Marshal and save merged config to disk
	data, err := yaml.Marshal(discoveredConfig)
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

	warnThirdPartyRDMAModules(&discoveredConfig, "discover", l.ui)
	warnStorageModules(&discoveredConfig, "discover", l.ui)

	return nil
}
