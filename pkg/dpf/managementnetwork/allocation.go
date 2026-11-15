// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package managementnetwork

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// CheckTenantAllocation evaluates the operator's explicit site declarations
// for tenant VIP reservation, DHCP exclusion, and host-local VRID uniqueness.
// A passed check means the site config declares the prerequisite; this function
// does not observe an IPAM, DHCP server, or host process.
func CheckTenantAllocation(tenant bootstrap.Tenant) []inventory.Check {
	var vipReserved, vipExcludedFromDHCP, routerIDUniqueOnSelectedNodes *bool
	if tenant.Allocation != nil {
		vipReserved = tenant.Allocation.VIPReserved
		vipExcludedFromDHCP = tenant.Allocation.VIPExcludedFromDHCP
		routerIDUniqueOnSelectedNodes = tenant.Allocation.RouterIDUniqueOnSelectedNodes
	}
	declarations := []struct {
		field        string
		id           string
		value        *bool
		prerequisite string
	}{
		{field: "vipReserved", id: "vip-reserved", value: vipReserved, prerequisite: "the tenant VIP is reserved for this deployment"},
		{field: "vipExcludedFromDHCP", id: "vip-excluded-from-dhcp", value: vipExcludedFromDHCP, prerequisite: "the tenant VIP is excluded from dynamic DHCP allocation"},
		{field: "routerIDUniqueOnSelectedNodes", id: "router-id-unique", value: routerIDUniqueOnSelectedNodes, prerequisite: "the VRID is unique on the selected Keepalived nodes"},
	}
	checks := make([]inventory.Check, 0, len(declarations))
	for _, declaration := range declarations {
		state, reason, message := "unknown", "TENANT_SITE_ALLOCATION_MISSING", fmt.Sprintf("Site configuration must explicitly declare that %s; this is not inferred from network observations.", declaration.prerequisite)
		if declaration.value != nil {
			if !*declaration.value {
				state, reason = "failed", "TENANT_SITE_ALLOCATION_REJECTED"
				message = fmt.Sprintf("Site configuration explicitly rejects the prerequisite that %s.", declaration.prerequisite)
			} else {
				state, reason = "passed", "TENANT_SITE_ALLOCATION_DECLARED"
				message = fmt.Sprintf("Site configuration declares that %s; Launch Kit did not independently observe this allocation.", declaration.prerequisite)
			}
		}
		checks = append(checks, inventory.Check{
			ID: "dpf.hbn.management.tenant-allocation." + declaration.id, Subject: "/provisioning/bootstrap/tenant/allocation/" + declaration.field,
			Vantage: "site-config", RequiredPhase: "deploy", State: state, ReasonCode: reason, Message: message,
		})
	}
	return checks
}
