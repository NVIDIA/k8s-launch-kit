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

	It("keeps the v26.4 summary wire shape and round-trips the BF4 artifact form", func() {
		bf3, err := Resolve("26.4.0")
		Expect(err).NotTo(HaveOccurred())
		bf3JSON, err := json.Marshal(Summarize(bf3))
		Expect(err).NotTo(HaveOccurred())
		bf3YAML, err := yaml.Marshal(Summarize(bf3))
		Expect(err).NotTo(HaveOccurred())
		for _, encoded := range [][]byte{bf3JSON, bf3YAML} {
			Expect(string(encoded)).NotTo(ContainSubstring("supportedKubernetes"))
			Expect(string(encoded)).NotTo(ContainSubstring("tenantKubernetesVersion"))
			Expect(string(encoded)).NotTo(ContainSubstring("supportLevel"))
			Expect(string(encoded)).NotTo(ContainSubstring("blueFieldSoftware"))
		}

		original, err := Resolve("26.8.0")
		Expect(err).NotTo(HaveOccurred())
		summary := Summarize(original)
		Expect(summary.SupportedKubernetes).To(Equal(&VersionRange{Minimum: "1.33", Maximum: "1.36"}))
		Expect(summary.TenantKubernetesVersion).To(Equal("v1.35.6"))
		bf4 := summary.Hardware[1]
		Expect(bf4.Family).To(Equal("BlueField-4"))
		Expect(bf4.Models).To(BeEmpty())
		Expect(bf4.BFB).To(BeNil())
		Expect(bf4.BlueFieldSoftware).NotTo(BeNil())
		Expect(bf4.BlueFieldSoftware.PLDMFwBundle).To(BeEmpty())
		encoded, err := yaml.Marshal(summary)
		Expect(err).NotTo(HaveOccurred())
		for _, removed := range []string{"sourceRef", "sources", "revision"} {
			Expect(string(encoded)).NotTo(ContainSubstring(removed))
		}
		var saved Summary
		Expect(yaml.Unmarshal(encoded, &saved)).To(Succeed())
		restored, err := saved.Release()
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.SupportedKubernetes).To(Equal(original.SupportedKubernetes))
		Expect(restored.TenantKubernetesVersion).To(Equal(original.TenantKubernetesVersion))
		Expect(restored.Hardware[1].Family).To(Equal("BlueField-4"))
		Expect(restored.Hardware[1].Models).To(BeEmpty())
		Expect(restored.Hardware[1].BFB).To(BeNil())
		Expect(restored.Hardware[1].BlueFieldSoftware.OSISO.ReasonCode).To(Equal("BF4_ARTIFACT_NOT_PUBLIC"))
		Expect(restored.Hardware[1].BlueFieldSoftware.PLDMFwBundle).To(BeEmpty())
		for _, model := range []string{"B4240", "BlueField-4", "MT_0000001774"} {
			_, found := restored.RequirementsFor(model)
			Expect(found).To(BeFalse())
		}

		original.Hardware[1].BlueFieldSoftware.PLDMFwBundle = map[string]Artifact{
			"MT_TESTPSID": {Version: "test-release", URL: "https://artifacts.example/test-pldm.bin",
				Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		}
		withPSID := Summarize(original)
		withPSIDBytes, err := yaml.Marshal(withPSID)
		Expect(err).NotTo(HaveOccurred())
		var withPSIDSaved Summary
		Expect(yaml.Unmarshal(withPSIDBytes, &withPSIDSaved)).To(Succeed())
		withPSIDRestored, err := withPSIDSaved.Release()
		Expect(err).NotTo(HaveOccurred())
		Expect(withPSIDRestored.Hardware[1].BlueFieldSoftware.PLDMFwBundle).To(HaveKey("MT_TESTPSID"))
		Expect(withPSIDRestored.Hardware[1].BlueFieldSoftware.PLDMFwBundle["MT_TESTPSID"].URL).To(Equal("https://artifacts.example/test-pldm.bin"))
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
			{"incomplete Kubernetes range", func(s *Summary) { s.SupportedKubernetes = &VersionRange{Minimum: "1.33"} }},
			{"invalid tenant Kubernetes version", func(s *Summary) { s.TenantKubernetesVersion = "1.35.6" }},
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
		bf4Release, err := Resolve("26.8.0")
		Expect(err).NotTo(HaveOccurred())
		bf4Summary := Summarize(bf4Release)
		bf4Summary.Hardware[1].BlueFieldSoftware.OSISO.ReasonCode = ""
		_, err = bf4Summary.Release()
		Expect(err).To(HaveOccurred())
	})
})

func summaryFixture() Summary {
	r, err := Resolve(DefaultRelease)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return Summarize(r)
}
