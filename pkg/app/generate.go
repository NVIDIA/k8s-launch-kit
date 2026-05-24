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

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/presets"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/nvidia/k8s-launch-kit/pkg/resolve"
)

// executeGeneration handles the profile selection and manifest generation phase.
// Returns nil if no profile is configured and generation is skipped.
func (l *Launcher) executeGeneration(configPath string) error {
	fullConfig, err := config.LoadFullConfig(configPath, l.logger)
	if err != nil {
		return fmt.Errorf("failed to load full config: %w", err)
	}

	// Validate `--groups` / `--gpu-type` against the loaded config before
	// the profile-configured check. Without this, a filter that matches
	// no source group silently succeeds when no profile flags were
	// supplied (the in-plugin filter validation is skipped along with
	// the entire generation phase) — a typo produces no error.
	for _, plugin := range l.plugins {
		if validator, ok := plugin.(interface {
			ValidateGroupFilter(*config.LaunchKubernetesConfig) error
		}); ok {
			if err := validator.ValidateGroupFilter(fullConfig); err != nil {
				// Pass nil as the cause — the error message already
				// includes everything the user needs (mismatched
				// values + available alternatives). Including err as
				// the cause would duplicate the text in
				// StructuredError.Error()'s "msg: cause" formatting.
				return apperrors.NewValidationError(
					err.Error(), nil,
					"Pass identifiers from cluster-config.yaml's clusterConfig[].identifier, or use --gpu-type with a value matching a group's gpuType",
				)
			}
		}
	}

	profilesConfiguredInCmd := true
	for _, plugin := range l.plugins {
		if !plugin.ProfileConfiguredInCmd(l.options) {
			profilesConfiguredInCmd = false
			break
		}
	}

	// Phase 1.5: hardware-derived defaults. Fills empty profile fields
	// from cluster hardware (linkType, east-west PF deviceID, etc.) so
	// the user can run `l8k generate` against a discovered config
	// without repeating obvious flags. CLI overlay (next) overrides
	// these; config-file values were already loaded into cfg.Profile by
	// LoadFullConfig and are preserved.
	decisions := resolve.ApplyHardwareDefaults(fullConfig, l.options)
	for _, d := range decisions {
		l.ui.Info("%s", d.String())
		l.logger.Info("Applied hardware default", "flag", d.Flag, "value", d.Value, "reason", d.Reason)
	}

	// Apply CLI options to override config values + hardware defaults.
	for _, plugin := range l.plugins {
		if applier, ok := plugin.(interface {
			ApplyOptionsToConfig(options.Options, *config.LaunchKubernetesConfig) error
		}); ok {
			if err := applier.ApplyOptionsToConfig(l.options, fullConfig); err != nil {
				return fmt.Errorf("failed to apply options to config for plugin %s: %w", plugin.GetName(), err)
			}
		}
	}

	// Now that fullConfig.NetworkOperator is fully resolved (catalog
	// values + CLI overrides + l8k-config.yaml), expose it to the plugin
	// so DeployProfile can drive Phase 0 helm install from the same
	// metadata that drove `00-values.yaml` rendering.
	if nop, ok := l.plugins[networkoperatorplugin.PluginName]; ok {
		if p, ok := nop.(*networkoperatorplugin.NetworkOperatorPlugin); ok {
			p.NetworkOperator = fullConfig.NetworkOperator
			p.OverwriteExisting = l.options.OverwriteExisting
			p.DryRun = l.options.DryRun
			if fullConfig.DOCADriver != nil {
				p.DOCAVersion = fullConfig.DOCADriver.Version
			}
		}
	}

	// Sufficiency: if defaults + CLI couldn't produce a Fabric, we
	// have nothing to render. This is the legacy "no profile
	// configured" path under the new defaults-aware flow — Fabric
	// is the one field that defaults can fail to fill (depends on
	// Unit 5's per-port linkType probe being unanimous; if any port
	// is unverified, defaults skip). Deployment + Multirail always
	// default.
	if fullConfig.Profile == nil || fullConfig.Profile.Fabric == "" {
		if !profilesConfiguredInCmd && len(fullConfig.ClusterConfig) == 0 {
			l.ui.Info("Profiles not configured, skipping deployment file generation")
			l.logger.Info("Profiles are not configured for every plugin, skipping deployment files generation")
			return nil
		}
		return apperrors.NewValidationError(
			"no fabric resolved: hardware defaulting couldn't pick a fabric (groups disagree on linkType, or fabric probe couldn't verify)",
			nil,
			"Pass --fabric <ethernet|infiniband> explicitly, or run `l8k discover` again so per-port linkType probes can produce a confirmed verdict.",
		)
	}

	// Phase 2 cohort validation — runs against the fully-resolved cfg
	// so cross-flag rules ("--spectrum-x requires fabric=ethernet",
	// "RA2.1 requires --network-operator-release 26.1", etc.) see
	// values that defaults filled, not just user-supplied ones.
	if err := resolve.ValidateResolvedConfig(fullConfig); err != nil {
		return apperrors.NewValidationError(err.Error(), nil,
			"Adjust the conflicting flags or fields in cluster-config.yaml.")
	}

	// --for: replace clusterConfig with a synthesized group from a preset.
	// This is the explicit ahead-of-time generation path that skips live
	// discovery in favor of a static preset description. The CLI layer has
	// already enforced --node-selector being set; here we do the substitution
	// before the rest of the pipeline runs.
	if l.options.ForPreset != "" {
		preset, err := presets.LoadPresetByDir(l.options.ForPreset)
		if err != nil {
			return apperrors.NewValidationError(
				fmt.Sprintf("invalid --for value %q", l.options.ForPreset),
				err,
				"Run 'l8k preset list' to see available presets",
			)
		}
		selectorMap := parseNodeSelector(l.options.NodeSelector)
		cc, synthErr := presets.SynthesizeClusterConfig(l.options.ForPreset, preset, selectorMap)
		if synthErr != nil {
			return apperrors.NewValidationError(
				fmt.Sprintf("preset %q cannot be used with --for", l.options.ForPreset),
				synthErr,
				"Add a 'capabilities.nodes.{sriov,rdma,ib}' block to the preset's topology.yaml",
			)
		}
		fullConfig.ClusterConfig = []config.ClusterConfig{cc}
		l.ui.Info("Using preset %q (clusterConfig replaced from preset)", l.options.ForPreset)
	}

	// Capture profile info for JSON result
	if fullConfig.Profile != nil {
		l.result.Profile = map[string]string{
			"fabric":     fullConfig.Profile.Fabric,
			"deployment": fullConfig.Profile.Deployment,
			"multirail":  fmt.Sprintf("%v", fullConfig.Profile.Multirail),
		}
		if fullConfig.Profile.SpectrumX != nil {
			l.result.Profile["spectrumX"] = "true"
			l.result.Profile["multiplaneMode"] = fullConfig.Profile.SpectrumX.MultiplaneMode
			l.result.Profile["numberOfPlanes"] = fmt.Sprintf("%d", fullConfig.Profile.SpectrumX.NumberOfPlanes)
			l.result.Profile["spcxVersion"] = fullConfig.Profile.SpectrumX.SPCXVersion
		}
	}

	aggregatedCapabilities := config.AggregateCapabilities(fullConfig.ClusterConfig)

	foundProfiles := []profiles.Profile{}
	for pluginName, plugin := range l.plugins {
		selectedRelease := ""
		if fullConfig.NetworkOperator != nil {
			selectedRelease = fullConfig.NetworkOperator.SelectedRelease
		}
		profile, err := profiles.FindApplicableProfile(fullConfig.Profile, aggregatedCapabilities, pluginName, selectedRelease)
		if err != nil {
			l.ui.Error("Failed to find profile: %v", err)
			l.logger.Error(err, "Failed to find applicable profile for the plugin", "plugin", plugin.GetName(), "cluster capabilities", aggregatedCapabilities, "profile requirements", fullConfig.Profile)
			return err
		}
		foundProfiles = append(foundProfiles, *profile)
	}

	l.result.Phase = "generate"
	l.ui.Section("Deployment File Generation")
	for _, profile := range foundProfiles {
		l.ui.Info("Generating files for profile: %s", profile.Name)
		l.logger.Info("Generating deployment files for profile", "profile", profile.Name)

		if err := l.generateDeploymentFiles(&profile, fullConfig); err != nil {
			l.ui.Error("File generation failed: %v", err)
			return apperrors.NewGeneralError("deployment files generation failed", err)
		}
	}

	// Store found profiles for deploy phase
	l.foundProfiles = foundProfiles

	warnThirdPartyRDMAModules(fullConfig, "generate", l.ui)
	warnStorageModules(fullConfig, "generate", l.ui)

	return nil
}

