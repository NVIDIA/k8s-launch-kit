// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package config

// PopulateSpectrumXInterfaceNames resolves desired naming per source group.
// Observed PF names are retained as inventory, not replaced with desired names.
// Recompute on resolution as well as discovery so old configs and later mode or
// naming overrides follow the same rules without trusting stale derived values.
func PopulateSpectrumXInterfaceNames(cfg *LaunchKitConfig) {
	if cfg == nil || cfg.Profile == nil || cfg.Profile.SpectrumX == nil || !cfg.Profile.SpectrumX.Enable {
		return
	}
	mode := cfg.Profile.SpectrumX.MultiplaneMode
	rdma, netdev := SpectrumXInterfaceNamePrefixes(cfg.SpectrumX, mode)
	for i := range cfg.ClusterConfig {
		group := &cfg.ClusterConfig[i]
		if group.SpectrumX == nil {
			group.SpectrumX = &SpectrumXGroupConfig{}
		}
		groupRDMA := rdma
		// VR has two RDMA bonds per logical rail, one per four-PF ASIC NIC.
		// Keep explicit custom names; generation validates their NIC placeholder.
		if SpectrumXPlatformForGroup(*group) == "vr" && mode == "hwplb" && groupRDMA == "roce_r%rail_id%" {
			groupRDMA += "_n%nic_id%"
		}
		group.SpectrumX.InterfaceNames = &SpectrumXInterfaceNamePrefixConfig{
			NetdevPrefix: netdev,
			RdmaPrefix:   groupRDMA,
		}
	}
}
