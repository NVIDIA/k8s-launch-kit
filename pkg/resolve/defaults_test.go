// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyHardwareDefaultsRespectsExplicitMultirailFalse(t *testing.T) {
	cfg := &config.LaunchKitConfig{
		Profile: &config.Profile{
			Multirail:    false,
			MultirailSet: true,
		},
	}

	decisions := ApplyHardwareDefaults(cfg, options.Options{})

	assert.False(t, cfg.Profile.Multirail)
	assert.True(t, cfg.Profile.MultirailSet)
	for _, decision := range decisions {
		assert.NotEqual(t, "--multirail", decision.Flag)
	}
}

func TestApplyHardwareDefaultsFillsMissingProfileSettings(t *testing.T) {
	cfg := &config.LaunchKitConfig{
		Profile: &config.Profile{Deployment: "host_device"},
		ClusterConfig: []config.ClusterConfig{
			{Identifier: "group-a", LinkType: "Ethernet"},
			{Identifier: "group-b", LinkType: "Ethernet"},
		},
	}

	decisions := ApplyHardwareDefaults(cfg, options.Options{})

	require.NotNil(t, cfg.Profile)
	assert.Equal(t, "ethernet", cfg.Profile.Fabric)
	assert.Equal(t, "host_device", cfg.Profile.Deployment)
	assert.True(t, cfg.Profile.Multirail)
	assert.True(t, cfg.Profile.MultirailSet)
	assert.Len(t, decisions, 2)
}

func TestApplyHardwareDefaultsHonorsExplicitCLIMultirailFalse(t *testing.T) {
	cfg := &config.LaunchKitConfig{Profile: &config.Profile{}}

	ApplyHardwareDefaults(cfg, options.Options{Multirail: false, MultirailSet: true})

	assert.False(t, cfg.Profile.Multirail)
	assert.False(t, cfg.Profile.MultirailSet,
		"the CLI applier records presence after the defaulting phase")
}

func TestValidateResolvedConfigRejectsSpectrumXWithExplicitMultirailFalse(t *testing.T) {
	cfg := &config.LaunchKitConfig{
		NetworkOperator: &config.NetworkOperatorConfig{SelectedRelease: "26.4"},
		Profile: &config.Profile{
			Fabric:       "ethernet",
			Deployment:   "sriov",
			Multirail:    false,
			MultirailSet: true,
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.2",
				MultiplaneMode: "swplb",
				NumberOfPlanes: 2,
			},
		},
	}

	err := ValidateResolvedConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multirail=true")
}

func TestApplyHardwareDefaultsUsesCLISpectrumXVersionForDependentRelease(t *testing.T) {
	cfg := &config.LaunchKitConfig{
		NetworkOperator: &config.NetworkOperatorConfig{},
		Profile: &config.Profile{
			SpectrumX: &config.ProfileSpectrumX{
				Enable:         true,
				SPCXVersion:    "RA2.1",
				MultiplaneMode: "swplb",
				NumberOfPlanes: 2,
			},
		},
	}

	ApplyHardwareDefaults(cfg, options.Options{
		SpectrumX:   true,
		SPCXVersion: "RA2.2",
	})

	assert.Equal(t, "RA2.1", cfg.Profile.SpectrumX.SPCXVersion,
		"the defaults phase must not apply CLI overrides")
	assert.Equal(t, "26.4", cfg.NetworkOperator.SelectedRelease)
}

func TestApplyHardwareDefaultsRecordsSpectrumXMultirailReason(t *testing.T) {
	cfg := &config.LaunchKitConfig{Profile: &config.Profile{}}

	decisions := ApplyHardwareDefaults(cfg, options.Options{SpectrumX: true})

	var multirailDecisions []DefaultDecision
	for _, decision := range decisions {
		if decision.Flag == "--multirail" {
			multirailDecisions = append(multirailDecisions, decision)
		}
	}
	require.Len(t, multirailDecisions, 1)
	assert.Equal(t, "implied by --spectrum-x", multirailDecisions[0].Reason)
}
