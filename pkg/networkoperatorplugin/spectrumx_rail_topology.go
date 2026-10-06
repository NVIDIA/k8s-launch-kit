// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/internal/pfutil"
)

func cloneSpectrumXGroup(source *config.SpectrumXGroupConfig) *config.SpectrumXGroupConfig {
	if source == nil {
		return nil
	}
	return &config.SpectrumXGroupConfig{PlatformType: source.PlatformType, SwPlaneByRail: maps.Clone(source.SwPlaneByRail)}
}

func spectrumXSwPlane(group *config.ClusterConfig, rail int) int {
	if group == nil || group.SpectrumX == nil {
		return 0
	}
	return group.SpectrumX.SwPlaneByRail[rail]
}

// Generation-only checks run after source filtering, never during discovery.
func validateSpectrumXGeneration(cfg *config.LaunchKitConfig, groups []config.ClusterConfig) error {
	ra24 := isSpectrumX(cfg) && cfg.Profile.SpectrumX.SPCXVersion == "RA2.4"
	if cfg.SpectrumX != nil && len(cfg.SpectrumX.OVSConfig) > 0 && !ra24 {
		return fmt.Errorf("spectrumX.ovsConfig requires RA2.4")
	}
	var unresolved []string
	for _, group := range groups {
		if group.SpectrumX != nil && len(group.SpectrumX.SwPlaneByRail) > 0 {
			if !ra24 {
				return fmt.Errorf("group %q swPlaneByRail requires RA2.4", group.Identifier)
			}
			if err := validateSpectrumXSwPlanes(group, cfg.Profile.SpectrumX.MultiplaneMode); err != nil {
				return err
			}
		}
		if !ra24 {
			continue
		}
		platform := config.SpectrumXPlatformForGPUProduct(group.GPUType)
		if platform == "" {
			unresolved = append(unresolved, fmt.Sprintf("%q (GPU product %q)", group.Identifier, group.GPUType))
			continue
		}
		if _, err := spectrumXRailLayout(group, cfg.Profile.SpectrumX.NumberOfPlanes); err != nil {
			return err
		}
		if platform == "rtx" {
			id, _, err := config.EastWestDeviceID(group)
			if err != nil {
				return err
			}
			if id != "1023" || cfg.Profile.SpectrumX.MultiplaneMode != "none" || cfg.Profile.SpectrumX.NumberOfPlanes != 1 {
				return fmt.Errorf("RA2.4 rtx group %q requires ConnectX-8 (1023), multiplaneMode=none and numberOfPlanes=1", group.Identifier)
			}
		}
	}
	if len(unresolved) > 0 {
		return fmt.Errorf("cannot generate RA2.4: unresolved doSPCX platform for groups %s; verify GPU products or add support to Launch Kit's internal platform list", strings.Join(unresolved, ", "))
	}
	if ra24 {
		return config.ValidateSpectrumXProfileFormat(cfg.Profile.SpectrumX)
	}
	return nil
}

// spectrumXRailLayout describes the effective NIC naming layout, independent
// of machine-specific PCI addresses. RA2.4 must not use the legacy chunking
// fallback: a missing Rail on one PF otherwise changes every rendered rail.
func spectrumXRailLayout(group config.ClusterConfig, planes int) ([]int, error) {
	ewPFs := pfutil.FilterEastWestPFs(group.PFs)
	if !allHaveRail(ewPFs) {
		return nil, fmt.Errorf("RA2.4 group %q requires complete east-west rail metadata", group.Identifier)
	}
	// NCO assigns one rail index to a physical NIC. Its PCI functions
	// cannot name distinct rails even when both functions are discovered.
	nicRails := map[string]int{}
	for _, pf := range ewPFs {
		nic, ok := pfutil.PciBusDevicePrefix(pf.PciAddress)
		if !ok {
			return nil, fmt.Errorf("RA2.4 group %q has invalid east-west PCI address %q", group.Identifier, pf.PciAddress)
		}
		if previous, exists := nicRails[nic]; exists && previous != *pf.Rail {
			return nil, fmt.Errorf("RA2.4 group %q assigns physical NIC %q to multiple rails (%d and %d); all PFs of one NIC must share one rail", group.Identifier, nic, previous, *pf.Rail)
		}
		nicRails[nic] = *pf.Rail
	}
	rails, order := groupPFsByRail(ewPFs)
	layout := make([]int, len(order))
	for index, rail := range order {
		if rail != index {
			return nil, fmt.Errorf("RA2.4 group %q requires dense zero-based rail IDs", group.Identifier)
		}
		layout[index] = len(masterPFsByNIC(rails[rail]))
		if layout[index] == 0 {
			return nil, fmt.Errorf("RA2.4 group %q rail %d has no valid master NIC PCI addresses", group.Identifier, rail)
		}
		if layout[index] != layout[0] {
			return nil, fmt.Errorf("RA2.4 group %q requires uniform master NIC counts across rails: rail 0 has %d, rail %d has %d", group.Identifier, layout[0], rail, layout[index])
		}
	}
	if planes < 1 || planes%layout[0] != 0 {
		return nil, fmt.Errorf("RA2.4 group %q requires numberOfPlanes (%d) to be a positive multiple of master NICs per rail (%d)", group.Identifier, planes, layout[0])
	}
	return layout, nil
}

