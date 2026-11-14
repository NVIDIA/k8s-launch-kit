// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var outputToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._()+-]{0,127}$`)

func runtimeToOutput(r *RuntimeObservation, role string, issues *[]DeviceIssue) (*RuntimeOutput, error) {
	w := &RuntimeOutput{Hostname: r.Hostname, OS: r.OS, Interfaces: cloneInterfaces(r.Interfaces), Routes: slices.Clone(r.Routes), DNS: slices.Clone(r.DNS), Network: cloneNetwork(r.Network)}
	for _, key := range sortedObservationKeys(r.Observations) {
		o := r.Observations[key]
		switch key {
		case "board-product", "mft-version":
			field := "runtime.boardProduct"
			if key == "mft-version" {
				field = "runtime.mftVersion"
			}
			value, err := observedValue(o, field)
			if err != nil {
				return nil, err
			}
			if key == "board-product" {
				w.BoardProduct = value
			} else {
				w.MFTVersion = value
			}
			addIssue(issues, field, o)
		case "pciScope", "bmcAddress":
			// PCI scope is constant prose; BMC address already has a field.
			if key == "bmcAddress" {
				addIssue(issues, "bmcEndpoint", o)
			}
		default:
			addIssue(issues, "runtime."+key, o)
		}
	}
	if r.Hardware == nil {
		return w, nil
	}
	w.Hardware = &HardwareOutput{}
	if role == "hosts" {
		if r.Hardware.RShim.State == "observed" {
			service, err := rshimToOutput(r.Hardware.RShim.Values)
			if err != nil {
				return nil, err
			}
			w.Hardware.RShim = service
		} else {
			addIssue(issues, "runtime.hardware.rshim", r.Hardware.RShim)
		}
	}
	for index, card := range r.Hardware.Cards {
		prefix := fmt.Sprintf("runtime.hardware.cards[%d].", index)
		out := CardOutput{PCIAddress: card.PCIAddress, Domain: card.Domain, Model: card.Model, SerialNumber: card.SerialNumber}
		var err error
		out.FirmwareVersion, err = observedValue(card.Firmware, prefix+"firmwareVersion")
		if err != nil {
			return nil, err
		}
		out.PSID, err = observedValue(card.PSID, prefix+"psid")
		if err != nil {
			return nil, err
		}
		addIssue(issues, prefix+"firmwareVersion", card.Firmware)
		addIssue(issues, prefix+"psid", card.PSID)
		for _, parameter := range []struct{ key, field string }{
			{"INTERNAL_CPU_OFFLOAD_ENGINE", "internalCPUOffloadEngine"}, {"INTERNAL_CPU_RSHIM", "internalCPURShim"}, {"NUM_OF_VFS", "numVFs"},
		} {
			o := card.NVConfig[parameter.key]
			value, err := observedValue(o, prefix+"nextBoot."+parameter.field)
			if err != nil {
				return nil, err
			}
			addIssue(issues, prefix+"nextBoot."+parameter.field, o)
			if value == "" {
				continue
			}
			value, ok := strings.CutPrefix(value, "next-boot=")
			if !ok || !outputToken.MatchString(value) {
				return nil, fmt.Errorf("%s must be an explicit next-boot value", prefix+parameter.field)
			}
			if out.NextBoot == nil {
				out.NextBoot = &NVConfigOutput{}
			}
			switch parameter.key {
			case "INTERNAL_CPU_OFFLOAD_ENGINE":
				out.NextBoot.InternalCPUOffloadEngine = value
			case "INTERNAL_CPU_RSHIM":
				out.NextBoot.InternalCPURShim = value
			case "NUM_OF_VFS":
				n, err := strconv.ParseUint(value, 10, 32)
				if err != nil {
					return nil, fmt.Errorf("%snumVFs is not a valid VF count", prefix)
				}
				count := uint32(n)
				out.NextBoot.NumVFs = &count
			}
		}
		if role == "dpus" {
			if card.Privilege.State == "observed" {
				out.HostPrivileges, err = privilegeToOutput(card.Privilege.Values)
				if err != nil {
					return nil, err
				}
			} else {
				addIssue(issues, prefix+"hostPrivileges", card.Privilege)
			}
		}
		w.Hardware.Cards = append(w.Hardware.Cards, out)
	}
	return w, nil
}

