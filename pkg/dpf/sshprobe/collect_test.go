// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package sshprobe

import (
	"context"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestSSHProbe(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "DPF SSH discovery") }

const testUUID = "12345678-1234-1234-1234-123456789abc"
const testSerial = "CARD000123"
const testFRU = `FRU Device Description : Builtin FRU Device (ID 0)
 Board Mfg : Nvidia
 Board Product : B4240
 Board Serial : CARD000123
 Product Manufacturer : Nvidia
 Product Name : BlueField-4
 Product Serial : CARD000123
`
const testLAN = `Set in Progress : Set Complete
Auth Type Enable : Callback :
                 : User :
                 : Operator :
IP Address Source : DHCP Address
IP Address : 192.0.2.42
MAC Address : 02:00:00:00:00:42
`
const testPCI = `0000:21:00.0 Ethernet controller [0200]: Vendor BlueField-4 [15b3:a2df]
0000:21:00.1 Ethernet controller [0200]: Vendor BlueField-4 [15b3:a2df]
0000:41:00.0 Ethernet controller [0200]: Vendor ConnectX-9 [15b3:1025]
`

type fakeRunner struct {
	values map[string]string
	errors map[string]error
	calls  []string
}

func (r *fakeRunner) Run(_ context.Context, stage, command string) (string, error) {
	r.calls = append(r.calls, stage+" "+command)
	if err, ok := r.errors[stage]; ok {
		return "", err
	}
	if value, ok := r.values[stage]; ok {
		return value, nil
	}
	return "", &Error{Code: "SSH_COMMAND_FAILED", Stage: stage}
}

func armRunner() *fakeRunner {
	return &fakeRunner{values: map[string]string{
		"arm-dmi": testSerial + "\n", "arm-fru": testFRU, "arm-ipmi-lan": testLAN,
		"arm-dmi-recheck": testSerial + "\n", "arm-fru-recheck": testFRU,
		"hostname": "arm.example\n", "os": "PRETTY_NAME=\"Example Linux\"\n",
		"interfaces": `[{"ifindex":2,"ifname":"oob_net0","mtu":1500,"addr_info":[{"local":"192.0.2.41","prefixlen":24}]}]`,
		"routes":     `[{"dst":"default","gateway":"192.0.2.1","dev":"oob_net0"}]`,
		"dns":        "nameserver 192.0.2.53\n",
	}, errors: map[string]error{}, calls: []string{}}
}

func hostRunner() *fakeRunner {
	return &fakeRunner{values: map[string]string{
		"host-dmi": testUUID, "host-dmi-recheck": testUUID,
		"host-pci": testPCI, "host-pci-recheck": testPCI,
		"host-vpd:0000:21:00.0":         "Physical Slot: 5\n [SN] Serial number: " + testSerial,
		"host-vpd-recheck:0000:21:00.0": "Physical Slot: 5\n [SN] Serial number: " + testSerial,
		"host-vpd:0000:41:00.0":         "Capabilities: <access denied>",
	}, errors: map[string]error{}, calls: []string{}}
}

