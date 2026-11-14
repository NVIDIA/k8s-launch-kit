// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
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
		Expect(checks).To(ContainElement(And(HaveField("ID", "dpf.hbn.management.tenant-network"), HaveField("State", "unknown"))))
		Expect(checks[0].State).To(Equal("unknown"))
		Expect(checks[0].ReasonCode).To(Equal("BOOTSTRAP_INPUT_MISSING"))
		Expect(checks[1].State).To(Equal("unknown"))
		Expect(checks[1].ReasonCode).To(Equal("BOOTSTRAP_INPUT_MISSING"))
		Expect(checks).To(ContainElement(And(HaveField("ID", "dpf.hbn.management.tenant-clusters"), HaveField("State", "unknown"), HaveField("ReasonCode", "TENANT_VIP_UNRESOLVED"))))
		input := &config.Provisioning{Bootstrap: &config.BootstrapOptions{Tenant: config.TenantOptions{VIP: str("192.0.2.20"), Interface: str("eth0")}}}
		checks, tenantFacts = hbnBootstrapChecks(input, "dpf-system", inv, nil)
		Expect(tenantFacts).To(BeEmpty())
		Expect(checks).To(ContainElement(And(HaveField("ID", "dpf.hbn.management.tenant-network"), HaveField("State", "unknown"))))
		Expect(checks[0].State).To(Equal("passed"))
		Expect(checks[0].Message).To(ContainSubstring("require separate checks"))
		Expect(checks).To(ContainElement(And(HaveField("ID", "dpf.hbn.management.tenant-clusters"), HaveField("State", "unknown"), HaveField("ReasonCode", "TENANT_CLUSTERS_UNOBSERVED"))))
		inv.DPUs[0].BMCEndpoint = "https://192.0.2.30/path"
		checks, tenantFacts = hbnBootstrapChecks(input, "dpf-system", inv, nil)
		Expect(tenantFacts).To(BeEmpty())
		for _, check := range checks {
			if check.ID == "dpf.hbn.bootstrap-inputs" {
				Expect(check.State).NotTo(Equal("passed"))
			}
		}
	})
	It("checks new VIP vacancy only for a verified absent tenant", func() {
		input, inv, target := hbnBootstrapNetworkFixture()
		absent := tenantSnapshot(target, managementprobe.CollectionComplete)
		checks, _ := hbnBootstrapChecks(input, "dpf-system", inv, &absent)
		network := findCheck(checks, "dpf.hbn.management.tenant-network")
		Expect(network.State).To(Equal("passed"))
		Expect(network.ReasonCode).To(Equal("TENANT_VIP_ONLINK_ROUTE_VERIFIED"))
		Expect(findCheck(checks, "dpf.hbn.management.tenant-clusters").ReasonCode).To(Equal("TENANT_TARGET_ABSENT_FROM_DPUCLUSTERS"))

		// A matching current tenant may already own the VIP on its selected
		// interface. That is not a free address for new creation, but it is not a
		// conflict that should make exact existing-tenant reuse fail either.
		inv.Management.Nodes[0].Interfaces[0].Addresses = []string{target.Tenant.VIP + "/24"}
		inv.Management.Nodes[0].TenantVIPRoute.Source = target.Tenant.VIP
		existing := tenantSnapshot(target, managementprobe.CollectionComplete, observedTenantCluster(target, "tenant-uid", true))
		checks, _ = hbnBootstrapChecks(input, "dpf-system", inv, &existing)
		network = findCheck(checks, "dpf.hbn.management.tenant-network")
		Expect(network.State).To(Equal("unknown"))
		Expect(network.ReasonCode).To(Equal("TENANT_EXISTING_NETWORK_UNVERIFIED"))
		Expect(findCheck(checks, "dpf.hbn.management.tenant-clusters").ReasonCode).To(Equal("TENANT_TARGET_MATCHES"))
		Expect(network.ReasonCode).NotTo(Equal("TENANT_VIP_ALREADY_ASSIGNED"))

		// Until lifecycle binds a fresh node observation, neither an incomplete
		// node selection nor an incomplete DPUCluster list may turn into a pass.
		inv.Management.Nodes = nil
		checks, _ = hbnBootstrapChecks(input, "dpf-system", inv, &absent)
		network = findCheck(checks, "dpf.hbn.management.tenant-network")
		Expect(network.State).To(Equal("unknown"))
		Expect(network.ReasonCode).To(Equal("TENANT_CONTROL_PLANE_UNAVAILABLE"))

		inv.Management.Nodes = []inventory.Node{validTenantNetworkNode(target.Tenant.VIP)}
		incomplete := tenantSnapshot(target, managementprobe.CollectionIncomplete)
		checks, _ = hbnBootstrapChecks(input, "dpf-system", inv, &incomplete)
		network = findCheck(checks, "dpf.hbn.management.tenant-network")
		Expect(network.State).To(Equal("unknown"))
		Expect(network.ReasonCode).To(Equal("TENANT_NETWORK_SCOPE_UNVERIFIED"))
	})
})

func hbnBootstrapNetworkFixture() (*config.Provisioning, *inventory.Inventory, bootstrap.Inputs) {
	name, namespace, vip, iface := "tenant-a", "tenant-ns", "192.0.2.20", "eth0"
	routerID := 126
	policy := bootstrap.BMCFactoryResetPolicyNever
	input := &config.Provisioning{Bootstrap: &config.BootstrapOptions{BMCFactoryResetPolicy: &policy, Tenant: config.TenantOptions{Name: &name, Namespace: &namespace, VIP: &vip, Interface: &iface, RouterID: &routerID}}}
	api := "https://192.0.2.10:6443"
	devices := []bootstrap.Device{{Serial: "DPU-A", BMCIP: "192.0.2.30", BMCPort: 443}}
	target, _, err := config.ResolveBootstrap(input.Bootstrap, "dpf-system", api, devices)
	if err != nil {
		Fail("could not resolve test tenant: " + err.Error())
	}
	return input, &inventory.Inventory{
		Management: &inventory.Management{APIEndpoint: api, Nodes: []inventory.Node{validTenantNetworkNode(vip)}},
		DPUs:       []inventory.Device{{SerialNumber: "DPU-A", BMCEndpoint: "https://192.0.2.30"}},
	}, target
}

func validTenantNetworkNode(vip string) inventory.Node {
	adminUp := true
	return inventory.Node{
		Name: "cp-a", UID: "node-a", ControlPlane: true, Ready: true, OS: "linux",
		Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
		Interfaces: []inventory.Interface{
			{Name: "eth0", Index: 2, AdminUp: &adminUp, OperState: "UP", Addresses: []string{"192.0.2.10/24"}},
			{Name: "eth1", Index: 3, AdminUp: &adminUp, OperState: "UP", Addresses: []string{}},
		},
		TenantVIPRoute: &inventory.RouteLookup{Destination: vip, InterfaceIndex: 2, Source: "192.0.2.10"},
	}
}

func findCheck(checks []inventory.Check, id string) inventory.Check {
	for _, check := range checks {
		if check.ID == id {
			return check
		}
	}
	Fail("check not found: " + id)
	return inventory.Check{}
}
