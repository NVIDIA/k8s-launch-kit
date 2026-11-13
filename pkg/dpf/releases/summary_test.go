// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package releases

import (
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

var _ = Describe("persisted software BOM", func() {
	It("publishes artifact coordinates and named known minima without catalog research metadata", func() {
		s := summaryFixture()
		Expect(s.Hardware[0].MinimumNICFirmware).To(Equal("32.38.1002"))
		Expect(s.Hardware[0].MinimumHostMFT).To(Equal("4.33.0-169"))
		Expect(s.Hardware[0].MinimumBMCFirmware).To(BeEmpty())
		Expect(s.Hardware[0].BFB.Digest).To(BeEmpty())
		for _, encode := range []func(any) ([]byte, error){json.Marshal, yaml.Marshal} {
			encoded, err := encode(s)
			Expect(err).NotTo(HaveOccurred())
			for _, removed := range []string{"sourceRef", "sources", "revision", "requirements", "limitations", "ARTIFACT_DIGEST_NOT_RECORDED", "RELEASE_REQUIREMENT_NOT_DOCUMENTED"} {
				Expect(string(encoded)).NotTo(ContainSubstring(removed))
			}
			Expect(string(encoded)).To(ContainSubstring("minimumNICFirmware"))
			Expect(string(encoded)).To(ContainSubstring("minimumHostMFT"))
		}
	})

	It("preserves known and unknown requirement meanings across a serialized round trip", func() {
		original, err := Resolve(DefaultRelease)
		Expect(err).NotTo(HaveOccurred())
		encoded, err := yaml.Marshal(Summarize(original))
		Expect(err).NotTo(HaveOccurred())
		var saved Summary
		Expect(yaml.Unmarshal(encoded, &saved)).To(Succeed())
		restored, err := saved.Release()
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.Hardware[0].Requirements).To(HaveLen(len(original.Hardware[0].Requirements)))
		for i, requirement := range original.Hardware[0].Requirements {
			actual := restored.Hardware[0].Requirements[i]
			Expect(actual.Component).To(Equal(requirement.Component))
			Expect(actual.Scope).To(Equal(requirement.Scope))
			Expect(actual.State).To(Equal(requirement.State))
			Expect(actual.Minimum).To(Equal(requirement.Minimum))
			Expect(actual.Target).To(Equal(requirement.Target))
			Expect(actual.ReasonCode).To(Equal(requirement.ReasonCode))
			if requirement.State == "unknown" {
				Expect(actual.Message).NotTo(BeEmpty())
			}
		}
		Expect(restored.Hardware[0].BFB.Digest).To(BeEmpty())
		Expect(restored.Hardware[0].BFB.Version).To(Equal(original.Hardware[0].BFB.Version))
		Expect(restored.Hardware[0].BFB.URL).To(Equal(original.Hardware[0].BFB.URL))
		for _, model := range []string{"B4240", "BlueField-3", "BlueField-4", "B3240 extra"} {
			_, supported := restored.RequirementsFor(model)
			Expect(supported).To(BeFalse())
		}
		Expect(restored.Sources).To(BeEmpty())
		Expect(restored.Limitations).To(BeEmpty())
	})

	It("does not infer omitted minima or replace stored coordinates with catalog defaults", func() {
		s := summaryFixture()
		s.Version, s.Operator.Version = "99.1.2", "v99.1.2"
		s.Operator.Image = "registry.example/operator:stored"
		s.Components[0].Version = "v0.0.1-stored"
		s.Hardware[0].BFB.URL = "https://artifacts.example/stored.bfb"
		s.Hardware[0].BFB.Digest = "sha256:" + strings.Repeat("a", 64)
		s.Hardware[0].MinimumNICFirmware, s.Hardware[0].MinimumHostMFT = "", ""
		r, err := s.Release()
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Version).To(Equal(s.Version))
		Expect(r.Operator.Image).To(Equal(s.Operator.Image))
		Expect(r.Components[0].Version).To(Equal(s.Components[0].Version))
		Expect(r.Hardware[0].BFB.URL).To(Equal(s.Hardware[0].BFB.URL))
		Expect(r.Hardware[0].BFB.Digest).To(Equal(s.Hardware[0].BFB.Digest))
		for _, requirement := range r.Hardware[0].Requirements {
			Expect(requirement.State).To(Equal("unknown"))
			Expect(requirement.Minimum).To(BeEmpty())
			Expect(requirement.Target).To(BeEmpty())
		}
	})

	It("isolates mutable values from both the catalog and decoded summary", func() {
		r, err := Resolve(DefaultRelease)
		Expect(err).NotTo(HaveOccurred())
		s := Summarize(r)
		s.Components[0].Version = "changed"
		s.Hardware[0].Models[0] = "changed"
		Expect(r.Components[0].Version).NotTo(Equal("changed"))
		Expect(r.Hardware[0].Models[0]).NotTo(Equal("changed"))

		s = summaryFixture()
		restored, err := s.Release()
		Expect(err).NotTo(HaveOccurred())
		restored.Components[0].Version = "changed"
		restored.Hardware[0].Models[0] = "changed"
		Expect(s.Components[0].Version).NotTo(Equal("changed"))
		Expect(s.Hardware[0].Models[0]).NotTo(Equal("changed"))
	})

	It("rejects malformed or ambiguous saved coordinates and constraints", func() {
		for _, tc := range []struct {
			name string
			edit func(*Summary)
		}{
			{"missing version", func(s *Summary) { s.Version = "" }},
			{"inconsistent operator version", func(s *Summary) { s.Operator.Version = "v0.0.1" }},
			{"credential-bearing URL", func(s *Summary) { s.Operator.ChartRepository = "https://user:pass@example.com" }},
			{"query URL", func(s *Summary) { s.Hardware[0].BFB.URL += "?token=private" }},
			{"insecure URL", func(s *Summary) { s.Hardware[0].BFB.URL = "http://example.com/a.bfb" }},
			{"invalid chart digest", func(s *Summary) { s.Operator.ChartDigest = "sha256:invalid" }},
			{"invalid BFB digest", func(s *Summary) { s.Hardware[0].BFB.Digest = "sha256:invalid" }},
			{"component absent", func(s *Summary) { s.Components = nil }},
			{"component duplicated", func(s *Summary) { s.Components = append(s.Components, s.Components[0]) }},
			{"component applicability", func(s *Summary) { s.Components[0].Applicability = "qualified" }},
			{"component empty version", func(s *Summary) { s.Components[0].Version = "" }},
			{"hardware absent", func(s *Summary) { s.Hardware = nil }},
			{"model absent", func(s *Summary) { s.Hardware[0].Models = nil }},
			{"model whitespace", func(s *Summary) { s.Hardware[0].Models[0] = " B3210" }},
			{"model duplicated case", func(s *Summary) { s.Hardware[0].Models = append(s.Hardware[0].Models, "b3210") }},
			{"model ambiguous hardware", func(s *Summary) { s.Hardware = append(s.Hardware, s.Hardware[0]) }},
			{"non-numeric minimum", func(s *Summary) { s.Hardware[0].MinimumNICFirmware = "current" }},
			{"unknown minimum placeholder", func(s *Summary) { s.Hardware[0].MinimumHostMFT = "unknown" }},
		} {
			By(tc.name)
			s := summaryFixture()
			tc.edit(&s)
			_, err := s.Release()
			Expect(err).To(HaveOccurred())
		}
	})
})

func summaryFixture() Summary {
	r, err := Resolve(DefaultRelease)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return Summarize(r)
}
