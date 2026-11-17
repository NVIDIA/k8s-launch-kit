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
	HostLLDP  *HostLLDPCapture    `json:"hostLLDP,omitempty" yaml:"hostLLDP,omitempty"`
	Links     []NetworkLink       `json:"links" yaml:"links"`
	Resolver  ResolverObservation `json:"resolver" yaml:"resolver"`
	Neighbors []Neighbor          `json:"neighbors" yaml:"neighbors"`
	LLDP      []LLDPNeighbor      `json:"lldp" yaml:"lldp"`
}

type NetworkLink struct {
	// PhysicalPFRoots are measured through lower links and PCI physfn ancestry.
	PhysicalPFRoots  []string `json:"physicalPFRoots,omitempty" yaml:"physicalPFRoots,omitempty"`
	PCIAddress       string   `json:"pciAddress,omitempty" yaml:"pciAddress,omitempty"`
	PhysicalPortName string   `json:"physicalPortName,omitempty" yaml:"physicalPortName,omitempty"`
	Name             string   `json:"name" yaml:"name"`
	MAC              string   `json:"mac,omitempty" yaml:"mac,omitempty"`
	PermanentMAC     string   `json:"permanentMAC,omitempty" yaml:"permanentMAC,omitempty"`
	Master           string   `json:"master,omitempty" yaml:"master,omitempty"`
	Kind             string   `json:"kind,omitempty" yaml:"kind,omitempty"`
	// Zero means iproute did not report a usable MTU bound.
	MinMTU uint32 `json:"minMTU,omitempty" yaml:"minMTU,omitempty"`
	MaxMTU uint32 `json:"maxMTU,omitempty" yaml:"maxMTU,omitempty"`
}

// HostLLDPCaptureRequest binds a bounded packet capture to one already-selected
// host/card identity. It is an execution request, not serialized inventory.
type HostLLDPCaptureRequest struct {
	HostUUID          string
	AdapterKey        string
	AdapterSerial     string
	AdapterPCIAddress string
	Ports             []HostLLDPCapturePort
}

type HostLLDPCapturePort struct {
	PCIAddress string
	Interface  string
}

// HostLLDPCapture contains only parsed frames and selected identity, never the
// captured packet bytes. Per-port unknown results preserve partial observation.
type HostLLDPCapture struct {
	Ports []HostLLDPCaptureResult `json:"ports" yaml:"ports"`
}

type HostLLDPCaptureResult struct {
	PCIAddress       string             `json:"pciAddress" yaml:"pciAddress"`
	Interface        string             `json:"interface" yaml:"interface"`
	PhysicalPortName string             `json:"physicalPortName" yaml:"physicalPortName"`
	State            string             `json:"state,omitempty" yaml:"state,omitempty"`
	ReasonCode       string             `json:"reasonCode,omitempty" yaml:"reasonCode,omitempty"`
	Message          string             `json:"message,omitempty" yaml:"message,omitempty"`
	Neighbors        []HostLLDPNeighbor `json:"neighbors" yaml:"neighbors"`
}

type HostLLDPNeighbor struct {
	SourceMAC        string `json:"sourceMAC" yaml:"sourceMAC"`
	ChassisIDSubtype uint8  `json:"chassisIDSubtype" yaml:"chassisIDSubtype"`
	ChassisID        string `json:"chassisID" yaml:"chassisID"`
	PortIDSubtype    uint8  `json:"portIDSubtype" yaml:"portIDSubtype"`
	PortID           string `json:"portID" yaml:"portID"`
	SystemName       string `json:"systemName,omitempty" yaml:"systemName,omitempty"`
	PortDescription  string `json:"portDescription,omitempty" yaml:"portDescription,omitempty"`
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
