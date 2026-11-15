// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package sshprobe

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/crypto/ssh"
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
		"routes-v6":  `[]`,
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
	It("reconnects after an optional command timeout and completes the required DPU identity recheck", func() {
		for _, overflow := range []bool{false, true} {
			server := startServer()
			DeferCleanup(server.close)
			var mu sync.Mutex
			hostnameCalls, dmiCalls, fruCalls := 0, 0, 0
			server.mu.Lock()
			server.onCommand = func(ch ssh.Channel, command string) bool {
				mu.Lock()
				switch command {
				case "LC_ALL=C hostname":
					hostnameCalls++
					if hostnameCalls == 1 {
						mu.Unlock()
						if overflow {
							go func() { _, _ = ch.Write(make([]byte, maxOutput+1)) }()
						}
						time.Sleep(1300 * time.Millisecond)
						return true
					}
					mu.Unlock()
					return false
				case "LC_ALL=C dmidecode -s system-serial-number":
					dmiCalls++
				case "LC_ALL=C ipmitool fru print 0":
					fruCalls++
				default:
					mu.Unlock()
					return false
				}
				mu.Unlock()
				text := testSerial + "\n"
				if command == "LC_ALL=C ipmitool fru print 0" {
					text = testFRU
				}
				_, _ = ch.Write([]byte(text))
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				return true
			}
			server.mu.Unlock()
			runner, err := newReconnectingRunner(context.Background(), "ssh://"+server.listener.Addr().String(), server.auth(), time.Second)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(runner.Close)

			device, _, err := collect(context.Background(), config.Target{Role: "dpus", Endpoint: "ssh://" + server.listener.Addr().String()}, runner)
			Expect(err).NotTo(HaveOccurred())
			Expect(device.ObservationState).To(Equal("observed"))
			Expect(device.ID).NotTo(BeEmpty())
			Expect(device.Runtime.Observations["hostname"].ReasonCode).To(Equal("SSH_TIMEOUT_OR_CANCELLED"))
			mu.Lock()
			Expect(dmiCalls).To(Equal(2))
			Expect(fruCalls).To(Equal(2))
			mu.Unlock()
			server.mu.Lock()
			connectionCount := len(server.connections)
			server.mu.Unlock()
			Expect(connectionCount).To(Equal(2))
		}
	})
	It("keeps cancellation, changed identity, and reconnect trust/authentication failures fatal", func() {
		for _, scenario := range []string{"cancel", "identity-change", "auth-rejected", "host-key-changed"} {
			By(scenario)
			server := startServer()
			DeferCleanup(server.close)
			ctx, cancel := context.WithCancel(context.Background())
			DeferCleanup(cancel)
			var alternate ssh.Signer
			if scenario == "host-key-changed" {
				_, private, err := ed25519.GenerateKey(rand.Reader)
				Expect(err).NotTo(HaveOccurred())
				alternate, err = ssh.NewSignerFromKey(private)
				Expect(err).NotTo(HaveOccurred())
				server.mu.Lock()
				server.alternateKey = alternate
				server.rekeyAfter = 2
				server.mu.Unlock()
			}
			if scenario == "auth-rejected" {
				server.mu.Lock()
				server.rejectAfter = 2
				server.mu.Unlock()
			}
			var mu sync.Mutex
			hostnameCalls, dmiCalls, fruCalls := 0, 0, 0
			server.mu.Lock()
			server.onCommand = func(ch ssh.Channel, command string) bool {
				mu.Lock()
				switch command {
				case "LC_ALL=C hostname":
					hostnameCalls++
					if hostnameCalls == 1 {
						mu.Unlock()
						if scenario == "cancel" {
							cancel()
						}
						time.Sleep(1300 * time.Millisecond)
						return true
					}
				case "LC_ALL=C dmidecode -s system-serial-number":
					dmiCalls++
				case "LC_ALL=C ipmitool fru print 0":
					fruCalls++
				default:
					mu.Unlock()
					return false
				}
				serial := testSerial
				if scenario == "identity-change" && dmiCalls > 1 {
					serial = "REPLACED-CARD"
				}
				text := serial + "\n"
				if command == "LC_ALL=C ipmitool fru print 0" {
					text = strings.ReplaceAll(testFRU, testSerial, serial)
				}
				mu.Unlock()
				_, _ = ch.Write([]byte(text))
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				return true
			}
			server.mu.Unlock()
			runner, err := newReconnectingRunner(context.Background(), "ssh://"+server.listener.Addr().String(), server.auth(), time.Second)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(runner.Close)
			device, _, collectErr := collect(ctx, config.Target{Role: "dpus", Endpoint: "ssh://" + server.listener.Addr().String()}, runner)
			cancel()
			Expect(collectErr).To(HaveOccurred())
			Expect(device.ObservationState).NotTo(Equal("observed"))
			if scenario == "identity-change" {
				Expect(Code(collectErr)).To(Equal("INVENTORY_CHANGED"))
			}
			if scenario == "cancel" {
				Expect(Code(collectErr)).To(Equal("SSH_TIMEOUT_OR_CANCELLED"))
				server.mu.Lock()
				connectionCount := len(server.connections)
				server.mu.Unlock()
				Expect(connectionCount).To(Equal(1))
			}
			if scenario == "auth-rejected" {
				Expect(Code(collectErr)).To(Equal("SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED"))
			}
			if scenario == "host-key-changed" {
				Expect(Code(collectErr)).To(Equal("SSH_HOST_KEY_REJECTED"))
			}
			if scenario == "auth-rejected" || scenario == "host-key-changed" {
				server.mu.Lock()
				connectionCount := len(server.connections)
				server.mu.Unlock()
				Expect(connectionCount).To(Equal(2))
			}
			_ = runner.Close()
		}
	})
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
	It("preserves IOMMU group zero and retains absent, denied and malformed results per PCI function", func() {
		const presentStage = "host-pf:0000:21:00.0:iommu-group"
		const absentStage = "host-pf:0000:21:00.1:iommu-group"
		const malformedStage = "host-pf:0000:41:00.0:iommu-group"
		r := hostRunner()
		r.values[presentStage] = "/sys/kernel/iommu_groups/0\n"
		r.values[absentStage] = "absent\n"
		r.values[malformedStage] = "/sys/kernel/iommu_groups/not-a-number\n"
		d, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		first := d.Adapters[0].PCIFunctions[0]
		Expect(first.IOMMUGroup).NotTo(BeNil())
		Expect(*first.IOMMUGroup).To(Equal(0))
		Expect(d.Runtime.Observations[presentStage].State).To(Equal("observed"))
		Expect(d.Runtime.Observations[presentStage].Values).To(Equal([]string{"group=0"}))
		Expect(d.Adapters[0].PCIFunctions[1].IOMMUGroup).To(BeNil())
		Expect(d.Runtime.Observations[absentStage].State).To(Equal("unknown"))
		Expect(d.Runtime.Observations[absentStage].ReasonCode).To(Equal("IOMMU_GROUP_ABSENT"))
		Expect(d.Adapters[1].PCIFunctions[0].IOMMUGroup).To(BeNil())
		Expect(d.Runtime.Observations[malformedStage].State).To(Equal("unavailable"))
		Expect(d.Runtime.Observations[malformedStage].ReasonCode).To(Equal("SSH_RUNTIME_INVALID"))

		denied := hostRunner()
		denied.errors[malformedStage] = &Error{Code: "PERMISSION_DENIED", Stage: malformedStage, Tool: "readlink"}
		device, _, err := collect(context.Background(), config.Target{Role: "hosts", Endpoint: "ssh://192.0.2.11:22"}, denied)
		Expect(err).NotTo(HaveOccurred())
		Expect(device.Adapters[1].PCIFunctions[0].IOMMUGroup).To(BeNil())
		Expect(device.Runtime.Observations[malformedStage].ReasonCode).To(Equal("PERMISSION_DENIED"))
	})
	It("does not interpolate an unvalidated BDF into sysfs reads", func() {
		r := hostRunner()
		runtime := &inventory.RuntimeObservation{Observations: map[string]inventory.Observation{}}
		pf := collectPF(context.Background(), r, "0000:21:00.0;touch /tmp/nope", runtime, func(string, string) string { return "evidence" })
		Expect(pf.IOMMUGroup).To(BeNil())
		Expect(r.calls).To(BeEmpty())
		Expect(runtime.Observations["host-pf:0000:21:00.0;touch /tmp/nope:iommu-group"].ReasonCode).To(Equal("SSH_PCI_INVENTORY_INVALID"))
	})
	It("combines validated IPv4 and IPv6 route tables with the same interface indexes", func() {
		r := &fakeRunner{values: map[string]string{
			"hostname": "host.example\n", "os": "PRETTY_NAME=\"Ubuntu\"\n",
			"interfaces": `[{"ifindex":7,"ifname":"eno1","mtu":1500,"addr_info":[]}]`,
			"routes":     `[{"dst":"default","gateway":"192.0.2.1","dev":"eno1"}]`,
			"routes-v6":  `[{"dst":"default","gateway":"fe80::1","dev":"eno1"}]`,
			"dns":        "nameserver 192.0.2.53\n",
		}, errors: map[string]error{}, calls: []string{}}
		runtime := &inventory.RuntimeObservation{Observations: map[string]inventory.Observation{}}
		collectRuntime(context.Background(), r, runtime, func(string, string) string { return "evidence" })
		Expect(runtime.Routes).To(HaveLen(2))
		Expect(runtime.Routes[0].Destination).To(Equal("0.0.0.0/0"))
		Expect(runtime.Routes[1].Destination).To(Equal("::/0"))
		Expect(runtime.Routes[0].InterfaceIndex).To(Equal(7))
		Expect(runtime.Routes[1].InterfaceIndex).To(Equal(7))
		Expect(runtime.Observations["routes"].State).To(Equal("observed"))
	})
	It("marks the combined routes unavailable when either family fails or contains the wrong IP family", func() {
		for _, test := range []struct {
			name     string
			v6       string
			v6Error  error
			wantCode string
		}{
			{name: "IPv6 permission denied", v6Error: &Error{Code: "PERMISSION_DENIED", Stage: "routes-v6", Tool: "ip"}, wantCode: "PERMISSION_DENIED"},
			{name: "malformed IPv6 table", v6: `[{"dst":"192.0.2.0/24","dev":"eno1"}]`, wantCode: "SSH_RUNTIME_INVALID"},
		} {
			By(test.name)
			r := &fakeRunner{values: map[string]string{
				"hostname": "host.example\n", "os": "PRETTY_NAME=\"Ubuntu\"\n",
				"interfaces": `[{"ifindex":7,"ifname":"eno1","mtu":1500,"addr_info":[]}]`,
				"routes":     `[{"dst":"default","gateway":"192.0.2.1","dev":"eno1"}]`,
				"routes-v6":  test.v6,
				"dns":        "nameserver 192.0.2.53\n",
			}, errors: map[string]error{}, calls: []string{}}
			if test.v6Error != nil {
				r.errors["routes-v6"] = test.v6Error
			}
			runtime := &inventory.RuntimeObservation{Observations: map[string]inventory.Observation{}}
			collectRuntime(context.Background(), r, runtime, func(string, string) string { return "evidence" })
			Expect(runtime.Routes).To(BeNil())
			Expect(runtime.Observations["routes"].State).To(Equal("unavailable"))
			Expect(runtime.Observations["routes"].ReasonCode).To(Equal(test.wantCode))
			Expect(r.calls).To(ContainElement(ContainSubstring("routes ip -j -4 route show table all")))
			Expect(r.calls).To(ContainElement(ContainSubstring("routes-v6 ip -j -6 route show table all")))
		}
	})
	It("parses all-table local, named, numeric and multipath routes without losing egress interfaces", func() {
		ifaces := []inventory.Interface{
			{Name: "eno1", Index: 7},
			{Name: "eno2", Index: 8},
			{Name: "lo", Index: 1},
		}
		got, err := routes(`[
			{"type":"local","dst":"192.0.2.7","dev":"eno1","table":"local","protocol":2},
			{"dst":"default","gateway":"192.0.2.1","dev":"eno1","table":1001},
			{"type":"unreachable","dst":"default","table":"main"},
			{"dst":"198.51.100.0/24","table":"main","nexthops":[{"gateway":"192.0.2.2","dev":"eno1"},{"gateway":"192.0.2.3","dev":"eno2"}]}
		]`, ifaces, 4)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(5))
		Expect(got[0].Destination).To(Equal("192.0.2.7/32"))
		Expect(got[0].Table).To(Equal(255))
		Expect(got[0].InterfaceIndex).To(Equal(7))
		Expect(got[1].Table).To(Equal(1001))
		Expect(got[2].Table).To(Equal(254))
		Expect(got[2].InterfaceIndex).To(BeZero())
		Expect(got[3].InterfaceIndex).To(Equal(7))
		Expect(got[4].InterfaceIndex).To(Equal(8))
		v6, err := routes(`[{"dst":"default","gateway":"fe80::1","dev":"eno2","table":"default"}]`, ifaces, 6)
		Expect(err).NotTo(HaveOccurred())
		Expect(v6).To(HaveLen(1))
		Expect(v6[0].Table).To(Equal(253))
		Expect(v6[0].InterfaceIndex).To(Equal(8))
	})
	It("invalidates complete route coverage for unsupported tables and malformed multipath", func() {
		ifaces := []inventory.Interface{{Name: "eno1", Index: 7}}
		for _, text := range []string{
			`[{"dst":"default","dev":"eno1","table":"mgmt"}]`,
			`[{"dst":"203.0.113.0/24","nexthops":[{"gateway":"192.0.2.1"}]}]`,
			`[{"dst":"203.0.113.0/24","nexthops":[{"gateway":"2001:db8::1","dev":"eno1"}]}]`,
		} {
			_, err := routes(text, ifaces, 4)
			Expect(err).To(HaveOccurred(), text)
		}
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
