// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Bootstrap resolution", func() {
	selected := []bootstrap.Device{{Serial: "DPU-A", BMCIP: "192.0.2.30", BMCPort: 443}}
	str := func(s string) *string { return &s }
	integer := func(n int) *int { return &n }
	complete := func() *BootstrapOptions {
		return &BootstrapOptions{Tenant: TenantOptions{VIP: str("192.0.2.20"), Interface: str("eth0")}}
	}
	It("defaults from profile and observed literal endpoint without opening credential files", func() {
		options := complete()
		resolved, issues, err := ResolveBootstrap(options, "dpf-system", "https://192.0.2.10:6443", selected)
		Expect(err).NotTo(HaveOccurred())
		Expect(issues).To(BeEmpty())
		Expect(resolved.ManagementAPIIP).To(Equal("192.0.2.10"))
		Expect(resolved.ManagementAPIPort).To(Equal(6443))
		Expect(resolved.Tenant.RouterID).To(Equal(126))
		Expect(resolved.PodCIDR).To(Equal("10.244.0.0/14"))
		Expect(resolved.OperatorConfigName).To(Equal("dpfoperatorconfig"))
		Expect(resolved.BMCFactoryResetPolicy).To(Equal(bootstrap.BMCFactoryResetPolicyNever))
		Expect(options.Tenant.RouterID).To(BeNil())
		Expect(options.ManagementAPIIP).To(BeNil())
		resolved.Devices[0].Serial = "changed"
		Expect(selected[0].Serial).To(Equal("DPU-A"))
		resolved.Tenant.NodeSelector["extra"] = "value"
		again, _, err := ResolveBootstrap(options, "dpf-system", "https://192.0.2.10:6443", selected)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Tenant.NodeSelector).NotTo(HaveKey("extra"))
	})
	It("preserves an explicit OnInitialization policy while omitted policy defaults to Never", func() {
		options := complete()
		resolved, issues, err := ResolveBootstrap(options, "dpf-system", "https://192.0.2.10:6443", selected)
		Expect(err).NotTo(HaveOccurred())
		Expect(issues).To(BeEmpty())
		Expect(resolved.BMCFactoryResetPolicy).To(Equal(bootstrap.BMCFactoryResetPolicyNever))
		policy := bootstrap.BMCFactoryResetPolicyOnInitialization
		options.BMCFactoryResetPolicy = &policy
		resolved, issues, err = ResolveBootstrap(options, "dpf-system", "https://192.0.2.10:6443", selected)
		Expect(err).NotTo(HaveOccurred())
		Expect(issues).To(BeEmpty())
		Expect(resolved.BMCFactoryResetPolicy).To(Equal(bootstrap.BMCFactoryResetPolicyOnInitialization))
	})
	It("reports missing allocations and refuses to guess an address behind DNS", func() {
		resolved, issues, err := ResolveBootstrap(nil, "dpf-system", "https://management.example:6443", selected)
		Expect(err).NotTo(HaveOccurred())
		Expect(issues).To(HaveLen(4))
		Expect(resolved.BMCFactoryResetPolicy).To(Equal(bootstrap.BMCFactoryResetPolicyNever))
		options := complete()
		options.ManagementAPIIP = str("192.0.2.10")
		options.ManagementAPIPort = integer(6443)
		_, issues, err = ResolveBootstrap(options, "dpf-system", "https://management.example:6443", selected)
		Expect(err).NotTo(HaveOccurred())
		Expect(issues).To(BeEmpty())
	})
	It("preserves explicit invalid values instead of silently falling back", func() {
		for _, mutate := range []func(*BootstrapOptions){
			func(o *BootstrapOptions) { o.ManagementAPIIP = str("") },
			func(o *BootstrapOptions) { o.ManagementAPIPort = integer(0) },
			func(o *BootstrapOptions) { o.Tenant.RouterID = integer(0) },
			func(o *BootstrapOptions) { o.Tenant.VIP = str("") },
			func(o *BootstrapOptions) { o.Tenant.NodeSelector = &map[string]string{} },
		} {
			options := complete()
			mutate(options)
			_, _, err := ResolveBootstrap(options, "dpf-system", "https://192.0.2.10:6443", selected)
			Expect(err).To(HaveOccurred())
		}
	})
	It("strictly decodes nested integers and preserves null defaults", func() {
		in, err := Decode([]byte("provisioning: {bootstrap: {managementAPIPort: 6443, tenant: {routerID: 126, maxNodes: null}}}\n"+seed), "/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(in.Site.Provisioning.Bootstrap.Tenant.MaxNodes).To(BeNil())
		Expect(in.Site.Provisioning.Bootstrap.BMCFactoryResetPolicy).To(BeNil())
		nullPolicy, err := Decode([]byte("provisioning: {bootstrap: {bmcFactoryResetPolicy: null}}\n"+seed), "/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(nullPolicy.Site.Provisioning.Bootstrap.BMCFactoryResetPolicy).To(BeNil())
		withPolicy, err := Decode([]byte("provisioning: {bootstrap: {bmcFactoryResetPolicy: OnInitialization}}\n"+seed), "/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(withPolicy.Site.Provisioning.Bootstrap.BMCFactoryResetPolicy).NotTo(BeNil())
		Expect(*withPolicy.Site.Provisioning.Bootstrap.BMCFactoryResetPolicy).To(Equal(bootstrap.BMCFactoryResetPolicyOnInitialization))
		explicitEmpty, err := Decode([]byte("provisioning: {bootstrap: {bmcFactoryResetPolicy: ''}}\n"+seed), "/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(explicitEmpty.Site.Provisioning.Bootstrap.BMCFactoryResetPolicy).NotTo(BeNil())
		Expect(*explicitEmpty.Site.Provisioning.Bootstrap.BMCFactoryResetPolicy).To(BeEmpty())
		_, _, err = ResolveBootstrap(explicitEmpty.Site.Provisioning.Bootstrap, "dpf-system", "https://192.0.2.10:6443", selected)
		Expect(err).To(HaveOccurred())
		_, err = Decode([]byte("provisioning: {bootstrap: {tenant: {routerID: '126'}}}\n"+seed), "/input.yaml")
		Expect(err).To(HaveOccurred())
	})
	It("rejects explicit empty and unsupported reset policies even while allocations are missing", func() {
		for _, policy := range []bootstrap.BMCFactoryResetPolicy{"", "OnReset", "never"} {
			options := &BootstrapOptions{BMCFactoryResetPolicy: &policy}
			_, _, err := ResolveBootstrap(options, "dpf-system", "https://management.example:6443", selected)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("/provisioning/bootstrap/bmcFactoryResetPolicy"))
		}
	})
})
