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

package cmd

import (
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// boolPtr creates a pointer to a bool value.
func boolPtr(b bool) *bool { return &b }

// Profile definitions mirroring the real profile.yaml files.
var (
	spectrumXProfile = profiles.Profile{
		Name:   "Spectrum-X Multi-Rail",
		Plugin: "network-operator",
		ProfileRequirements: profiles.ProfileRequirements{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  boolPtr(true),
			SpectrumX: &profiles.ProfileRequirementsSpectrumX{
				SPCXVersion:    "RA2.1",
				MultiplaneMode: []string{"hwplb", "uniplane", "none"},
			},
		},
		NodeCapabilities: profiles.NodeCapabilities{
			Sriov: boolPtr(true),
			Rdma:  boolPtr(true),
		},
	}

	spectrumXSwplbProfile = profiles.Profile{
		Name:   "Spectrum-X Multi-Rail SWPLB",
		Plugin: "network-operator",
		ProfileRequirements: profiles.ProfileRequirements{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  boolPtr(true),
			SpectrumX: &profiles.ProfileRequirementsSpectrumX{
				SPCXVersion:    "RA2.1",
				MultiplaneMode: []string{"swplb"},
			},
		},
		NodeCapabilities: profiles.NodeCapabilities{
			Sriov: boolPtr(true),
			Rdma:  boolPtr(true),
		},
	}

	sriovEthernetProfile = profiles.Profile{
		Name:   "SR-IOV Ethernet RDMA",
		Plugin: "network-operator",
		ProfileRequirements: profiles.ProfileRequirements{
			Fabric:     "ethernet",
			Deployment: "sriov",
		},
		NodeCapabilities: profiles.NodeCapabilities{
			Rdma: boolPtr(true),
		},
	}

	sriovIBProfile = profiles.Profile{
		Name:   "SR-IOV Infiniband RDMA",
		Plugin: "network-operator",
		ProfileRequirements: profiles.ProfileRequirements{
			Fabric:     "infiniband",
			Deployment: "sriov",
		},
		NodeCapabilities: profiles.NodeCapabilities{
			Ib:   boolPtr(true),
			Rdma: boolPtr(true),
		},
	}

	defaultCapabilities = &config.ClusterCapabilities{
		Nodes: &config.NodesCapabilities{
			Sriov: true,
			Rdma:  true,
			Ib:    false,
		},
	}

	ibCapabilities = &config.ClusterCapabilities{
		Nodes: &config.NodesCapabilities{
			Sriov: true,
			Rdma:  true,
			Ib:    true,
		},
	}
)

// resolveProfile simulates the full CLI→config→validate chain:
// 1. applySpectrumXDefaults on CLI options
// 2. Build or use config profile
// 3. ApplyOptionsToConfig to merge CLI overrides
// 4. Validate against a target profile definition
func resolveProfile(
	t *testing.T,
	opts options.Options,
	configProfile *config.Profile,
	profileDef profiles.Profile,
	capabilities *config.ClusterCapabilities,
) (bool, string) {
	t.Helper()

	// Step 1: Apply spectrum-x defaults to CLI options
	err := applySpectrumXDefaults(&opts)
	require.NoError(t, err)

	plugin := &networkoperatorplugin.NetworkOperatorPlugin{}

	// Step 2: Build profile from config or CLI
	fullConfig := &config.LaunchKubernetesConfig{}
	if configProfile != nil {
		// Simulate: config file had a profile section
		profileCopy := *configProfile
		if configProfile.SpectrumX != nil {
			sxCopy := *configProfile.SpectrumX
			profileCopy.SpectrumX = &sxCopy
		}
		fullConfig.Profile = &profileCopy
	} else if plugin.ProfileConfiguredInCmd(opts) {
		// Simulate: no config profile, build from CLI flags
		fullConfig.Profile = &config.Profile{}
		err := plugin.BuildProfileFromOptions(opts, fullConfig.Profile)
		require.NoError(t, err)
	} else {
		// No profile source at all — validate with empty profile
		fullConfig.Profile = &config.Profile{}
	}

	// Step 3: Apply CLI overrides on top of config
	err = plugin.ApplyOptionsToConfig(opts, fullConfig)
	require.NoError(t, err)

	// Step 4: Validate against the target profile definition
	return profileDef.Validate(fullConfig.Profile, capabilities)
}

func TestCLIOnlyProfileResolution(t *testing.T) {
	t.Run("spectrum-x hwplb matches spectrum-x profile", func(t *testing.T) {
		opts := options.Options{
			SpectrumX:      true,
			SPCXVersion:    "RA2.1",
			MultiplaneMode: "hwplb",
			NumberOfPlanes: 2,
		}
		valid, reason := resolveProfile(t, opts, nil, spectrumXProfile, defaultCapabilities)
		assert.True(t, valid, "should match spectrum-x profile; reason: %s", reason)
	})

	t.Run("spectrum-x swplb matches spectrum-x-swplb profile", func(t *testing.T) {
		opts := options.Options{
			SpectrumX:      true,
			SPCXVersion:    "RA2.1",
			MultiplaneMode: "swplb",
			NumberOfPlanes: 4,
		}
		valid, reason := resolveProfile(t, opts, nil, spectrumXSwplbProfile, defaultCapabilities)
		assert.True(t, valid, "should match spectrum-x-swplb profile; reason: %s", reason)
	})

	t.Run("spectrum-x swplb does NOT match non-swplb spectrum-x profile", func(t *testing.T) {
		opts := options.Options{
			SpectrumX:      true,
			SPCXVersion:    "RA2.1",
			MultiplaneMode: "swplb",
			NumberOfPlanes: 4,
		}
		valid, reason := resolveProfile(t, opts, nil, spectrumXProfile, defaultCapabilities)
		assert.False(t, valid)
		assert.Contains(t, reason, "multiplane mode swplb not in profile's allowed modes")
	})

	t.Run("ethernet sriov matches sriov-ethernet-rdma", func(t *testing.T) {
		opts := options.Options{
			Fabric:         "ethernet",
			DeploymentType: "sriov",
		}
		valid, reason := resolveProfile(t, opts, nil, sriovEthernetProfile, defaultCapabilities)
		assert.True(t, valid, "should match sriov-ethernet-rdma; reason: %s", reason)
	})

	t.Run("infiniband sriov matches sriov-ib-rdma", func(t *testing.T) {
		opts := options.Options{
			Fabric:         "infiniband",
			DeploymentType: "sriov",
		}
		valid, reason := resolveProfile(t, opts, nil, sriovIBProfile, ibCapabilities)
		assert.True(t, valid, "should match sriov-ib-rdma; reason: %s", reason)
	})

	t.Run("spectrum-x without version fails RA2.1 profile", func(t *testing.T) {
		opts := options.Options{
			SpectrumX:      true,
			MultiplaneMode: "hwplb",
			NumberOfPlanes: 2,
			// SPCXVersion intentionally empty
		}
		valid, reason := resolveProfile(t, opts, nil, spectrumXProfile, defaultCapabilities)
		assert.False(t, valid)
		assert.Contains(t, reason, "SPCX version")
	})
}

func TestConfigOnlyProfileResolution(t *testing.T) {
	t.Run("config with full spectrum-x hwplb matches spectrum-x profile", func(t *testing.T) {
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.1",
				MultiplaneMode: "hwplb",
				NumberOfPlanes: 2,
			},
		}
		valid, reason := resolveProfile(t, options.Options{}, cfgProfile, spectrumXProfile, defaultCapabilities)
		assert.True(t, valid, "should match spectrum-x profile; reason: %s", reason)
	})

	t.Run("config with full spectrum-x swplb matches spectrum-x-swplb profile", func(t *testing.T) {
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.1",
				MultiplaneMode: "swplb",
				NumberOfPlanes: 4,
			},
		}
		valid, reason := resolveProfile(t, options.Options{}, cfgProfile, spectrumXSwplbProfile, defaultCapabilities)
		assert.True(t, valid, "should match spectrum-x-swplb profile; reason: %s", reason)
	})

	t.Run("config with basic sriov ethernet matches sriov-ethernet-rdma", func(t *testing.T) {
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
		}
		valid, reason := resolveProfile(t, options.Options{}, cfgProfile, sriovEthernetProfile, defaultCapabilities)
		assert.True(t, valid, "should match sriov-ethernet-rdma; reason: %s", reason)
	})

	t.Run("config with multirail false does not match multirail-required profile", func(t *testing.T) {
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  false,
		}
		valid, reason := resolveProfile(t, options.Options{}, cfgProfile, spectrumXProfile, defaultCapabilities)
		assert.False(t, valid)
		assert.Contains(t, reason, "multirail")
	})
}