func validateSpectrumXSwPlanes(group config.ClusterConfig, mode string) error {
	rails := map[int]bool{}
	for _, pf := range pfutil.FilterEastWestPFs(group.PFs) {
		if pf.Rail == nil {
			return fmt.Errorf("group %q swPlaneByRail requires complete east-west rail metadata", group.Identifier)
		}
		rails[*pf.Rail] = true
	}
	for rail := 0; rail < len(rails); rail++ {
		if !rails[rail] {
			return fmt.Errorf("group %q swPlaneByRail requires dense zero-based rail IDs", group.Identifier)
		}
	}
	keys := slices.Sorted(maps.Keys(group.SpectrumX.SwPlaneByRail))
	for _, rail := range keys {
		plane := group.SpectrumX.SwPlaneByRail[rail]
		if !rails[rail] {
			return fmt.Errorf("group %q swPlaneByRail references unknown east-west rail %d", group.Identifier, rail)
		}
		if plane < 0 {
			return fmt.Errorf("group %q rail %d swPlane must be nonnegative", group.Identifier, rail)
		}
		if mode != "hwplb" && plane != 0 {
			return fmt.Errorf("group %q nonzero swPlane requires hwplb", group.Identifier)
		}
	}
	return nil
}

func validateSpectrumXPlaneBuckets(cfg *config.LaunchKitConfig, plans []RenderBucket) error {
	if !isSpectrumX(cfg) || cfg.Profile.SpectrumX.SPCXVersion != "RA2.4" {
		return nil
	}
	// Rail names are also operator-created child policy and network names.
	// SimpleSelect fans strict subsets out per source, while complete cohorts
	// render one parent per bucket. Multiple parents would overwrite the same
	// children because this profile uses shared rail0/rail0p0 names.
	parentCount := 0
	for _, plan := range plans {
		if plan.ModeB {
			parentCount += len(plan.Sources)
		} else {
			parentCount++
		}
	}
	if parentCount > 1 {
		return fmt.Errorf("cannot generate RA2.4: %d SpectrumXRailPoolConfig parents would cause child-name collisions for shared rail names; select the full source cohort of one GPU/rail-count bucket, or select a single source with --groups", parentCount)
	}
	for _, plan := range plans {
		if len(plan.Sources) == 0 {
			continue
		}
		first := plan.Sources[0]
		if !plan.ModeB && len(plan.Sources) > 1 {
			// A merged parent's GPU selector spans every source with that
			// GPU, including sources excluded from a different PF-count
			// bucket. Only a complete GPU cohort is safe to merge.
			selected := map[string]bool{}
			for _, source := range plan.Sources {
				selected[source.Identifier] = true
			}
			for _, source := range cfg.ClusterConfig {
				if source.GPUType == first.GPUType && !selected[source.Identifier] {
					return fmt.Errorf("cannot generate RA2.4: merged GPU selector for %q would include excluded group %q; select a single source with --groups", first.GPUType, source.Identifier)
				}
			}
		}
		firstLayout, err := spectrumXRailLayout(first, cfg.Profile.SpectrumX.NumberOfPlanes)
		if err != nil {
			return err
		}
		for _, group := range plan.Sources[1:] {
			layout, err := spectrumXRailLayout(group, cfg.Profile.SpectrumX.NumberOfPlanes)
			if err != nil {
				return err
			}
			if !slices.Equal(firstLayout, layout) {
				return fmt.Errorf("cannot merge Spectrum-X groups %q and %q: east-west rail layouts differ (master NIC counts by rail: %v and %v); select a single source with --groups", first.Identifier, group.Identifier, firstLayout, layout)
			}
			keys := map[int]bool{}
			if first.SpectrumX != nil {
				for key := range first.SpectrumX.SwPlaneByRail {
					keys[key] = true
				}
			}
			if group.SpectrumX != nil {
				for key := range group.SpectrumX.SwPlaneByRail {
					keys[key] = true
				}
			}
			for _, rail := range slices.Sorted(maps.Keys(keys)) {
				if spectrumXSwPlane(&first, rail) != spectrumXSwPlane(&group, rail) {
					return fmt.Errorf("cannot merge Spectrum-X groups %q and %q: rail %d resolves to swPlane %d and %d", first.Identifier, group.Identifier, rail, spectrumXSwPlane(&first, rail), spectrumXSwPlane(&group, rail))
				}
			}
		}
	}
	return nil
}
