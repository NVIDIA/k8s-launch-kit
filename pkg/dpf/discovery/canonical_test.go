// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Enriched observation ordering", func() {
	It("canonicalizes unordered facts without mutating source facts", func() {
		n1 := inventory.Neighbor{Address: "192.0.2.2", Interface: "eth1", State: []string{"REACHABLE", "PERMANENT"}}
		n2 := inventory.Neighbor{Address: "192.0.2.1", Interface: "eth0", State: []string{"STALE"}}
		r := inventory.RuntimeObservation{Network: &inventory.NetworkObservation{Neighbors: []inventory.Neighbor{n1, n2}, Links: []inventory.NetworkLink{{Name: "eth1"}, {Name: "eth0"}}}, Hardware: &inventory.HardwareObservation{Cards: []inventory.CardHardware{{PCIAddress: "0000:02:00.0", Domain: "host-pci"}, {PCIAddress: "0000:01:00.0", Domain: "host-pci"}}}}
		a := canonicalRuntime(r)
		Expect(r.Network.Neighbors[0].Address).To(Equal("192.0.2.2"))
		Expect(r.Network.Neighbors[0].State[0]).To(Equal("REACHABLE"))
		Expect(r.Hardware.Cards[0].PCIAddress).To(Equal("0000:02:00.0"))
		b := r
		b.Network = &inventory.NetworkObservation{Neighbors: []inventory.Neighbor{n2, n1}, Links: []inventory.NetworkLink{{Name: "eth0"}, {Name: "eth1"}}}
		b.Hardware = &inventory.HardwareObservation{Cards: []inventory.CardHardware{r.Hardware.Cards[1], r.Hardware.Cards[0]}}
		Expect(canonicalRuntime(b)).To(Equal(a))
	})
})
