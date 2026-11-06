// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"slices"
	"strconv"
	"strings"
)

// SoftwareChecks reports release compatibility separately from discovery success.
// Only exact catalog models and identity-bound measurements can satisfy a minimum.
func SoftwareChecks(inv *inventory.Inventory) []inventory.Check {
	out := []inventory.Check{}
	if inv == nil || inv.Software == nil {
		return out
	}
	for _, d := range inv.DPUs {
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
			c := inventory.Check{ID: "dpf.software." + req.Component, Subject: d.RecordKey, Vantage: "executor", RequiredPhase: "generate", State: "unknown", ReasonCode: req.ReasonCode, Message: req.Message, EvidenceRefs: slices.Clone(d.EvidenceRefs)}
			if req.State != "known" {
				out = append(out, c)
				continue
			}
			values := []string{}
			for _, fw := range d.Firmware {
				if fw.Component == req.Component && fw.State == "known" && fw.Activation == "active" {
					values = append(values, fw.RawVersion)
					c.EvidenceRefs = append(c.EvidenceRefs, fw.EvidenceRefs...)
				}
			}
			if req.Component == "nic" {
				for _, group := range [][]inventory.Device{inv.Hosts, []inventory.Device{d}} {
					for _, device := range group {
						if device.Runtime != nil && device.Runtime.Hardware != nil {
							for _, card := range device.Runtime.Hardware.Cards {
								if d.SerialNumber != "" && card.SerialNumber == d.SerialNumber && card.Firmware.State == "observed" {
									values = append(values, card.Firmware.Values...)
									c.EvidenceRefs = append(c.EvidenceRefs, card.EvidenceRefs...)
								}
							}
						}
					}
				}
			}
			if req.Scope == "host" && req.Component == "mft" {
				for _, a := range inv.Associations {
					if a.DPUID == d.ID {
						for _, h := range inv.Hosts {
							if h.ID == a.HostID && h.Runtime != nil {
								v := h.Runtime.Observations["mft-version"]
								if v.State == "observed" {
									values = append(values, v.Values...)
									c.EvidenceRefs = append(c.EvidenceRefs, h.EvidenceRefs...)
								}
							}
						}
					}
				}
			}
			slices.Sort(values)
			values = slices.Compact(values)
			switch len(values) {
			case 0:
				c.ReasonCode = "SOFTWARE_VERSION_UNAVAILABLE"
				c.Message = "No active or current, identity-bound version was measured for this requirement; inspect firmware/tool observation reasons."
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
