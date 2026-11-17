// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package inventory

// HardwareObservation keeps PCI-domain-local facts separate from device identity.
// A host or ARM may expose multiple independent cards; PCI addresses are not
// comparable across those domains.
type HardwareObservation struct {
	Cards []CardHardware `json:"cards" yaml:"cards"`
	RShim Observation    `json:"rshim" yaml:"rshim"`
}

type CardHardware struct {
	PCIAddress   string                 `json:"pciAddress" yaml:"pciAddress"`
	Domain       string                 `json:"domain" yaml:"domain"`
	Model        string                 `json:"model" yaml:"model"`
	SerialNumber string                 `json:"serialNumber,omitempty" yaml:"serialNumber,omitempty"`
	Firmware     Observation            `json:"firmware" yaml:"firmware"`
	PSID         Observation            `json:"psid" yaml:"psid"`
	NVConfig     map[string]Observation `json:"nvConfig" yaml:"nvConfig"`
	Privilege    Observation            `json:"privilege" yaml:"privilege"`
	EvidenceRefs []string               `json:"evidenceRefs" yaml:"evidenceRefs"`
}

type NetworkObservation struct {
	Links     []NetworkLink       `json:"links" yaml:"links"`
	Resolver  ResolverObservation `json:"resolver" yaml:"resolver"`
	Neighbors []Neighbor          `json:"neighbors" yaml:"neighbors"`
	LLDP      []LLDPNeighbor      `json:"lldp" yaml:"lldp"`
}

type NetworkLink struct {
	Name   string `json:"name" yaml:"name"`
	MAC    string `json:"mac,omitempty" yaml:"mac,omitempty"`
	Master string `json:"master,omitempty" yaml:"master,omitempty"`
	Kind   string `json:"kind,omitempty" yaml:"kind,omitempty"`
}

type ResolverObservation struct {
	Owner           string              `json:"owner" yaml:"owner"`
	Path            string              `json:"path,omitempty" yaml:"path,omitempty"`
	Search          []string            `json:"search" yaml:"search"`
	PerLink         map[string][]string `json:"perLink" yaml:"perLink"`
	Domains         map[string][]string `json:"domains" yaml:"domains"`
	DNSMasqNoResolv *bool               `json:"dnsmasqNoResolv,omitempty" yaml:"dnsmasqNoResolv,omitempty"`
	DNSMasqServers  []string            `json:"dnsmasqServers" yaml:"dnsmasqServers"`
}

// Neighbor is an observed address candidate, never device identity or authority
// to connect. Its state and collection vantage must be retained by consumers.
type Neighbor struct {
	Address   string   `json:"address" yaml:"address"`
	MAC       string   `json:"mac,omitempty" yaml:"mac,omitempty"`
	Interface string   `json:"interface" yaml:"interface"`
	State     []string `json:"state" yaml:"state"`
}

type LLDPNeighbor struct {
	Interface string `json:"interface" yaml:"interface"`
	Chassis   string `json:"chassis,omitempty" yaml:"chassis,omitempty"`
	Port      string `json:"port,omitempty" yaml:"port,omitempty"`
}