// generateDeploymentFiles handles deployment file generation for a single profile.
func (l *Launcher) generateDeploymentFiles(profile *profiles.Profile, clusterConfig *config.LaunchKubernetesConfig) error {
	l.logger.Info("Generating deployment files", "profile", profile.Name)
	l.logger.Info("Generating deployment files", "config", clusterConfig)

	plugin, ok := l.plugins[profile.Plugin]
	if !ok {
		return fmt.Errorf("plugin %s not found", profile.Plugin)
	}

	renderedFiles, err := plugin.GenerateProfileDeploymentFiles(profile, clusterConfig)
	if err != nil {
		return fmt.Errorf("failed to process profile templates: %w", err)
	}

	if l.options.SaveDeploymentFiles != "" {
		if err := l.saveDeploymentFiles(renderedFiles, filepath.Join(l.options.SaveDeploymentFiles, profile.Plugin)); err != nil {
			return fmt.Errorf("failed to save deployment files: %w", err)
		}
	}

	return nil
}

// saveDeploymentFiles saves the rendered deployment files to disk.
func (l *Launcher) saveDeploymentFiles(renderedFiles map[string]string, outputDir string) error {
	l.logger.Info("Saving deployment files", "directory", outputDir)

	// Clean the output directory before saving files
	if err := os.RemoveAll(outputDir); err != nil {
		return fmt.Errorf("failed to clean output directory %s: %w", outputDir, err)
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory %s: %w", outputDir, err)
	}

	for filename, content := range renderedFiles {
		outputPath := fmt.Sprintf("%s/%s", outputDir, filename)

		if err := os.WriteFile(outputPath, []byte(content), 0644); err != nil {
			l.ui.Error("Failed to write file %s: %v", outputPath, err)
			return fmt.Errorf("failed to write file %s: %w", outputPath, err)
		}

		l.logger.Info("Saved deployment file", "file", outputPath)
		l.result.GeneratedFiles = append(l.result.GeneratedFiles, outputPath)
	}

	l.ui.Success("Saved %d file(s) to: %s", len(renderedFiles), outputDir)
	l.logger.Info("All deployment files saved successfully",
		"directory", outputDir,
		"fileCount", len(renderedFiles))

	return nil
}
