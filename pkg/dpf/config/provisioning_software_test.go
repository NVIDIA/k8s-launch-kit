// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DPF provisioning software input", func() {
	BeforeEach(func() {
		Expect(os.Setenv("DPF_TEST_PASSWORD", "provisioning-test-canary")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_TEST_PASSWORD")
	})

	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	valid := func(name string) Artifact {
		return Artifact{URL: "https://artifacts.example/" + name, SHA256: digest}
	}

	It("strictly decodes optional provisioning software without changing discovery defaults", func() {
		for _, document := range []string{"", "provisioning: null\n", "provisioning: {software: {}}\n"} {
			input, err := Decode([]byte(document+seed), "/site/input.yaml")
			Expect(err).NotTo(HaveOccurred())
			req, err := Resolve(input)
			Expect(err).NotTo(HaveOccurred())
			Expect(req.Software).NotTo(BeNil())
			if strings.Contains(document, "software") {
				Expect(input.Site.Provisioning).NotTo(BeNil())
				_, issues, err := ValidateProvisioningSoftware(input.Site.Provisioning, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(issues).To(ContainElement(ProvisioningIssue{Field: "/provisioning/software/osISO", ReasonCode: "OS_ISO_ARTIFACT_MISSING"}))
			}
		}

		input, err := Decode([]byte("provisioning: {software: {osISO: {url: https://a.example/os.iso, sha256: "+digest+"}, pldm: {MT_0000001774: {url: https://a.example/fw.pkg, sha256: "+digest+"}}}}\n"+seed), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(input.Site.Provisioning.Software.OSISO.SHA256).To(Equal(digest))
		Expect(input.Site.Provisioning.Software.PLDM).To(HaveKey("MT_0000001774"))
		_, err = Decode([]byte("provisioning: {unknown: true}\n"+seed), "/site/input.yaml")
		Expect(err).To(HaveOccurred())
	})

	It("canonicalizes PSID keys, reports absent inputs, and returns independent values", func() {
		input := &Provisioning{Software: Software{OSISO: valid("os.iso"), PLDM: map[string]Artifact{"mt_0000001774": valid("fw.pkg")}}}
		resolved, issues, err := ValidateProvisioningSoftware(input, []string{"MT_0000001774", "mt_0000001774"})
		Expect(err).NotTo(HaveOccurred())
		Expect(issues).To(BeEmpty())
		Expect(resolved.PLDM).To(HaveKey("MT_0000001774"))
		resolved.PLDM["MT_0000001774"] = Artifact{}
		Expect(input.Software.PLDM["mt_0000001774"].URL).To(Equal("https://artifacts.example/fw.pkg"))

		resolved, issues, err = ValidateProvisioningSoftware(nil, []string{"MT_0000001774"})
		Expect(err).NotTo(HaveOccurred())
		Expect(resolved.PLDM).To(BeEmpty())
		Expect(issues).To(ConsistOf(
			ProvisioningIssue{Field: "/provisioning/software/osISO", ReasonCode: "OS_ISO_ARTIFACT_MISSING"},
			ProvisioningIssue{Field: "/provisioning/software/pldm/MT_0000001774", ReasonCode: "PLDM_ARTIFACT_MISSING"},
		))
	})

	DescribeTable("reports partial inputs and rejects malformed supplied values", func(osISO Artifact, pldm map[string]Artifact, selected []string, issueCodes []string, errorText string) {
		software, issues, err := ValidateProvisioningSoftware(&Provisioning{Software: Software{OSISO: osISO, PLDM: pldm}}, selected)
		if errorText != "" {
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(errorText))
			return
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(software.OSISO).To(Equal(osISO))
		codes := make([]string, len(issues))
		for i, issue := range issues {
			codes[i] = issue.ReasonCode
		}
		Expect(codes).To(Equal(issueCodes))
	},
		Entry("partial ISO and PLDM are discoverable", Artifact{URL: "https://artifacts.example/os.iso"}, map[string]Artifact{"MT_0000001774": {SHA256: digest}}, []string{"MT_0000001774"}, []string{"OS_ISO_ARTIFACT_MISSING", "PLDM_ARTIFACT_MISSING"}, ""),
		Entry("missing selected PSID", valid("os.iso"), nil, []string{"MT_0000001774"}, []string{"PLDM_ARTIFACT_MISSING"}, ""),
		Entry("malformed selected PSID", valid("os.iso"), nil, []string{"bad"}, nil, "invalid selected PSID"),
		Entry("malformed map PSID", valid("os.iso"), map[string]Artifact{"MT_bad-value": valid("fw.pkg")}, []string{"MT_0000001774"}, nil, "invalid PLDM PSID"),
		Entry("duplicate case variants", valid("os.iso"), map[string]Artifact{"MT_0000001774": valid("a.pkg"), "mt_0000001774": valid("b.pkg")}, []string{"MT_0000001774"}, nil, "duplicate PLDM PSID"),
		Entry("unselected extra", valid("os.iso"), map[string]Artifact{"MT_0000001774": valid("a.pkg"), "MT_0000001775": valid("b.pkg")}, []string{"MT_0000001774"}, nil, "unselected PSID"),
		Entry("URL credentials", Artifact{URL: "https://user:password@artifacts.example/os.iso", SHA256: digest}, nil, []string{"MT_0000001774"}, nil, "credential-free HTTPS"),
		Entry("URL query", Artifact{URL: "https://artifacts.example/os.iso?token=secret", SHA256: digest}, nil, []string{"MT_0000001774"}, nil, "without query"),
		Entry("empty URL query delimiter", Artifact{URL: "https://artifacts.example/os.iso?", SHA256: digest}, nil, []string{"MT_0000001774"}, nil, "without query"),
		Entry("empty URL fragment delimiter", Artifact{URL: "https://artifacts.example/os.iso#", SHA256: digest}, nil, []string{"MT_0000001774"}, nil, "without query or fragment"),
		Entry("non HTTPS URL", Artifact{URL: "http://artifacts.example/os.iso", SHA256: digest}, nil, []string{"MT_0000001774"}, nil, "credential-free HTTPS"),
		Entry("uppercase digest", Artifact{URL: "https://artifacts.example/os.iso", SHA256: strings.Repeat("A", 64)}, nil, []string{"MT_0000001774"}, nil, "lowercase hexadecimal"),
		Entry("short digest", Artifact{URL: "https://artifacts.example/os.iso", SHA256: "abcd"}, nil, []string{"MT_0000001774"}, nil, "lowercase hexadecimal"),
	)
})
