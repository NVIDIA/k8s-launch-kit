// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package config

import "strings"

// Matches dospcx-data/data/platform-types.yaml at 6b2a141. Embedded so discovery
// works without presets, network access or a supplied doSPCX bundle.
var spectrumXPlatforms = []string{"h100", "h200", "b200", "gb200", "b300", "gb300", "vr", "rtx"}

// SpectrumXPlatformForGPUProduct selects the longest case-insensitive substring.
// An equal-length ambiguity is unresolved, independent of catalog order.
func SpectrumXPlatformForGPUProduct(product string) string {
	product = strings.ToLower(strings.TrimSpace(product))
	best := ""
	ambiguous := false
	for _, platform := range spectrumXPlatforms {
		if !strings.Contains(product, platform) {
			continue
		}
		if len(platform) > len(best) {
			best, ambiguous = platform, false
		} else if len(platform) == len(best) && platform != best {
			ambiguous = true
		}
	}
	if ambiguous {
		return ""
	}
	return best
}

// PopulateSpectrumXPlatforms recomputes derived metadata, preserving rail intent.
func PopulateSpectrumXPlatforms(groups []ClusterConfig) {
	for i := range groups {
		if groups[i].SpectrumX == nil {
			groups[i].SpectrumX = &SpectrumXGroupConfig{}
		}
		groups[i].SpectrumX.PlatformType = SpectrumXPlatformForGPUProduct(groups[i].GPUType)
	}
}
