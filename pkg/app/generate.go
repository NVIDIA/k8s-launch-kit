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
	"github.com/nvidia/k8s-launch-kit/pkg/llm"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
)

// executeGeneration handles the profile selection and manifest generation phase.
// If configPath is empty and interactive mode is requested, it starts an interactive session.
// Returns nil if no profile is configured and generation is skipped.
func (l *Launcher) executeGeneration(configPath string) error {
	// When in interactive mode without a config, go directly to interactive session
	// (supports troubleshooting via sosreport without needing a cluster config)
	if configPath == "" && l.options.LLMInteractive {
		l.ui.Section("Interactive Session (AI-Assisted)")
		l.logger.Info("Starting interactive session (no cluster config)")
		_, err := l.runInteractiveSession(nil)
		if err != nil {
			l.ui.Error("Interactive session failed: %v", err)
			return fmt.Errorf("interactive session failed: %w", err)
		}
		return nil
	}

	fullConfig, err := config.LoadFullConfig(configPath, l.logger)
	if err != nil {
		return fmt.Errorf("failed to load full config: %w", err)
	}

	profilesConfiguredInCmd := true
	for _, plugin := range l.plugins {
		if !plugin.ProfileConfiguredInCmd(l.options) {
			profilesConfiguredInCmd = false
			break
		}
	}

	profileInConfig := fullConfig.Profile != nil
	if !profilesConfiguredInCmd && !profileInConfig && l.options.Prompt == "" && !l.options.LLMInteractive {
		l.ui.Info("Profiles not configured, skipping deployment file generation")
		l.logger.Info("Profiles are not configured for every plugin, skipping deployment files generation")
		return nil
	}

	if fullConfig.Profile == nil {
		fullConfig.Profile = &config.Profile{}

		if profilesConfiguredInCmd {
			for _, plugin := range l.plugins {
				if err := plugin.BuildProfileFromOptions(l.options, fullConfig.Profile); err != nil {
					return fmt.Errorf("failed to build profile for plugin %s: %w", plugin.GetName(), err)
				}
			}
		} else if l.options.LLMInteractive {
			if err := l.selectProfileInteractive(fullConfig); err != nil {
				return err
			}
		} else if l.options.Prompt != "" {
			if err := l.selectProfileFromPrompt(fullConfig); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("no profile configured in the command line and no prompt provided")
		}
	}

	// Apply CLI options to override config values
	for _, plugin := range l.plugins {
		if applier, ok := plugin.(interface {
			ApplyOptionsToConfig(options.Options, *config.LaunchKubernetesConfig) error
		}); ok {
			if err := applier.ApplyOptionsToConfig(l.options, fullConfig); err != nil {
				return fmt.Errorf("failed to apply options to config for plugin %s: %w", plugin.GetName(), err)
			}
		}
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

// selectProfileInteractive runs an interactive LLM session for profile selection.
func (l *Launcher) selectProfileInteractive(fullConfig *config.LaunchKubernetesConfig) error {
	l.ui.Section("Profile Selection (AI-Assisted)")
	l.logger.Info("Starting interactive LLM session")

	prompt, err := l.runInteractiveSession(fullConfig.ClusterConfig)
	if err != nil {
		l.ui.Error("Interactive session failed: %v", err)
		return fmt.Errorf("interactive session failed: %w", err)
	}

	for _, plugin := range l.plugins {
		if err := plugin.BuildProfileFromLLMResponse(prompt, fullConfig.Profile); err != nil {
			return fmt.Errorf("failed to build profile for plugin %s: %w", plugin.GetName(), err)
		}
	}

	l.logProfileSelection(fullConfig, prompt)
	return nil
}

// selectProfileFromPrompt uses a one-shot LLM call for profile selection.
func (l *Launcher) selectProfileFromPrompt(fullConfig *config.LaunchKubernetesConfig) error {
	l.ui.Section("Profile Selection (AI-Assisted)")
	l.ui.Info("Analyzing requirements with AI")
	progress := l.ui.StartProgress("Waiting for AI recommendation")

	l.logger.Info("Selecting a profile using LLM-assisted prompt")

	prompt, err := llm.SelectPromptWithModel(l.options.Prompt, fullConfig.ClusterConfig, l.options.LLMApiKey, l.options.LLMApiUrl, l.options.LLMVendor, l.options.LLMModel)
	if err != nil {
		progress.Fail("AI selection failed")
		l.ui.Error("Failed to get AI recommendation: %v", err)
		return fmt.Errorf("failed to select prompt: %w", err)
	}
	confidence := prompt["confidence"]
	if confidence == "low" {
		progress.Fail("Low confidence recommendation")
		l.ui.Warning("AI has low confidence: %s", prompt["reasoning"])
		return fmt.Errorf("couldn't select a deployment profile based on the user prompt. Try again with a different prompt or use the cli flags (--fabric, --deployment-type, --multirail) to select the profile manually. Reason: %s", prompt["reasoning"])
	}

	for _, plugin := range l.plugins {
		if err := plugin.BuildProfileFromLLMResponse(prompt, fullConfig.Profile); err != nil {
			progress.Fail("Profile building failed")
			return fmt.Errorf("failed to build profile for plugin %s: %w", plugin.GetName(), err)
		}
	}

	progress.Success("Profile selected")
	l.logProfileSelection(fullConfig, prompt)
	return nil
}

// logProfileSelection logs the selected profile details.
func (l *Launcher) logProfileSelection(fullConfig *config.LaunchKubernetesConfig, prompt map[string]string) {
	l.ui.Info("  Fabric: %s", fullConfig.Profile.Fabric)
	l.ui.Info("  Deployment: %s", fullConfig.Profile.Deployment)
	l.ui.Info("  Multirail: %v", fullConfig.Profile.Multirail)
	if fullConfig.Profile.SpectrumX != nil {
		l.ui.Info("  Spectrum-X: enabled")
		l.ui.Info("    Multiplane Mode: %s", fullConfig.Profile.SpectrumX.MultiplaneMode)
		l.ui.Info("    Number of Planes: %d", fullConfig.Profile.SpectrumX.NumberOfPlanes)
		l.ui.Info("    SPCX Version: %s", fullConfig.Profile.SpectrumX.SPCXVersion)
	}
	l.logger.Info("Selected options",
		"fabric", fullConfig.Profile.Fabric,
		"deployment", fullConfig.Profile.Deployment,
		"multirail", fullConfig.Profile.Multirail,
		"spectrumX", fullConfig.Profile.SpectrumX,
		"ai", fullConfig.Profile.Ai,
		"reasoning", prompt["reasoning"])
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