func TestMixedCLIConfigProfileResolution(t *testing.T) {
	t.Run("config base + CLI spectrum-x overrides to spectrum-x profile", func(t *testing.T) {
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  false, // config says false
		}
		opts := options.Options{
			SpectrumX:      true,       // CLI adds spectrum-x → multirail becomes true
			SPCXVersion:    "RA2.1",
			MultiplaneMode: "hwplb",
			NumberOfPlanes: 2,
		}
		// applySpectrumXDefaults sets Multirail=true, then ApplyOptionsToConfig applies it
		valid, reason := resolveProfile(t, opts, cfgProfile, spectrumXProfile, defaultCapabilities)
		assert.True(t, valid, "CLI spectrum-x should override config multirail:false; reason: %s", reason)
	})

	t.Run("config spectrum-x hwplb + CLI overrides multiplane to swplb", func(t *testing.T) {
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.1",
				MultiplaneMode: "hwplb",
				NumberOfPlanes: 2,
			},
		}
		opts := options.Options{
			SpectrumX:      true,
			MultiplaneMode: "swplb", // CLI overrides mode
		}
		valid, reason := resolveProfile(t, opts, cfgProfile, spectrumXSwplbProfile, defaultCapabilities)
		assert.True(t, valid, "CLI --multiplane-mode swplb should switch to swplb profile; reason: %s", reason)
	})

	t.Run("config spectrum-x 2 planes + CLI overrides to 4 planes", func(t *testing.T) {
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.1",
				MultiplaneMode: "hwplb",
				NumberOfPlanes: 2,
			},
		}
		opts := options.Options{
			SpectrumX:      true,
			NumberOfPlanes: 4, // CLI overrides planes
		}

		// Run the merge manually to check the final value
		err := applySpectrumXDefaults(&opts)
		require.NoError(t, err)

		plugin := &networkoperatorplugin.NetworkOperatorPlugin{}
		profileCopy := *cfgProfile
		sxCopy := *cfgProfile.SpectrumX
		profileCopy.SpectrumX = &sxCopy
		fullConfig := &config.LaunchKubernetesConfig{Profile: &profileCopy}

		err = plugin.ApplyOptionsToConfig(opts, fullConfig)
		require.NoError(t, err)
		assert.Equal(t, 4, fullConfig.Profile.SpectrumX.NumberOfPlanes, "CLI should override planes to 4")
	})

	t.Run("config has full profile + empty CLI preserves config", func(t *testing.T) {
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.1",
				MultiplaneMode: "hwplb",
				NumberOfPlanes: 2,
			},
		}
		valid, reason := resolveProfile(t, options.Options{}, cfgProfile, spectrumXProfile, defaultCapabilities)
		assert.True(t, valid, "empty CLI should preserve config; reason: %s", reason)
	})

	t.Run("config ethernet + CLI overrides to infiniband", func(t *testing.T) {
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
		}
		opts := options.Options{
			Fabric: "infiniband", // CLI overrides fabric
		}
		valid, reason := resolveProfile(t, opts, cfgProfile, sriovIBProfile, ibCapabilities)
		assert.True(t, valid, "CLI --fabric infiniband should override config; reason: %s", reason)
	})
}

