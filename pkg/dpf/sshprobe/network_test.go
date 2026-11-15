// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package sshprobe

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	yaml "gopkg.in/yaml.v3"
)

var _ = Describe("network observations", func() {
	It("preserves actual bridges, upstream resolvers and candidate neighbors", func() {
		r := armRunner()
		r.values["network-links"] = `[{"ifname":"oob_net0","address":"02:00:00:00:00:41","master":"br0"},{"ifname":"br0","linkinfo":{"info_kind":"bridge"}}]`
		r.values["resolver-path"] = "/run/systemd/resolve/stub-resolv.conf\n"
		r.values["resolver-dns"] = "Global: 192.0.2.53\nLink 2 (oob_net0): 2001:db8::53\n"
		r.values["resolver-domains"] = "Global:\nLink 2 (oob_net0): example.test ~.\n"
		r.values["dns"] = "nameserver 127.0.0.53\nsearch example.test\n"
		r.values["dnsmasq"] = "server=/example.test/192.0.2.53\nno-resolv\n"
		r.values["neighbors"] = `[{"dst":"192.0.2.42","dev":"oob_net0","lladdr":"02:00:00:00:00:42","state":["STALE"]},{"dst":"192.0.2.43","dev":"oob_net0","state":["FAILED"]}]`
		r.values["lldp"] = "lldp.oob_net0.chassis.name=switch-a\nlldp.oob_net0.port.ifname=swp1\n"
		d, _, err := collect(context.Background(), config.Target{Role: "dpus", Endpoint: "ssh://192.0.2.41:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		n := d.Runtime.Network
		Expect(n.Links[0].Master).To(Equal("br0"))
		Expect(n.Links[1].Kind).To(Equal("bridge"))
		Expect(n.Resolver.Owner).To(Equal("systemd-resolved"))
		Expect(n.Resolver.PerLink["global"]).To(Equal([]string{"192.0.2.53"}))
		Expect(n.Resolver.Search).To(Equal([]string{"example.test"}))
		Expect(d.Runtime.DNS).To(Equal([]string{"127.0.0.53"}))
		Expect(n.Resolver.DNSMasqServers).To(Equal([]string{"/example.test/192.0.2.53"}))
		Expect(n.Neighbors).To(HaveLen(2))
		Expect(n.Neighbors[1].MAC).To(BeEmpty())
		Expect(n.LLDP[0].Chassis).To(Equal("switch-a"))
		Expect(strings.Join(r.calls, "\n")).NotTo(ContainSubstring("192.0.2.43"))
	})
	It("retains base link facts when detailed iproute VXLAN JSON is malformed", func() {
		r := armRunner()
		r.values["network-links"] = `[{"ifname":"oob_net0","address":"02:00:00:00:00:41","master":"br0"},{"ifname":"br0"}]`
		r.values["network-link-details"] = `[{"ifname":"vxlan0","linkinfo":{"info_kind":"vxlan","info_data":{"fan_map":}}}]`
		d, _, err := collect(context.Background(), config.Target{Role: "dpus", Endpoint: "ssh://192.0.2.41:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.Runtime.Network.Links).To(HaveLen(2))
		Expect(d.Runtime.Network.Links[0].MAC).To(Equal("02:00:00:00:00:41"))
		Expect(d.Runtime.Network.Links[0].Master).To(Equal("br0"))
		Expect(d.Runtime.Observations["network-links"].State).To(Equal("observed"))
		Expect(d.Runtime.Observations["network-link-details"].ReasonCode).To(Equal("SSH_RUNTIME_INVALID"))
		Expect(d.Runtime.Observations["network-link-details"].Message).NotTo(BeEmpty())
	})
	It("enriches base link kinds only when detailed observations still match", func() {
		base, err := parseLinks(`[{"ifname":"br0","address":"02:00:00:00:00:01"}]`)
		Expect(err).NotTo(HaveOccurred())
		details, err := parseLinks(`[{"ifname":"br0","address":"02:00:00:00:00:01","linkinfo":{"info_kind":"bridge"}}]`)
		Expect(err).NotTo(HaveOccurred())
		Expect(mergeLinkDetails(base, details)).To(Succeed())
		Expect(base[0].Kind).To(Equal("bridge"))
		details[0].MAC = "02:00:00:00:00:02"
		Expect(mergeLinkDetails(base, details)).NotTo(Succeed())
		Expect(base[0].MAC).To(Equal("02:00:00:00:00:01"))
	})
	It("retains PCI parent and physical port identity from detailed link output", func() {
		base, err := parseLinks(`[{"ifname":"ens5f0np0","address":"20:4d:52:3e:9a:b8"}]`)
		Expect(err).NotTo(HaveOccurred())
		details, err := parseLinks(`[{"ifname":"ens5f0np0","address":"20:4d:52:3e:9a:b8","parentdev":"0000:82:00.0","phys_port_name":"p0"}]`)
		Expect(err).NotTo(HaveOccurred())
		Expect(mergeLinkDetails(base, details)).To(Succeed())
		Expect(base[0].PCIAddress).To(Equal("0000:82:00.0"))
		Expect(base[0].PhysicalPortName).To(Equal("p0"))
		details[0].PhysicalPortName = "p1"
		Expect(mergeLinkDetails(base, details)).NotTo(Succeed())
	})
	It("retains positive kernel MTU bounds and treats zero or missing bounds as unavailable", func() {
		links, err := parseLinks(`[{"ifname":"eth0","address":"02:00:00:00:00:01","min_mtu":68,"max_mtu":65535},{"ifname":"lo","min_mtu":0,"max_mtu":0},{"ifname":"veth0"}]`)
		Expect(err).NotTo(HaveOccurred())
		Expect(links[0].MinMTU).To(Equal(uint32(68)))
		Expect(links[0].MaxMTU).To(Equal(uint32(65535)))
		Expect(links[1].MinMTU).To(BeZero())
		Expect(links[1].MaxMTU).To(BeZero())
		Expect(links[2].MinMTU).To(BeZero())
		Expect(links[2].MaxMTU).To(BeZero())
	})
	It("rejects malformed and contradictory kernel MTU bounds", func() {
		for _, text := range []string{
			`[{"ifname":"eth0","min_mtu":-1}]`,
			`[{"ifname":"eth0","max_mtu":"9000"}]`,
			`[{"ifname":"eth0","min_mtu":68.5}]`,
			`[{"ifname":"eth0","max_mtu":4294967296}]`,
			`[{"ifname":"eth0","min_mtu":9000,"max_mtu":1500}]`,
			`[{"ifname":"eth0","min_mtu":null}]`,
		} {
			_, err := parseLinks(text)
			Expect(err).To(HaveOccurred(), text)
		}
	})
	It("merges matching link details atomically without erasing known base facts", func() {
		base := []inventory.NetworkLink{
			{Name: "eth0", MAC: "02:00:00:00:00:01", Kind: "ether", PermanentMAC: "02:00:00:00:00:02", MinMTU: 68},
			{Name: "eth1", MAC: "02:00:00:00:00:03", PCIAddress: "0000:81:00.0", PhysicalPortName: "p1", MaxMTU: 9000},
		}
		before := append([]inventory.NetworkLink(nil), base...)
		details, err := parseLinks(`[{"ifname":"eth0","address":"02:00:00:00:00:01","max_mtu":65535},{"ifname":"eth1","address":"02:00:00:00:00:03","parentdev":"0000:81:00.0","phys_port_name":"p1"}]`)
		Expect(err).NotTo(HaveOccurred())
		Expect(mergeLinkDetails(base, details)).To(Succeed())
		Expect(base[0].Kind).To(Equal("ether"))
		Expect(base[0].PermanentMAC).To(Equal("02:00:00:00:00:02"))
		Expect(base[0].MinMTU).To(Equal(uint32(68)))
		Expect(base[0].MaxMTU).To(Equal(uint32(65535)))
		Expect(base[1].PCIAddress).To(Equal("0000:81:00.0"))
		Expect(base[1].PhysicalPortName).To(Equal("p1"))
		Expect(base[1].MaxMTU).To(Equal(uint32(9000)))

		changed := append([]inventory.NetworkLink(nil), before...)
		lateConflict, err := parseLinks(`[{"ifname":"eth0","address":"02:00:00:00:00:01","min_mtu":68,"max_mtu":65535},{"ifname":"eth1","address":"02:00:00:00:00:03","parentdev":"0000:82:00.0","phys_port_name":"p1"}]`)
		Expect(err).NotTo(HaveOccurred())
		Expect(mergeLinkDetails(changed, lateConflict)).NotTo(Succeed())
		Expect(changed).To(Equal(before))
	})
	It("rejects changed positive MTU capabilities across the two link reads", func() {
		base, err := parseLinks(`[{"ifname":"eth0","address":"02:00:00:00:00:01","min_mtu":68,"max_mtu":9000}]`)
		Expect(err).NotTo(HaveOccurred())
		details, err := parseLinks(`[{"ifname":"eth0","address":"02:00:00:00:00:01","min_mtu":68,"max_mtu":65535}]`)
		Expect(err).NotTo(HaveOccurred())
		Expect(mergeLinkDetails(base, details)).NotTo(Succeed())
	})
	It("rejects contradictory effective MTU ranges before enriching any link", func() {
		cases := []struct {
			name          string
			baseMinimum   uint32
			baseMaximum   uint32
			detailMinimum uint32
			detailMaximum uint32
		}{
			{name: "detail maximum below base minimum", baseMinimum: 1500, detailMaximum: 1000},
			{name: "detail minimum above base maximum", baseMaximum: 1500, detailMinimum: 2000},
			{name: "invalid base range", baseMinimum: 2000, baseMaximum: 1500},
		}
		for _, tc := range cases {
			base := []inventory.NetworkLink{
				{Name: "eth0", MAC: "02:00:00:00:00:01"},
				{Name: "eth1", MAC: "02:00:00:00:00:02", MinMTU: tc.baseMinimum, MaxMTU: tc.baseMaximum},
			}
			before := append([]inventory.NetworkLink(nil), base...)
			details := []inventory.NetworkLink{
				{Name: "eth0", MAC: "02:00:00:00:00:01", Kind: "ether"},
				{Name: "eth1", MAC: "02:00:00:00:00:02", MinMTU: tc.detailMinimum, MaxMTU: tc.detailMaximum},
			}
			Expect(mergeLinkDetails(base, details)).NotTo(Succeed(), tc.name)
			Expect(base).To(Equal(before), tc.name)
		}
	})
	It("serializes reported MTU limits while omitting unavailable zero values", func() {
		link := inventory.NetworkLink{Name: "eth0", MinMTU: 68, MaxMTU: 65535}
		jsonBytes, err := json.Marshal(link)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(jsonBytes)).To(ContainSubstring(`"minMTU":68`))
		Expect(string(jsonBytes)).To(ContainSubstring(`"maxMTU":65535`))
		zeroJSON, err := json.Marshal(inventory.NetworkLink{Name: "lo"})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(zeroJSON)).NotTo(ContainSubstring("MTU"))
		yamlBytes, err := yaml.Marshal(link)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(yamlBytes)).To(ContainSubstring("minMTU: 68"))
		Expect(string(yamlBytes)).To(ContainSubstring("maxMTU: 65535"))
	})
	It("does not interpret failed or malformed probes as empty successful evidence", func() {
		r := armRunner()
		r.values["neighbors"] = "null"
		r.values["network-links"] = "bad-json"
		r.errors["resolver-dns"] = &Error{Code: "TOOL_NOT_AVAILABLE", Stage: "resolver-dns", Tool: "resolvectl"}
		d, _, err := collect(context.Background(), config.Target{Role: "dpus", Endpoint: "ssh://192.0.2.41:22"}, r)
		Expect(err).NotTo(HaveOccurred())
		for _, stage := range []string{"neighbors", "network-links", "resolver-dns", "dnsmasq", "lldp"} {
			Expect(d.Runtime.Observations[stage].State).To(Equal("unavailable"))
			Expect(d.Runtime.Observations[stage].ReasonCode).NotTo(BeEmpty())
			Expect(d.Runtime.Observations[stage].Message).NotTo(BeEmpty())
		}
		Expect(d.Runtime.Observations["resolver-dns"].Tool).To(Equal("resolvectl"))
	})
	It("rejects invalid addresses and duplicate resolver records", func() {
		for _, text := range []string{"Global: malformed", "Global: 192.0.2.53\nGlobal: 192.0.2.54"} {
			_, err := parseResolved(text, true)
			Expect(err).To(HaveOccurred())
		}
		_, err := parseNeighbors(`[{"dst":"bad","dev":"eth0"}]`)
		Expect(err).To(HaveOccurred())
		_, err = parseDNSMasq("server=credential with spaces")
		Expect(err).To(HaveOccurred())
	})
})
