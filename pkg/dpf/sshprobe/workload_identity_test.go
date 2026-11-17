// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("selected host workload network identity", func() {
	It("selects the explicit p0 PCI and permanent-MAC mapping independent of list order", func() {
		host := hostWorkloadIdentityFixture()
		host.Adapters[0].PCIFunctions[0], host.Adapters[0].PCIFunctions[1] = host.Adapters[0].PCIFunctions[1], host.Adapters[0].PCIFunctions[0]
		host.Runtime.Network.Links[0], host.Runtime.Network.Links[1] = host.Runtime.Network.Links[1], host.Runtime.Network.Links[0]
		before := cloneHostWorkloadIdentityFixture(host)

		identity, err := SelectHostWorkloadNetworkIdentity(host, "card-key", "CARD000123")
		Expect(err).NotTo(HaveOccurred())
		Expect(identity).To(Equal(HostNetworkIdentity{
			SystemUUID: "12345678-1234-1234-1234-123456789abc", AdapterSerial: "CARD000123",
			PCIAddress: "0000:21:00.0", InterfaceName: "ens5f0np0", PermanentMAC: "20:4d:52:3e:9a:b8", PhysicalPort: "p0",
		}))
		Expect(host).To(Equal(before), "selector must not mutate inventory")
	})

	It("follows the explicit p0 label rather than assuming PCI function zero", func() {
		host := hostWorkloadIdentityFixture()
		host.Runtime.Network.Links[0].PhysicalPortName = "p1"
		host.Runtime.Network.Links[1].PhysicalPortName = "p0"
		identity, err := SelectHostWorkloadNetworkIdentity(host, "card-key", "CARD000123")
		Expect(err).NotTo(HaveOccurred())
		Expect(identity.PCIAddress).To(Equal("0000:21:00.1"))
		Expect(identity.InterfaceName).To(Equal("ens5f1np1"))
		Expect(identity.PermanentMAC).To(Equal("20:4d:52:3e:9a:b9"))
	})

	It("canonicalizes case-insensitive PCI and permanent MAC facts", func() {
		host := hostWorkloadIdentityFixture()
		host.Adapters[0].PCIAddress = "000A:2B:0C.0"
		host.Adapters[0].PCIFunctions[0].Address = "000A:2B:0C.0"
		host.Adapters[0].PCIFunctions[1].Address = "000A:2B:0C.1"
		host.Runtime.Network.Links[0].PCIAddress = "000A:2B:0C.0"
		host.Runtime.Network.Links[1].PCIAddress = "000A:2B:0C.1"
		host.Runtime.Network.Links[0].PermanentMAC = "20:4D:52:3E:9A:B8"
		identity, err := SelectHostWorkloadNetworkIdentity(host, "card-key", "CARD000123")
		Expect(err).NotTo(HaveOccurred())
		Expect(identity.PCIAddress).To(Equal("000a:2b:0c.0"))
		Expect(identity.PermanentMAC).To(Equal("20:4d:52:3e:9a:b8"))
	})

	It("rejects incomplete, duplicate, conflicting, virtual, or weakly bound identity facts", func() {
		cases := []struct {
			name   string
			mutate func(*inventory.Device)
		}{
			{name: "missing adapter", mutate: func(h *inventory.Device) { h.Adapters = nil }},
			{name: "duplicate key with conflicting serial", mutate: func(h *inventory.Device) {
				h.Adapters = append(h.Adapters, inventory.Adapter{Key: "card-key", SerialNumber: "OTHER", IdentityState: "known", PCIAddress: "0000:41:00.0"})
			}},
			{name: "duplicate serial with conflicting key", mutate: func(h *inventory.Device) {
				h.Adapters = append(h.Adapters, inventory.Adapter{Key: "other-key", SerialNumber: "CARD000123", IdentityState: "known", PCIAddress: "0000:41:00.0"})
			}},
			{name: "same PCI slot assigned to another adapter", mutate: func(h *inventory.Device) {
				h.Adapters = append(h.Adapters, inventory.Adapter{Key: "other-key", SerialNumber: "OTHER", IdentityState: "known", PCIAddress: "0000:21:00.2"})
			}},
			{name: "uppercase other-adapter PCI alias to selected slot", mutate: func(h *inventory.Device) {
				h.Adapters[0].PCIAddress = "000A:2B:0C.0"
				h.Adapters[0].PCIFunctions[0].Address = "000A:2B:0C.0"
				h.Adapters[0].PCIFunctions[1].Address = "000A:2B:0C.1"
				h.Runtime.Network.Links[0].PCIAddress = "000A:2B:0C.0"
				h.Runtime.Network.Links[1].PCIAddress = "000A:2B:0C.1"
				h.Adapters = append(h.Adapters, inventory.Adapter{Key: "other-key", SerialNumber: "OTHER", IdentityState: "known", PCIAddress: "000B:2C:0D.0",
					PCIFunctions: []inventory.PCIFunction{{Address: "000A:2B:0C.2", Driver: "mlx5_core", Netdevs: []string{"other0"}}}})
			}},
			{name: "selected netdev claimed by another adapter", mutate: func(h *inventory.Device) {
				h.Adapters = append(h.Adapters, inventory.Adapter{Key: "other-key", SerialNumber: "OTHER", IdentityState: "known", PCIAddress: "0000:41:00.0",
					PCIFunctions: []inventory.PCIFunction{{Address: "0000:41:00.0", Driver: "mlx5_core", Netdevs: []string{"ens5f0np0"}}}})
			}},
			{name: "invalid adapter PCI root", mutate: func(h *inventory.Device) { h.Adapters[0].PCIAddress = "not-a-pci-address" }},
			{name: "adapter PCI root absent from functions", mutate: func(h *inventory.Device) { h.Adapters[0].PCIAddress = "0000:21:00.2" }},
			{name: "invalid function PCI", mutate: func(h *inventory.Device) { h.Adapters[0].PCIFunctions[0].Address = "not-a-pci-address" }},
			{name: "function from another card slot", mutate: func(h *inventory.Device) { h.Adapters[0].PCIFunctions[1].Address = "0000:22:00.1" }},
			{name: "duplicate function identity", mutate: func(h *inventory.Device) {
				h.Adapters[0].PCIFunctions[1].Address = h.Adapters[0].PCIFunctions[0].Address
			}},
			{name: "ambiguous netdev ownership", mutate: func(h *inventory.Device) {
				h.Adapters[0].PCIFunctions[1].Netdevs = append(h.Adapters[0].PCIFunctions[1].Netdevs, "ens5f0np0")
			}},
			{name: "duplicate netdev entry", mutate: func(h *inventory.Device) {
				h.Adapters[0].PCIFunctions[0].Netdevs = append(h.Adapters[0].PCIFunctions[0].Netdevs, "ens5f0np0")
			}},
			{name: "missing runtime links", mutate: func(h *inventory.Device) { h.Runtime = nil }},
			{name: "no exact p0", mutate: func(h *inventory.Device) { h.Runtime.Network.Links[0].PhysicalPortName = "" }},
			{name: "duplicate p0", mutate: func(h *inventory.Device) { h.Runtime.Network.Links[1].PhysicalPortName = "p0" }},
			{name: "missing PCI to netdev mapping", mutate: func(h *inventory.Device) { h.Runtime.Network.Links[0].Name = "unknown0" }},
			{name: "conflicting PCI to netdev mapping", mutate: func(h *inventory.Device) { h.Runtime.Network.Links[0].PCIAddress = "0000:21:00.1" }},
			{name: "unsupported driver", mutate: func(h *inventory.Device) { h.Adapters[0].PCIFunctions[0].Driver = "ixgbe" }},
			{name: "virtual master", mutate: func(h *inventory.Device) { h.Runtime.Network.Links[0].Master = "br0" }},
			{name: "virtual link kind", mutate: func(h *inventory.Device) { h.Runtime.Network.Links[0].Kind = "bridge" }},
			{name: "permanent MAC is required", mutate: func(h *inventory.Device) { h.Runtime.Network.Links[0].PermanentMAC = "" }},
			{name: "current MAC is not a substitute", mutate: func(h *inventory.Device) {
				h.Runtime.Network.Links[0].PermanentMAC = ""
				h.Runtime.Network.Links[0].MAC = "02:00:00:00:00:01"
			}},
			{name: "invalid permanent MAC", mutate: func(h *inventory.Device) { h.Runtime.Network.Links[0].PermanentMAC = "01:00:00:00:00:01" }},
		}
		for _, tc := range cases {
			By(tc.name)
			host := hostWorkloadIdentityFixture()
			tc.mutate(&host)
			_, err := SelectHostWorkloadNetworkIdentity(host, "card-key", "CARD000123")
			Expect(err).To(HaveOccurred(), tc.name)
		}
	})
})

