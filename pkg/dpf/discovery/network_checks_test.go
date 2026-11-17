// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"encoding/json"
	"maps"
	"os"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

var _ = Describe("management network observations", func() {
	It("detects changed node selection and scheduling without persisting arbitrary labels", func() {
		n := inventory.Node{Name: "cp-one", UID: "node-one", ControlPlane: true, Ready: true, Architecture: "amd64", OS: "linux", Labels: map[string]string{"tenant-placement": "selected"}}
		same := n
		same.Labels = maps.Clone(n.Labels)
		Expect(sameNodes([]inventory.Node{n}, []inventory.Node{same})).To(BeTrue())
		for _, mutate := range []func(*inventory.Node){
			func(n *inventory.Node) { n.Labels["tenant-placement"] = "other" },
			func(n *inventory.Node) { n.Ready = false },
			func(n *inventory.Node) { n.Unschedulable = true },
			func(n *inventory.Node) { n.ControlPlane = false },
			func(n *inventory.Node) { n.Architecture = "arm64" },
		} {
			changed := n
			changed.Labels = maps.Clone(n.Labels)
			mutate(&changed)
			Expect(sameNodes([]inventory.Node{n}, []inventory.Node{changed})).To(BeFalse())
		}
		encoded, err := json.Marshal(n)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(encoded)).NotTo(ContainSubstring("tenant-placement"))
		encoded, err = yaml.Marshal(n)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(encoded)).NotTo(ContainSubstring("tenant-placement"))
	})
	It("accepts explicitly unverified BMC transport without weakening other endpoints", func() {
		Expect(os.Setenv("PROBE_TEST_PASSWORD", "not-a-real-secret")).To(Succeed())
		DeferCleanup(os.Unsetenv, "PROBE_TEST_PASSWORD")
		in, err := config.Decode([]byte(`credentials:
  bmc: {username: admin, passwordRef: {env: PROBE_TEST_PASSWORD}}
discovery:
  probes: {mode: inventory}
  hosts: [{credentialRef: bmc, bmcEndpoint: "https://192.0.2.60"}]
  dpus:
    - {credentialRef: bmc, bmcEndpoint: "https://192.0.2.50", tls: {insecureSkipVerify: true}}
    - {credentialRef: bmc, bmcEndpoint: "https://192.0.2.51"}
`), "/site.yaml")
		Expect(err).NotTo(HaveOccurred())
		request, err := config.Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		inv := &inventory.Inventory{Collection: inventory.Collection{Mode: "management"}, Management: &inventory.Management{Nodes: []inventory.Node{{Name: "cp-one", UID: "node-one"}}}, Evidence: map[string]inventory.Evidence{"source": {}}}
		for _, destination := range []string{"management-api", inventory.Digest("probe-destination-v1", "https://192.0.2.50"), inventory.Digest("probe-destination-v1", "https://192.0.2.51")} {
			inv.Probes = append(inv.Probes, inventory.ProbeResult{NodeUID: "node-one", NetworkMode: "pod-network", Destination: destination, DNS: "not-applicable", EvidenceRef: "source", Addresses: []inventory.ProbeAddress{{Address: "192.0.2.50", TCP: "reachable", TLS: "unverified"}}})
		}
		checks := ProbeChecks(inv, request.Targets)
		Expect(checks[0].State).To(Equal("failed"), "management API stays strict")
		Expect(checks[1].State).To(Equal("passed"))
		Expect(checks[1].ReasonCode).To(Equal("NETWORK_PATH_REACHABLE_TLS_UNVERIFIED"))
		Expect(checks[1].Message).To(ContainSubstring("identity remains unverified"))
		Expect(checks[2].State).To(Equal("failed"), "strict BMC stays strict")
		inv.Probes[1].Addresses[0].TLS = "unavailable"
		Expect(ProbeChecks(inv, request.Targets)[1].State).To(Equal("failed"), "bypass does not waive failed handshake")
	})
	It("retains actionable per-path reasons and readable subjects", func() {
		target := config.Target{Name: "selected-dpu", Role: "dpus", Endpoint: "https://192.0.2.50"}
		destination := inventory.Digest("probe-destination-v1", target.Endpoint)
		for _, scenario := range []struct{ dns, tcp, tls, code, state string }{
			{"resolved", "reachable", "verified", "NETWORK_PATH_VERIFIED", "passed"},
			{"unavailable", "reachable", "verified", "PROBE_DNS_UNAVAILABLE", "unknown"},
			{"", "reachable", "verified", "PROBE_DNS_UNAVAILABLE", "unknown"},
			{"resolved", "unavailable", "unavailable", "PROBE_TCP_UNREACHABLE", "failed"},
			{"resolved", "reachable", "unavailable", "PROBE_TLS_UNVERIFIED", "failed"},
		} {
			inv := &inventory.Inventory{Collection: inventory.Collection{Mode: "management"}, Management: &inventory.Management{Nodes: []inventory.Node{{Name: "cp-one", UID: "node-one"}}}, Evidence: map[string]inventory.Evidence{"source": {}}, Probes: []inventory.ProbeResult{{NodeUID: "node-one", NetworkMode: "pod-network", Destination: destination, DNS: scenario.dns, EvidenceRef: "source", Addresses: []inventory.ProbeAddress{{Address: "192.0.2.50", TCP: scenario.tcp, TLS: scenario.tls}}}}}
			checks := ProbeChecks(inv, []config.Target{target})
			Expect(checks).To(HaveLen(4))
			Expect(checks[1].State).To(Equal(scenario.state))
			Expect(checks[1].ReasonCode).To(Equal(scenario.code))
			Expect(checks[1].Message).NotTo(BeEmpty())
			Expect(checks[1].Subject).To(ContainSubstring("cp-one/pod-network/" + target.DisplayName()))
			Expect(checks[1].Subject).NotTo(ContainSubstring("sha256:"))
			Expect(checks[3].State).To(Equal("unknown"))
			Expect(checks[3].ReasonCode).To(Equal("PROBE_NOT_OBSERVED"))
			Expect(checks[3].Message).NotTo(BeEmpty())
		}
	})
})
