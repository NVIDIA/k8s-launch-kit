// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/configflags"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/resolve"
)

// ConfigFlagMapping describes the configuration fields controlled by one CLI
// flag. It is retained as a compatibility view over the tagged schema.
type ConfigFlagMapping struct {
	FlagName    string
	ConfigPaths []string
}

// ConfigFlagMappings returns the canonical CLI-to-config mapping.
func ConfigFlagMappings() []ConfigFlagMapping {
	definitions := configflags.Definitions()
	out := make([]ConfigFlagMapping, 0, len(definitions))
	for _, definition := range definitions {
		out = append(out, ConfigFlagMapping{
			FlagName:    definition.FlagName,
			ConfigPaths: append([]string(nil), definition.ConfigPaths...),
		})
	}
	return out
}

// ConfigPathsForFlag returns the YAML paths controlled by flagName.
func ConfigPathsForFlag(flagName string) []string {
	return configflags.ConfigPathsForFlag(strings.TrimPrefix(flagName, "--"))
}

// ApplyCLIConfigOverrides is the legacy plugin entry point for the central
// resolver's tagged CLI overlay.
func ApplyCLIConfigOverrides(opts options.Options, cfg *config.LaunchKitConfig) error {
	return resolve.ApplyCLIConfigOverrides(opts, cfg)
}

// ApplyExplicitCLIConfigOverrides applies only values supplied through CLI
// options and does not expand a YAML-only release a second time.
func ApplyExplicitCLIConfigOverrides(opts options.Options, cfg *config.LaunchKitConfig) error {
	return resolve.ApplyExplicitCLIConfigOverrides(opts, cfg)
}
