// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HBN bootstrap input checks", func() {
	str := func(value string) *string { return &value }
	It("keeps explicit BMC reset recovery separate from complete generation inputs", func() {
		policy := bootstrap.BMCFactoryResetPolicyOnInitialization
		input := &config.Provisioning{Bootstrap: &config.BootstrapOptions{BMCFactoryResetPolicy: &policy, Tenant: config.TenantOptions{VIP: str("192.0.2.20"), Interface: str("eth0")}}}
		inv := &inventory.Inventory{Management: &inventory.Management{APIEndpoint: "https://192.0.2.10:6443"}, DPUs: []inventory.Device{{SerialNumber: "DPU-A", BMCEndpoint: "https://192.0.2.30"}}}
		checks, _ := hbnBootstrapChecks(input, "dpf-system", inv, nil)
		Expect(checks[0].ReasonCode).To(Equal("BOOTSTRAP_INPUTS_COMPLETE"))
		Expect(checks).To(ContainElement(And(
			HaveField("ID", "dpf.hbn.bmc-reset-recovery"), HaveField("State", "unknown"), HaveField("RequiredPhase", "deploy"),
		)))
		policy = bootstrap.BMCFactoryResetPolicyNever
		checks, _ = hbnBootstrapChecks(input, "dpf-system", inv, nil)
		Expect(checks).NotTo(ContainElement(HaveField("ID", "dpf.hbn.bmc-reset-recovery")))
	})
	It("reports missing site inputs without treating planned resources as installed", func() {
		inv := &inventory.Inventory{Management: &inventory.Management{APIEndpoint: "https://192.0.2.10:6443"}, DPUs: []inventory.Device{{SerialNumber: "DPU-A", BMCEndpoint: "https://192.0.2.30"}}}
		checks, tenantFacts := hbnBootstrapChecks(nil, "dpf-system", inv, nil)
		Expect(tenantFacts).To(BeEmpty())
		Expect(checks).To(HaveLen(3))
		Expect(checks[0].State).To(Equal("unknown"))
		Expect(checks[0].ReasonCode).To(Equal("BOOTSTRAP_INPUT_MISSING"))
		Expect(checks[1].State).To(Equal("unknown"))
		Expect(checks[1].ReasonCode).To(Equal("BOOTSTRAP_INPUT_MISSING"))
		Expect(checks[2].ID).To(Equal("dpf.hbn.management.tenant-clusters"))
		Expect(checks[2].State).To(Equal("unknown"))
		Expect(checks[2].ReasonCode).To(Equal("TENANT_VIP_UNRESOLVED"))
		input := &config.Provisioning{Bootstrap: &config.BootstrapOptions{Tenant: config.TenantOptions{VIP: str("192.0.2.20"), Interface: str("eth0")}}}
		checks, tenantFacts = hbnBootstrapChecks(input, "dpf-system", inv, nil)
		Expect(tenantFacts).To(BeEmpty())
		Expect(checks).To(HaveLen(2))
		Expect(checks[0].State).To(Equal("passed"))
		Expect(checks[0].Message).To(ContainSubstring("require separate checks"))
		Expect(checks[1].State).To(Equal("unknown"))
		Expect(checks[1].ReasonCode).To(Equal("TENANT_CLUSTERS_UNOBSERVED"))
		inv.DPUs[0].BMCEndpoint = "https://192.0.2.30/path"
		checks, tenantFacts = hbnBootstrapChecks(input, "dpf-system", inv, nil)
		Expect(tenantFacts).To(BeEmpty())
		for _, check := range checks {
			if check.ID == "dpf.hbn.bootstrap-inputs" {
				Expect(check.State).NotTo(Equal("passed"))
			}
		}
	})
})
