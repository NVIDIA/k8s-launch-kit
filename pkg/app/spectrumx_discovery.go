// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"maps"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

func warnSpectrumXPlatforms(groups []config.ClusterConfig, output ui.Output) {
	for _, group := range groups {
		if group.SpectrumX != nil && group.SpectrumX.PlatformType == "" {
			output.Warning("Group %q GPU product %q has no unambiguous doSPCX platform match; saved spectrumX.platformType as empty. RA2.4 generation requires a supported GPU product.", group.Identifier, group.GPUType)
		}
	}
}

// Restore explicit rail assignments only when their source cohort and hardware are unchanged.
func preserveSpectrumXRailAssignments(previous, current []config.ClusterConfig, output ui.Output) {
	byID := map[string]config.ClusterConfig{}
	for _, group := range current {
		byID[group.Identifier] = group
	}
	for _, prior := range previous {
		if prior.SpectrumX == nil || len(prior.SpectrumX.SwPlaneByRail) == 0 {
			continue
		}
		fresh, ok := byID[prior.Identifier]
		if !ok || fresh.GPUType != prior.GPUType || !maps.Equal(spectrumXRailIdentity(prior), spectrumXRailIdentity(fresh)) ||
			!sameSpectrumXWorkers(prior.WorkerNodes, fresh.WorkerNodes) {
			output.Warning("Group %q hardware or worker membership changed or could not be verified; previous swPlaneByRail assignments were not carried forward. Review the current workers and east-west rail topology, then reapply the intended assignments before generation.", prior.Identifier)
			continue
		}
		for i := range current {
			if current[i].Identifier != prior.Identifier {
				continue
			}
			if current[i].SpectrumX == nil {
				current[i].SpectrumX = &config.SpectrumXGroupConfig{}
			}
			current[i].SpectrumX.SwPlaneByRail = maps.Clone(prior.SpectrumX.SwPlaneByRail)
		}
	}
}

func sameSpectrumXWorkers(previous, current []string) bool {
	workerSet := func(workers []string) map[string]struct{} {
		if len(workers) == 0 {
			return nil
		}
		set := make(map[string]struct{}, len(workers))
		for _, worker := range workers {
			if strings.TrimSpace(worker) == "" {
				return nil
			}
			set[worker] = struct{}{}
		}
		return set
	}
	prior, fresh := workerSet(previous), workerSet(current)
	return prior != nil && fresh != nil && maps.Equal(prior, fresh)
}

func spectrumXRailIdentity(group config.ClusterConfig) map[string]string {
	identity := map[string]string{}
	for _, pf := range group.PFs {
		if pf.Traffic != "east-west" {
			continue
		}
		rail := "unset"
		if pf.Rail != nil {
			rail = fmt.Sprint(*pf.Rail)
		}
		identity[pf.PciAddress] = pf.DeviceID + ":" + rail
	}
	return identity
}
