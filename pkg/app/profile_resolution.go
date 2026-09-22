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
	"github.com/nvidia/k8s-launch-kit/pkg/resolve"
)

func (l *Launcher) resolveProfileInput(input *config.Input) (*config.LaunchKitConfig, error) {
	result, err := resolve.Resolve(resolve.Request{
		Input:                 input,
		Options:               l.options,
		ApplyHardwareDefaults: true,
		ValidateReady:         true,
	})
	if err != nil {
		return nil, apperrors.NewValidationError(err.Error(), nil,
			"Adjust the conflicting flags or fields in cluster-config.yaml.")
	}

	for _, decision := range result.Decisions {
		l.ui.Info("%s", decision.String())
		l.logger.Info("Applied hardware default",
			"flag", decision.Flag,
			"value", decision.Value,
			"reason", decision.Reason)
	}

	l.recordResolvedProfile(result.Config)
	return result.Config, nil
}

func (l *Launcher) recordResolvedProfile(fullConfig *config.LaunchKitConfig) {
	if fullConfig.Profile == nil {
		return
	}

	l.result.Profile = map[string]string{
		"fabric":     fullConfig.Profile.Fabric,
		"deployment": fullConfig.Profile.Deployment,
		"multirail":  fmt.Sprintf("%v", fullConfig.Profile.Multirail),
		"routing":    fullConfig.Profile.Routing,
		"ignoreARP":  fmt.Sprintf("%v", fullConfig.Profile.IgnoreARP),
	}
	if spectrumX := fullConfig.Profile.SpectrumX; spectrumX != nil && spectrumX.Enable {
		l.result.Profile["spectrumX"] = "true"
		l.result.Profile["multiplaneMode"] = spectrumX.MultiplaneMode
		l.result.Profile["numberOfPlanes"] = fmt.Sprintf("%d", spectrumX.NumberOfPlanes)
		l.result.Profile["spcxVersion"] = spectrumX.SPCXVersion
	}
}
