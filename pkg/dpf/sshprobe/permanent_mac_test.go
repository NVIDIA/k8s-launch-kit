// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"context"
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const permanentMACStage = "host-interface:ens5f0np0:permanent-mac"

func permanentMACFixture() (inventory.Device, *fakeRunner) {
	return inventory.Device{
			Adapters: []inventory.Adapter{{PCIFunctions: []inventory.PCIFunction{{Address: "0000:21:00.0", Netdevs: []string{"ens5f0np0"}}}}},
			Runtime: &inventory.RuntimeObservation{
				Network:      &inventory.NetworkObservation{Links: []inventory.NetworkLink{{Name: "ens5f0np0", MAC: "02:00:00:00:00:99", PermanentMAC: "02:00:00:00:00:98"}}},
				Observations: map[string]inventory.Observation{},
			},
		}, &fakeRunner{values: map[string]string{
			permanentMACStage + ":binding-before": "/sys/devices/pci0000:20/0000:21:00.0\n",
			permanentMACStage:                     "Permanent address: 02:00:00:00:00:01\n",
			permanentMACStage + ":binding-after":  "/sys/devices/pci0000:20/0000:21:00.0\n",
		}, errors: map[string]error{}}
}

var _ = Describe("host permanent MAC discovery", func() {
	It("records a separate permanent address only after both PCI binding reads", func() {
		device, remote := permanentMACFixture()
		var progress strings.Builder
		ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&progress))
		collectHostPermanentMACs(ctx, remote, &device, "host-a")
		Expect(device.Runtime.Network.Links[0].PermanentMAC).To(Equal("02:00:00:00:00:01"))
		Expect(device.Runtime.Network.Links[0].MAC).To(Equal("02:00:00:00:00:99"))
		Expect(device.Runtime.Network.Links[0].PCIAddress).To(Equal("0000:21:00.0"))
		Expect(device.Runtime.Observations[permanentMACStage].State).To(Equal("observed"))
		Expect(progress.String()).To(ContainSubstring("host-a: interface ens5f0np0 permanent MAC and PCI binding verified"))
		Expect(remote.calls).To(Equal([]string{
			permanentMACStage + ":binding-before readlink -f /sys/class/net/ens5f0np0/device",
			permanentMACStage + " ethtool -P ens5f0np0",
			permanentMACStage + ":binding-after readlink -f /sys/class/net/ens5f0np0/device",
		}))
	})
	It("keeps tool failures explicit and never falls back to the current MAC", func() {
		device, remote := permanentMACFixture()
		remote.errors[permanentMACStage] = &Error{Code: "TOOL_NOT_AVAILABLE", Stage: permanentMACStage, Tool: "ethtool"}
		collectHostPermanentMACs(context.Background(), remote, &device, "host-a")
		Expect(device.Runtime.Network.Links[0].PermanentMAC).To(BeEmpty())
		observation := device.Runtime.Observations[permanentMACStage]
		Expect(observation.State).To(Equal("unavailable"))
		Expect(observation.ReasonCode).To(Equal("TOOL_NOT_AVAILABLE"))
		Expect(observation.Message).NotTo(BeEmpty())
		Expect(observation.Tool).To(Equal("ethtool"))
		Expect(remote.calls).To(HaveLen(2))
	})
	It("rejects changed and malformed PCI bindings before publishing the address", func() {
		for _, stage := range []string{":binding-before", ":binding-after"} {
			for _, value := range []string{
				"/sys/devices/pci0000:20/0000:21:00.1\n",
				"/tmp/0000:21:00.0\n",
				"/sys/devices/../other/0000:21:00.0\n",
				"/sys/devices/pci0000:20/0000:21:00.0\nextra\n",
				"/sys/devices/pci0000:20/\x000000:21:00.0",
			} {
				device, remote := permanentMACFixture()
				remote.values[permanentMACStage+stage] = value
				collectHostPermanentMACs(context.Background(), remote, &device, "host-a")
				Expect(device.Runtime.Network.Links[0].PermanentMAC).To(BeEmpty(), value)
				Expect(device.Runtime.Observations[permanentMACStage].ReasonCode).To(Equal("INVENTORY_CHANGED"), value)
			}
		}
	})
	It("refuses ambiguous links, PCI claims and unsafe interface names without running commands", func() {
		for _, edit := range []func(*inventory.Device){
			func(d *inventory.Device) {
				d.Adapters[0].PCIFunctions = append(d.Adapters[0].PCIFunctions, d.Adapters[0].PCIFunctions[0])
			},
			func(d *inventory.Device) {
				d.Runtime.Network.Links = append(d.Runtime.Network.Links, d.Runtime.Network.Links[0])
			},
			func(d *inventory.Device) { d.Adapters[0].PCIFunctions[0].Address = "invalid" },
			func(d *inventory.Device) {
				d.Adapters[0].PCIFunctions[0].Netdevs = []string{"ens5f0np0;id"}
				d.Runtime.Network.Links[0].Name = "ens5f0np0;id"
			},
			func(d *inventory.Device) {
				d.Adapters[0].PCIFunctions[0].Netdevs = []string{"-device"}
				d.Runtime.Network.Links[0].Name = "-device"
			},
			func(d *inventory.Device) {
				d.Adapters[0].PCIFunctions[0].Netdevs = []string{"."}
				d.Runtime.Network.Links[0].Name = "."
			},
			func(d *inventory.Device) {
				d.Adapters[0].PCIFunctions[0].Netdevs = []string{".."}
				d.Runtime.Network.Links[0].Name = ".."
			},
		} {
			device, remote := permanentMACFixture()
			edit(&device)
			collectHostPermanentMACs(context.Background(), remote, &device, "host-a")
			for _, link := range device.Runtime.Network.Links {
				Expect(link.PermanentMAC).To(BeEmpty())
			}
			Expect(remote.calls).To(BeEmpty())
			for _, observation := range device.Runtime.Observations {
				Expect(observation.ReasonCode).To(Equal("HOST_INTERFACE_BINDING_UNAVAILABLE"))
			}
		}
	})
	It("preserves an explicit contradictory PCI observation without replacing it", func() {
		device, remote := permanentMACFixture()
		device.Runtime.Network.Links[0].PCIAddress = "0000:41:00.0"
		collectHostPermanentMACs(context.Background(), remote, &device, "host-a")
		Expect(device.Runtime.Network.Links[0].PCIAddress).To(Equal("0000:41:00.0"))
		Expect(device.Runtime.Network.Links[0].PermanentMAC).To(BeEmpty())
		Expect(device.Runtime.Observations[permanentMACStage].ReasonCode).To(Equal("HOST_INTERFACE_BINDING_CONFLICT"))
		Expect(remote.calls).To(BeEmpty())
	})
	It("bounds interface claims without publishing stale values or running commands", func() {
		device, remote := permanentMACFixture()
		for i := 0; i < maxHostMACInterfaces; i++ {
			device.Adapters[0].PCIFunctions[0].Netdevs = append(device.Adapters[0].PCIFunctions[0].Netdevs, fmt.Sprintf("extra%d", i))
		}
		collectHostPermanentMACs(context.Background(), remote, &device, "host-a")
		Expect(device.Runtime.Network.Links[0].PermanentMAC).To(BeEmpty())
		Expect(device.Runtime.Observations["host-interfaces:permanent-mac"].ReasonCode).To(Equal("HOST_INTERFACE_INVENTORY_LIMIT"))
		Expect(remote.calls).To(BeEmpty())
	})
	It("collects the optional host source inside the full physical identity fence", func() {
		remote := hostRunner()
		remote.values["network-links"] = `[{"ifname":"ens5f0np0","address":"02:00:00:00:00:99"}]`
		remote.values["network-link-details"] = remote.values["network-links"]
		remote.values["host-pf:0000:21:00.0:netdevs"] = "ens5f0np0\n"
		_, source := permanentMACFixture()
		for stage, value := range source.values {
			remote.values[stage] = value
		}
		device, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, remote)
		Expect(err).NotTo(HaveOccurred())
		Expect(device.Runtime.Network.Links[0].PermanentMAC).To(Equal("02:00:00:00:00:01"))
		Expect(strings.Join(remote.calls, "\n")).To(ContainSubstring("host-dmi-recheck"))
	})
})

var _ = Describe("permanent MAC response parsing", func() {
	It("normalizes an actual EUI-48 response", func() {
		value, err := parsePermanentMAC("Permanent address: 20:4D:52:3E:9A:B8\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(value).To(Equal("20:4d:52:3e:9a:b8"))
	})
	It("rejects missing, unknown, multicast, zero, non-EUI-48 and multiline responses", func() {
		for _, text := range []string{"", "not set", "02:00:00:00:00:01", "Permanent address: 00:00:00:00:00:00", "Permanent address: 01:00:00:00:00:01", "Permanent address: 00:00:00:00:00:00:00:01", "Permanent address: 02:00:00:00:00:01\nextra"} {
			value, err := parsePermanentMAC(text)
			Expect(err).To(HaveOccurred(), text)
			Expect(value).To(BeEmpty())
		}
	})
})
