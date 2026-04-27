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
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProfileConfiguredInCmd(t *testing.T) {
	p := &NetworkOperatorPlugin{}

	t.Run("fabric set", func(t *testing.T) {
		assert.True(t, p.ProfileConfiguredInCmd(options.Options{Fabric: "ethernet"}))
	})

	t.Run("deployment type set", func(t *testing.T) {
		assert.True(t, p.ProfileConfiguredInCmd(options.Options{DeploymentType: "sriov"}))
	})

	t.Run("neither set", func(t *testing.T) {
		assert.False(t, p.ProfileConfiguredInCmd(options.Options{}))
	})
}

func TestBuildProfileFromOptions(t *testing.T) {
	p := &NetworkOperatorPlugin{}

	t.Run("basic ethernet sriov", func(t *testing.T) {
		opts := options.Options{
			Fabric:         "ethernet",
			DeploymentType: "sriov",
		}
		profile := &config.Profile{}
		err := p.BuildProfileFromOptions(opts, profile)
		require.NoError(t, err)
		assert.Equal(t, "ethernet", profile.Fabric)
		assert.Equal(t, "sriov", profile.Deployment)
		assert.False(t, profile.Multirail)
		assert.False(t, profile.Ai)
		assert.Nil(t, profile.SpectrumX)
	})

	t.Run("with spectrum-x creates sub-struct", func(t *testing.T) {
		opts := options.Options{
			Fabric:         "ethernet",
			DeploymentType: "sriov",
			Multirail:      true,
			SpectrumX:      true,
			SPCXVersion:    "RA2.2",
			MultiplaneMode: "hwplb",
			NumberOfPlanes: 2,
		}
		profile := &config.Profile{}
		err := p.BuildProfileFromOptions(opts, profile)
		require.NoError(t, err)
		assert.Equal(t, "ethernet", profile.Fabric)
		assert.Equal(t, "sriov", profile.Deployment)
		assert.True(t, profile.Multirail)
		require.NotNil(t, profile.SpectrumX)
		assert.Equal(t, "RA2.2", profile.SpectrumX.SPCXVersion)
		assert.Equal(t, "hwplb", profile.SpectrumX.MultiplaneMode)
		assert.Equal(t, 2, profile.SpectrumX.NumberOfPlanes)
	})

	t.Run("without spectrum-x leaves sub-struct nil", func(t *testing.T) {
		opts := options.Options{
			Fabric:         "ethernet",
			DeploymentType: "sriov",
			SpectrumX:      false,
			SPCXVersion:    "RA2.2", // these should be ignored
			MultiplaneMode: "hwplb",
		}
		profile := &config.Profile{}
		err := p.BuildProfileFromOptions(opts, profile)
		require.NoError(t, err)
		assert.Nil(t, profile.SpectrumX)
	})

	t.Run("all fields populated", func(t *testing.T) {
		opts := options.Options{
			Fabric:         "ethernet",
			DeploymentType: "sriov",
			Multirail:      true,
			Ai:             true,
			SpectrumX:      true,
			SPCXVersion:    "RA2.2",
			MultiplaneMode: "swplb",
			NumberOfPlanes: 4,
		}
		profile := &config.Profile{}
		err := p.BuildProfileFromOptions(opts, profile)
		require.NoError(t, err)
		assert.Equal(t, "ethernet", profile.Fabric)
		assert.Equal(t, "sriov", profile.Deployment)
		assert.True(t, profile.Multirail)
		assert.True(t, profile.Ai)
		require.NotNil(t, profile.SpectrumX)
		assert.Equal(t, "RA2.2", profile.SpectrumX.SPCXVersion)
		assert.Equal(t, "swplb", profile.SpectrumX.MultiplaneMode)
		assert.Equal(t, 4, profile.SpectrumX.NumberOfPlanes)
	})
}

