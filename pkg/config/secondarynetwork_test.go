// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecondaryNetworkRenderingPolicy(t *testing.T) {
	for _, tc := range []struct {
		name            string
		policy          *SecondaryNetworkPolicy
		status          string
		flavor          string
		multus, plugins bool
		wantErr         bool
	}{
		{name: "legacy offline", multus: true, plugins: true},
		{name: "Multus only external", status: InstallerExternal, multus: false, plugins: true},
		{name: "operator rerun", status: InstallerOperatorManaged, multus: true, plugins: true},
		{name: "ambiguous auto", status: InstallerAmbiguous, wantErr: true},
		{name: "unavailable auto", status: InstallerUnavailable, wantErr: true},
		{name: "explicit external despite uncertainty", policy: &SecondaryNetworkPolicy{Multus: ManagementExternal, CNIPlugins: ManagementExternal}, status: InstallerUnavailable},
		{name: "mixed policy", policy: &SecondaryNetworkPolicy{Multus: ManagementExternal, CNIPlugins: ManagementManaged}, multus: false, plugins: true},
		{name: "explicit managed", policy: &SecondaryNetworkPolicy{Multus: ManagementManaged}, status: InstallerExternal, multus: true, plugins: true},
		{name: "invalid policy", policy: &SecondaryNetworkPolicy{Multus: "broken"}, wantErr: true},
		{name: "OpenShift external", flavor: FlavorOCP},
		{name: "OpenShift managed rejected", flavor: FlavorOCP, policy: &SecondaryNetworkPolicy{Multus: ManagementManaged}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &LaunchKitConfig{Flavor: tc.flavor, NetworkOperator: &NetworkOperatorConfig{SecondaryNetwork: tc.policy}}
			if tc.status != "" {
				cfg.DiscoveredSecondaryNetwork = &SecondaryNetworkObservation{Multus: ComponentObservation{Status: tc.status}, CNIPlugins: ComponentObservation{Status: InstallerNotDetected}}
			}
			before, err := CloneConfig(cfg)
			require.NoError(t, err)
			got, err := ResolveSecondaryNetworkRendering(cfg)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.multus, got.Multus)
				assert.Equal(t, tc.plugins, got.CNIPlugins)
			}
			assert.Equal(t, before, cfg, "rendering must not replace desired policy")
		})
	}
}

func TestSecondaryNetworkEvidenceRoundTripAndIsolation(t *testing.T) {
	cfg := &LaunchKitConfig{NetworkOperator: &NetworkOperatorConfig{SecondaryNetwork: &SecondaryNetworkPolicy{Multus: ManagementAuto, CNIPlugins: ManagementExternal}}, DiscoveredSecondaryNetwork: &SecondaryNetworkObservation{Multus: ComponentObservation{Status: InstallerUnavailable, Reason: "denied"}, CNIPlugins: ComponentObservation{Status: InstallerAmbiguous, Installers: []InstallerReference{{Name: "installer", UID: "original"}}}}}
	require.NoError(t, ValidateSecondaryNetworkConfig(cfg), "uncertainty must not prevent discovery save")
	root := t.TempDir()
	path, err := WriteEffectiveConfig(root, cfg)
	require.NoError(t, err)
	input, err := LoadEffectiveConfig(path)
	require.NoError(t, err)
	assert.Equal(t, cfg, input)
	copy, err := CloneConfig(cfg)
	require.NoError(t, err)
	copy.DiscoveredSecondaryNetwork.CNIPlugins.Installers[0].UID = "changed"
	copy.NetworkOperator.SecondaryNetwork.Multus = ManagementExternal
	assert.Equal(t, "original", cfg.DiscoveredSecondaryNetwork.CNIPlugins.Installers[0].UID)
	assert.Equal(t, ManagementAuto, cfg.NetworkOperator.SecondaryNetwork.Multus)
}

func TestSecondaryNetworkYAMLPresence(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         ComponentManagement
	}{
		{"omitted", "networkOperator: {}", ManagementAuto},
		{"null", "networkOperator:\n  secondaryNetwork:\n    multus: null", ManagementAuto},
		{"empty", "networkOperator:\n  secondaryNetwork:\n    multus: \"\"", ""},
		{"explicit", "networkOperator:\n  secondaryNetwork:\n    multus: external", ManagementExternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, err := DecodeInput([]byte(tc.source), "")
			require.NoError(t, err)
			defaults, err := DefaultLaunchKitConfig()
			require.NoError(t, err)
			require.NoError(t, MergeDefaults(input.Config, defaults, input.Present))
			require.NoError(t, ValidateSecondaryNetworkConfig(input.Config))
			require.Equal(t, tc.want, input.Config.NetworkOperator.SecondaryNetwork.Multus)
		})
	}
}