func runtimeFromOutput(w *RuntimeOutput, role string, issues []DeviceIssue) (*RuntimeObservation, error) {
	r := &RuntimeObservation{Hostname: w.Hostname, OS: w.OS, Interfaces: cloneInterfaces(w.Interfaces), Routes: slices.Clone(w.Routes), DNS: slices.Clone(w.DNS),
		Network: cloneNetwork(w.Network), Observations: map[string]Observation{}}
	for _, value := range []struct{ name, key, value, tool string }{
		{"boardProduct", "board-product", w.BoardProduct, "ipmitool"}, {"mftVersion", "mft-version", w.MFTVersion, "mlxfwmanager"},
	} {
		if value.value != "" {
			if !safeFact(value.value) || hasIssue(issues, "runtime."+value.name) {
				return nil, fmt.Errorf("runtime.%s has conflicting or invalid values", value.name)
			}
			r.Observations[value.key] = Observation{State: "observed", Values: []string{value.value}, Tool: value.tool}
		}
	}
	for _, issue := range issues {
		if strings.HasPrefix(issue.Field, "runtime.") && !strings.HasPrefix(issue.Field, "runtime.hardware.") {
			key := strings.TrimPrefix(issue.Field, "runtime.")
			switch key {
			case "boardProduct":
				key = "board-product"
			case "mftVersion":
				key = "mft-version"
			}
			r.Observations[key] = issueObservation(issue)
		}
	}
	if w.Hardware == nil {
		return r, nil
	}
	r.Hardware = &HardwareObservation{}
	if w.Hardware.RShim != nil {
		if role != "hosts" || hasIssue(issues, "runtime.hardware.rshim") || !outputToken.MatchString(w.Hardware.RShim.LoadState) || !outputToken.MatchString(w.Hardware.RShim.ActiveState) {
			return nil, fmt.Errorf("rshim requires valid host service states without a read failure")
		}
		r.Hardware.RShim = Observation{State: "observed", Values: []string{"LoadState=" + w.Hardware.RShim.LoadState, "ActiveState=" + w.Hardware.RShim.ActiveState}, Tool: "systemctl"}
	}
	for _, issue := range issues {
		if issue.Field == "runtime.hardware.rshim" {
			r.Hardware.RShim = issueObservation(issue)
		}
	}
	seen := map[string]bool{}
	for index, card := range w.Hardware.Cards {
		prefix := fmt.Sprintf("runtime.hardware.cards[%d].", index)
		if card.Domain != "host-pci" && card.Domain != "arm-pci" || card.PCIAddress == "" || seen[card.Domain+"/"+card.PCIAddress] {
			return nil, fmt.Errorf("%srequires a unique PCI address and domain", prefix)
		}
		seen[card.Domain+"/"+card.PCIAddress] = true
		out := CardHardware{PCIAddress: card.PCIAddress, Domain: card.Domain, Model: card.Model, SerialNumber: card.SerialNumber, NVConfig: map[string]Observation{}}
		for _, value := range []struct {
			name, value string
			target      *Observation
		}{
			{"firmwareVersion", card.FirmwareVersion, &out.Firmware}, {"psid", card.PSID, &out.PSID},
		} {
			if value.value != "" {
				if !outputToken.MatchString(value.value) || hasIssue(issues, prefix+value.name) {
					return nil, fmt.Errorf("%s%s has invalid or conflicting facts", prefix, value.name)
				}
				*value.target = Observation{State: "observed", Values: []string{value.value}, Tool: "mlxfwmanager"}
			}
		}
		if card.NextBoot != nil {
			for _, parameter := range []struct{ key, name, value string }{
				{"INTERNAL_CPU_OFFLOAD_ENGINE", "internalCPUOffloadEngine", card.NextBoot.InternalCPUOffloadEngine},
				{"INTERNAL_CPU_RSHIM", "internalCPURShim", card.NextBoot.InternalCPURShim},
			} {
				if parameter.value != "" {
					if !outputToken.MatchString(parameter.value) || hasIssue(issues, prefix+"nextBoot."+parameter.name) {
						return nil, fmt.Errorf("%snextBoot.%s has invalid or conflicting facts", prefix, parameter.name)
					}
					out.NVConfig[parameter.key] = Observation{State: "observed", Values: []string{"next-boot=" + parameter.value}, Tool: "mlxconfig"}
				}
			}
			if card.NextBoot.NumVFs != nil {
				if hasIssue(issues, prefix+"nextBoot.numVFs") {
					return nil, fmt.Errorf("%snextBoot.numVFs conflicts with a read failure", prefix)
				}
				out.NVConfig["NUM_OF_VFS"] = Observation{State: "observed", Values: []string{"next-boot=" + strconv.FormatUint(uint64(*card.NextBoot.NumVFs), 10)}, Tool: "mlxconfig"}
			}
		}
		if len(card.HostPrivileges) != 0 {
			if role != "dpus" || hasIssue(issues, prefix+"hostPrivileges") {
				return nil, fmt.Errorf("%shostPrivileges has invalid source or conflicting facts", prefix)
			}
			values, err := privilegeFromOutput(card.HostPrivileges)
			if err != nil {
				return nil, err
			}
			out.Privilege = Observation{State: "observed", Values: values, Tool: "mlxprivhost"}
		}
		for _, issue := range issues {
			switch issue.Field {
			case prefix + "firmwareVersion":
				out.Firmware = issueObservation(issue)
			case prefix + "psid":
				out.PSID = issueObservation(issue)
			case prefix + "hostPrivileges":
				out.Privilege = issueObservation(issue)
			case prefix + "nextBoot.internalCPUOffloadEngine":
				out.NVConfig["INTERNAL_CPU_OFFLOAD_ENGINE"] = issueObservation(issue)
			case prefix + "nextBoot.internalCPURShim":
				out.NVConfig["INTERNAL_CPU_RSHIM"] = issueObservation(issue)
			case prefix + "nextBoot.numVFs":
				out.NVConfig["NUM_OF_VFS"] = issueObservation(issue)
			}
		}
		r.Hardware.Cards = append(r.Hardware.Cards, out)
	}
	return r, nil
}

