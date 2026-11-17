// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package discovery

import (
	"slices"
	"strconv"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
)

// SoftwareChecks reports release compatibility separately from discovery success.
// Only exact catalog models and identity-bound measurements can satisfy a minimum.
func SoftwareChecks(inv *inventory.Inventory) []inventory.Check {
	out := []inventory.Check{}
	if inv == nil || inv.Software == nil {
		return out
	}
	for _, d := range inv.DPUs {
		if !softwareIdentityCurrent(inv, d) {
			out = append(out, inventory.Check{ID: "dpf.software.applicability", Subject: d.RecordKey, Vantage: "executor", RequiredPhase: "generate", State: "unknown", ReasonCode: "SOFTWARE_IDENTITY_UNAVAILABLE", Message: "Software compatibility requires a successful current observation of a unique DPU identity; inspect this device's identity observation reason.", EvidenceRefs: slices.Clone(d.EvidenceRefs)})
			continue
		}
		host := softwareHost(inv, d)
		model := d.Model
		if d.Runtime != nil {
			board := d.Runtime.Observations["board-product"]
			if board.State == "observed" && len(board.Values) == 1 {
				model = board.Values[0]
			}
		}
		hw, ok := inv.Software.RequirementsFor(model)
		base := inventory.Check{ID: "dpf.software.applicability", Subject: d.RecordKey, Vantage: "executor", RequiredPhase: "generate", State: "unknown", ReasonCode: "BOM_MODEL_UNSUPPORTED", Message: "The selected release BOM has no exact applicability entry for this observed model; software compatibility remains unverified.", EvidenceRefs: slices.Clone(d.EvidenceRefs)}
		if !ok {
			out = append(out, base)
			continue
		}
		base.State = "passed"
		base.ReasonCode = "BOM_MODEL_MATCH"
		base.Message = "Observed model matches a documented BOM entry; hardware qualification remains separate."
		out = append(out, base)
		for _, req := range hw.Requirements {
			// Target coordinates describe desired software, not a minimum
			// readiness predicate. Unknown constraints still explain the gap.
			if req.State == "known" && req.Minimum == "" {
				continue
			}
			c := inventory.Check{ID: "dpf.software." + req.Component, Subject: d.RecordKey, Vantage: "executor", RequiredPhase: "generate", State: "unknown", ReasonCode: req.ReasonCode, Message: req.Message, EvidenceRefs: []string{}}
			if req.State != "known" {
				out = append(out, c)
				continue
			}
			values, refs, issue := softwareVersions(d, host, req)
			c.EvidenceRefs = append(c.EvidenceRefs, refs...)
			if issue.ReasonCode == "SOFTWARE_VERSION_CONFLICT" {
				c.ReasonCode, c.Message = issue.ReasonCode, issue.Message
				out = append(out, c)
				continue
			}
			slices.Sort(values)
			values = slices.Compact(values)
			switch len(values) {
			case 0:
				c.ReasonCode = "SOFTWARE_VERSION_UNAVAILABLE"
				c.Message = "No running or current, identity-bound version was measured for this requirement; inspect firmware/tool observation reasons."
				if issue.ReasonCode != "" {
					c.ReasonCode, c.Message = issue.ReasonCode, issue.Message
				}
			case 1:
				cmp, valid := compareVersion(values[0], req.Minimum)
				if req.Minimum == "" || !valid {
					c.ReasonCode = "SOFTWARE_VERSION_UNCOMPARABLE"
					c.Message = "The observed version cannot be safely compared with the declared minimum."
				} else if cmp < 0 {
					c.State = "failed"
					c.ReasonCode = "SOFTWARE_BELOW_MINIMUM"
					c.Message = "Observed version " + values[0] + " is below the selected release minimum " + req.Minimum + "."
				} else {
					c.State = "passed"
					c.ReasonCode = "SOFTWARE_MINIMUM_MET"
					c.Message = "The measured version satisfies the declared minimum; this is not overall deployment qualification."
				}
			default:
				c.ReasonCode = "SOFTWARE_VERSION_CONFLICT"
				c.Message = "Identity-bound sources report conflicting versions; no version was selected to satisfy the requirement."
			}
			out = append(out, c)
		}
	}
	return out
}

// A failed final identity check invalidates measurements retained for diagnosis.
// Duplicate identities must not become software-compatibility evidence either.
func softwareIdentityCurrent(inv *inventory.Inventory, d inventory.Device) bool {
	if d.ObservationState != "observed" || d.ID == "" || d.SerialNumber == "" {
		return false
	}
	count := 0
	for _, other := range inv.DPUs {
		if other.ID == d.ID || other.SerialNumber == d.SerialNumber {
			count++
		}
	}
	for _, host := range inv.Hosts {
		if host.ID == d.ID {
			count++
		}
	}
	return count == 1
}

