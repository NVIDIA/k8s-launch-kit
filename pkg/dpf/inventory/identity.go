// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package inventory

// LegacyHostCardBindings identifies preview handoffs whose Redfish card record
// key changed with the serial. Those records cannot safely guard a refresh and
// require a fresh seed and output; do not silently migrate physical bindings.
func LegacyHostCardBindings(old *Inventory) bool {
	if old == nil {
		return false
	}
	derived := map[string]bool{}
	for _, d := range old.DPUs {
		if d.ReaderAdapter == "host-card-v1" {
			derived[d.RecordKey] = true
		}
	}
	for _, host := range old.Hosts {
		for _, card := range host.Adapters {
			if card.SerialNumber != "" && card.Key == Digest("dpf-adapter-v1", []string{host.ID, card.SerialNumber}) &&
				derived[Digest("dpf-host-card-v1", []string{host.ID, card.Key})] {
				return true
			}
		}
	}
	return false
}

// IdentityConflict rejects replacement of a previously observed physical binding
// and legacy bindings that cannot support a safe comparison.
func IdentityConflict(old, current *Inventory) bool {
	if old == nil || current == nil {
		return false
	}
	if LegacyHostCardBindings(old) {
		return true
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
