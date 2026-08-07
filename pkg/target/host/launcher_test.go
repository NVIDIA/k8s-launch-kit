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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/target"
)

func TestPrepareLauncherOptionsPreservesExplicitKubeconfig(t *testing.T) {
	tests := []struct {
		name    string
		phase   target.Phase
		options options.Options
	}{
		{
			name:  "discover",
			phase: target.Discover,
			options: options.Options{
				Kubeconfig: "/explicit/kubeconfig",
			},
		},
		{
			name:  "generate with deploy",
			phase: target.Generate,
			options: options.Options{
				UserConfig: "/explicit/config.yaml",
				Deploy:     true,
				Kubeconfig: "/explicit/kubeconfig",
			},
		},
		{
			name:  "pipeline with discovery",
			phase: target.Pipeline,
			options: options.Options{
				DiscoverClusterConfig: true,
				Kubeconfig:            "/explicit/kubeconfig",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, prepareLauncherOptions(test.phase, &test.options))
			assert.Equal(t, "/explicit/kubeconfig", test.options.Kubeconfig)
		})
	}
}

func TestValidateLauncherOptionsByPhase(t *testing.T) {
	t.Run("discover rejects invalid profile syntax", func(t *testing.T) {
		err := validateLauncherOptions(target.Discover, &options.Options{Fabric: "invalid"})
		assert.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(err))
		assert.ErrorContains(t, err, "--fabric")
	})

	t.Run("generate requires node selector for preset", func(t *testing.T) {
		err := validateLauncherOptions(target.Generate, &options.Options{ForPreset: "server-sku"})
		assert.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(err))
		assert.ErrorContains(t, err, "--for requires --node-selector")
	})

	t.Run("pipeline accepts established host options", func(t *testing.T) {
		err := validateLauncherOptions(target.Pipeline, &options.Options{
			UserConfig:     "cluster-config.yaml",
			EnabledPlugins: []string{"network-operator"},
			OutputFormat:   "text",
		})
		require.NoError(t, err)
	})

	t.Run("unsupported phase fails", func(t *testing.T) {
		err := validateLauncherOptions(target.Deploy, &options.Options{})
		assert.ErrorContains(t, err, "does not support phase")
	})
}

func TestPrepareLauncherOptionsRejectsUnsupportedPhase(t *testing.T) {
	assert.ErrorContains(t, prepareLauncherOptions(target.Validate, &options.Options{}), "does not support phase")
}
