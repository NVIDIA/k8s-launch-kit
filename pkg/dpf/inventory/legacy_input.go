// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package inventory

import "github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"

// legacyInventoryInput is the v1alpha1/v1alpha2 read contract. Retired device
// metadata is accepted here without restoring it to observations or output.
type legacyInventoryInput struct {
	Software       *releases.Release   `json:"software,omitempty" yaml:"software,omitempty"`
	AddressLookup  *AddressLookup      `json:"addressLookup,omitempty" yaml:"addressLookup,omitempty"`
	FormatVersion  string              `json:"formatVersion" yaml:"formatVersion"`
	Collection     Collection          `json:"collection" yaml:"collection"`
	RequestedScope []ScopeEndpoint     `json:"requestedScope" yaml:"requestedScope"`
	ScopeDigest    string              `json:"scopeDigest,omitempty" yaml:"scopeDigest,omitempty"`
	Fingerprint    string              `json:"fingerprint,omitempty" yaml:"fingerprint,omitempty"`
	Management     *Management         `json:"management,omitempty" yaml:"management,omitempty"`
	DPUs           []legacyDeviceInput `json:"dpus" yaml:"dpus"`
	Hosts          []legacyDeviceInput `json:"hosts" yaml:"hosts"`
	Associations   []Association       `json:"associations" yaml:"associations"`
	Probes         []ProbeResult       `json:"probes,omitempty" yaml:"probes,omitempty"`
	Evidence       map[string]Evidence `json:"evidence,omitempty" yaml:"evidence,omitempty"`
}

type legacyDeviceInput struct {
	Device                `yaml:",inline"`
	Qualification         string `json:"qualification,omitempty" yaml:"qualification,omitempty"`
	InventoryCompleteness string `json:"inventoryCompleteness,omitempty" yaml:"inventoryCompleteness,omitempty"`
}

func (wire legacyInventoryInput) inventory() Inventory {
	return Inventory{
		Software: wire.Software, AddressLookup: wire.AddressLookup,
		FormatVersion: wire.FormatVersion, Collection: wire.Collection,
		RequestedScope: wire.RequestedScope, ScopeDigest: wire.ScopeDigest,
		Fingerprint: wire.Fingerprint, Management: wire.Management,
		DPUs: legacyInputDevices(wire.DPUs), Hosts: legacyInputDevices(wire.Hosts),
		Associations: wire.Associations, Probes: wire.Probes, Evidence: wire.Evidence,
	}
}

func legacyInputDevices(devices []legacyDeviceInput) []Device {
	if devices == nil {
		return nil
	}
	result := make([]Device, len(devices))
	for index, device := range devices {
		result[index] = device.Device
	}
	return result
}
