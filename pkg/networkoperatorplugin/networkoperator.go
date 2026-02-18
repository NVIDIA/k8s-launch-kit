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

package networkoperatorplugin

import (
	"os"
	"strconv"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/plugin"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	PluginName    = "network-operator"
	PluginVersion = "1.0.0"
)

type NetworkOperatorPlugin struct {
	GroupFilter   string
	LabelSelector map[string]string
}

func (p *NetworkOperatorPlugin) GetName() string {
	return PluginName
}

func (p *NetworkOperatorPlugin) GetVersion() string {
	return PluginVersion
}

func (p *NetworkOperatorPlugin) ProfileConfiguredInCmd(options options.Options) bool {
	return options.Fabric != "" || options.DeploymentType != ""
}

func (p *NetworkOperatorPlugin) BuildProfileFromOptions(options options.Options, profile *config.Profile) error {
	profile.Fabric = options.Fabric
	profile.Deployment = options.DeploymentType
	profile.Multirail = options.Multirail
	profile.Ai = options.Ai
	
	// Build SpectrumX nested struct if enabled
	if options.SpectrumX {
		profile.SpectrumX = &config.ProfileSpectrumX{
			SPCXVersion:    options.SPCXVersion,
			MultiplaneMode: options.MultiplaneMode,
			NumberOfPlanes: options.NumberOfPlanes,
		}
	}

	log.Log.V(1).Info("Built profile for plugin", "plugin", p.GetName(), "profile", profile)
	return nil
}

// ApplyOptionsToConfig applies CLI options to the configuration, overriding file values.
// CLI flags take precedence over config file values for any explicitly set option.
func (p *NetworkOperatorPlugin) ApplyOptionsToConfig(options options.Options, fullConfig *config.LaunchKubernetesConfig) error {
	// Apply network operator namespace override from CLI
	if options.NetworkOperatorNamespace != "" {
		if fullConfig.NetworkOperator == nil {
			fullConfig.NetworkOperator = &config.NetworkOperatorConfig{}
		}
		fullConfig.NetworkOperator.Namespace = options.NetworkOperatorNamespace
	}

	if fullConfig.Profile == nil {
		return nil
	}

	// Apply base profile overrides from CLI (non-empty strings and true bools override config)
	if options.Fabric != "" {
		fullConfig.Profile.Fabric = options.Fabric
	}
	if options.DeploymentType != "" {
		fullConfig.Profile.Deployment = options.DeploymentType
	}
	if options.Multirail {
		fullConfig.Profile.Multirail = true
	}
	if options.Ai {
		fullConfig.Profile.Ai = true
	}

	// Apply Spectrum-X CLI options
	if options.SpectrumX {
		if fullConfig.Profile.SpectrumX == nil {
			fullConfig.Profile.SpectrumX = &config.ProfileSpectrumX{}
		}
		if options.SPCXVersion != "" {
			fullConfig.Profile.SpectrumX.SPCXVersion = options.SPCXVersion
		}
		if options.MultiplaneMode != "" {
			fullConfig.Profile.SpectrumX.MultiplaneMode = options.MultiplaneMode
		}
		if options.NumberOfPlanes != 0 {
			fullConfig.Profile.SpectrumX.NumberOfPlanes = options.NumberOfPlanes
		}
	}

	log.Log.V(1).Info("Applied options to config", "plugin", p.GetName())
	return nil
}

func (p *NetworkOperatorPlugin) BuildProfileFromLLMResponse(llmResponse map[string]string, profile *config.Profile) error {
	profile.Fabric = llmResponse["fabric"]
	profile.Deployment = llmResponse["deploymentType"]
	profile.Multirail = llmResponse["multirail"] == "true"
	profile.Ai = llmResponse["ai"] == "true"

	// Build SpectrumX nested struct if enabled
	if llmResponse["spectrumX"] == "true" {
		// Enforce Spectrum-X implied settings
		if profile.Fabric == "" {
			profile.Fabric = "ethernet"
		}
		if profile.Deployment == "" {
			profile.Deployment = "sriov"
		}
		profile.Multirail = true

		spcxVersion := llmResponse["spectrumXVersion"]
		if spcxVersion == "" {
			spcxVersion = "RA2.1"
		}

		multiplaneMode := llmResponse["spectrumXMultiplaneMode"]
		if multiplaneMode == "" {
			multiplaneMode = "swplb"
		}

		numberOfPlanes := 4 // default for swplb/hwplb
		if np, ok := llmResponse["spectrumXNumberOfPlanes"]; ok && np != "" {
			if parsed, err := strconv.Atoi(np); err == nil && (parsed == 1 || parsed == 2 || parsed == 4) {
				numberOfPlanes = parsed
			}
		}

		// Enforce: "none" and "uniplane" modes always use 1 plane
		if multiplaneMode == "none" || multiplaneMode == "uniplane" {
			numberOfPlanes = 1
		}

		profile.SpectrumX = &config.ProfileSpectrumX{
			SPCXVersion:    spcxVersion,
			MultiplaneMode: multiplaneMode,
			NumberOfPlanes: numberOfPlanes,
		}
	}

	log.Log.V(1).Info("Built profile for plugin", "plugin", p.GetName(), "profile", profile)
	return nil
}

func (p *NetworkOperatorPlugin) GetSystemPromptAddendum() (string, error) {
	data, err := os.ReadFile("network-operator-system-prompt-addendum")
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func (p *NetworkOperatorPlugin) SelectProfile(config *config.LaunchKubernetesConfig) (*profiles.Profile, error) {
	return nil, nil
}

var _ plugin.Plugin = &NetworkOperatorPlugin{}
