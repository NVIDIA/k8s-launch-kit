// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"net/url"
	"strconv"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
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
	if resolved.BMCFactoryResetPolicy == bootstrap.BMCFactoryResetPolicyOnInitialization {
		checks = append(checks, inventory.Check{
			ID: "dpf.hbn.bmc-reset-recovery", Subject: "selected DPU BMCs", Vantage: "executor", RequiredPhase: "deploy",
			State: "unknown", ReasonCode: "BMC_RESET_RECOVERY_UNVERIFIED",
			Message: "OnInitialization clears BMC networking. Stable DHCP recovery to each selected BMC address and reauthentication after reset have not been verified; use Never to preserve existing BMC network settings.",
		})
	}
	return append(checks, tenantClusterCheck(snapshot, &resolved)), tenantClusterProjection(snapshot, &resolved)
}
