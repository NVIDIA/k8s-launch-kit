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
	assert.Equal(t, config.RoutingDestinationBased, cfg.Profile.Routing)
	assert.Len(t, decisions, 3)
}

func TestApplyHardwareDefaultsPreservesConfiguredRouting(t *testing.T) {
	cfg := &config.LaunchKitConfig{
		Profile: &config.Profile{Routing: config.RoutingSourceBased},
	}

	decisions := ApplyHardwareDefaults(cfg, options.Options{})

	assert.Equal(t, config.RoutingSourceBased, cfg.Profile.Routing)
	for _, decision := range decisions {
		assert.NotEqual(t, "--routing", decision.Flag)
	}
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

func TestSpectrumXDefaultsForHardwareUsesPlatformAndNIC(t *testing.T) {
	tests := []struct {
		name        string
		machineType string
		gpuType     string
		deviceID    string
		wantMode    string
		wantN       int
	}{
		{name: "ConnectX-7 fallback", deviceID: "1021", wantMode: "none", wantN: 1},
		{name: "BF3 fallback", deviceID: "a2dc", wantMode: "none", wantN: 1},
		{name: "H100 single plane", gpuType: "NVIDIA-H100-NVL", deviceID: "1023", wantMode: "none", wantN: 1},
		{name: "H200 single plane", gpuType: "NVIDIA-H200", deviceID: "1023", wantMode: "none", wantN: 1},
		{name: "B200 single plane", gpuType: "NVIDIA-B200", deviceID: "1023", wantMode: "none", wantN: 1},
		{name: "GB200 single plane", gpuType: "NVIDIA-GB200", deviceID: "1023", wantMode: "none", wantN: 1},
		{name: "B300 conservative dual plane", gpuType: "NVIDIA-B300", deviceID: "1023", wantMode: "swplb", wantN: 2},
		{name: "GB300 dual plane", gpuType: "NVIDIA-GB300", deviceID: "1023", wantMode: "swplb", wantN: 2},
		{name: "machine type fallback", machineType: "GB300-NVL", deviceID: "0x1023", wantMode: "swplb", wantN: 2},
		{name: "unknown CX8 platform fallback", gpuType: "NVIDIA-GB10", deviceID: "1023", wantMode: "swplb", wantN: 2},
		{name: "ConnectX-9 fallback", deviceID: "1025", wantMode: "hwplb", wantN: 4},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mode, planes, ok, reason := spectrumXDefaultsForHardware([]config.ClusterConfig{
				spectrumXTestGroup("group-a", test.machineType, test.gpuType, test.deviceID),
			})

			require.True(t, ok)
			assert.Equal(t, test.wantMode, mode)
			assert.Equal(t, test.wantN, planes)
			assert.Contains(t, config.SupportedMultiplaneModes, mode)
			assert.NotEmpty(t, reason)
		})
	}
}

func TestSpectrumXDefaultsForHardwareRejectsConflictingPlatforms(t *testing.T) {
	mode, planes, ok, reason := spectrumXDefaultsForHardware([]config.ClusterConfig{
		spectrumXTestGroup("single-plane", "HGX", "NVIDIA-B200", "1023"),
		spectrumXTestGroup("dual-plane", "GB300-NVL", "NVIDIA-GB300", "1023"),
	})

	assert.False(t, ok)
	assert.Empty(t, mode)
	assert.Zero(t, planes)
	assert.Contains(t, reason, "different Spectrum-X defaults")
}

func TestSpectrumXDefaultsForHardwareAcceptsPlatformsWithSameDefault(t *testing.T) {
	mode, planes, ok, reason := spectrumXDefaultsForHardware([]config.ClusterConfig{
		spectrumXTestGroup("b300", "HGX-B300", "NVIDIA-B300", "1023"),
		spectrumXTestGroup("gb300", "GB300-NVL", "NVIDIA-GB300", "1023"),
	})

	require.True(t, ok)
	assert.Equal(t, "swplb", mode)
	assert.Equal(t, 2, planes)
	assert.Contains(t, reason, "share the swplb/2 default")
}

func TestApplyHardwareDefaultsCompletesExplicitSinglePlaneSettings(t *testing.T) {
	t.Run("mode none implies one plane", func(t *testing.T) {
		cfg := &config.LaunchKitConfig{
			Profile: &config.Profile{
				SpectrumX: &config.ProfileSpectrumX{
					Enable:         true,
					MultiplaneMode: "none",
				},
			},
		}

		ApplyHardwareDefaults(cfg, options.Options{})

		assert.Equal(t, 1, cfg.Profile.SpectrumX.NumberOfPlanes)
	})

	t.Run("one plane implies mode none", func(t *testing.T) {
		cfg := &config.LaunchKitConfig{
			Profile: &config.Profile{
				SpectrumX: &config.ProfileSpectrumX{
					Enable:         true,
					NumberOfPlanes: 1,
				},
			},
		}

		ApplyHardwareDefaults(cfg, options.Options{})

		assert.Equal(t, "none", cfg.Profile.SpectrumX.MultiplaneMode)
	})
}

func spectrumXTestGroup(identifier, machineType, gpuType, deviceID string) config.ClusterConfig {
	rail := 0
	return config.ClusterConfig{
		Identifier:      identifier,
		MachineType:     machineType,
		GPUType:         gpuType,
		LinkType:        "Ethernet",
		PresetApplied:   false,
		PresetDeviation: nil,
		Capabilities:    nil,
		PFs: []config.PFConfig{{
			DeviceID:               deviceID,
			RdmaDevice:             "",
			PciAddress:             "0000:01:00.0",
			NetworkInterface:       "",
			Traffic:                "east-west",
			Rail:                   &rail,
			PSID:                   "",
			PartNumber:             "",
			Model:                  "",
			NumaNode:               nil,
			ConnectedGPU:           "",
			ConnectedGPUPCIAddress: "",
			GPUProximity:           "",
		}},
		WorkerNodes:           nil,
		NodeSelector:          nil,
		ThirdPartyRDMAModules: nil,
		StorageModules:        nil,
		RailPciAddresses:      nil,
		MergedIdentifier:      "",
		SourceMachineLabels:   nil,
	}
}
