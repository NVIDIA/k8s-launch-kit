// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfig(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "DPF configuration") }

const seed = `# retained comment
management:
  kubeconfigRef: {file: credentials/mgmt}
credentials:
  read:
    username: reader
    passwordRef: {env: DPF_TEST_PASSWORD}
discovery:
  dpus: [{bmcEndpoint: 'https://192.0.2.1', credentialRef: read}]
  hosts: [{bmcEndpoint: 'https://192.0.2.2', credentialRef: read}]
`

var _ = Describe("DPF input", func() {
	BeforeEach(func() {
		Expect(os.Setenv("DPF_TEST_PASSWORD", " canary \n")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_TEST_PASSWORD")
	})
	It("retains endpoint names for presentation without changing access scope", func() {
		in, err := Decode([]byte(seed), "/site/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		before, err := Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		name := "Rack A DPU"
		in.Site.Discovery.DPUs[0].Name = &name
		after, err := Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(after.Targets[0].Name).To(Equal(name))
		Expect(after.Targets[0].DisplayName()).To(Equal(`"Rack A DPU" (https://192.0.2.1)`))
		Expect(Scope(after.Targets[0])).To(Equal(Scope(before.Targets[0])))
	})
	It("elides private URL components and escapes control characters in display labels", func() {
		target := Target{Name: "Rack\nA\x1b", Endpoint: "https://user-canary:password-canary@host.example:443/private-path-canary?token=query-canary#fragment-canary"}
		Expect(target.DisplayName()).To(Equal(`"Rack\nA\x1b" (https://host.example:443)`))
		Expect(Target{Endpoint: "ssh://host.example:22"}.DisplayName()).To(Equal("ssh://host.example:22"))
		Expect(Target{Endpoint: "invalid\nendpoint"}.DisplayName()).To(Equal("<invalid endpoint>"))
	})
	It("uses bounded defaults and keeps private values out of saved YAML", func() {
		in, e := Decode([]byte(seed), "/site/input.yaml")
		Expect(e).NotTo(HaveOccurred())
		r, e := Resolve(in)
		Expect(e).NotTo(HaveOccurred())
		Expect(r.Timeout).To(Equal(10 * time.Minute))
		_, password := r.Targets[0].Access.Credentials()
		Expect(password).To(Equal(" canary \n"))
		b, e := Saved(in, &inventory.Inventory{FormatVersion: "v1alpha1"}, "/results/out.yaml")
		Expect(e).NotTo(HaveOccurred())
		Expect(string(b)).To(ContainSubstring("# retained comment"))
		Expect(string(b)).To(ContainSubstring("../site/credentials/mgmt"))
		Expect(string(b)).NotTo(ContainSubstring("canary"))
	})
	It("rejects duplicate, unknown, cyclic and multiple documents", func() {
		for _, input := range []string{seed + "management: {}\n", strings.Replace(seed, "discovery:", "typo:", 1), "a: &x {b: *x}", seed + "---\nfoo: bar\n"} {
			_, e := Decode([]byte(input), "input.yaml")
			Expect(e).To(HaveOccurred())
		}
	})
	It("does not replace explicit zero, empty mode or empty TLS path with defaults", func() {
		for _, addition := range []string{"  limits: {maxConcurrentBMCs: 0}\n", "  probes: {mode: ''}\n", "  tls: {caFile: ''}\n"} {
			in, e := Decode([]byte(seed+addition), "/site/in.yaml")
			Expect(e).NotTo(HaveOccurred())
			_, e = Resolve(in)
			Expect(e).To(HaveOccurred())
		}
	})
	It("uses null as omitted", func() {
		in, e := Decode([]byte(seed+"  limits: {maxConcurrentBMCs: null}\n"), "/site/in.yaml")
		Expect(e).NotTo(HaveOccurred())
		r, e := Resolve(in)
		Expect(e).NotTo(HaveOccurred())
		Expect(r.MaxConcurrentBMCs).To(Equal(4))
	})
	It("validates merged scalar values at their effective paths and preserves explicit overrides", func() {
		for _, addition := range []string{
			"  limits: {<<: {maxConcurrentBMCs: 2}}\n",
			"  limits: {<<: [{maxConcurrentBMCs: 2}, {maxConcurrentBMCs: 3}]}\n",
			"  limits: {<<: {maxConcurrentBMCs: wrong}, maxConcurrentBMCs: 2}\n",
		} {
			in, err := Decode([]byte(seed+addition+"  tls: {<<: {insecureSkipVerify: true}, insecureSkipVerify: false}\n"), "/site/in.yaml")
			Expect(err).NotTo(HaveOccurred())
			r, err := Resolve(in)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.MaxConcurrentBMCs).To(Equal(2))
			Expect(r.Targets[0].Access.InsecureSkipVerify()).To(BeFalse())
			b, err := Saved(in, &inventory.Inventory{FormatVersion: "v1alpha1"}, "/site/out.yaml")
			Expect(err).NotTo(HaveOccurred())
			again, err := Decode(b, "/site/out.yaml")
			Expect(err).NotTo(HaveOccurred())
			resolved, err := Resolve(again)
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved.MaxConcurrentBMCs).To(Equal(2))
			Expect(resolved.Targets[0].Access.InsecureSkipVerify()).To(BeFalse())
		}
	})
	It("rejects invalid effective merged types and duplicate keys before expansion", func() {
		for _, addition := range []string{
			"  limits: {<<: {maxConcurrentBMCs: '2'}}\n",
			"  tls: {<<: {insecureSkipVerify: 'true'}}\n",
			"  limits: {<<: {maxConcurrentBMCs: 2, maxConcurrentBMCs: 3}}\n",
		} {
			_, err := Decode([]byte(seed+addition), "/site/in.yaml")
			Expect(err).To(HaveOccurred())
		}
	})
	It("expands aliased merge policies without losing an explicit false override", func() {
		text := strings.Replace(seed, "discovery:\n", "discovery:\n  tls: &policy {insecureSkipVerify: true}\n", 1)
		text = strings.ReplaceAll(text, "credentialRef: read}", "credentialRef: read, tls: {<<: *policy, insecureSkipVerify: false}}")
		in, err := Decode([]byte(text), "/site/in.yaml")
		Expect(err).NotTo(HaveOccurred())
		r, err := Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		for _, target := range r.Targets {
			Expect(target.Access.InsecureSkipVerify()).To(BeFalse())
		}
	})
	It("keeps BMC verification enabled unless explicitly disabled and preserves presence", func() {
		for _, value := range []string{"", "false", "null", "true"} {
			text := seed
			if value != "" {
				text += "  tls: {insecureSkipVerify: " + value + "}\n"
			}
			in, err := Decode([]byte(text), "/site/in.yaml")
			Expect(err).NotTo(HaveOccurred())
			r, err := Resolve(in)
			Expect(err).NotTo(HaveOccurred())
			for _, target := range r.Targets {
				Expect(target.Access.InsecureSkipVerify()).To(Equal(value == "true"))
				Expect(target.Access.TLSConfig().InsecureSkipVerify).To(Equal(value == "true"))
			}
			b, err := Saved(in, &inventory.Inventory{FormatVersion: "v1alpha1"}, "/results/out.yaml")
			Expect(err).NotTo(HaveOccurred())
			if value == "" {
				Expect(string(b)).NotTo(ContainSubstring("insecureSkipVerify"))
			} else {
				Expect(string(b)).To(ContainSubstring("insecureSkipVerify: " + value))
			}
		}
	})
	It("isolates cached TLS policies and lets endpoint false replace global true", func() {
		for _, role := range []string{"dpus", "hosts"} {
			text := seed + "  tls: {insecureSkipVerify: true}\n"
			lines := strings.Split(text, "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "  "+role+":") {
					lines[i] = strings.Replace(line, "credentialRef: read", "credentialRef: read, tls: {insecureSkipVerify: false}", 1)
				}
			}
			in, err := Decode([]byte(strings.Join(lines, "\n")), "/site/in.yaml")
			Expect(err).NotTo(HaveOccurred())
			r, err := Resolve(in)
			Expect(err).NotTo(HaveOccurred())
			for _, target := range r.Targets {
				Expect(target.Access.InsecureSkipVerify()).To(Equal(target.Role != role))
				clone := target.Access.TLSConfig()
				clone.InsecureSkipVerify = !clone.InsecureSkipVerify
				Expect(target.Access.InsecureSkipVerify()).To(Equal(target.Role != role))
			}
		}
	})
	It("rejects nonboolean insecure settings and contradictory CA trust", func() {
		for _, value := range []string{"'true'", "1", "''"} {
			_, err := Decode([]byte(seed+"  tls: {insecureSkipVerify: "+value+"}\n"), "/site/in.yaml")
			Expect(err).To(HaveOccurred())
		}
		in, err := Decode([]byte(seed+"  tls: {insecureSkipVerify: true, caFile: ca.pem}\n"), "/site/in.yaml")
		Expect(err).NotTo(HaveOccurred())
		_, err = Resolve(in)
		Expect(err).To(MatchError(ContainSubstring("mutually exclusive")))
	})
	It("normalizes DNS/default ports and rejects paths, userinfo and duplicate origins", func() {
		origin, e := Origin("https://EXAMPLE.invalid:443/")
		Expect(e).NotTo(HaveOccurred())
		Expect(origin).To(Equal("https://example.invalid"))
		for _, v := range []string{"http://x", "https://u:p@x", "https://x/path", "https://x?x=y", "https://x:99999"} {
			_, e := Origin(v)
			Expect(e).To(HaveOccurred())
		}
		in, e := Decode([]byte(strings.Replace(seed, "192.0.2.2", "192.0.2.1:443", 1)), "/site/in.yaml")
		Expect(e).NotTo(HaveOccurred())
		_, e = Resolve(in)
		Expect(e).To(HaveOccurred())
	})
	It("removes only one final LF or CRLF from file passwords", func() {
		dir, e := os.MkdirTemp("", "dpf-config-")
		Expect(e).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		for _, value := range []string{" space \r\n", " space \n\n", " space \r"} {
			Expect(os.WriteFile(filepath.Join(dir, "password"), []byte(value), 0600)).To(Succeed())
			text := strings.Replace(seed, "env: DPF_TEST_PASSWORD", "file: password", 1)
			in, e := Decode([]byte(text), filepath.Join(dir, "in.yaml"))
			Expect(e).NotTo(HaveOccurred())
			r, e := Resolve(in)
			Expect(e).NotTo(HaveOccurred())
			_, actual := r.Targets[0].Access.Credentials()
			expected := value
			if strings.HasSuffix(expected, "\n") {
				expected = strings.TrimSuffix(strings.TrimSuffix(expected, "\n"), "\r")
			}
			Expect(actual).To(Equal(expected))
		}
	})
})

var _ = Describe("strict scalar regression", func() {
	It("rejects boolean and numeric access fields rather than coercing strings", func() {
		for _, value := range []string{"false", "123"} {
			_, err := Decode([]byte(strings.Replace(seed, "username: reader", "username: "+value, 1)), "/site/in.yaml")
			Expect(err).To(HaveOccurred())
		}
	})
})