func TestMissingInvalidParams(t *testing.T) {
	t.Run("missing fabric and deployment fails profile match", func(t *testing.T) {
		opts := options.Options{} // no flags at all
		valid, reason := resolveProfile(t, opts, nil, sriovEthernetProfile, defaultCapabilities)
		assert.False(t, valid)
		assert.Contains(t, reason, "fabric")
	})

	t.Run("spectrum-x wrong version fails", func(t *testing.T) {
		opts := options.Options{
			SpectrumX:      true,
			SPCXVersion:    "RA2.0", // wrong version
			MultiplaneMode: "hwplb",
			NumberOfPlanes: 2,
		}
		valid, reason := resolveProfile(t, opts, nil, spectrumXProfile, defaultCapabilities)
		assert.False(t, valid)
		assert.Contains(t, reason, "SPCX version")
	})

	t.Run("spectrum-x swplb rejected by non-swplb profile", func(t *testing.T) {
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.1",
				MultiplaneMode: "swplb",
				NumberOfPlanes: 4,
			},
		}
		valid, reason := resolveProfile(t, options.Options{}, cfgProfile, spectrumXProfile, defaultCapabilities)
		assert.False(t, valid)
		assert.Contains(t, reason, "multiplane mode swplb not in profile's allowed modes")
	})

	t.Run("config multirail false fails multirail-required profile", func(t *testing.T) {
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  false,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.1",
				MultiplaneMode: "hwplb",
				NumberOfPlanes: 2,
			},
		}
		// No CLI flags to override multirail
		valid, reason := resolveProfile(t, options.Options{}, cfgProfile, spectrumXProfile, defaultCapabilities)
		assert.False(t, valid)
		assert.Contains(t, reason, "multirail")
	})
}

func TestImagePullSecretsOptionFlow(t *testing.T) {
	plugin := &networkoperatorplugin.NetworkOperatorPlugin{}

	t.Run("CLI image-pull-secrets flows through to config", func(t *testing.T) {
		fullConfig := &config.LaunchKubernetesConfig{
			NetworkOperator: &config.NetworkOperatorConfig{
				Repository: "nvcr.io/nvidia/mellanox",
			},
			Profile: &config.Profile{
				Fabric:     "ethernet",
				Deployment: "sriov",
			},
		}
		opts := options.Options{
			ImagePullSecrets: []string{"registry-secret", "other-secret"},
		}
		err := plugin.ApplyOptionsToConfig(opts, fullConfig)
		require.NoError(t, err)
		assert.Equal(t, []string{"registry-secret", "other-secret"}, fullConfig.NetworkOperator.ImagePullSecrets)
	})

	t.Run("empty CLI preserves config image-pull-secrets", func(t *testing.T) {
		fullConfig := &config.LaunchKubernetesConfig{
			NetworkOperator: &config.NetworkOperatorConfig{
				ImagePullSecrets: []string{"existing-secret"},
			},
			Profile: &config.Profile{
				Fabric:     "ethernet",
				Deployment: "sriov",
			},
		}
		err := plugin.ApplyOptionsToConfig(options.Options{}, fullConfig)
		require.NoError(t, err)
		assert.Equal(t, []string{"existing-secret"}, fullConfig.NetworkOperator.ImagePullSecrets)
	})
}
