// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	yaml "gopkg.in/yaml.v3"
)

var _ = Describe("validation trust proof", func() {
	var in Input
	var dir string
	var originalCA, known []byte

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		originalCA, known = trustTestCA(10), []byte("host.example ssh-ed25519 AAAA\n")
		_, key, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		der, err := x509.MarshalPKCS8PrivateKey(key)
		Expect(err).NotTo(HaveOccurred())
		for name, contents := range map[string][]byte{
			"ca.pem": originalCA, "known_hosts": known,
			"host-key":      pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
			"host-password": []byte("host-secret-canary\n"), "read-password": []byte("read-secret-canary\n"),
		} {
			Expect(os.WriteFile(filepath.Join(dir, name), contents, 0o600)).To(Succeed())
		}
		in, err = Decode([]byte(`dpf: {useCase: hbn-bgp}
credentials:
  read: {username: reader, passwordRef: {file: read-password}}
  host: {username: host, passwordRef: {file: host-password}}
discovery:
  dpus:
    - bmcEndpoint: https://dpu.example
      credentialRef: read
      systemURI: /redfish/v1/Systems/Selected
      tls: {caFile: ca.pem}
  hosts:
    - ssh: {address: host.example, knownHostsFile: known_hosts, sudo: true}
      credentialRef: host
  switches:
    - name: leaf
      ssh: {address: leaf.example, knownHostsFile: known_hosts, sudo: false}
      credentialRef: read
      ports: [{name: swp1, dpuSerial: SERIAL-A, dpuInterface: p0}]
`), filepath.Join(dir, "input.yaml"))
		Expect(err).NotTo(HaveOccurred())
	})

	It("proves every source's policy with missing password or private key and detects changed trust elsewhere", func() {
		for _, keyAuth := range []bool{false, true} {
			for _, changedFile := range []string{"known_hosts", "ca.pem"} {
				Expect(os.WriteFile(filepath.Join(dir, "known_hosts"), known, 0o600)).To(Succeed())
				Expect(os.WriteFile(filepath.Join(dir, "ca.pem"), originalCA, 0o600)).To(Succeed())
				Expect(os.WriteFile(filepath.Join(dir, "host-password"), []byte("host-secret-canary"), 0o600)).To(Succeed())
				credential := Credential{Username: "host"}
				if keyAuth {
					credential.PrivateKeyRef = &FileRef{File: "host-key"}
				} else {
					password := "host-password"
					credential.PasswordRef.File = &password
				}
				in.Site.Credentials["host"] = credential
				before, issues, err := ResolveForValidation(in)
				Expect(err).NotTo(HaveOccurred())
				Expect(issues).To(BeEmpty())
				baseline, complete := before.ValidationScope()
				Expect(complete).To(BeTrue())
				Expect(baseline).To(HaveLen(3))
				Expect(baseline).To(ContainElement(Scope(before.Targets[0])))
				Expect(baseline).To(ContainElement(Scope(before.Targets[1])))
				Expect(baseline).To(ContainElement(Scope(Target{Role: "switches", Endpoint: before.Switches[0].Endpoint, SSH: before.Switches[0].Access})))
				Expect(Scope(before.Targets[0]).SystemURI).To(Equal("/redfish/v1/Systems/Selected"))

				message := "cannot resolve password file"
				if keyAuth {
					credential.PrivateKeyRef = &FileRef{File: "missing-private-key-canary"}
					in.Site.Credentials["host"] = credential
					message = "cannot resolve SSH private key"
				} else {
					Expect(os.Remove(filepath.Join(dir, "host-password"))).To(Succeed())
				}
				partial, issues, err := ResolveForValidation(in)
				Expect(err).NotTo(HaveOccurred())
				Expect(issues).To(Equal([]AccessIssue{{InputRef: "/discovery/hosts/0", Role: "hosts", Message: message}}))
				Expect(partial.Targets).To(HaveLen(1))
				Expect(partial.Switches).To(HaveLen(1))
				scope, complete := partial.ValidationScope()
				Expect(complete).To(BeTrue())
				Expect(scope).To(Equal(baseline), "authentication availability cannot remove trust from the aggregate")

				changed := []byte("leaf.example ssh-ed25519 BBBB\n")
				if changedFile == "ca.pem" {
					changed = trustTestCA(11)
				}
				Expect(os.WriteFile(filepath.Join(dir, changedFile), changed, 0o600)).To(Succeed())
				after, _, err := ResolveForValidation(in)
				Expect(err).NotTo(HaveOccurred())
				changedScope, complete := after.ValidationScope()
				Expect(complete).To(BeTrue())
				Expect(changedScope).NotTo(Equal(baseline))
				frozen, complete := partial.ValidationScope()
				Expect(complete).To(BeTrue())
				Expect(frozen).To(Equal(baseline))
				Expect(partial.Targets[0].Access.PublicCA()).To(Equal(originalCA))
				Expect(partial.Switches[0].Access.KnownHosts()).To(Equal(known))
			}
		}
	})

	It("retains custom Redfish CA proof independently of its missing password", func() {
		before, _, err := ResolveForValidation(in)
		Expect(err).NotTo(HaveOccurred())
		baseline, complete := before.ValidationScope()
		Expect(complete).To(BeTrue())
		Expect(os.Remove(filepath.Join(dir, "read-password"))).To(Succeed())
		partial, issues, err := ResolveForValidation(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(partial.Targets).To(HaveLen(1))
		Expect(partial.Targets[0].Role).To(Equal("hosts"))
		Expect(partial.Switches).To(BeEmpty())
		Expect(issues).To(ContainElement(AccessIssue{InputRef: "/discovery/dpus/0", Role: "dpus", Message: "cannot resolve password file"}))
		scope, complete := partial.ValidationScope()
		Expect(complete).To(BeTrue())
		Expect(scope).To(Equal(baseline))
		Expect(os.WriteFile(filepath.Join(dir, "ca.pem"), trustTestCA(12), 0o600)).To(Succeed())
		after, _, err := ResolveForValidation(in)
		Expect(err).NotTo(HaveOccurred())
		scope, complete = after.ValidationScope()
		Expect(complete).To(BeTrue())
		Expect(scope).NotTo(Equal(baseline))
		Expect(os.Remove(filepath.Join(dir, "ca.pem"))).To(Succeed())
		in.Site.Discovery.Switches = nil
		_, err = Resolve(in)
		Expect(err).To(MatchError("/discovery/dpus/0: cannot resolve password file"), "ordinary Redfish resolution keeps authentication-first precedence")
		unproven, issues, err := ResolveForValidation(in)
		Expect(err).NotTo(HaveOccurred())
		_, complete = unproven.ValidationScope()
		Expect(complete).To(BeFalse())
		Expect(issues).To(ContainElement(AccessIssue{InputRef: "/discovery/dpus/0", Role: "dpus", Message: "cannot resolve password file"}))
	})

	It("freezes all successful transport trust against later file and returned-byte changes", func() {
		request, issues, err := ResolveForValidation(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(issues).To(BeEmpty())
		baseline, complete := request.ValidationScope()
		Expect(complete).To(BeTrue())
		Expect(os.WriteFile(filepath.Join(dir, "known_hosts"), []byte("replacement trust"), 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "ca.pem"), trustTestCA(13), 0o600)).To(Succeed())
		request.Targets[0].Access.PublicCA()[0] = '!'
		request.Targets[1].SSH.KnownHosts()[0] = '!'
		request.Switches[0].Access.KnownHosts()[0] = '!'
		for _, target := range request.Targets {
			Expect(baseline).To(ContainElement(Scope(target)))
		}
		Expect(baseline).To(ContainElement(Scope(Target{Role: "switches", Endpoint: request.Switches[0].Endpoint, SSH: request.Switches[0].Access})))
		Expect(request.Targets[0].Access.PublicCA()).To(Equal(originalCA))
		Expect(request.Targets[1].SSH.KnownHosts()).To(Equal(known))
		Expect(request.Switches[0].Access.KnownHosts()).To(Equal(known))
	})

	It("marks unreadable trust incomplete while retaining independently authenticated targets and issues", func() {
		for _, trustFile := range []string{"ca.pem", "known_hosts"} {
			path := filepath.Join(dir, trustFile)
			contents, err := os.ReadFile(path)
			Expect(err).NotTo(HaveOccurred())
			Expect(os.Remove(path)).To(Succeed())
			request, issues, err := ResolveForValidation(in)
			Expect(err).NotTo(HaveOccurred())
			scope, complete := request.ValidationScope()
			Expect(complete).To(BeFalse())
			Expect(scope).NotTo(BeEmpty())
			Expect(issues).NotTo(BeEmpty())
			Expect(request.Input.Site.Discovery.Hosts).To(HaveLen(1))
			Expect(request.Input.Site.Discovery.DPUs).To(HaveLen(1))
			Expect(request.Input.Site.Discovery.Switches).To(HaveLen(1))
			if trustFile == "ca.pem" {
				Expect(request.Targets).To(HaveLen(1))
				Expect(request.Targets[0].Role).To(Equal("hosts"))
				Expect(request.Switches).To(HaveLen(1))
			} else {
				Expect(request.Targets).To(HaveLen(1))
				Expect(request.Targets[0].Role).To(Equal("dpus"))
				Expect(request.Switches).To(BeEmpty())
			}
			Expect(os.WriteFile(path, contents, 0o600)).To(Succeed())
		}
	})

	It("does not mistake unavailable authentication for proof when that source's trust is also unreadable", func() {
		Expect(os.Remove(filepath.Join(dir, "host-password"))).To(Succeed())
		Expect(os.Remove(filepath.Join(dir, "known_hosts"))).To(Succeed())
		request, issues, err := ResolveForValidation(in)
		Expect(err).NotTo(HaveOccurred())
		_, complete := request.ValidationScope()
		Expect(complete).To(BeFalse())
		Expect(issues).To(ContainElement(AccessIssue{InputRef: "/discovery/hosts/0", Role: "hosts", Message: "cannot resolve password file"}))
		_, err = Resolve(in)
		Expect(err).To(MatchError(ContainSubstring("cannot resolve nonempty SSH knownHostsFile")), "ordinary resolution still stops at the first switch before hosts")
		in.Site.Discovery.Switches = nil
		_, err = Resolve(in)
		Expect(err).To(MatchError(ContainSubstring("cannot resolve password file")), "ordinary SSH resolution retains authentication-first error precedence")
	})

	It("copies the proof through observer snapshots without serializing or authorizing mutated targets", func() {
		request, issues, err := ResolveForValidation(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(issues).To(BeEmpty())
		baseline, complete := request.ValidationScope()
		Expect(complete).To(BeTrue())
		frozen, err := SnapshotRequestForValidation(request)
		Expect(err).NotTo(HaveOccurred())
		scope, complete := frozen.ValidationScope()
		Expect(complete).To(BeTrue())
		Expect(scope).To(Equal(baseline))
		scope[0].Endpoint = "ssh://changed.example:22"
		request.validationScope[0].Endpoint = "ssh://caller.example:22"
		request.Targets[1].SSH.Sudo = false
		proof, complete := frozen.ValidationScope()
		Expect(complete).To(BeTrue())
		Expect(proof).To(Equal(baseline))
		Expect(proof).To(ContainElement(Scope(frozen.Targets[1])))
		frozen.Targets[1].SSH.Sudo = false
		Expect(proof).NotTo(ContainElement(Scope(frozen.Targets[1])), "the retained proof cannot authorize public target mutation")
		// Input retains YAML document nodes, which are not standalone nested YAML values.
		serializable := frozen
		serializable.Input = Input{}
		for _, encode := range []func(interface{}) ([]byte, error){json.Marshal, yaml.Marshal} {
			encoded, err := encode(serializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(encoded)).NotTo(ContainSubstring("validationScope"))
			for _, endpoint := range baseline {
				if endpoint.SSHTrustDigest != "" {
					Expect(string(encoded)).NotTo(ContainSubstring(endpoint.SSHTrustDigest))
				}
				if endpoint.TLSTrustDigest != "" {
					Expect(string(encoded)).NotTo(ContainSubstring(endpoint.TLSTrustDigest))
				}
			}
		}
	})

	It("includes repeated cached Redfish policy for every declared endpoint and leaves ordinary requests unproven", func() {
		in.Site.Discovery.Hosts[0] = Endpoint{BMCEndpoint: "https://host.example", CredentialRef: "read", TLS: in.Site.Discovery.DPUs[0].TLS}
		request, issues, err := ResolveForValidation(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(issues).To(BeEmpty())
		scope, complete := request.ValidationScope()
		Expect(complete).To(BeTrue())
		Expect(scope).To(HaveLen(3))
		for _, target := range request.Targets {
			Expect(scope).To(ContainElement(Scope(target)))
		}
		ordinary, err := Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		_, complete = ordinary.ValidationScope()
		Expect(complete).To(BeFalse())
		cloned, err := SnapshotRequestForValidation(ordinary)
		Expect(err).NotTo(HaveOccurred())
		_, complete = cloned.ValidationScope()
		Expect(complete).To(BeFalse(), "cloning cannot manufacture a validation proof")
	})
})
