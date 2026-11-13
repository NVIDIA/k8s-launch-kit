// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"github.com/nvidia/k8s-launch-kit/pkg/configinput"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	yaml "gopkg.in/yaml.v3"
)

var _ = Describe("DPF config-backed overrides", func() {
	It("applies explicit shared CLI values on an isolated copy and preserves the source", func() {
		in := validGenerationInput(GinkgoT().TempDir())
		in = decodeGenerationInput(in)
		source := append([]byte(nil), in.Source...)
		original, err := yaml.Marshal(in.Site)
		Expect(err).NotTo(HaveOccurred())

		out, err := WithOverrides(in, configinput.Values{Overrides: []configinput.Override{
			{Flag: "--dpf-release", Path: "dpf.release", Value: "26.8.0-rc.1"},
			{Flag: "--dpf-profile", Path: "dpf.profile", Value: "sentry-bf4"},
		}})
		Expect(err).NotTo(HaveOccurred())
		Expect(*out.Site.DPF.Release).To(Equal("26.8.0-rc.1"))
		Expect(*out.Site.DPF.Profile).To(Equal("sentry-bf4"))
		Expect(out.Source).To(Equal(source))
		after, err := yaml.Marshal(in.Site)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(original))
	})

	It("keeps explicitly empty CLI values and rejects them through normal config validation", func() {
		in := decodeGenerationInput(validGenerationInput(GinkgoT().TempDir()))
		out, err := WithOverrides(in, configinput.Values{Overrides: []configinput.Override{{
			Flag: "--dpf-release", Path: "dpf.release", Value: "",
		}}})
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Site.DPF.Release).NotTo(BeNil())
		Expect(*out.Site.DPF.Release).To(BeEmpty())
		_, err = ResolveGeneration(out)
		Expect(err).To(MatchError(ContainSubstring("has no Sentry BF4 generation contract")))
	})

	It("rejects Host resolver requests and malformed override paths", func() {
		in := decodeGenerationInput(validGenerationInput(GinkgoT().TempDir()))
		_, err := WithOverrides(in, configinput.Values{Requests: []configinput.Request{{Flag: "--network-operator-release", Kind: configinput.ResolverNetworkOperatorRelease, Value: "26.8"}}})
		Expect(err).To(MatchError(ContainSubstring("host configuration resolvers are not valid for DPF")))
		for _, path := range []string{"release", "dpf.", "dpf..release"} {
			_, err = WithOverrides(in, configinput.Values{Overrides: []configinput.Override{{Flag: "--x", Path: path, Value: "x"}}})
			Expect(err).To(HaveOccurred(), "path %q", path)
		}
	})

})
