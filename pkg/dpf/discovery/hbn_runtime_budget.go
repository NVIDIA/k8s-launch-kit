// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func hbnRuntimeBudgetCheck(inv *inventory.Inventory) inventory.Check {
	check := hostCheck("runtime-budget", "selected physical hosts", "unknown", "HBN_RUNTIME_COHORT_UNVERIFIED", "Complete unique selected host associations are required to evaluate the native traffic budget.")
	if inv == nil || len(inv.DPUs) == 0 {
		return check
	}
	selectedHosts := make(map[string]bool, len(inv.DPUs))
	for _, dpu := range inv.DPUs {
		_, host, _, state, _, _ := resolveHostHBNAssociation(inv, dpu)
		if state != "passed" || host == nil {
			return check
		}
		if selectedHosts[host.ID] {
			check.State, check.ReasonCode = "failed", "HOST_NETWORK_MULTIPLE_DPUS_UNSUPPORTED"
			check.Message = "The native workload route requires one selected DPU per physical host."
			return check
		}
		selectedHosts[host.ID] = true
	}
	if err := hbn.ValidateRuntimeTrafficBudget(len(selectedHosts), hbn.MaxNativeDeploymentDuration); err != nil {
		check.State, check.ReasonCode, check.Message = "failed", "HBN_RUNTIME_BUDGET_INSUFFICIENT", err.Error()
		return check
	}
	check.State, check.ReasonCode = "passed", "HBN_RUNTIME_BUDGET_POSSIBLE"
	check.Message = "Packet spacing for the complete selected ordinary/DF matrix fits the native maximum; provisioning, SSH and API checks require additional time. Deployment rechecks the actual remaining deadline."
	return check
}
