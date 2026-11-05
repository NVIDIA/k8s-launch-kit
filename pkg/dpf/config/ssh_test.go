// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

var _ = Describe("explicit SSH input", func() {
	var dir, document string
	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		public, _, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		key, err := ssh.NewPublicKey(public)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(dir, "known_hosts"), []byte(knownhosts.Line([]string{"host.example"}, key)+"\n"), 0600)).To(Succeed())
		Expect(os.Setenv("SSH_TEST_PASSWORD", "secret-canary")).To(Succeed())
		DeferCleanup(os.Unsetenv, "SSH_TEST_PASSWORD")
		document = `credentials:
  reader: {username: reader, passwordRef: {env: SSH_TEST_PASSWORD}}
discovery:
  hosts:
    - credentialRef: reader
      ssh: {address: host.example, knownHostsFile: known_hosts, sudo: false}
  probes: {mode: inventory}
`
	})
	It("resolves without Kubernetes or DPU endpoints, snapshots trust and preserves references", func() {
		in, err := Decode([]byte(document), filepath.Join(dir, "site.yaml"))
		Expect(err).NotTo(HaveOccurred())
		r, err := Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Kubeconfig).To(BeEmpty())
		Expect(r.Targets).To(HaveLen(1))
		t := r.Targets[0]
		Expect(t.Endpoint).To(Equal("ssh://host.example:22"))
		Expect(t.SSH.Sudo).To(BeFalse())
		_, password := t.SSH.Credentials()
		Expect(password).To(Equal("secret-canary"))
		raw, err := json.Marshal(t.SSH)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).NotTo(ContainSubstring("canary"))
		Expect(fmt.Sprintf("%#v", t.SSH)).NotTo(ContainSubstring("canary"))
		before := Scope(t).SSHTrustDigest
		changed := t.SSH.KnownHosts()
		changed[0] = 'X'
		Expect(Scope(t).SSHTrustDigest).To(Equal(before))
		out, err := Saved(in, nil, filepath.Join(dir, "result", "out.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(ContainSubstring("../known_hosts"))
		Expect(string(out)).To(ContainSubstring("sudo: false"))
		Expect(string(out)).NotTo(ContainSubstring("secret-canary"))
	})
	It("rejects ambiguous transports, auth, invalid presence and management-mode SSH", func() {
		for _, text := range []string{
			strings.Replace(document, "ssh:", "bmcEndpoint: https://host.example\n      ssh:", 1),
			strings.Replace(document, "ssh:", "tls: {insecureSkipVerify: true}\n      ssh:", 1),
			strings.Replace(document, "knownHostsFile: known_hosts", "knownHostsFile: ''", 1),
			strings.Replace(document, "sudo: false", "sudo: 'false'", 1),
			strings.Replace(document, "mode: inventory", "mode: management", 1),
			strings.Replace(document, "passwordRef: {env: SSH_TEST_PASSWORD}", "passwordRef: {env: SSH_TEST_PASSWORD}, privateKeyRef: {file: key}", 1),
		} {
			in, err := Decode([]byte(text), filepath.Join(dir, "site.yaml"))
			if err == nil {
				_, err = Resolve(in)
			}
			Expect(err).To(HaveOccurred())
		}
	})
	It("canonicalizes only bounded address forms", func() {
		for input, want := range map[string]string{"HOST.EXAMPLE": "ssh://host.example:22", "host.example:2222": "ssh://host.example:2222", "[2001:db8::1]": "ssh://[2001:db8::1]:22"} {
			got, err := SSHOrigin(input)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		}
		for _, bad := range []string{"", "user@host", "host/path", "host:0", "host:65536", "host:", "host?x", "ssh://host", "host\ncommand"} {
			_, err := SSHOrigin(bad)
			Expect(err).To(HaveOccurred())
		}
	})
})
