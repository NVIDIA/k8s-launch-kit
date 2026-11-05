// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSpectrumXDesiredNamesFollowModeAndPlatform(t *testing.T) {
	for _, tc := range []struct {
		name, mode, machine, rdma, netdev string
	}{
		{"single plane", "none", "machine-a", "roce_r%rail_id%", "eth_r%rail_id%"},
		{"software PLB", "swplb", "machine-a", "roce_r%rail_id%_p%plane_id%", "eth_r%rail_id%_p%plane_id%"},
		{"hardware PLB", "hwplb", "machine-a", "roce_r%rail_id%", "eth_r%rail_id%_p%plane_id%"},
		{"VR hardware PLB", "hwplb", "machine-VR", "roce_r%rail_id%_n%nic_id%", "eth_r%rail_id%_p%plane_id%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &LaunchKitConfig{
				Profile: &Profile{SpectrumX: &ProfileSpectrumX{Enable: true, MultiplaneMode: tc.mode}},
				ClusterConfig: []ClusterConfig{{MachineType: tc.machine,
					PFs: []PFConfig{{NetworkInterface: "observed-net", RdmaDevice: "observed-rdma"}}}},
			}
			PopulateSpectrumXInterfaceNames(cfg)
			require.Equal(t, &SpectrumXInterfaceNamePrefixConfig{RdmaPrefix: tc.rdma, NetdevPrefix: tc.netdev}, cfg.ClusterConfig[0].SpectrumX.InterfaceNames)
			require.Equal(t, "observed-net", cfg.ClusterConfig[0].PFs[0].NetworkInterface)
			require.Equal(t, "observed-rdma", cfg.ClusterConfig[0].PFs[0].RdmaDevice)
		})
	}
}

func TestSpectrumXDesiredNamesRefreshPerGroupAndPreserveOverrides(t *testing.T) {
	cfg := &LaunchKitConfig{
		Profile:       &Profile{SpectrumX: &ProfileSpectrumX{Enable: true, MultiplaneMode: "hwplb"}},
		ClusterConfig: []ClusterConfig{{MachineType: "machine-VR"}, {GPUType: "NVIDIA-GB300"}},
	}
	PopulateSpectrumXInterfaceNames(cfg)
	require.Equal(t, "roce_r%rail_id%_n%nic_id%", cfg.ClusterConfig[0].SpectrumX.InterfaceNames.RdmaPrefix)
	require.Equal(t, "roce_r%rail_id%", cfg.ClusterConfig[1].SpectrumX.InterfaceNames.RdmaPrefix)
	cfg.ClusterConfig[0].SpectrumX.InterfaceNames.NetdevPrefix = "stale"
	require.NotEqual(t, "stale", cfg.ClusterConfig[1].SpectrumX.InterfaceNames.NetdevPrefix)
	cfg.SpectrumX = &SpectrumXConfig{HWPLB: &SpectrumXInterfaceNamePrefixConfig{
		NetdevPrefix: "net_r%rail_id%_p%plane_id%", RdmaPrefix: "rdma_r%rail_id%_n%nic_id%",
	}}
	PopulateSpectrumXInterfaceNames(cfg)
	for _, group := range cfg.ClusterConfig {
		require.Equal(t, cfg.SpectrumX.HWPLB, group.SpectrumX.InterfaceNames)
	}
	cfg.Profile.SpectrumX.MultiplaneMode = "swplb"
	PopulateSpectrumXInterfaceNames(cfg)
	for _, group := range cfg.ClusterConfig {
		require.Equal(t, "roce_r%rail_id%_p%plane_id%", group.SpectrumX.InterfaceNames.RdmaPrefix)
	}
}

func TestSpectrumXDesiredNamesDoNotAffectStandardProfiles(t *testing.T) {
	PopulateSpectrumXInterfaceNames(nil)
	for _, profile := range []*Profile{nil, {}, {SpectrumX: &ProfileSpectrumX{Enable: false}}} {
		cfg := &LaunchKitConfig{Profile: profile, ClusterConfig: []ClusterConfig{{MachineType: "machine-VR"}}}
		PopulateSpectrumXInterfaceNames(cfg)
		require.Nil(t, cfg.ClusterConfig[0].SpectrumX)
	}
}
