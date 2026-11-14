// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("resolved Redfish trust scope", func() {
	var dir string
	var firstCA, secondCA []byte
	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		firstCA, secondCA = trustTestCA(1), trustTestCA(2)
		Expect(os.WriteFile(filepath.Join(dir, "ca.pem"), firstCA, 0600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "copy.pem"), firstCA, 0600)).To(Succeed())
		Expect(os.Setenv("DPF_TEST_PASSWORD", "trust-secret-canary")).To(Succeed())
		DeferCleanup(os.Unsetenv, "DPF_TEST_PASSWORD")
	})
	resolve := func(policy string) Target {
		in, err := Decode([]byte(seed+policy), filepath.Join(dir, "site.yaml"))
		Expect(err).NotTo(HaveOccurred())
		r, err := Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		return r.Targets[0]
	}
	It("uses CA content instead of file spelling and distinguishes all verification policies", func() {
		custom := resolve("  tls: {caFile: ca.pem}\n")
		copied := resolve("  tls: {caFile: copy.pem}\n")
		Expect(Scope(custom)).To(Equal(Scope(copied)))
		Expect(Scope(custom).TLSTrustDigest).NotTo(BeEmpty())
		Expect(os.WriteFile(filepath.Join(dir, "ca.pem"), secondCA, 0600)).To(Succeed())
		replaced := resolve("  tls: {caFile: ca.pem}\n")
		Expect(Scope(replaced).TLSTrustDigest).NotTo(Equal(Scope(custom).TLSTrustDigest))
		system, insecure := resolve(""), resolve("  tls: {insecureSkipVerify: true}\n")
		Expect(Scope(system).TLSTrustDigest).To(Equal("system-roots"))
		Expect(Scope(insecure).TLSTrustDigest).To(Equal("verification-disabled"))
		Expect(Scope(custom).TLSTrustDigest).NotTo(Equal(Scope(system).TLSTrustDigest))
		Expect(Scope(custom).TLSTrustDigest).NotTo(Equal(Scope(insecure).TLSTrustDigest))
		encoded, err := json.Marshal(Scope(custom))
		Expect(err).NotTo(HaveOccurred())
		for _, private := range []string{"trust-secret-canary", "BEGIN CERTIFICATE", "ca.pem", string(firstCA)} {
			Expect(string(encoded)).NotTo(ContainSubstring(private))
		}
	})
	It("keeps resolved trust immutable when files, returned CA bytes or TLS roots change", func() {
		target := resolve("  tls: {caFile: ca.pem}\n")
		before := Scope(target)
		roots := target.Access.TLSConfig().RootCAs
		Expect(os.WriteFile(filepath.Join(dir, "ca.pem"), secondCA, 0600)).To(Succeed())
		bytes := target.Access.PublicCA()
		bytes[0] = 'X'
		clone := target.Access.TLSConfig()
		Expect(clone.RootCAs.AppendCertsFromPEM(secondCA)).To(BeTrue())
		Expect(clone.RootCAs.Equal(roots)).To(BeFalse())
		Expect(target.Access.TLSConfig().RootCAs.Equal(roots)).To(BeTrue())
		Expect(target.Access.PublicCA()).To(Equal(firstCA))
		Expect(Scope(target)).To(Equal(before))
		Expect((Access{}).TLSConfig()).To(BeNil())
	})
	It("records effective endpoint replacement policy instead of shared CA configuration", func() {
		text := strings.Replace(seed, "credentialRef: read}", "credentialRef: read, tls: {}}", 1)
		in, err := Decode([]byte(text+"  tls: {caFile: ca.pem}\n"), filepath.Join(dir, "site.yaml"))
		Expect(err).NotTo(HaveOccurred())
		r, err := Resolve(in)
		Expect(err).NotTo(HaveOccurred())
		Expect(Scope(r.Targets[0]).TLSTrustDigest).To(Equal("system-roots"))
		Expect(Scope(r.Targets[1]).TLSTrustDigest).NotTo(Equal("system-roots"))
	})
})

func trustTestCA(serial int64) []byte {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "Discovery trust test"}, NotBefore: time.Unix(0, 0), NotAfter: time.Unix(1<<32, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	Expect(err).NotTo(HaveOccurred())
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