func rshimToOutput(values []string) (*RShimServiceOutput, error) {
	w := &RShimServiceOutput{}
	for _, value := range values {
		key, value, ok := strings.Cut(value, "=")
		if !ok || !outputToken.MatchString(value) {
			return nil, fmt.Errorf("invalid rshim service value")
		}
		switch key {
		case "LoadState":
			if w.LoadState != "" {
				return nil, fmt.Errorf("duplicate rshim LoadState")
			}
			w.LoadState = value
		case "ActiveState":
			if w.ActiveState != "" {
				return nil, fmt.Errorf("duplicate rshim ActiveState")
			}
			w.ActiveState = value
		default:
			return nil, fmt.Errorf("unsupported rshim property %q", key)
		}
	}
	if w.LoadState == "" || w.ActiveState == "" {
		return nil, fmt.Errorf("rshim service requires load and active states")
	}
	return w, nil
}

func privilegeToOutput(values []string) ([]HostPrivilegeOutput, error) {
	byHost := map[uint32]*HostPrivilegeOutput{}
	seen := map[string]bool{}
	for _, value := range values {
		key, value, ok := strings.Cut(value, "=")
		host, property, hasProperty := strings.Cut(key, "].")
		host, hasHost := strings.CutPrefix(host, "host[")
		n, err := strconv.ParseUint(host, 10, 32)
		if !ok || !hasProperty || !hasHost || err != nil || seen[key] {
			return nil, fmt.Errorf("invalid or duplicate host privilege property")
		}
		seen[key] = true
		index := uint32(n)
		entry := byHost[index]
		if entry == nil {
			entry = &HostPrivilegeOutput{HostIndex: index}
			byHost[index] = entry
		}
		if property == "level" {
			entry.Level = value
			continue
		}
		if value != "TRUE" && value != "FALSE" {
			return nil, fmt.Errorf("invalid host privilege boolean")
		}
		flag := value == "TRUE"
		switch property {
		case "disable_rshim":
			entry.DisableRShim = &flag
		case "disable_tracer":
			entry.DisableTracer = &flag
		case "disable_port_owner":
			entry.DisablePortOwner = &flag
		case "disable_counter_rd":
			entry.DisableCounterRead = &flag
		default:
			return nil, fmt.Errorf("unsupported host privilege property %q", property)
		}
	}
	result := make([]HostPrivilegeOutput, 0, len(byHost))
	for _, entry := range byHost {
		result = append(result, *entry)
	}
	slices.SortFunc(result, func(a, b HostPrivilegeOutput) int {
		if a.HostIndex < b.HostIndex {
			return -1
		}
		if a.HostIndex > b.HostIndex {
			return 1
		}
		return 0
	})
	if _, err := privilegeFromOutput(result); err != nil {
		return nil, err
	}
	return result, nil
}

