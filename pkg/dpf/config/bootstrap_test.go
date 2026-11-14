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
	It("preserves omitted, explicit false and true allocation declarations without aliasing", func() {
		options := complete()
		reserved, excluded := true, false
		options.Tenant.Allocation = &bootstrap.SiteAllocation{VIPReserved: &reserved, VIPExcludedFromDHCP: &excluded}
		resolved, issues, err := ResolveBootstrap(options, "dpf-system", "https://192.0.2.10:6443", selected)
		Expect(err).NotTo(HaveOccurred())
		Expect(issues).To(BeEmpty())
		Expect(resolved.Tenant.Allocation).NotTo(BeNil())
		Expect(*resolved.Tenant.Allocation.VIPReserved).To(BeTrue())
		Expect(*resolved.Tenant.Allocation.VIPExcludedFromDHCP).To(BeFalse())
		Expect(resolved.Tenant.Allocation.RouterIDUniqueOnSelectedNodes).To(BeNil())
		Expect(resolved.Tenant.Allocation.VIPReserved).NotTo(BeIdenticalTo(options.Tenant.Allocation.VIPReserved))
		Expect(resolved.Tenant.Allocation.VIPExcludedFromDHCP).NotTo(BeIdenticalTo(options.Tenant.Allocation.VIPExcludedFromDHCP))

		reserved, excluded = false, true
		Expect(*resolved.Tenant.Allocation.VIPReserved).To(BeTrue())
		Expect(*resolved.Tenant.Allocation.VIPExcludedFromDHCP).To(BeFalse())
		*resolved.Tenant.Allocation.VIPReserved = false
		*resolved.Tenant.Allocation.VIPExcludedFromDHCP = true
		Expect(reserved).To(BeFalse())
		Expect(excluded).To(BeTrue())

		decoded, err := Decode([]byte("provisioning: {bootstrap: {tenant: {allocation: {vipReserved: true, vipExcludedFromDHCP: false, routerIDUniqueOnSelectedNodes: null}}}}\n"+seed), "/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		allocation := decoded.Site.Provisioning.Bootstrap.Tenant.Allocation
		Expect(allocation).NotTo(BeNil())
		Expect(allocation.VIPReserved).NotTo(BeNil())
		Expect(*allocation.VIPReserved).To(BeTrue())
		Expect(allocation.VIPExcludedFromDHCP).NotTo(BeNil())
		Expect(*allocation.VIPExcludedFromDHCP).To(BeFalse())
		Expect(allocation.RouterIDUniqueOnSelectedNodes).To(BeNil())

		missing, _, err := ResolveBootstrap(complete(), "dpf-system", "https://192.0.2.10:6443", selected)
		Expect(err).NotTo(HaveOccurred())
		Expect(missing.Tenant.Allocation).To(BeNil())
	})
	It("rejects quoted scalar booleans at each allocation field while retaining null as omitted", func() {
		for _, field := range []string{"vipReserved", "vipExcludedFromDHCP", "routerIDUniqueOnSelectedNodes"} {
			_, err := Decode([]byte("provisioning: {bootstrap: {tenant: {allocation: {"+field+": 'true'}}}}\n"+seed), "/input.yaml")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("/provisioning/bootstrap/tenant/allocation/" + field))
		}
		decoded, err := Decode([]byte("provisioning: {bootstrap: {tenant: {allocation: {vipReserved: null}}}}\n"+seed), "/input.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded.Site.Provisioning.Bootstrap.Tenant.Allocation).NotTo(BeNil())
		Expect(decoded.Site.Provisioning.Bootstrap.Tenant.Allocation.VIPReserved).To(BeNil())
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