// Host measurements are eligible only through a unique accepted association.
// A coincidentally matching serial elsewhere in the inventory is insufficient.
func softwareHost(inv *inventory.Inventory, d inventory.Device) *inventory.Device {
	var association *inventory.Association
	for i := range inv.Associations {
		a := &inv.Associations[i]
		if a.DPUID != d.ID {
			continue
		}
		if association != nil || a.HostID == "" || a.HostAdapterKey == "" || a.SerialNumber != d.SerialNumber {
			return nil
		}
		association = a
	}
	if association == nil {
		return nil
	}
	var host *inventory.Device
	for i := range inv.Hosts {
		candidate := &inv.Hosts[i]
		if candidate.ID != association.HostID {
			continue
		}
		if host != nil || candidate.ObservationState != "observed" {
			return nil
		}
		host = candidate
	}
	if host == nil {
		return nil
	}
	for _, other := range inv.DPUs {
		if other.ID == host.ID {
			return nil
		}
	}
	matches := 0
	for _, adapter := range host.Adapters {
		if adapter.SerialNumber != d.SerialNumber {
			continue
		}
		if adapter.Key != association.HostAdapterKey {
			return nil
		}
		matches++
	}
	if matches != 1 {
		return nil
	}
	return host
}

func softwareVersions(d inventory.Device, host *inventory.Device, req releases.Requirement) ([]string, []string, inventory.Observation) {
	values, refs := []string{}, []string{}
	issue := inventory.Observation{}
	source := d.Observations["firmware-inventory"]
	for _, fw := range d.Firmware {
		if fw.Component != req.Component {
			continue
		}
		if fw.State == "conflict" || fw.ActivationReasonCode == "ACTIVATION_CONFLICT" {
			refs = append(refs, fw.EvidenceRefs...)
			issue = inventory.Observation{State: "unknown", ReasonCode: "SOFTWARE_VERSION_CONFLICT", Message: "Component version or activation evidence conflicts; no alternate source can satisfy this requirement until the conflict is resolved."}
		}
		if source.State == "observed" && fw.State == "known" && fw.Activation == "running" {
			values = append(values, fw.RawVersion)
			refs = append(refs, fw.EvidenceRefs...)
		}
	}
	if req.Component == "nic" {
		for _, device := range []*inventory.Device{&d, host} {
			if device == nil || device.Runtime == nil || device.Runtime.Hardware == nil {
				continue
			}
			for _, card := range device.Runtime.Hardware.Cards {
				if card.SerialNumber == d.SerialNumber && card.Firmware.State == "observed" {
					values = append(values, card.Firmware.Values...)
					refs = append(refs, card.EvidenceRefs...)
				}
			}
		}
	}
	if req.Scope == "host" && req.Component == "mft" && host != nil && host.Runtime != nil {
		version := host.Runtime.Observations["mft-version"]
		if version.State == "observed" {
			values = append(values, version.Values...)
			refs = append(refs, host.EvidenceRefs...)
		}
	}
	if len(values) == 0 && issue.ReasonCode == "" && source.State != "observed" && source.ReasonCode != "" && req.Scope != "host" {
		issue = inventory.Observation{State: "unknown", ReasonCode: source.ReasonCode, Message: "The Redfish firmware inventory is unavailable or incomplete: " + source.Message}
	}
	return values, refs, issue
}

// Version grammar is deliberately narrow: numeric release components separated by
// dots and an optional numeric build suffix. Vendor strings require normalization
// by their specific collector before they can satisfy a minimum.
func compareVersion(a, b string) (int, bool) {
	parse := func(v string) ([]uint64, bool) {
		if v == "" {
			return nil, false
		}
		out := []uint64{}
		for _, s := range strings.Split(strings.ReplaceAll(v, "-", "."), ".") {
			if s == "" {
				return nil, false
			}
			for _, r := range s {
				if r < '0' || r > '9' {
					return nil, false
				}
			}
			n, e := strconv.ParseUint(s, 10, 64)
			if e != nil {
				return nil, false
			}
			out = append(out, n)
		}
		return out, true
	}
	aa, ok := parse(a)
	if !ok {
		return 0, false
	}
	bb, ok := parse(b)
	if !ok {
		return 0, false
	}
	n := max(len(aa), len(bb))
	for i := 0; i < n; i++ {
		var x, y uint64
		if i < len(aa) {
			x = aa[i]
		}
		if i < len(bb) {
			y = bb[i]
		}
		if x < y {
			return -1, true
		}
		if x > y {
			return 1, true
		}
	}
	return 0, true
}
