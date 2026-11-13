// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func hbnGenerationInputChecks(input *config.Provisioning) []inventory.Check {
	flavor := inventory.Check{ID: "dpf.hbn.flavor-input", Subject: "provisioning.flavor", Vantage: "executor", RequiredPhase: "generate", State: "unknown", ReasonCode: "DPU_FLAVOR_INPUT_MISSING", Message: "Supply the concrete DPUFlavor spec for the selected hardware and HBN datapath; a device family alone cannot select boot and NIC configuration."}
	resources := inventory.Check{ID: "dpf.hbn.resource-input", Subject: "provisioning.resources", Vantage: "executor", RequiredPhase: "generate", State: "passed", ReasonCode: "HBN_RESOURCE_INPUTS_VALID", Message: "HBN resource quantities resolve from profile defaults and overrides; available DPU capacity must be checked before deployment."}
	var override map[string]string
	if input != nil {
		override = input.Resources
		if len(input.Flavor) > 0 {
			flavor.State, flavor.ReasonCode, flavor.Message = "passed", "DPU_FLAVOR_INPUT_PRESENT", "An explicit DPUFlavor spec is supplied. Generation validates its schema; installed hardware/software compatibility requires separate verification."
		}
	}
	if _, err := config.ResolveHBNResources(override); err != nil {
		resources.State, resources.ReasonCode, resources.Message = "failed", "HBN_RESOURCE_INPUT_INVALID", err.Error()
	}
	return []inventory.Check{flavor, resources}
}
