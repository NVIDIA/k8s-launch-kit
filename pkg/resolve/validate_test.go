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

package resolve

import (
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestEmbeddedDefaultIsResolvedConfigValid(t *testing.T) {
	cfg, err := config.DefaultLaunchKitConfig()
	require.NoError(t, err)
	require.NoError(t, ValidateResolvedConfig(cfg))
}

func TestValidateResolvedConfigAllowsEmptyRoutingWithoutDefaulting(t *testing.T) {
	cfg := &config.LaunchKitConfig{Profile: &config.Profile{}}

	require.NoError(t, ValidateResolvedConfig(cfg))
	require.Empty(t, cfg.Profile.Routing)
}

func TestValidateResolvedConfigDoesNotPersistDefaultIPVersion(t *testing.T) {
	cfg := &config.LaunchKitConfig{
		NetworkOperator: &config.NetworkOperatorConfig{SelectedRelease: "26.4"},
		Profile: &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "hwplb",
				NumberOfPlanes: 2,
				TopologyType:   config.SpectrumXTopology2Tier,
			},
		},
	}

	require.NoError(t, ValidateResolvedConfig(cfg))
	require.Empty(t, cfg.Profile.SpectrumX.IPVersion)
}

func TestValidateResolvedConfigRejectsInvalidRouting(t *testing.T) {
	cfg := &config.LaunchKitConfig{
		Profile: &config.Profile{Routing: "gateway-based"},
	}

	err := ValidateResolvedConfig(cfg)
	require.ErrorContains(t, err, "profile.routing must be one of")
}

func TestValidateResolvedConfigRejectsRoutingForSpectrumX(t *testing.T) {
	cfg := &config.LaunchKitConfig{
		NetworkOperator: &config.NetworkOperatorConfig{SelectedRelease: "26.4"},
		Profile: &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			Routing:    config.RoutingSourceBased,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "hwplb",
				NumberOfPlanes: 4,
			},
		},
	}

	err := ValidateResolvedConfig(cfg)
	require.ErrorContains(t, err, "--routing does not apply to Spectrum-X profiles")
}

func TestValidateResolvedConfigRejectsIgnoreARPForSpectrumX(t *testing.T) {
	cfg := &config.LaunchKitConfig{
		NetworkOperator: &config.NetworkOperatorConfig{SelectedRelease: "26.4"},
		Profile: &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			Routing:    config.RoutingDestinationBased,
			IgnoreARP:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "hwplb",
				NumberOfPlanes: 4,
			},
		},
	}

	err := ValidateResolvedConfig(cfg)
	require.ErrorContains(t, err, "--ignore-arp does not apply to Spectrum-X profiles")
}

func TestValidateResolvedConfigRequiresConfigMapProfileForRA23(t *testing.T) {
	cfg := &config.LaunchKitConfig{
		NetworkOperator: &config.NetworkOperatorConfig{SelectedRelease: "26.7"},
		Profile: &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.3",
				MultiplaneMode: "hwplb",
				NumberOfPlanes: 4,
			},
		},
	}

	err := ValidateResolvedConfig(cfg)
	require.ErrorContains(t, err, "requires a Spectrum-X profile ConfigMap input")
}

func TestValidateResolvedConfigAcceptsConfigMapProfileForRA23(t *testing.T) {
	cfg := &config.LaunchKitConfig{
		NetworkOperator: &config.NetworkOperatorConfig{SelectedRelease: "26.7"},
		Profile: &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.3",
				MultiplaneMode: "hwplb",
				NumberOfPlanes: 4,
				TopologyType:   config.SpectrumXTopology2Tier,
				ConfigMapName:  "site-ra23",
				Profile:        "useSoftwareCCAlgorithm: true\n",
			},
		},
	}

	require.NoError(t, ValidateResolvedConfig(cfg))
}

func TestValidateResolvedConfigRejectsMultiplaneModeWithOnePlane(t *testing.T) {
	cfg := &config.LaunchKitConfig{
		NetworkOperator: &config.NetworkOperatorConfig{SelectedRelease: "26.4"},
		Profile: &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "swplb",
				NumberOfPlanes: 1,
				TopologyType:   config.SpectrumXTopology2Tier,
			},
		},
	}

	err := ValidateResolvedConfig(cfg)
	require.ErrorContains(t, err, "--multiplane-mode swplb requires --number-of-planes 2 or 4")
}
