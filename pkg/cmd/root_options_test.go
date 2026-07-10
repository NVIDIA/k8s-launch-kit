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
	"github.com/nvidia/k8s-launch-kit/pkg/resolve"
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
				SPCXVersion:    "RA2.2",
				MultiplaneMode: []string{"swplb", "hwplb", "uniplane", "none"},
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

// resolveProfile simulates the full Unit-8 CLI→config→validate chain:
// 1. Phase 1 syntax checks on CLI options.
// 2. Build cfg with config profile (or empty when no source).
// 3. ApplyHardwareDefaults — fills empty profile fields from cluster
//    hardware (always-on defaults + Spectrum-X-implied defaults).
// 4. ApplyOptionsToConfig — CLI overlay; non-zero CLI values override
//    HW defaults; bool override gated by MultirailSet.
// 5. Phase 2 cohort validation.
// 6. Validate against the target profile definition.
func resolveProfile(
	t *testing.T,
	opts options.Options,
	configProfile *config.Profile,
	profileDef profiles.Profile,
	capabilities *config.ClusterCapabilities,
) (bool, string) {
	t.Helper()

	// Step 1: Phase 1 syntax checks
	err := applySpectrumXSyntaxChecks(&opts)
	require.NoError(t, err)

	plugin := &networkoperatorplugin.NetworkOperatorPlugin{}

	// Step 2: Build cfg with config-file profile
	fullConfig := &config.LaunchKitConfig{}
	if configProfile != nil {
		profileCopy := *configProfile
		if configProfile.SpectrumX != nil {
			sxCopy := *configProfile.SpectrumX
			profileCopy.SpectrumX = &sxCopy
		}
		fullConfig.Profile = &profileCopy
	} else {
		fullConfig.Profile = &config.Profile{}
	}

	// Step 3: Hardware defaults fill empty profile fields.
	resolve.ApplyHardwareDefaults(fullConfig, opts)

	// Step 4: Apply CLI overrides on top of config + hardware defaults.
	err = plugin.ApplyOptionsToConfig(opts, fullConfig)
	require.NoError(t, err)

	// Step 5: Phase 2 cohort validation.
	if err := resolve.ValidateResolvedConfig(fullConfig); err != nil {
		return false, err.Error()
	}

	// Step 4: Validate against the target profile definition
	selectedRelease := ""
	if fullConfig.NetworkOperator != nil {
		selectedRelease = fullConfig.NetworkOperator.SelectedRelease
	}
	return profileDef.Validate(fullConfig.Profile, capabilities, selectedRelease)
}

