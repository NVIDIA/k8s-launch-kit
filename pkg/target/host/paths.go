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

package host

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/k8s-launch-kit/pkg/app"
	"github.com/nvidia/k8s-launch-kit/pkg/assets"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/presets"
)

const (
	// DefaultDeploymentDir is the conventional Host manifest root.
	DefaultDeploymentDir  = "./deployment"
	defaultUserConfigPath = "./cluster-config.yaml"
	manifestSubdir        = "network-operator"
)

// UserConfigInput contains the Host path-resolution inputs.
type UserConfigInput struct {
	Explicit        string
	DeploymentFiles string
	ConfigDir       string
}

// ResolveKubeconfig applies kubectl-compatible Host fallback semantics.
func ResolveKubeconfig(flagValue string) (string, error) {
	return resolveKubeconfig(flagValue, clientcmd.RecommendedHomeFile, os.Getenv, os.Stat)
}

func resolveKubeconfig(flagValue, homeFile string, getenv func(string) string, stat func(string) (os.FileInfo, error)) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if envValue := getenv("KUBECONFIG"); envValue != "" {
		return envValue, nil
	}
	if homeFile != "" {
		if _, err := stat(homeFile); err == nil {
			return homeFile, nil
		}
	}
	return "", fmt.Errorf("no kubeconfig found: set $KUBECONFIG, pass --kubeconfig <path>, or create %s", homeFile)
}

// ResolveDeploymentDir prefers the generated Host subdirectory and falls back
// to a caller-supplied directory containing manifests directly.
func ResolveDeploymentDir(dir string) (string, error) {
	if dir == "" {
		dir = DefaultDeploymentDir
	}
	candidate := filepath.Join(dir, manifestSubdir)
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate, nil
	}
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir, nil
	}
	return "", fmt.Errorf("not found: %s (also checked %s)", dir, candidate)
}

// UserConfigPathFor resolves a Host cluster config using the established
// standalone deploy/validate lookup order.
func UserConfigPathFor(input UserConfigInput) (string, error) {
	if path := userConfigPathBeforeDefaults(input); path != "" {
		return path, nil
	}
	return defaultConfigPathFor(input.ConfigDir)
}

func userConfigPathBeforeDefaults(input UserConfigInput) string {
	if input.Explicit != "" {
		return input.Explicit
	}
	candidates := []string{defaultUserConfigPath}
	if input.DeploymentFiles != "" {
		candidates = append(candidates,
			filepath.Join(input.DeploymentFiles, "..", "cluster-config.yaml"),
			filepath.Join(input.DeploymentFiles, "cluster-config.yaml"),
		)
	}
	return firstExistingConfigPath(candidates)
}

// UserConfigPathBeforeDefaults resolves only explicit, conventional, and
// deployment-adjacent Host configs without installed defaults.
func UserConfigPathBeforeDefaults(input UserConfigInput) string {
	return userConfigPathBeforeDefaults(input)
}

func defaultConfigPathFor(configRoot string) (string, error) {
	var candidates []string
	if configRoot != "" {
		resolved, err := assets.ResolveConfigDir(configRoot)
		if err != nil {
			return "", err
		}
		if resolved.DefaultConfigPath != "" {
			candidates = append(candidates, resolved.DefaultConfigPath)
		}
	} else {
		candidates = append(candidates,
			"l8k-config.yaml",
			filepath.Join(app.ResolveShareDir(), "l8k-config.yaml"),
		)
	}
	return firstExistingConfigPath(candidates), nil
}

// DefaultConfigPathFor resolves Host config-dir or legacy installed defaults.
func DefaultConfigPathFor(configRoot string) (string, error) {
	return defaultConfigPathFor(configRoot)
}

func firstExistingConfigPath(candidates []string) string {
	for _, path := range candidates {
		if path == "" {
			continue
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

// UserConfigPathForGenerate resolves the legacy generate-path defaults while
// leaving explicit config-dir resolution to app.Launcher.
func UserConfigPathForGenerate(input UserConfigInput) string {
	if path := userConfigPathBeforeDefaults(input); path != "" {
		return path
	}
	if input.ConfigDir != "" {
		return ""
	}
	path, _ := defaultConfigPathFor("")
	return path
}

// LoadUserConfig loads the resolved Host config and applies release and
// namespace overlays used by standalone deploy and validate.
func LoadUserConfig(input UserConfigInput, opts options.Options) (*config.LaunchKitConfig, string, error) {
	path, err := UserConfigPathFor(input)
	if err != nil {
		return nil, "", err
	}
	if path == "" {
		return nil, "", nil
	}
	cfg, err := config.LoadFullConfig(path, log.Log)
	if err != nil {
		return nil, path, fmt.Errorf("load %s: %w", path, err)
	}
	if cfg == nil {
		return nil, path, fmt.Errorf("user-config %s is empty", path)
	}
	if err := networkoperatorplugin.ApplyNetworkOperatorRelease(opts, cfg); err != nil {
		return nil, path, fmt.Errorf("apply release catalog: %w", err)
	}
	if opts.NetworkOperatorNamespace != "" {
		if cfg.NetworkOperator == nil {
			cfg.NetworkOperator = &config.NetworkOperatorConfig{}
		}
		cfg.NetworkOperator.Namespace = opts.NetworkOperatorNamespace
	}
	return cfg, path, nil
}

// PresetCatalogForConfigDir resolves the Host preset catalog and config assets.
func PresetCatalogForConfigDir(configRoot string) (*presets.Catalog, assets.ConfigDir, error) {
	resolved, err := assets.ResolveConfigDir(configRoot)
	if err != nil {
		return nil, assets.ConfigDir{}, err
	}
	catalog, err := presets.CatalogForConfigDir(resolved)
	if err != nil {
		return nil, assets.ConfigDir{}, err
	}
	return catalog, resolved, nil
}
