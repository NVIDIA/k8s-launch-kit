// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
)

func hbnResetSupportChecks(inv *inventory.Inventory) []inventory.Check {
	checks := []inventory.Check{}
	if inv == nil {
		return checks
	}
	for _, dpu := range inv.DPUs {
		check := inventory.Check{ID: "dpf.hbn.host-reset-support", Subject: dpu.SerialNumber, Vantage: "executor", RequiredPhase: "deploy", State: "unknown", ReasonCode: "RESET_SUPPORT_UNVERIFIED", Message: "The selected release's host reset backend must support the observed DPU model and BMC firmware before provisioning."}
		observed := dpu.Observations["host-reset-support"]
		if inv.Software != nil && redfish.ResetSupportVerified(dpu, inv.Software.Version) {
			check.State, check.ReasonCode, check.Message = "passed", observed.ReasonCode, observed.Message
		} else if observed.State != "observed" && observed.ReasonCode != "" && observed.Message != "" {
			check.ReasonCode, check.Message = observed.ReasonCode, observed.Message
			if observed.State == "failed" {
				check.State = "failed"
			}
		}
		checks = append(checks, check)
	}
	return checks
}
