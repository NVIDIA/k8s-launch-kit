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

package app

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/resolve"
)

// resolveProfileSettings applies the shared profile-resolution pipeline used
// by both discover and generate:
//
//	hardware defaults < values already present in config < explicit CLI flags
//
// Hardware defaults only fill fields that are absent, plugin option appliers
// then overlay explicitly-set CLI values, and the final cohort is validated
// before it is rendered or persisted.
func (l *Launcher) resolveProfileSettings(fullConfig *config.LaunchKitConfig) error {
	decisions := resolve.ApplyHardwareDefaults(fullConfig, l.options)
	for _, decision := range decisions {
		l.ui.Info("%s", decision.String())
		l.logger.Info("Applied hardware default",
			"flag", decision.Flag,
			"value", decision.Value,
			"reason", decision.Reason)
	}

	for _, enabledPlugin := range l.plugins {
		applier, ok := enabledPlugin.(interface {
			ApplyOptionsToConfig(options.Options, *config.LaunchKitConfig) error
		})
		if !ok {
			continue
		}
		if err := applier.ApplyOptionsToConfig(l.options, fullConfig); err != nil {
			return fmt.Errorf("failed to apply options to config for plugin %s: %w", enabledPlugin.GetName(), err)
		}
	}

	if err := resolve.ValidateResolvedConfig(fullConfig); err != nil {
		return apperrors.NewValidationError(err.Error(), nil,
			"Adjust the conflicting flags or fields in cluster-config.yaml.")
	}

	l.recordResolvedProfile(fullConfig)
	return nil
}

func (l *Launcher) recordResolvedProfile(fullConfig *config.LaunchKitConfig) {
	if fullConfig.Profile == nil {
		return
	}

	l.result.Profile = map[string]string{
		"fabric":     fullConfig.Profile.Fabric,
		"deployment": fullConfig.Profile.Deployment,
		"multirail":  fmt.Sprintf("%v", fullConfig.Profile.Multirail),
	}
	if spectrumX := fullConfig.Profile.SpectrumX; spectrumX != nil && spectrumX.Enable {
		l.result.Profile["spectrumX"] = "true"
		l.result.Profile["multiplaneMode"] = spectrumX.MultiplaneMode
		l.result.Profile["numberOfPlanes"] = fmt.Sprintf("%d", spectrumX.NumberOfPlanes)
		l.result.Profile["spcxVersion"] = spectrumX.SPCXVersion
	}
}
