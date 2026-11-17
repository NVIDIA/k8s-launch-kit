// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

// NVUE's interface list omits some fields present in an individual interface
// query. Enrich only explicitly selected ports; retain contradictory list/detail
// observations as failures rather than choosing whichever command ran last.
func parseSelectedInterfaces(in NVUEInputs, out *Snapshot) {
	for name, raw := range in.SelectedAppliedInterfaces {
		input := "appliedInterface/" + name
		entry, ok := parseObject(raw, input, &out.Issues)
		if !ok {
			continue
		}
		fact := configuredInterface(name, entry, input, "$", &out.Issues)
		if fact.Type == nil || *fact.Type != "swp" {
			addIssue(&out.Issues, input, "$.type", "physical_interface_required", "Selected interface detail must identify a physical switch port.")
		}
		index := -1
		for i := range out.Configured.Interfaces {
			if out.Configured.Interfaces[i].Name == name {
				index = i
				break
			}
		}
		if index < 0 {
			addIssue(&out.Issues, input, "$", "interface_not_listed", "Selected interface is absent from the applied interface list.")
			continue
		}
		current := &out.Configured.Interfaces[index]
		mergeInterfaceValue(&current.Type, fact.Type, input, "type", &out.Issues)
		mergeInterfaceValue(&current.VRF, fact.VRF, input, "vrf", &out.Issues)
		mergeInterfaceValue(&current.MTU, fact.MTU, input, "link.mtu", &out.Issues)
		mergeInterfaceValue(&current.BridgeDomain, fact.BridgeDomain, input, "bridge.domain", &out.Issues)
	}
	for name, raw := range in.SelectedOperationalInterfaces {
		input := "operationalInterface/" + name
		entry, ok := parseObject(raw, input, &out.Issues)
		if !ok {
			continue
		}
		fact := operationalInterface(name, entry, input, "$", &out.Issues)
		if fact.Type == nil || *fact.Type != "swp" {
			addIssue(&out.Issues, input, "$.type", "physical_interface_required", "Selected interface detail must identify a physical switch port.")
		}
		index := -1
		for i := range out.Operational.Interfaces {
			if out.Operational.Interfaces[i].Name == name {
				index = i
				break
			}
		}
		if index < 0 {
			addIssue(&out.Issues, input, "$", "interface_not_listed", "Selected interface is absent from the operational interface list.")
			continue
		}
		current := &out.Operational.Interfaces[index]
		mergeInterfaceValue(&current.Type, fact.Type, input, "type", &out.Issues)
		mergeInterfaceValue(&current.VRF, fact.VRF, input, "vrf", &out.Issues)
		mergeInterfaceValue(&current.Parent, fact.Parent, input, "parent", &out.Issues)
		mergeInterfaceValue(&current.MTU, fact.MTU, input, "link.mtu", &out.Issues)
		mergeInterfaceValue(&current.AdminState, fact.AdminState, input, "link.admin-status", &out.Issues)
		mergeInterfaceValue(&current.OperState, fact.OperState, input, "link.oper-status", &out.Issues)
		mergeInterfaceValue(&current.Speed, fact.Speed, input, "link.speed", &out.Issues)
	}
}

func mergeInterfaceValue[T comparable](current **T, detail *T, input, field string, issues *[]Issue) {
	if detail == nil {
		return
	}
	if *current == nil {
		*current = detail
		return
	}
	if **current != *detail {
		addIssue(issues, input, "$."+field, "conflicting_observation", "Interface list and selected-interface query disagree; collect a fresh snapshot after configuration settles.")
	}
}