func TestCLIOnlyProfileResolution(t *testing.T) {
	t.Run("spectrum-x hwplb matches spectrum-x profile", func(t *testing.T) {
		opts := options.Options{
			SpectrumX:              true,
			SPCXVersion:            "RA2.2",
			MultiplaneMode:         "hwplb",
			NumberOfPlanes:         2,
			NetworkOperatorRelease: "26.4",
		}
		valid, reason := resolveProfile(t, opts, nil, spectrumXProfile, defaultCapabilities)
		assert.True(t, valid, "should match spectrum-x profile; reason: %s", reason)
	})

	t.Run("spectrum-x swplb matches merged spectrum-x profile", func(t *testing.T) {
		opts := options.Options{
			SpectrumX:              true,
			SPCXVersion:            "RA2.2",
			MultiplaneMode:         "swplb",
			NumberOfPlanes:         4,
			NetworkOperatorRelease: "26.4",
		}
		valid, reason := resolveProfile(t, opts, nil, spectrumXProfile, defaultCapabilities)
		assert.True(t, valid, "should match spectrum-x profile (now covers swplb); reason: %s", reason)
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

	t.Run("spectrum-x without version is rejected at CLI level", func(t *testing.T) {
		// applySpectrumXDefaults now rejects an empty SPCXVersion outright,
		// before the matcher ever runs — the user's mistake gets a specific
		// error rather than an ambiguous "no applicable profile found".
		opts := options.Options{
			SpectrumX:      true,
			MultiplaneMode: "hwplb",
			NumberOfPlanes: 2,
			// SPCXVersion intentionally empty
		}
		err := applySpectrumXDefaults(&opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--spectrum-x requires the SPC-X RA version")
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
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "hwplb",
				NumberOfPlanes: 2,
			},
		}
		valid, reason := resolveProfile(t, options.Options{}, cfgProfile, spectrumXProfile, defaultCapabilities)
		assert.True(t, valid, "should match spectrum-x profile; reason: %s", reason)
	})

	t.Run("config with full spectrum-x swplb matches merged spectrum-x profile", func(t *testing.T) {
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "swplb",
				NumberOfPlanes: 4,
			},
		}
		valid, reason := resolveProfile(t, options.Options{}, cfgProfile, spectrumXProfile, defaultCapabilities)
		assert.True(t, valid, "should match spectrum-x profile (now covers swplb); reason: %s", reason)
	})

	t.Run("config with basic sriov ethernet matches sriov-ethernet-rdma", func(t *testing.T) {
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
		}
		valid, reason := resolveProfile(t, options.Options{}, cfgProfile, sriovEthernetProfile, defaultCapabilities)
		assert.True(t, valid, "should match sriov-ethernet-rdma; reason: %s", reason)
	})

	t.Run("config with explicit CLI --multirail=false rejects multirail-required profile", func(t *testing.T) {
		// CLI presence is tracked independently from YAML presence, so an
		// explicit `--multirail=false` prevents the true default from firing.
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  false,
		}
		opts := options.Options{
			Multirail:    false,
			MultirailSet: true,
		}
		valid, reason := resolveProfile(t, opts, cfgProfile, spectrumXProfile, defaultCapabilities)
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
			SpectrumX:              true, // CLI adds spectrum-x → multirail becomes true
			SPCXVersion:            "RA2.2",
			MultiplaneMode:         "hwplb",
			NumberOfPlanes:         2,
			NetworkOperatorRelease: "26.4",
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
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "hwplb",
				NumberOfPlanes: 2,
			},
		}
		// applySpectrumXDefaults requires the full cohort whenever --spectrum-x
		// is set on the CLI, even when overriding only one knob — that's the
		// price for catching typos like `--spectrum-x 2.1` early. Tests that
		// override a single flag must restate the rest of the cohort.
		opts := options.Options{
			SpectrumX:              true,
			SPCXVersion:            "RA2.2",
			MultiplaneMode:         "swplb", // CLI overrides mode
			NumberOfPlanes:         2,
			NetworkOperatorRelease: "26.4",
		}
		valid, reason := resolveProfile(t, opts, cfgProfile, spectrumXProfile, defaultCapabilities)
		assert.True(t, valid, "CLI --multiplane-mode swplb stays in merged profile; reason: %s", reason)
	})

	t.Run("config spectrum-x 2 planes + CLI overrides to 4 planes", func(t *testing.T) {
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "hwplb",
				NumberOfPlanes: 2,
			},
		}
		opts := options.Options{
			SpectrumX:              true,
			SPCXVersion:            "RA2.2",
			MultiplaneMode:         "hwplb",
			NumberOfPlanes:         4, // CLI overrides planes
			NetworkOperatorRelease: "26.4",
		}

		// Run the merge manually to check the final value
		err := applySpectrumXDefaults(&opts)
		require.NoError(t, err)

		plugin := &networkoperatorplugin.NetworkOperatorPlugin{}
		profileCopy := *cfgProfile
		sxCopy := *cfgProfile.SpectrumX
		profileCopy.SpectrumX = &sxCopy
		fullConfig := &config.LaunchKitConfig{Profile: &profileCopy}

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
				SPCXVersion:    "RA2.2",
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

	t.Run("spectrum-x wrong version is rejected at CLI level", func(t *testing.T) {
		// Bogus version values like "unsupported" or "2.1" are caught by
		// applySpectrumXDefaults's value validation against
		// config.SupportedSPCXVersions, before the matcher runs.
		opts := options.Options{
			SpectrumX:      true,
			SPCXVersion:    "unsupported", // not in the supported set
			MultiplaneMode: "hwplb",
			NumberOfPlanes: 2,
		}
		err := applySpectrumXDefaults(&opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid --spectrum-x value")
	})

	t.Run("explicit CLI --multirail=false fails multirail-required profile", func(t *testing.T) {
		// `--multirail=false` sets `MultirailSet`, so it remains an explicit
		// override rather than being replaced by the true default.
		cfgProfile := &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  false,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "hwplb",
				NumberOfPlanes: 2,
			},
		}
		opts := options.Options{
			Multirail:              false,
			MultirailSet:           true,
			NetworkOperatorRelease: "26.4",
		}
		valid, reason := resolveProfile(t, opts, cfgProfile, spectrumXProfile, defaultCapabilities)
		assert.False(t, valid)
		assert.Contains(t, reason, "multirail")
	})
}

func TestImagePullSecretsOptionFlow(t *testing.T) {
	plugin := &networkoperatorplugin.NetworkOperatorPlugin{}

	t.Run("CLI image-pull-secrets flows through to config", func(t *testing.T) {
		fullConfig := &config.LaunchKitConfig{
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
		fullConfig := &config.LaunchKitConfig{
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
