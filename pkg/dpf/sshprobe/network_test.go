// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package sshprobe

import (
	"context"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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
