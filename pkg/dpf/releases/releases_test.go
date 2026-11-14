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
		Expect(SupportedReleases()).To(Equal([]string{"26.4.0", "26.8.0"}))
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
	It("describes the v26.8 BF4 beta artifact mechanism without claiming exact-model support", func() {
		r, err := Resolve("26.8.0")
		Expect(err).NotTo(HaveOccurred())
		Expect(r.TenantKubernetesVersion).To(Equal("v1.35.6"))
		Expect(r.SupportedKubernetes).To(Equal(&VersionRange{Minimum: "1.33", Maximum: "1.36"}))
		Expect(r.Operator.Chart).To(Equal("dpf-operator"))
		Expect(r.Operator.Version).To(Equal("v26.8.0"))
		Expect(r.Operator.ChartDigest).To(Equal("sha256:0252d473beaf473e93a09b23a39b7b78fff931c9a801f4e60ef93bface5e28c8"))
		Expect(r.Operator.SourceRef).To(Equal("published-chart"))
		var chartSource *Source
		for i := range r.Sources {
			if r.Sources[i].ID == r.Operator.SourceRef {
				chartSource = &r.Sources[i]
			}
		}
		Expect(chartSource).NotTo(BeNil())
		Expect(chartSource.URL).To(Equal("https://helm.ngc.nvidia.com/nvidia/doca/charts/dpf-operator-v26.8.0.tgz"))
		Expect(chartSource.Revision).To(Equal("1751dc679f702fdf91c2641d925e69908ae4921f"))
		var bf4 *HardwareBOM
		for i := range r.Hardware {
			if r.Hardware[i].Family == "BlueField-4" {
				bf4 = &r.Hardware[i]
			}
		}
		Expect(bf4).NotTo(BeNil())
		Expect(bf4.Models).To(BeEmpty())
		Expect(bf4.SupportLevel).To(Equal("beta"))
		Expect(bf4.BFB).To(BeNil())
		Expect(bf4.BlueFieldSoftware).NotTo(BeNil())
		Expect(bf4.BlueFieldSoftware.OSISO.URL).To(BeEmpty())
		Expect(bf4.BlueFieldSoftware.OSISO.Version).To(BeEmpty())
		Expect(bf4.BlueFieldSoftware.OSISO.ReasonCode).To(Equal("BF4_ARTIFACT_NOT_PUBLIC"))
		Expect(bf4.BlueFieldSoftware.PLDMFwBundle).To(BeEmpty())
		Expect(bf4.Requirements).To(BeEmpty())
		Expect(r.Components).To(ContainElement(And(HaveField("Name", "openbao"), HaveField("Version", "v2.5.5"), HaveField("Applicability", "optional"))))
		Expect(r.Components).To(ContainElement(And(HaveField("Name", "external-secrets"), HaveField("Version", "v2.7.0"), HaveField("Applicability", "optional"))))
		for _, model := range []string{"B4240", "BlueField-4", "MT_0000001774", "unknown"} {
			_, found := r.RequirementsFor(model)
			Expect(found).To(BeFalse(), "family-only beta entry must not match %q", model)
		}
	})
	It("keeps tenant control-plane versions separate from management Kubernetes support", func() {
		older, err := Resolve("26.4.0")
		Expect(err).NotTo(HaveOccurred())
		Expect(older.TenantKubernetesVersion).To(BeEmpty(), "do not infer an older release's compiled tenant version")
		for _, value := range []string{"", "v1.35", "1.35.6", "v01.35.6", "v1.035.6", "v1.35.06", "v1.35.6-rc.1", " v1.35.6"} {
			Expect(validTenantKubernetesVersion(value)).To(BeFalse(), value)
		}
		for _, value := range []string{"v1.0.0", "v1.35.6"} {
			Expect(validTenantKubernetesVersion(value)).To(BeTrue(), value)
		}
	})
	It("validates PSID-specific BF4 artifact coordinates without fabricating catalog defaults", func() {
		r, err := Resolve("26.8.0")
		Expect(err).NotTo(HaveOccurred())
		bf4 := &r.Hardware[1]
		bf4.BlueFieldSoftware.PLDMFwBundle = map[string]Artifact{
			"MT_TESTPSID": {Version: "test-release", URL: "https://artifacts.example/test-pldm.bin", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SourceRef: "release-notes"},
		}
		Expect(validate(r.Version, r)).To(Succeed())
		bf4.BlueFieldSoftware.PLDMFwBundle["MT_TESTPSID"] = Artifact{Version: "test-release", URL: "https://artifacts.example/test-pldm.bin", SourceRef: "release-notes"}
		Expect(validate(r.Version, r)).To(HaveOccurred(), "a known bundle without a digest or explicit digest limitation must be rejected")
		bf4.BlueFieldSoftware.PLDMFwBundle = map[string]Artifact{"": {Version: "test-release", URL: "https://artifacts.example/test-pldm.bin", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SourceRef: "release-notes"}}
		Expect(validate(r.Version, r)).To(HaveOccurred(), "PSID map keys must be explicit")
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
			strings.Replace(string(catalogYAML), "message: Requires an authorized release-compatible BF4 ISO override; public v26.8 release material does not publish its version or URL.", "message: ''", 1),
			strings.Replace(string(catalogYAML), "maximum: \"1.36\"", "maximum: \"1.32\"", 1),
			strings.Replace(string(catalogYAML), "tenantKubernetesVersion: v1.35.6", "tenantKubernetesVersion: v1.35", 1),
		} {
			_, err := load([]byte(data))
			Expect(err).To(HaveOccurred())
		}
	})
})
