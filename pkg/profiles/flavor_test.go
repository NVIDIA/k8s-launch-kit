// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package profiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestProfileSelectionSeparatesOpenShiftAndKubernetes(t *testing.T) {
	old, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(filepath.Join(old, "../..")))
	t.Cleanup(func() { _ = os.Chdir(old) })

	requirements := &config.Profile{Fabric: "ethernet", Deployment: "sriov"}
	capabilities := &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Sriov: true, Rdma: true}}
	for _, flavor := range []string{config.FlavorK8s, config.FlavorOCP} {
		profile, err := FindApplicableProfile(requirements, capabilities, "network-operator", "26.7", flavor)
		require.NoError(t, err)
		if flavor == config.FlavorOCP {
			require.Equal(t, config.FlavorOCP, profile.ProfileRequirements.Flavor)
			require.Contains(t, profile.Name, "OpenShift")
		} else {
			require.Empty(t, profile.ProfileRequirements.Flavor)
		}
		for _, manifest := range profile.Templates {
			require.FileExists(t, manifest)
		}
	}

	requirements.SpectrumX = &config.ProfileSpectrumX{Enable: true, SPCXVersion: "RA2.2"}
	_, err = FindApplicableProfile(requirements, capabilities, "network-operator", "26.7", config.FlavorOCP)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "no applicable profile found"))
}

func TestProfileSelectionChecksInfiniBandCapability(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	require.NoError(t, err)
	t.Chdir(repoRoot)

	for _, tc := range []struct {
		name      string
		fabric    string
		ib        bool
		wantError bool
	}{
		{name: "rejects InfiniBand without the capability", fabric: "infiniband", ib: false, wantError: true},
		{name: "accepts InfiniBand with the capability", fabric: "infiniband", ib: true},
		{name: "keeps Ethernet available without the capability", fabric: "ethernet", ib: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := &config.Profile{Fabric: tc.fabric, Deployment: "host_device"}
			capabilities := &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Rdma: true, Ib: tc.ib}}

			profile, err := FindApplicableProfile(requirements, capabilities, "network-operator", "", config.FlavorK8s)
			if tc.wantError {
				require.ErrorContains(t, err, "no applicable profile found")
				require.ErrorContains(t, err, "selected InfiniBand fabric requires cluster ib capability to be true")
				return
			}

			require.NoError(t, err)
			require.Equal(t, "Host device RDMA", profile.Name)
		})
	}
}
