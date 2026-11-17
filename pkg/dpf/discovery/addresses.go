// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/addresslookup"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"slices"
	"time"
)

func collectAddresses(r config.Request, inv *inventory.Inventory) {
	if r.AddressLookup == nil {
		return
	}
	neighbors := []addresslookup.Neighbor{}
	for _, group := range [][]inventory.Device{inv.Hosts, inv.DPUs} {
		for _, d := range group {
			if d.Runtime == nil || d.Runtime.Network == nil {
				continue
			}
			ref := ""
			for id, e := range inv.Evidence {
				if e.InputRef == d.InputRef && e.ResourceURI == "neighbors" {
					ref = id
					break
				}
			}
			for _, n := range d.Runtime.Network.Neighbors {
				state := ""
				if slices.Contains(n.State, "REACHABLE") {
					state = "REACHABLE"
				} else if slices.Contains(n.State, "PERMANENT") {
					state = "PERMANENT"
				}
				// Require the collector's authenticated provenance, not an address alone.
				if ref != "" {
					neighbors = append(neighbors, addresslookup.Neighbor{Address: n.Address, MAC: n.MAC, State: state, Source: d.RecordKey + "/" + n.Interface, EvidenceRef: ref})
				}
			}
		}
	}
	lookup, ev := addresslookup.Collect(*r.AddressLookup, time.Now().UTC(), config.ReadReference, neighbors)
	inv.AddressLookup = lookup
	for id, e := range ev {
		inv.Evidence[id] = e
	}
}
