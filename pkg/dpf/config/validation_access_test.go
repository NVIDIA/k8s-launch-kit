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

var _ = Describe("validation access resolution", func() {
	BeforeEach(func() {
		Expect(os.Setenv("DPF_TEST_PASSWORD", "validation-secret-canary")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_TEST_PASSWORD")
	})

	It("retains the management binding and independent hosts when a DPU access file is missing", func() {
		in, err := Decode([]byte(seed), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		missing := "private-credential-canary"
		in.Site.Credentials["missing"] = Credential{Username: "reader", PasswordRef: PasswordRef{File: &missing}}
		in.Site.Discovery.DPUs[0].CredentialRef = "missing"

		request, issues, err := ResolveForValidation(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(request.Kubeconfig).To(Equal("/site/credentials/mgmt"))
		Expect(request.Targets).To(HaveLen(1))
		Expect(request.Targets[0].InputRef).To(Equal("/discovery/hosts/0"))
		Expect(request.Input.Site.Discovery.DPUs).To(HaveLen(1), "unavailable sources remain in the expected scope")
		Expect(issues).To(Equal([]AccessIssue{{InputRef: "/discovery/dpus/0", Role: "dpus", Message: "cannot resolve password file"}}))
		_, err = Resolve(in)
		Expect(err).To(HaveOccurred(), "deployment and discovery retain fail-fast access semantics")
	})

	It("records every unavailable source without inventing usable targets or leaking access paths", func() {
		missing := strings.Replace(seed, "passwordRef: {env: DPF_TEST_PASSWORD}", "passwordRef: {file: private-path-canary}", 1)
		in, err := Decode([]byte(missing), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		request, issues, err := ResolveForValidation(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(request.Targets).To(BeEmpty())
		Expect(issues).To(HaveLen(2))
		Expect(issues[0].InputRef).To(Equal("/discovery/dpus/0"))
		Expect(issues[1].InputRef).To(Equal("/discovery/hosts/0"))
		for _, issue := range issues {
			Expect(issue.Message).NotTo(ContainSubstring("canary"))
		}
	})

	It("rejects unsafe structural input after an unavailable source without returning a partial request", func() {
		missing := strings.Replace(seed, "passwordRef: {env: DPF_TEST_PASSWORD}", "passwordRef: {file: missing}", 1)
		in, err := Decode([]byte(missing), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		in.Site.Discovery.Hosts[0].BMCEndpoint = "https://credential-canary@host.example"
		request, issues, err := ResolveForValidation(in)
		Expect(err).To(HaveOccurred())
		Expect(request).To(Equal(Request{}))
		Expect(issues).To(BeNil())
		Expect(err.Error()).NotTo(ContainSubstring("credential-canary"))
	})

	It("reports missing SSH trust separately while preserving other resolved access", func() {
		document := strings.Replace(seed, "hosts: [{bmcEndpoint: 'https://192.0.2.2', credentialRef: read}]", "hosts: [{ssh: {address: host.example, knownHostsFile: missing-known-hosts}, credentialRef: read}]", 1)
		in, err := Decode([]byte(document), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		request, issues, err := ResolveForValidation(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(request.Targets).To(HaveLen(1))
		Expect(request.Targets[0].Role).To(Equal("dpus"))
		Expect(issues).To(Equal([]AccessIssue{{InputRef: "/discovery/hosts/0", Role: "hosts", Message: "cannot resolve nonempty SSH knownHostsFile"}}))
	})

	It("preserves switch selection and validates its ports even when switch credentials cannot be resolved", func() {
		document := "dpf: {useCase: hbn-bgp}\n" + seed + `  switches:
    - name: leaf-a
      ssh: {address: leaf.example, knownHostsFile: unavailable}
      credentialRef: read
      ports: [{name: swp1, dpuSerial: SERIAL-A, dpuInterface: p0}]
`
		in, err := Decode([]byte(document), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		request, issues, err := ResolveForValidation(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(request.Targets).To(HaveLen(2))
		Expect(request.Switches).To(BeEmpty())
		Expect(request.Input.Site.Discovery.Switches).To(HaveLen(1))
		Expect(issues).To(Equal([]AccessIssue{{InputRef: "/discovery/switches/0", Role: "switches", Message: "cannot resolve nonempty SSH knownHostsFile"}}))
		in.Site.Discovery.Switches[0].Ports[0].DPUInterface = "guessed-interface"
		request, issues, err = ResolveForValidation(in)
		Expect(err).To(HaveOccurred())
		Expect(request).To(Equal(Request{}))
		Expect(issues).To(BeNil())
	})

	It("uses identical access snapshots for ordinary and validation resolution when all references are available", func() {
		dir := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(dir, "known_hosts"), []byte("host.example ssh-ed25519 AAAA\n"), 0o600)).To(Succeed())
		document := strings.Replace(seed, "hosts: [{bmcEndpoint: 'https://192.0.2.2', credentialRef: read}]", "hosts: [{ssh: {address: host.example, knownHostsFile: known_hosts}, credentialRef: read}]", 1)
		in, err := Decode([]byte(document), filepath.Join(dir, "input.yaml"))
		Expect(err).NotTo(HaveOccurred())
		ordinary, err := Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		observed, issues, err := ResolveForValidation(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(issues).To(BeEmpty())
		Expect(observed.Kubeconfig).To(Equal(ordinary.Kubeconfig))
		Expect(observed.Targets).To(HaveLen(len(ordinary.Targets)))
		for i := range observed.Targets {
			Expect(observed.Targets[i].InputRef).To(Equal(ordinary.Targets[i].InputRef))
			Expect(observed.Targets[i].Endpoint).To(Equal(ordinary.Targets[i].Endpoint))
			if observed.Targets[i].SSH != nil {
				Expect(observed.Targets[i].SSH.KnownHosts()).To(Equal(ordinary.Targets[i].SSH.KnownHosts()))
			} else {
				Expect(Scope(observed.Targets[i])).To(Equal(Scope(ordinary.Targets[i])))
			}
		}
	})
})
