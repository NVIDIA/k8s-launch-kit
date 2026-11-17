// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
)

// inventoryOutput is the public v3 handoff. Collection bookkeeping stays in the
// collector model; required facts and failures have explicit domain fields here.
type inventoryOutput struct {
	Software      *releases.Summary   `json:"software,omitempty" yaml:"software,omitempty"`
	AddressLookup *AddressLookup      `json:"addressLookup,omitempty" yaml:"addressLookup,omitempty"`
	FormatVersion string              `json:"formatVersion" yaml:"formatVersion"`
	Collection    Collection          `json:"collection" yaml:"collection"`
	ScopeDigest   string              `json:"scopeDigest,omitempty" yaml:"scopeDigest,omitempty"`
	Fingerprint   string              `json:"fingerprint,omitempty" yaml:"fingerprint,omitempty"`
	Management    *Management         `json:"management,omitempty" yaml:"management,omitempty"`
	DPUs          []DeviceOutput      `json:"dpus,omitempty" yaml:"dpus,omitempty"`
	Hosts         []DeviceOutput      `json:"hosts,omitempty" yaml:"hosts,omitempty"`
	Associations  []associationOutput `json:"associations,omitempty" yaml:"associations,omitempty"`
	Probes        []ProbeResult       `json:"probes,omitempty" yaml:"probes,omitempty"`
	Evidence      map[string]Evidence `json:"evidence,omitempty" yaml:"evidence,omitempty"`
}

type associationOutput struct {
	DPUID          string `json:"dpuID" yaml:"dpuID"`
	HostID         string `json:"hostID" yaml:"hostID"`
	HostAdapterKey string `json:"hostAdapterKey" yaml:"hostAdapterKey"`
	SerialNumber   string `json:"serialNumber" yaml:"serialNumber"`
}

func (i Inventory) output() (inventoryOutput, error) {
	out := inventoryOutput{
		FormatVersion: FormatVersion, Collection: i.Collection,
		ScopeDigest: i.ScopeDigest, Fingerprint: i.Fingerprint,
		Management: i.Management, AddressLookup: i.AddressLookup, Probes: i.Probes,
	}
	if i.Software != nil {
		summary := releases.Summarize(*i.Software)
		out.Software = &summary
	}
	for _, group := range []struct {
		role    string
		devices []Device
		output  *[]DeviceOutput
	}{{"dpus", i.DPUs, &out.DPUs}, {"hosts", i.Hosts, &out.Hosts}} {
		for _, device := range group.devices {
			value, err := DeviceToOutput(device, group.role)
			if err != nil {
				return inventoryOutput{}, err
			}
			*group.output = append(*group.output, value)
		}
	}
	for _, association := range i.Associations {
		out.Associations = append(out.Associations, associationOutput{
			DPUID: association.DPUID, HostID: association.HostID,
			HostAdapterKey: association.HostAdapterKey, SerialNumber: association.SerialNumber,
		})
	}
	// Device source diagnostics are carried with their fields. Management/probe
	// evidence has its own contract and is independent of the host/DPU records.
	for key, evidence := range i.Evidence {
		if evidence.Kind == "redfish" {
			continue
		}
		if out.Evidence == nil {
			out.Evidence = map[string]Evidence{}
		}
		out.Evidence[key] = evidence
	}
	return out, nil
}

func (w inventoryOutput) inventory() (Inventory, error) {
	out := Inventory{
		FormatVersion: FormatVersion, Collection: w.Collection,
		ScopeDigest: w.ScopeDigest, Fingerprint: w.Fingerprint,
		Management: w.Management, AddressLookup: w.AddressLookup,
		Probes: w.Probes, Evidence: w.Evidence,
	}
	if w.Software != nil {
		release, err := w.Software.Release()
		if err != nil {
			return Inventory{}, err
		}
		out.Software = &release
	}
	for _, group := range []struct {
		role    string
		devices []DeviceOutput
		output  *[]Device
	}{{"dpus", w.DPUs, &out.DPUs}, {"hosts", w.Hosts, &out.Hosts}} {
		for _, device := range group.devices {
			value, err := DeviceFromOutput(device, group.role)
			if err != nil {
				return Inventory{}, err
			}
			*group.output = append(*group.output, value)
		}
	}
	for _, association := range w.Associations {
		out.Associations = append(out.Associations, Association{
			DPUID: association.DPUID, HostID: association.HostID,
			HostAdapterKey: association.HostAdapterKey, SerialNumber: association.SerialNumber,
		})
	}
	return out, nil
}
