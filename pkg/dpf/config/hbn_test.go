// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HBN use-case inputs", func() {
	BeforeEach(func() {
		Expect(os.Setenv("DPF_TEST_PASSWORD", "hbn-secret-canary")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_TEST_PASSWORD")
	})
	It("resolves omitted profile values without changing the source document", func() {
		in, err := Decode([]byte("dpf: {useCase: hbn-bgp}\n"+seed), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		r, err := Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.UseCase).To(Equal("hbn-bgp"))
		Expect(r.Software.Version).To(Equal("26.8.0"))
		Expect(r.DPFNamespace).To(Equal("dpf-operator-system"))
		Expect(r.Mode).To(Equal("inventory"))
		Expect(r.HBN.FabricVRF).To(Equal("default"))
		Expect(r.HBNGeneration.HostAddressIndex).To(Equal(2))
		Expect(in.Site.DPF.HBN).To(BeNil())
		Expect(r.Switches).To(BeEmpty()) // Readiness reports missing access.
	})
	It("resolves generation overrides without coercing scalars or modifying omitted inputs", func() {
		document := "dpf: {useCase: hbn-bgp}\nprovisioning: {hbn: {nodeEffect: noEffect, workloadGatewayIndex: 2, hostAddressIndex: 3, vxlanStartupProbeEnabled: false}}\n" + seed
		in, err := Decode([]byte(document), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		r, err := Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(r.HBNGeneration.NodeEffect)).To(Equal("noEffect"))
		Expect(r.HBNGeneration.WorkloadGatewayIndex).To(Equal(2))
		Expect(r.HBNGeneration.HostAddressIndex).To(Equal(3))
		Expect(r.HBNGeneration.VXLANStartupProbeEnabled).To(BeFalse())
		Expect(r.HBNGeneration.ChartVersion).To(Equal("3.5.0"))
		Expect(in.Site.Provisioning.HBN.ChartVersion).To(BeNil())
		Expect(string(in.Source)).To(Equal(document))
		for _, value := range []string{"hostAddressIndex: '3'", "workloadGatewayIndex: '2'", "vxlanStartupProbeEnabled: 'false'", "chartVersion: 3.5"} {
			_, err := Decode([]byte("dpf: {useCase: hbn-bgp}\nprovisioning: {hbn: {"+value+"}}\n"+seed), "/site/input.yaml")
			Expect(err).To(HaveOccurred())
		}
	})
	It("rejects generation overrides without a use case and explicit invalid or conflicting values", func() {
		for _, document := range []string{
			"provisioning: {hbn: {}}\n",
			"provisioning: {bootstrap: {}}\n",
			"dpf: {useCase: hbn-bgp}\nprovisioning: {hbn: {chartVersion: ''}}\n",
			"dpf: {useCase: hbn-bgp}\nprovisioning: {hbn: {workloadGatewayIndex: 0}}\n",
			"dpf: {useCase: hbn-bgp}\nprovisioning: {hbn: {hostAddressIndex: 0}}\n",
			"dpf: {useCase: hbn-bgp}\nprovisioning: {hbn: {hostAddressIndex: 1}}\n",
			"dpf: {useCase: hbn-bgp}\nprovisioning: {hbn: {nodeEffect: ''}}\n",
			"dpf: {useCase: hbn-bgp}\nprovisioning: {hbn: {loopbackCIDR: 10.0.120.0/24}}\n",
		} {
			in, err := Decode([]byte(document+seed), "/site/input.yaml")
			Expect(err).NotTo(HaveOccurred())
			_, err = Resolve(in)
			Expect(err).To(HaveOccurred(), document)
		}
	})
	It("preserves explicit release and namespace overrides while null selects the profile", func() {
		for _, doc := range []string{
			"dpf: {useCase: hbn-bgp, release: null, namespace: null}\n",
			"dpf: {useCase: hbn-bgp, release: '26.4.0', namespace: selected-dpf}\n",
		} {
			in, err := Decode([]byte(doc+seed), "/site/input.yaml")
			Expect(err).NotTo(HaveOccurred())
			r, err := Resolve(in)
			Expect(err).NotTo(HaveOccurred())
			if in.Site.DPF.Release == nil {
				Expect(r.Software.Version).To(Equal("26.8.0"))
				Expect(r.DPFNamespace).To(Equal("dpf-operator-system"))
			} else {
				Expect(r.Software.Version).To(Equal("26.4.0"))
				Expect(r.DPFNamespace).To(Equal("selected-dpf"))
			}
		}
	})
	It("persists an explicit CLI override while retaining original bytes for concurrent-edit detection", func() {
		in, err := Decode([]byte("dpf: {useCase: unknown}\n"+seed), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		selection := "hbn-bgp"
		changed, err := WithUseCase(in, &selection)
		Expect(err).NotTo(HaveOccurred())
		Expect(changed.Source).To(Equal(in.Source))
		Expect(*in.Site.DPF.UseCase).To(Equal("unknown"))
		Expect(*changed.Site.DPF.UseCase).To(Equal("hbn-bgp"))
		out, err := Saved(changed, nil, in.Path)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(ContainSubstring("useCase: hbn-bgp"))
		Expect(string(out)).To(ContainSubstring("# retained comment"))
		Expect(string(out)).NotTo(ContainSubstring("hbn-secret-canary"))
	})
	It("rejects explicit empty selectors and invalid scalar types before remote access", func() {
		for _, document := range []string{
			"dpf: {useCase: ''}\n", "dpf: {useCase: sentry}\n",
			"dpf: {hbn: {}}\n", "dpf: {useCase: hbn-bgp, hbn: {fabricASN: '65000'}}\n",
			"dpf: {useCase: hbn-bgp, hbn: {fabricASN: 0}}\n",
			"dpf: {useCase: hbn-bgp, namespace: ''}\n",
			"dpf: {useCase: hbn-bgp, namespace: Bad_Name}\n",
			"dpf: {useCase: hbn-bgp, hbn: {fabricVRF: ''}}\n",
		} {
			in, err := Decode([]byte(document+seed), "/site/input.yaml")
			if err == nil {
				_, err = Resolve(in)
			}
			Expect(err).To(HaveOccurred(), document)
		}
		in, err := Decode([]byte(seed), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		empty := ""
		_, err = WithUseCase(in, &empty)
		Expect(err).To(HaveOccurred())
	})
	It("resolves and rebases switch trust references, preserving explicit sudo false", func() {
		dir := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(dir, "switch-trust"), []byte("nonempty-pinned-trust"), 0600)).To(Succeed())
		document := "dpf: {useCase: hbn-bgp, hbn: {fabricASN: 65100}}\n" + seed + `  switches:
    - name: leaf-a
      credentialRef: read
      ssh: {address: LEAF.example, knownHostsFile: switch-trust, sudo: false}
      ports:
        - {name: swp1s1, dpuSerial: DPU-A, dpuInterface: p1}
        - {name: swp1s0, dpuSerial: DPU-A, dpuInterface: p0}
`
		in, err := Decode([]byte(document), filepath.Join(dir, "site.yaml"))
		Expect(err).NotTo(HaveOccurred())
		r, err := Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Switches).To(HaveLen(1))
		Expect(r.Switches[0].Endpoint).To(Equal("ssh://leaf.example:22"))
		Expect(r.Switches[0].Access.Sudo).To(BeFalse())
		Expect(r.Switches[0].Ports[0].Name).To(Equal("swp1s0"))
		Expect(in.Site.Discovery.Switches[0].Ports[0].Name).To(Equal("swp1s1"))
		out, err := Saved(in, nil, filepath.Join(dir, "output", "discovered.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(ContainSubstring("../switch-trust"))
		for _, bad := range []string{
			strings.Replace(document, "dpuInterface: p1", "dpuInterface: p0", 1),
			strings.Replace(document, "name: swp1s1", "name: swp1s0", 1),
			strings.Replace(document, "name: swp1s1", "name: 'swp1; reboot'", 1),
			strings.Replace(document, "name: leaf-a", "name: leaf-a\n      platform: other", 1),
		} {
			input, err := Decode([]byte(bad), filepath.Join(dir, "site.yaml"))
			Expect(err).NotTo(HaveOccurred())
			_, err = Resolve(input)
			Expect(err).To(HaveOccurred())
		}
	})
})
