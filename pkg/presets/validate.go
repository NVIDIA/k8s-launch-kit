// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package presets

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

// ValidatePreset checks whether a topology preset matches the discovered
// hardware. It validates PF count, PCI address set, and device IDs.
// Part numbers and PSIDs are not strict-match criteria — mismatches are
// expected across vendor SKUs and firmware versions.
//
// Returns nil on successful validation, or an error describing the mismatch.
func ValidatePreset(preset *Topology, discoveredPFs []config.PFConfig) error {
	// Step 1: PF count must match
	if len(preset.PFs) != len(discoveredPFs) {
		return fmt.Errorf("PF count mismatch: preset has %d, discovered %d",
			len(preset.PFs), len(discoveredPFs))
	}

	// Step 2: PCI address sets must match exactly
	presetAddrs := make(map[string]string, len(preset.PFs))  // pciAddr -> deviceID
	for _, pf := range preset.PFs {
		presetAddrs[pf.PciAddress] = pf.DeviceID
	}

	discoveredAddrs := make(map[string]string, len(discoveredPFs)) // pciAddr -> deviceID
	for _, pf := range discoveredPFs {
		discoveredAddrs[pf.PciAddress] = pf.DeviceID
	}

	// Find addresses in preset but not in discovered
	var missingFromDiscovered []string
	for addr := range presetAddrs {
		if _, ok := discoveredAddrs[addr]; !ok {
			missingFromDiscovered = append(missingFromDiscovered, addr)
		}
	}

	// Find addresses in discovered but not in preset
	var missingFromPreset []string
	for addr := range discoveredAddrs {
		if _, ok := presetAddrs[addr]; !ok {
			missingFromPreset = append(missingFromPreset, addr)
		}
	}

	if len(missingFromDiscovered) > 0 || len(missingFromPreset) > 0 {
		sort.Strings(missingFromDiscovered)
		sort.Strings(missingFromPreset)

		var parts []string
		if len(missingFromDiscovered) > 0 {
			parts = append(parts, fmt.Sprintf("preset PCI addresses not found in discovered hardware: %s",
				strings.Join(missingFromDiscovered, ", ")))
		}
		if len(missingFromPreset) > 0 {
			parts = append(parts, fmt.Sprintf("discovered PCI addresses not in preset: %s",
				strings.Join(missingFromPreset, ", ")))
		}
		return fmt.Errorf("PCI address mismatch: %s", strings.Join(parts, "; "))
	}

	// Step 3: Device IDs must match at each PCI address
	var deviceMismatches []string
	for addr, presetDevID := range presetAddrs {
		discoveredDevID := discoveredAddrs[addr]
		if presetDevID != discoveredDevID {
			deviceMismatches = append(deviceMismatches, fmt.Sprintf(
				"%s: preset=%s, discovered=%s", addr, presetDevID, discoveredDevID))
		}
	}

	if len(deviceMismatches) > 0 {
		sort.Strings(deviceMismatches)
		return fmt.Errorf("device ID mismatch at PCI address(es): %s",
			strings.Join(deviceMismatches, "; "))
	}

	return nil
}