func TestWorkloadIdentitySelectorRejectsInvalidHostOrSelector(t *testing.T) {
	host := hostWorkloadIdentityFixture()
	for _, tc := range []struct{ key, serial, uuid string }{
		{key: "", serial: "CARD000123", uuid: testUUID},
		{key: "card-key", serial: "", uuid: testUUID},
		{key: "card-key", serial: "CARD000123", uuid: "not-a-uuid"},
	} {
		candidate := host
		candidate.SystemUUID = tc.uuid
		if _, err := SelectHostWorkloadNetworkIdentity(candidate, tc.key, tc.serial); err == nil {
			t.Fatalf("selector %q/%q host UUID %q unexpectedly succeeded", tc.key, tc.serial, tc.uuid)
		}
	}
}

func hostWorkloadIdentityFixture() inventory.Device {
	return inventory.Device{
		SystemUUID: testUUID,
		Adapters: []inventory.Adapter{{
			Key: "card-key", SerialNumber: "CARD000123", IdentityState: "known", PCIAddress: "0000:21:00.0",
			PCIFunctions: []inventory.PCIFunction{
				{Address: "0000:21:00.0", Driver: "mlx5_core", Netdevs: []string{"ens5f0np0"}},
				{Address: "0000:21:00.1", Driver: "mlx5_core", Netdevs: []string{"ens5f1np1"}},
			},
		}},
		Runtime: &inventory.RuntimeObservation{Network: &inventory.NetworkObservation{Links: []inventory.NetworkLink{
			{Name: "ens5f0np0", PCIAddress: "0000:21:00.0", PhysicalPortName: "p0", MAC: "02:00:00:00:00:01", PermanentMAC: "20:4d:52:3e:9a:b8"},
			{Name: "ens5f1np1", PCIAddress: "0000:21:00.1", PhysicalPortName: "p1", MAC: "02:00:00:00:00:02", PermanentMAC: "20:4d:52:3e:9a:b9"},
		}}},
	}
}

func cloneHostWorkloadIdentityFixture(d inventory.Device) inventory.Device {
	copy := d
	copy.Adapters = append([]inventory.Adapter(nil), d.Adapters...)
	for i := range copy.Adapters {
		copy.Adapters[i].PCIFunctions = append([]inventory.PCIFunction(nil), d.Adapters[i].PCIFunctions...)
		for j := range copy.Adapters[i].PCIFunctions {
			copy.Adapters[i].PCIFunctions[j].Netdevs = append([]string(nil), d.Adapters[i].PCIFunctions[j].Netdevs...)
		}
	}
	if d.Runtime != nil {
		runtime := *d.Runtime
		if d.Runtime.Network != nil {
			network := *d.Runtime.Network
			network.Links = append([]inventory.NetworkLink(nil), d.Runtime.Network.Links...)
			runtime.Network = &network
		}
		copy.Runtime = &runtime
	}
	return copy
}
