// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package profiles

import (
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestSpectrumXProfileReleaseBoundaries(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	require.NoError(t, err)
	t.Chdir(repoRoot)
	capabilities := &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Sriov: true, Rdma: true, Ib: false}}
	for _, tc := range []struct {
		ra, release, name string
	}{
		{"RA2.3", "26.4", ""},
		{"RA2.3", "26.7", "Spectrum-X Multi-Rail (RA2.3)"},
		{"RA2.3", "26.10", ""},
		{"RA2.4", "26.7", ""},
		{"RA2.4", "26.10", "Spectrum-X Multi-Rail (RA2.4)"},
		{"RA2.4", "26.11", "Spectrum-X Multi-Rail (RA2.4)"},
		{"RA2.4", "27.1", "Spectrum-X Multi-Rail (RA2.4)"},
	} {
		t.Run(tc.ra+"/"+tc.release, func(t *testing.T) {
			requirements := &config.Profile{Fabric: "ethernet", Deployment: "sriov", Multirail: true,
				SpectrumX: &config.ProfileSpectrumX{Enable: true, SPCXVersion: tc.ra, MultiplaneMode: "none", NumberOfPlanes: 1},
			}
			profile, err := FindApplicableProfile(requirements, capabilities, "network-operator", tc.release, config.FlavorK8s)
			if tc.name == "" {
				require.ErrorContains(t, err, "no applicable profile found")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.name, profile.Name)
			for _, path := range profile.Templates {
				require.FileExists(t, path)
			}
		})
	}
}