var _ = Describe("SSH inventory observations", func() {
	It("corroborates ARM identity and observes a BMC address without contacting it", func() {
		r := armRunner()
		d, e, err := collect(context.Background(), config.Target{Role: "dpus", Endpoint: "ssh://192.0.2.41:22", InputRef: "/discovery/dpus/0", SSH: &config.SSHAccess{Sudo: true}}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.SerialNumber).To(Equal(testSerial))
		Expect(d.BMCEndpoint).To(Equal("https://192.0.2.42"))
		Expect(d.BMCAddressState).To(Equal("observed-local-ipmi"))
		Expect(d.ObservationState).To(Equal("observed"))
		Expect(d.Qualification).To(Equal("unqualified"))
		Expect(d.Authentication.DPFClientAuthentication).To(Equal("not-evaluated"))
		Expect(d.Runtime.Interfaces).To(HaveLen(1))
		Expect(d.Runtime.Routes[0].InterfaceIndex).To(Equal(2))
		Expect(d.Runtime.Routes[0].Destination).To(Equal("0.0.0.0/0"))
		Expect(e).NotTo(BeEmpty())
		Expect(strings.Join(r.calls, "\n")).NotTo(ContainSubstring("192.0.2.42"))
		Expect(r.calls).To(ContainElement("arm-fru sudo -n ipmitool fru print 0"))
	})
	It("preserves ARM identity with an unavailable optional BMC address", func() {
		r := armRunner()
		r.errors["arm-ipmi-lan"] = &Error{Code: "SSH_COMMAND_FAILED", Stage: "arm-ipmi-lan"}
		d, _, err := collect(context.Background(), config.Target{Role: "dpus", Endpoint: "ssh://192.0.2.41:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.ID).NotTo(BeEmpty())
		Expect(d.BMCEndpoint).To(BeEmpty())
		Expect(d.Runtime.Observations["bmcAddress"].State).To(Equal("unavailable"))
	})
	It("rejects differing ARM identity sources without using hostname as identity", func() {
		r := armRunner()
		r.values["arm-dmi"] = "OTHER-CARD"
		d, _, err := collect(context.Background(), config.Target{Role: "dpus", Endpoint: "ssh://192.0.2.41:22"}, r)
		Expect(Code(err)).To(Equal("SSH_IDENTITY_CONFLICT"))
		Expect(d.ID).To(BeEmpty())
		Expect(d.ObservationState).To(Equal("unavailable"))
	})
	It("rejects a physical identity change during collection", func() {
		r := armRunner()
		r.values["arm-dmi-recheck"] = "NEW-CARD"
		r.values["arm-fru-recheck"] = strings.ReplaceAll(testFRU, testSerial, "NEW-CARD")
		d, _, err := collect(context.Background(), config.Target{Role: "dpus", Endpoint: "ssh://192.0.2.41:22"}, r)
		Expect(Code(err)).To(Equal("INVENTORY_CHANGED"))
		Expect(d.ObservationState).To(Equal("unavailable"))
	})
	It("groups PCI functions by slot and retains cards with unknown serials", func() {
		r := hostRunner()
		d, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Adapters).To(HaveLen(2))
		Expect(d.SystemUUID).To(Equal(testUUID))
		Expect(d.Adapters[0].SerialNumber).To(Equal(testSerial))
		Expect(d.Adapters[0].Slot).To(Equal("5"))
		Expect(d.Adapters[1].SerialNumber).To(BeEmpty())
		Expect(d.Adapters[0].IdentityState).To(Equal("known"))
		Expect(d.Adapters[1].IdentityState).To(Equal("unknown"))
		Expect(d.InventoryCompleteness).To(Equal("partial"))
		Expect(d.Runtime.Observations["interfaces"].State).To(Equal("unavailable"))
	})
	It("does not combine separate PCI slots with the same serial", func() {
		r := hostRunner()
		r.values["host-vpd:0000:41:00.0"] = "[SN] Serial number: " + testSerial
		r.values["host-vpd-recheck:0000:41:00.0"] = "[SN] Serial number: " + testSerial
		d, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Adapters).To(HaveLen(2))
		Expect(d.Adapters[0].Key).NotTo(Equal(d.Adapters[1].Key))
	})
	It("retains distinct per-function driver, NUMA and netdev observations", func() {
		r := hostRunner()
		for i, bdf := range []string{"0000:21:00.0", "0000:21:00.1"} {
			r.values["host-pf:"+bdf+":driver"] = "../../../../bus/pci/drivers/mlx5_core\n"
			r.values["host-pf:"+bdf+":numa"] = "0\n"
			r.values["host-pf:"+bdf+":netdevs"] = []string{"hostpf0\n", "hostpf1\n"}[i]
		}
		d, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Adapters[0].PCIFunctions).To(HaveLen(2))
		Expect(d.Adapters[0].PCIFunctions[0].Netdevs).To(Equal([]string{"hostpf0"}))
		Expect(d.Adapters[0].PCIFunctions[1].Netdevs).To(Equal([]string{"hostpf1"}))
		Expect(d.Adapters[0].PCIFunctions[0].Driver).To(Equal("mlx5_core"))
		Expect(d.Adapters[0].PCIFunctions[0].NUMANode).To(Equal("0"))
		Expect(d.Runtime.Observations["host-pf:0000:41:00.0:driver"].State).To(Equal("unavailable"))
	})
	It("rejects host identity or topology changes", func() {
		for _, field := range []string{"host-dmi-recheck", "host-pci-recheck", "host-vpd-recheck:0000:21:00.0"} {
			r := hostRunner()
			r.values[field] = ""
			d, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
			Expect(err).To(HaveOccurred())
			Expect(d.ObservationState).To(Equal("unavailable"))
		}
	})
	It("rejects malformed and shell-bearing PCI output before any VPD command", func() {
		r := hostRunner()
		r.values["host-pci"] = "0000:21:00.0;touch /tmp/unwanted Ethernet controller [0200]: Vendor [15b3:a2df]"
		_, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
		Expect(Code(err)).To(Equal("SSH_PCI_INVENTORY_INVALID"))
		Expect(strings.Join(r.calls, "\n")).NotTo(ContainSubstring("host-vpd"))
	})
	It("rejects ambiguous FRUs, invalid identities and invalid BMC addresses", func() {
		_, _, _, err := armIdentity(testSerial, testFRU+"Product Serial: OTHER\n")
		Expect(err).To(HaveOccurred())
		for _, serial := range []string{"unknown", "000000", "\n", "line\nbreak"} {
			Expect(validSerial(serial)).To(BeFalse())
		}
		for _, uuid := range []string{"00000000-0000-0000-0000-000000000000", "ffffffff-ffff-ffff-ffff-ffffffffffff", "example"} {
			Expect(validUUID(uuid)).To(BeFalse())
		}
		for _, ip := range []string{"0.0.0.0", "127.0.0.1", "224.0.0.1", "192.0.2.1; echo bad"} {
			_, err := bmcAddress(strings.ReplaceAll(testLAN, "192.0.2.42", ip))
			Expect(err).To(HaveOccurred())
		}
	})
})
