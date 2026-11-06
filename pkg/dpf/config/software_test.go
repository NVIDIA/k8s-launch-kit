// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"os"
	"strings"
)

var _ = Describe("DPF release and address source configuration", func() {
	BeforeEach(func() {
		Expect(os.Setenv("DPF_TEST_PASSWORD", "private")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_TEST_PASSWORD")
	})
	It("defaults omitted/null releases and rejects explicit empty or unknown releases", func() {
		for _, value := range []string{"", "dpf: {release: null}\n", "dpf: {release: '26.4.0'}\n"} {
			in, e := Decode([]byte(value+seed), "/site/in.yaml")
			Expect(e).NotTo(HaveOccurred())
			r, e := Resolve(in)
			Expect(e).NotTo(HaveOccurred())
			Expect(r.Software.Version).To(Equal(releases.DefaultRelease))
		}
		for _, value := range []string{"''", "'latest'", "'26.8.0'"} {
			in, e := Decode([]byte("dpf: {release: "+value+"}\n"+seed), "/site/in.yaml")
			Expect(e).NotTo(HaveOccurred())
			_, e = Resolve(in)
			Expect(e).To(HaveOccurred())
		}
	})
	It("preserves release intent and rebases optional lease source references", func() {
		text := strings.Replace(seed, "discovery:\n", "discovery:\n  addressLookup:\n    sources: [{file: leases.txt, format: dnsmasq-leases}]\n    devices: [{name: dpu1, role: arm, mac: '02:00:00:00:00:01'}]\n", 1)
		in, e := Decode([]byte(text), "/site/in.yaml")
		Expect(e).NotTo(HaveOccurred())
		r, e := Resolve(in)
		Expect(e).NotTo(HaveOccurred())
		Expect(r.AddressLookup.Sources[0].File).To(Equal("/site/leases.txt"))
		b, e := Saved(in, &inventory.Inventory{Software: r.Software}, "/output/result.yaml")
		Expect(e).NotTo(HaveOccurred())
		Expect(string(b)).To(ContainSubstring("../site/leases.txt"))
		Expect(string(b)).NotTo(ContainSubstring("dpf:\n"))
	})
})
