// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func hbnHostAddressPlanCheck(profile *hbn.Profile, generation *hbn.Generation) inventory.Check {
	check := inventory.Check{ID: "dpf.hbn.host-address-plan", Subject: "host workload addresses", Vantage: "executor", RequiredPhase: "generate",
		State: "unknown", ReasonCode: "HOST_ADDRESS_PLAN_MISSING", Message: "Resolve the HBN workload pool, gateway and host address index before generation."}
	if profile == nil || generation == nil {
		return check
	}
	ranges, err := hbn.HostReservations(*profile, *generation)
	if err != nil {
		check.State, check.ReasonCode, check.Message = "failed", "HOST_ADDRESS_PLAN_INVALID", err.Error()
		return check
	}
	check.State, check.ReasonCode = "passed", "HOST_ADDRESS_PLAN_VALID"
	check.Message = fmt.Sprintf("The plan reserves host address index %d in each of %d possible DPU subnets, separate from gateway index %d. Actual subnet allocation, PF0 identity and host route configuration require deployment-time checks.", generation.HostAddressIndex, len(ranges), generation.WorkloadGatewayIndex)
	return check
}
