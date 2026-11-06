// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"cmp"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
)

// HostCards preserves host-reported BlueField identity when no independent DPU
// endpoint was requested. It does not manufacture ARM or BMC endpoint bindings.
func HostCards(hosts []inventory.Device) []inventory.Device {
	devices := []inventory.Device{}
	for _, host := range hosts {
		if host.ObservationState != "observed" {
			continue
		}
		for _, card := range host.Adapters {
			if !strings.Contains(strings.ToLower(card.Model), "bluefield") {
				continue
			}
			d := inventory.Device{
				RecordKey: inventory.Digest("dpf-host-card-v1", []string{host.ID, card.Key}),
				InputRef:  host.InputRef, Model: card.Model, SerialNumber: card.SerialNumber,
				SerialNumberRaw: card.SerialNumberRaw, ReaderAdapter: "host-card-v1",
				ObservationState: "observed", Qualification: "unqualified", InventoryCompleteness: "partial",
				Authentication:  inventory.Authentication{PasswordRead: "not-evaluated", ClientCertificateSupport: "unknown", DPFClientAuthentication: "not-evaluated"},
				BMCAddressState: "unknown", EvidenceRefs: slices.Clone(card.EvidenceRefs),
			}
			if redfish.ValidSerial(card.SerialNumber) {
				d.IdentityKey = []string{"nvidia", card.SerialNumber}
				d.ID = inventory.PhysicalID("dpu", d.IdentityKey)
			} else {
				d.ReasonCode = "CARD_SERIAL_MISSING"
			}
			devices = append(devices, d)
		}
	}
	return devices
}

func canonicalRuntime(r inventory.RuntimeObservation) inventory.RuntimeObservation {
	r.Interfaces = slices.Clone(r.Interfaces)
	for i := range r.Interfaces {
		r.Interfaces[i].Addresses = slices.Clone(r.Interfaces[i].Addresses)
		slices.Sort(r.Interfaces[i].Addresses)
	}
	slices.SortFunc(r.Interfaces, func(a, b inventory.Interface) int {
		return cmp.Or(cmp.Compare(a.Index, b.Index), strings.Compare(a.Name, b.Name))
	})
	r.Routes = slices.Clone(r.Routes)
	slices.SortFunc(r.Routes, func(a, b inventory.Route) int {
		return cmp.Or(cmp.Compare(a.Table, b.Table), strings.Compare(a.Destination, b.Destination), strings.Compare(a.Gateway, b.Gateway), cmp.Compare(a.InterfaceIndex, b.InterfaceIndex))
	})
	r.DNS = slices.Clone(r.DNS)
	slices.Sort(r.DNS)
	if r.Network != nil {
		n := *r.Network
		n.Links = slices.Clone(n.Links)
		slices.SortFunc(n.Links, func(a, b inventory.NetworkLink) int { return strings.Compare(a.Name, b.Name) })
		n.Neighbors = slices.Clone(n.Neighbors)
		for i := range n.Neighbors {
			n.Neighbors[i].State = slices.Clone(n.Neighbors[i].State)
			slices.Sort(n.Neighbors[i].State)
		}
		slices.SortFunc(n.Neighbors, func(a, b inventory.Neighbor) int {
			return cmp.Or(strings.Compare(a.Interface, b.Interface), strings.Compare(a.Address, b.Address), strings.Compare(a.MAC, b.MAC), slices.Compare(a.State, b.State))
		})
		n.LLDP = slices.Clone(n.LLDP)
		slices.SortFunc(n.LLDP, func(a, b inventory.LLDPNeighbor) int {
			return cmp.Or(strings.Compare(a.Interface, b.Interface), strings.Compare(a.Chassis, b.Chassis), strings.Compare(a.Port, b.Port))
		})
		r.Network = &n
	}
	if r.Hardware != nil {
		h := *r.Hardware
		h.Cards = slices.Clone(h.Cards)
		slices.SortFunc(h.Cards, func(a, b inventory.CardHardware) int {
			return cmp.Or(strings.Compare(a.Domain, b.Domain), strings.Compare(a.PCIAddress, b.PCIAddress))
		})
		r.Hardware = &h
	}
	return r
}

func canonicalFunctions(functions []inventory.PCIFunction) []inventory.PCIFunction {
	out := slices.Clone(functions)
	for i := range out {
		out[i].Netdevs = slices.Clone(out[i].Netdevs)
		slices.Sort(out[i].Netdevs)
	}
	slices.SortFunc(out, func(a, b inventory.PCIFunction) int { return strings.Compare(a.Address, b.Address) })
	return out
}
