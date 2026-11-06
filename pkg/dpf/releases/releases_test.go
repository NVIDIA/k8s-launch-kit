// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package releases

import (
	"strings"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestReleases(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "DPF release catalog") }

var _ = Describe("release BOM resolution", func() {
	It("defaults to a pinned full release and retains separately sourced coordinates", func() {
		r, err := Resolve("")
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Version).To(Equal(DefaultRelease))
		Expect(r.Operator.Version).To(Equal("v26.4.0"))
		Expect(r.Operator.Image).To(Equal("nvcr.io/nvidia/doca/dpf-system:v26.4.0"))
		Expect(r.Operator.ChartDigest).To(Equal("sha256:0f9a18a0d64958e2f1769bdb468a46ad65e0ea485dadb86cad9e6457b3cec69a"))
		Expect(SupportedReleases()).To(Equal([]string{"26.4.0"}))
	})
	It("rejects unreviewed releases and does not interpret latest, prefixes or whitespace", func() {
		for _, version := range []string{"latest", "26.4", "26.4.1", "v26.4.0", " 26.4.0", "26.4.0 "} {
			_, err := Resolve(version)
			Expect(err).To(MatchError(ContainSubstring("unknown DPF release")))
		}
	})
	It("keeps unknown BMC requirements distinct from a documented NIC minimum", func() {
		r, err := Resolve(DefaultRelease)
		Expect(err).NotTo(HaveOccurred())
		h, ok := r.RequirementsFor("b3240")
		Expect(ok).To(BeTrue())
		Expect(h.Family).To(Equal("BlueField-3"))
		Expect(h.Requirements).To(ContainElement(And(HaveField("Component", "nic"), HaveField("Minimum", "32.38.1002"), HaveField("State", "known"))))
		Expect(h.Requirements).To(ContainElement(And(HaveField("Component", "bmc"), HaveField("Minimum", ""), HaveField("Target", ""), HaveField("State", "unknown"), HaveField("ReasonCode", "RELEASE_REQUIREMENT_NOT_DOCUMENTED"))))
		Expect(h.BFB.Digest).To(BeEmpty())
		Expect(h.BFB.ReasonCode).To(Equal("ARTIFACT_DIGEST_NOT_RECORDED"))
	})
	It("does not apply BF3 requirements to BF4, family names, or partial model matches", func() {
		r, err := Resolve(DefaultRelease)
		Expect(err).NotTo(HaveOccurred())
		for _, model := range []string{"", "BlueField-3", "BlueField-4", "B4240", "B3240 extra", "XXB3240"} {
			_, ok := r.RequirementsFor(model)
			Expect(ok).To(BeFalse())
		}
		var missing *Release
		_, ok := missing.RequirementsFor("B3240")
		Expect(ok).To(BeFalse())
	})
	It("isolates returned nested values from the catalog and hardware lookup", func() {
		r, err := Resolve(DefaultRelease)
		Expect(err).NotTo(HaveOccurred())
		h, ok := r.RequirementsFor("B3240")
		Expect(ok).To(BeTrue())
		h.Models[0] = "altered"
		h.Requirements[0].Minimum = "0"
		Expect(r.Hardware[0].Models[0]).To(Equal("B3210"))
		Expect(r.Hardware[0].Requirements[0].Minimum).To(Equal("32.38.1002"))
		r.Hardware[0].Models[0] = "changed"
		r.Hardware[0].Requirements[0].Minimum = "0"
		r.Components[0].Version = "changed"
		r.Sources[0].URL = "changed"
		r.Limitations[0] = "changed"
		next, err := Resolve(DefaultRelease)
		Expect(err).NotTo(HaveOccurred())
		Expect(next.Hardware[0].Models[0]).To(Equal("B3210"))
		Expect(next.Hardware[0].Requirements[0].Minimum).To(Equal("32.38.1002"))
		Expect(next.Components[0].Version).To(Equal("v3.3.0"))
		Expect(next.Sources[0].URL).To(HavePrefix("https://github.com/"))
		Expect(next.Limitations[0]).NotTo(Equal("changed"))
	})
	It("allows concurrent independent resolutions", func() {
		var wg sync.WaitGroup
		for range 10 {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				r, err := Resolve(DefaultRelease)
				Expect(err).NotTo(HaveOccurred())
				r.Hardware[0].Models[0] = "independent"
			}()
		}
		wg.Wait()
	})
	It("rejects malformed catalogs rather than hiding missing provenance", func() {
		for _, data := range []string{
			"releases: {}",
			string(catalogYAML) + "\n---\nreleases: {}\n",
			strings.Replace(string(catalogYAML), "sourceRef: published-chart", "sourceRef: absent", 1),
			strings.Replace(string(catalogYAML), "state: unknown", "state: known", 1),
			strings.Replace(string(catalogYAML), "models: [B3210, B3220, B3240]", "models: [B3210, B3210]", 1),
			strings.Replace(string(catalogYAML), "version: \"26.4.0\"", "version: \"26.8.0\"", 1),
			strings.Replace(string(catalogYAML), "chart: dpf-operator", "unknown: true", 1),
		} {
			_, err := load([]byte(data))
			Expect(err).To(HaveOccurred())
		}
	})
})
