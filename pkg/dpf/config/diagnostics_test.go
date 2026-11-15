// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Safe configuration diagnostics", func() {
	It("locates unknown nested flow keys without exposing their values", func() {
		_, err := Decode([]byte("# preserved source line\ndiscovery: {hosts: [{ssh: {adress: password-value-canary}}]}\n"), "site.yaml")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("/discovery/hosts/0/ssh/adress"))
		Expect(err.Error()).To(ContainSubstring("line 2, column 28"))
		Expect(err.Error()).NotTo(ContainSubstring("password-value-canary"))
	})

	It("locates a collection type error in original block YAML", func() {
		_, err := Decode([]byte("# comment\ndiscovery:\n  hosts: collection-value-canary\n"), "site.yaml")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("/discovery/hosts"))
		Expect(err.Error()).To(ContainSubstring("line 3, column 3"))
		Expect(err.Error()).NotTo(ContainSubstring("collection-value-canary"))
	})

	It("locates flow collection type errors after unrelated fields without exposing values", func() {
		_, err := Decode([]byte("# comment\ndiscovery: {limits: {}, hosts: collection-value-canary}\n"), "site.yaml")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("invalid type in DPF configuration"))
		Expect(err.Error()).To(ContainSubstring("/discovery/hosts"))
		Expect(err.Error()).To(ContainSubstring("line 2, column 25"))
		Expect(err.Error()).NotTo(ContainSubstring("collection-value-canary"))
	})

	It("preserves original source coordinates after merge expansion", func() {
		_, err := Decode([]byte("discovery:\n  hosts:\n    - ssh:\n        <<: {adress: alias-value-canary}\n"), "site.yaml")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("/discovery/hosts/0/ssh/adress"))
		Expect(err.Error()).To(ContainSubstring("line 4, column 14"))
		Expect(err.Error()).NotTo(ContainSubstring("alias-value-canary"))
	})

	It("locates scalar policy errors without echoing the scalar", func() {
		_, err := Decode([]byte("discovery:\n  tls:\n    insecureSkipVerify: quoted-bool-canary\n"), "site.yaml")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("/discovery/tls/insecureSkipVerify"))
		Expect(err.Error()).To(ContainSubstring("line 3, column 25"))
		Expect(err.Error()).NotTo(ContainSubstring("quoted-bool-canary"))
	})

	It("reports syntax line numbers without echoing parser context", func() {
		_, err := Decode([]byte("discovery:\n  hosts: [syntax-value-canary\n"), "site.yaml")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("invalid YAML at / (line"))
		Expect(err.Error()).NotTo(ContainSubstring("syntax-value-canary"))
	})
})
