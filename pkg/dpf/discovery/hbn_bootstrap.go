// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"net/url"
	"strconv"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementnetwork"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
)

// hbnBootstrapChecks reports input completeness, not live allocation or health.
func hbnBootstrapChecks(input *config.Provisioning, namespace string, inv *inventory.Inventory, snapshot *managementprobe.Snapshot) ([]inventory.Check, []inventory.HBNManagementCluster) {
	checks := []inventory.Check{}
	add := func(subject, state, code, message string) {
		checks = append(checks, inventory.Check{ID: "dpf.hbn.bootstrap-inputs", Subject: subject, Vantage: "executor", RequiredPhase: "generate", State: state, ReasonCode: code, Message: message})
	}
	var options *config.BootstrapOptions
	if input != nil {
		options = input.Bootstrap
	}
	endpoint := ""
	selected := []bootstrap.Device{}
	if inv != nil {
		if inv.Management != nil {
			endpoint = inv.Management.APIEndpoint
		}
		for _, device := range inv.DPUs {
			bmc, err := url.Parse(device.BMCEndpoint)
			if err != nil || bmc.Scheme != "https" || bmc.User != nil || bmc.RawQuery != "" || bmc.Fragment != "" || (bmc.Path != "" && bmc.Path != "/") {
				add(device.SerialNumber, "unknown", "BMC_ENROLLMENT_ADDRESS_UNAVAILABLE", "A selected DPU needs a canonical HTTPS BMC endpoint before a bounded enrollment scan can be generated.")
				continue
			}
			port := 443
			if bmc.Port() != "" {
				port, _ = strconv.Atoi(bmc.Port())
			}
			selected = append(selected, bootstrap.Device{Serial: device.SerialNumber, BMCIP: bmc.Hostname(), BMCPort: port})
		}
	}
	resolved, issues, err := config.ResolveBootstrap(options, namespace, endpoint, selected)
	if err != nil {
		add("provisioning.bootstrap", "failed", "BOOTSTRAP_INPUT_INVALID", err.Error())
		return append(checks, tenantClusterCheck(snapshot, nil)), []inventory.HBNManagementCluster{}
	}
	for _, issue := range issues {
		add(issue.Field, "unknown", issue.ReasonCode, "Supply the site allocation. Tenant VIP and management interface cannot be invented; a literal observed management API endpoint can be reused, otherwise supply its IP and port.")
	}
	if len(checks) == 0 {
		add("provisioning.bootstrap", "passed", "BOOTSTRAP_INPUTS_COMPLETE", "Bootstrap inputs and exact BMC scan targets are specified; VIP/VRID reservation, interface reachability, storage health and installed controllers require separate checks.")
	}
	checks = append(checks, managementnetwork.CheckTenantAllocation(resolved.Tenant)...)
	if resolved.BMCFactoryResetPolicy == bootstrap.BMCFactoryResetPolicyOnInitialization {
		checks = append(checks, inventory.Check{
			ID: "dpf.hbn.bmc-reset-recovery", Subject: "selected DPU BMCs", Vantage: "executor", RequiredPhase: "deploy",
			State: "unknown", ReasonCode: "BMC_RESET_RECOVERY_UNVERIFIED",
			Message: "OnInitialization clears BMC networking. Stable DHCP recovery to each selected BMC address and reauthentication after reset have not been verified; use Never to preserve existing BMC network settings.",
		})
	}
	targetCheck := tenantClusterCheck(snapshot, &resolved)
	var networkChecks []inventory.Check
	switch {
	case targetCheck.State == "passed" && targetCheck.ReasonCode == "TENANT_TARGET_ABSENT_FROM_DPUCLUSTERS":
		var nodes []inventory.Node
		if inv != nil && inv.Management != nil {
			nodes = inv.Management.Nodes
		}
		networkChecks = managementnetwork.CheckNewTenantNetwork(nodes, resolved.Tenant)
	case targetCheck.State == "passed" && targetCheck.ReasonCode == "TENANT_TARGET_MATCHES":
		networkChecks = []inventory.Check{tenantNetworkScopeCheck(resolved.Tenant, "TENANT_EXISTING_NETWORK_UNVERIFIED", "The selected DPUCluster already exists; fresh API-backed, ownership-aware endpoint checks are required because its status does not prove current node VIP assignment or route state.")}
	default:
		networkChecks = []inventory.Check{tenantNetworkScopeCheck(resolved.Tenant, "TENANT_NETWORK_SCOPE_UNVERIFIED", "The selected tenant is not verified as absent or as a matching owned object; do not treat the requested VIP as free until cross-namespace ownership is resolved.")}
	}
	checks = append(checks, networkChecks...)
	return append(checks, targetCheck), tenantClusterProjection(snapshot, &resolved)
}

func tenantNetworkScopeCheck(tenant bootstrap.Tenant, reason, message string) inventory.Check {
	subject := "tenant network selection"
	if tenant.Name != "" {
		subject = "tenant " + tenant.Name + " network selection"
	}
	return inventory.Check{ID: "dpf.hbn.management.tenant-network", Subject: subject, Vantage: "management-cluster", RequiredPhase: "deploy", State: "unknown", ReasonCode: reason, Message: message, EvidenceRefs: []string{}}
}