func privilegeFromOutput(values []HostPrivilegeOutput) ([]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("host privileges require at least one host level")
	}
	seen := map[uint32]bool{}
	var result []string
	for _, entry := range values {
		if seen[entry.HostIndex] || entry.Level != "PRIVILEGED" && entry.Level != "RESTRICTED" {
			return nil, fmt.Errorf("host privileges require unique host indices and valid levels")
		}
		seen[entry.HostIndex] = true
		prefix := "host[" + strconv.FormatUint(uint64(entry.HostIndex), 10) + "]."
		result = append(result, prefix+"level="+entry.Level)
		for _, value := range []struct {
			key  string
			flag *bool
		}{
			{"disable_rshim", entry.DisableRShim}, {"disable_tracer", entry.DisableTracer}, {"disable_port_owner", entry.DisablePortOwner}, {"disable_counter_rd", entry.DisableCounterRead},
		} {
			if value.flag != nil {
				text := "FALSE"
				if *value.flag {
					text = "TRUE"
				}
				result = append(result, prefix+value.key+"="+text)
			}
		}
	}
	return result, nil
}

func cloneInterfaces(in []Interface) []Interface {
	out := slices.Clone(in)
	for i := range out {
		out[i].Addresses = slices.Clone(out[i].Addresses)
		if out[i].AdminUp != nil {
			up := *out[i].AdminUp
			out[i].AdminUp = &up
		}
	}
	return out
}

func cloneNetwork(in *NetworkObservation) *NetworkObservation {
	if in == nil {
		return nil
	}
	out := *in
	out.Links = slices.Clone(in.Links)
	out.LLDP = slices.Clone(in.LLDP)
	out.Neighbors = slices.Clone(in.Neighbors)
	for i := range out.Neighbors {
		out.Neighbors[i].State = slices.Clone(out.Neighbors[i].State)
	}
	out.Resolver.Search = slices.Clone(in.Resolver.Search)
	out.Resolver.DNSMasqServers = slices.Clone(in.Resolver.DNSMasqServers)
	if in.Resolver.DNSMasqNoResolv != nil {
		flag := *in.Resolver.DNSMasqNoResolv
		out.Resolver.DNSMasqNoResolv = &flag
	}
	clone := func(in map[string][]string) map[string][]string {
		if in == nil {
			return nil
		}
		out := make(map[string][]string, len(in))
		for key, values := range in {
			out[key] = slices.Clone(values)
		}
		return out
	}
	out.Resolver.PerLink = clone(in.Resolver.PerLink)
	out.Resolver.Domains = clone(in.Resolver.Domains)
	return &out
}