func TestApplyOptionsToConfig(t *testing.T) {
	p := &NetworkOperatorPlugin{}

	t.Run("nil profile is no-op", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{Profile: nil}
		err := p.ApplyOptionsToConfig(options.Options{Fabric: "ethernet"}, cfg)
		require.NoError(t, err)
		assert.Nil(t, cfg.Profile)
	})

	t.Run("CLI fabric overrides config fabric", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{Fabric: "infiniband"},
		}
		err := p.ApplyOptionsToConfig(options.Options{Fabric: "ethernet"}, cfg)
		require.NoError(t, err)
		assert.Equal(t, "ethernet", cfg.Profile.Fabric)
	})

	t.Run("CLI deployment type overrides config deployment", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{Deployment: "rdma_shared"},
		}
		err := p.ApplyOptionsToConfig(options.Options{DeploymentType: "sriov"}, cfg)
		require.NoError(t, err)
		assert.Equal(t, "sriov", cfg.Profile.Deployment)
	})

	t.Run("CLI multirail true overrides config false", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{Multirail: false},
		}
		err := p.ApplyOptionsToConfig(options.Options{Multirail: true}, cfg)
		require.NoError(t, err)
		assert.True(t, cfg.Profile.Multirail)
	})

	t.Run("CLI multirail false does NOT override config true", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{Multirail: true},
		}
		err := p.ApplyOptionsToConfig(options.Options{Multirail: false}, cfg)
		require.NoError(t, err)
		assert.True(t, cfg.Profile.Multirail)
	})

	t.Run("empty CLI strings preserve config values", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{
				Fabric:     "ethernet",
				Deployment: "sriov",
				Multirail:  true,
			},
		}
		err := p.ApplyOptionsToConfig(options.Options{}, cfg)
		require.NoError(t, err)
		assert.Equal(t, "ethernet", cfg.Profile.Fabric)
		assert.Equal(t, "sriov", cfg.Profile.Deployment)
		assert.True(t, cfg.Profile.Multirail)
	})

	t.Run("spectrum-x CLI creates sub-struct when config has none", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{
				Fabric:    "ethernet",
				SpectrumX: nil,
			},
		}
		opts := options.Options{
			SpectrumX:      true,
			SPCXVersion:    "RA2.2",
			MultiplaneMode: "hwplb",
			NumberOfPlanes: 2,
		}
		err := p.ApplyOptionsToConfig(opts, cfg)
		require.NoError(t, err)
		require.NotNil(t, cfg.Profile.SpectrumX)
		assert.Equal(t, "RA2.2", cfg.Profile.SpectrumX.SPCXVersion)
		assert.Equal(t, "hwplb", cfg.Profile.SpectrumX.MultiplaneMode)
		assert.Equal(t, 2, cfg.Profile.SpectrumX.NumberOfPlanes)
	})

	t.Run("spectrum-x CLI overrides specific fields preserves others", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{
				SpectrumX: &config.ProfileSpectrumX{
					SPCXVersion:    "RA2.2",
					MultiplaneMode: "hwplb",
					NumberOfPlanes: 2,
				},
			},
		}
		opts := options.Options{
			SpectrumX:      true,
			MultiplaneMode: "swplb", // override only this
		}
		err := p.ApplyOptionsToConfig(opts, cfg)
		require.NoError(t, err)
		assert.Equal(t, "RA2.2", cfg.Profile.SpectrumX.SPCXVersion)     // preserved
		assert.Equal(t, "swplb", cfg.Profile.SpectrumX.MultiplaneMode)   // overridden
		assert.Equal(t, 2, cfg.Profile.SpectrumX.NumberOfPlanes)         // preserved
	})

	t.Run("CLI number-of-planes zero does NOT override config", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{
				SpectrumX: &config.ProfileSpectrumX{
					NumberOfPlanes: 4,
				},
			},
		}
		opts := options.Options{
			SpectrumX:      true,
			NumberOfPlanes: 0, // zero value should not override
		}
		err := p.ApplyOptionsToConfig(opts, cfg)
		require.NoError(t, err)
		assert.Equal(t, 4, cfg.Profile.SpectrumX.NumberOfPlanes)
	})

	t.Run("full spectrum-x overlay merges all fields", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{
				Fabric:     "ethernet",
				Deployment: "sriov",
				Multirail:  false,
				SpectrumX: &config.ProfileSpectrumX{
					SPCXVersion:    "",
					MultiplaneMode: "uniplane",
					NumberOfPlanes: 1,
				},
			},
		}
		opts := options.Options{
			Fabric:         "ethernet",
			DeploymentType: "sriov",
			Multirail:      true,
			SpectrumX:      true,
			SPCXVersion:    "RA2.2",
			MultiplaneMode: "hwplb",
			NumberOfPlanes: 2,
		}
		err := p.ApplyOptionsToConfig(opts, cfg)
		require.NoError(t, err)
		assert.Equal(t, "ethernet", cfg.Profile.Fabric)
		assert.Equal(t, "sriov", cfg.Profile.Deployment)
		assert.True(t, cfg.Profile.Multirail)
		require.NotNil(t, cfg.Profile.SpectrumX)
		assert.Equal(t, "RA2.2", cfg.Profile.SpectrumX.SPCXVersion)
		assert.Equal(t, "hwplb", cfg.Profile.SpectrumX.MultiplaneMode)
		assert.Equal(t, 2, cfg.Profile.SpectrumX.NumberOfPlanes)
	})

	t.Run("CLI image-pull-secrets overrides config", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			NetworkOperator: &config.NetworkOperatorConfig{
				ImagePullSecrets: []string{"old-secret"},
			},
			Profile: &config.Profile{},
		}
		err := p.ApplyOptionsToConfig(options.Options{ImagePullSecrets: []string{"new-secret"}}, cfg)
		require.NoError(t, err)
		assert.Equal(t, []string{"new-secret"}, cfg.NetworkOperator.ImagePullSecrets)
	})

	t.Run("empty CLI image-pull-secrets preserves config", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			NetworkOperator: &config.NetworkOperatorConfig{
				ImagePullSecrets: []string{"existing"},
			},
			Profile: &config.Profile{},
		}
		err := p.ApplyOptionsToConfig(options.Options{}, cfg)
		require.NoError(t, err)
		assert.Equal(t, []string{"existing"}, cfg.NetworkOperator.ImagePullSecrets)
	})

	t.Run("CLI image-pull-secrets creates NetworkOperator if nil", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			NetworkOperator: nil,
			Profile:         &config.Profile{},
		}
		err := p.ApplyOptionsToConfig(options.Options{ImagePullSecrets: []string{"my-secret"}}, cfg)
		require.NoError(t, err)
		require.NotNil(t, cfg.NetworkOperator)
		assert.Equal(t, []string{"my-secret"}, cfg.NetworkOperator.ImagePullSecrets)
	})

	t.Run("network operator release populates catalog values", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{Profile: &config.Profile{}}
		err := p.ApplyOptionsToConfig(options.Options{NetworkOperatorRelease: "26.4"}, cfg)
		require.NoError(t, err)
		require.NotNil(t, cfg.NetworkOperator)
		assert.Equal(t, "26.4", cfg.NetworkOperator.SelectedRelease)
		assert.NotEmpty(t, cfg.NetworkOperator.Version)
		assert.NotEmpty(t, cfg.NetworkOperator.ComponentVersion)
		assert.NotEmpty(t, cfg.NetworkOperator.Repository)
		require.NotNil(t, cfg.DOCADriver)
		assert.NotEmpty(t, cfg.DOCADriver.Version)
	})

	t.Run("network operator release overrides config-file values", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			NetworkOperator: &config.NetworkOperatorConfig{
				Version:          "v0.0.0-stale",
				ComponentVersion: "stale-tag",
				Repository:       "stale.example.com",
			},
			DOCADriver: &config.DOCADriverConfig{Version: "stale-doca"},
			Profile:    &config.Profile{},
		}
		err := p.ApplyOptionsToConfig(options.Options{NetworkOperatorRelease: "26.1"}, cfg)
		require.NoError(t, err)
		assert.Equal(t, "26.1", cfg.NetworkOperator.SelectedRelease)
		assert.NotEqual(t, "v0.0.0-stale", cfg.NetworkOperator.Version)
		assert.NotEqual(t, "stale-tag", cfg.NetworkOperator.ComponentVersion)
		assert.NotEqual(t, "stale.example.com", cfg.NetworkOperator.Repository)
		assert.NotEqual(t, "stale-doca", cfg.DOCADriver.Version)
	})

	t.Run("unknown release is rejected", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{Profile: &config.Profile{}}
		err := p.ApplyOptionsToConfig(options.Options{NetworkOperatorRelease: "99.0"}, cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported network operator release")
	})

	t.Run("empty release leaves SelectedRelease empty (no-op)", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			NetworkOperator: &config.NetworkOperatorConfig{
				Version:          "user-supplied",
				ComponentVersion: "user-tag",
				Repository:       "user.example.com",
			},
			Profile: &config.Profile{},
		}
		err := p.ApplyOptionsToConfig(options.Options{}, cfg)
		require.NoError(t, err)
		assert.Empty(t, cfg.NetworkOperator.SelectedRelease)
		assert.Equal(t, "user-supplied", cfg.NetworkOperator.Version)
		assert.Equal(t, "user-tag", cfg.NetworkOperator.ComponentVersion)
		assert.Equal(t, "user.example.com", cfg.NetworkOperator.Repository)
	})
}
