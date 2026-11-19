// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// VerifyRetainedPhysicalBindings binds current inventory to the physical scope
// recorded at generation. Fresh observations may change health, but not selected
// identities, trust policy or host-to-DPU associations.
func VerifyRetainedPhysicalBindings(before, current *inventory.Inventory) error {
	if before == nil || current == nil || before.FormatVersion != inventory.FormatVersion || current.FormatVersion != inventory.FormatVersion {
		return fmt.Errorf("retained and current discovery inventories are required")
	}
	if before.ScopeDigest == "" || before.ScopeDigest != current.ScopeDigest || inventory.IdentityConflict(before, current) {
		return fmt.Errorf("selected access scope, trust or physical identity changed since generation; rediscover and regenerate")
	}
	if len(before.DPUs) == 0 || len(before.DPUs) != len(current.DPUs) || len(before.Hosts) != len(current.Hosts) || len(before.Associations) != len(current.Associations) {
		return fmt.Errorf("selected host and DPU membership changed since generation")
	}
	previousDevices := append(append([]inventory.Device(nil), before.Hosts...), before.DPUs...)
	currentDevices := append(append([]inventory.Device(nil), current.Hosts...), current.DPUs...)
	seen := map[string]bool{}
	for _, previous := range previousDevices {
		if previous.InputRef == "" || seen[previous.InputRef] {
			return fmt.Errorf("retained physical source bindings are incomplete or duplicated")
		}
		seen[previous.InputRef] = true
		matches := 0
		for _, host := range currentDevices {
			if host.InputRef == previous.InputRef && host.ID == previous.ID && host.ID != "" &&
				host.SystemUUID == previous.SystemUUID && host.SerialNumber == previous.SerialNumber && host.SSHEndpoint == previous.SSHEndpoint &&
				host.BMCEndpoint == previous.BMCEndpoint && host.SystemURI == previous.SystemURI {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("a selected physical host or DPU binding changed since generation")
		}
	}
	seen = map[string]bool{}
	for _, previous := range before.Associations {
		if previous.DPUID == "" || previous.HostID == "" || previous.HostAdapterKey == "" || previous.SerialNumber == "" || seen[previous.DPUID] {
			return fmt.Errorf("retained DPU-to-host associations are incomplete or duplicated")
		}
		seen[previous.DPUID] = true
		matches := 0
		for _, association := range current.Associations {
			if association.DPUID == previous.DPUID && association.HostID == previous.HostID &&
				association.HostAdapterKey == previous.HostAdapterKey && association.SerialNumber == previous.SerialNumber {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("a selected DPU-to-host association changed since generation")
		}
	}
	return nil
}
