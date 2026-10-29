// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package inventory

// IdentityConflict rejects replacement of a previously observed physical binding.
func IdentityConflict(old, current *Inventory) bool {
	if old == nil || current == nil {
		return false
	}
	before := map[string]string{}
	for _, group := range [][]Device{old.DPUs, old.Hosts} {
		for _, d := range group {
			if d.ID != "" {
				before[d.RecordKey] = d.ID
			}
		}
	}
	for _, group := range [][]Device{current.DPUs, current.Hosts} {
		for _, d := range group {
			if id := before[d.RecordKey]; id != "" && d.ID != "" && id != d.ID {
				return true
			}
		}
	}
	return false
}
