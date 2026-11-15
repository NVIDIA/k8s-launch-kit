// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func hbnProvisioningAPIChecks(inv *inventory.Inventory) []inventory.Check {
	checks := []inventory.Check{}
	if inv == nil {
		return checks
	}
	for _, dpu := range inv.DPUs {
		check := inventory.Check{ID: "dpf.hbn.bmc-provisioning-api", Subject: dpu.SerialNumber, Vantage: "executor", RequiredPhase: "deploy", State: "unknown", ReasonCode: "DPF_PROVISIONING_API_NOT_MEASURED", Message: "The BMC's release-specific DPF enrollment reads have not been verified; inventory support alone does not establish provisioning support."}
		observation, found := dpu.Observations["dpf-provisioning-api"]
		if !found {
			checks = append(checks, check)
			continue
		}
		if observation.State == "observed" && observation.Tool == "redfish" && observation.ReasonCode == "DPF_PROVISIONING_API_READS_VERIFIED" && observation.Message != "" && len(observation.Values) == 2 && inv.Software != nil && observation.Values[0] == inv.Software.Version && lldpMAC(&observation.Values[1]) != "" {
			check.State, check.ReasonCode, check.Message = "passed", observation.ReasonCode, observation.Message
		} else if observation.State != "observed" && observation.ReasonCode != "" && observation.Message != "" {
			check.ReasonCode, check.Message = observation.ReasonCode, observation.Message
			switch observation.ReasonCode {
			case "DPF_PROVISIONING_API_NOT_EXPOSED", "DPF_PROVISIONING_IDENTITY_MISMATCH", "DPF_PROVISIONING_PSID_UNAVAILABLE", "DPF_PROVISIONING_PF0_UNAVAILABLE", "DPF_PROVISIONING_API_CHANGED":
				check.State = "failed"
			}
		}
		checks = append(checks, check)
	}
	return checks
}
