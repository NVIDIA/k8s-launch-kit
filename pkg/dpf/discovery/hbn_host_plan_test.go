// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
)

func TestHBNHostAddressPlanSeparatesReservationFromRuntimeConfiguration(t *testing.T) {
	definition, err := hbn.LoadDefinition()
	if err != nil {
		t.Fatal(err)
	}
	profile, generation := definition.Defaults.Networking, definition.Defaults.Generation
	check := hbnHostAddressPlanCheck(&profile, &generation)
	if check.State != "passed" || check.RequiredPhase != "generate" || !strings.Contains(check.Message, "128 possible DPU subnets") || !strings.Contains(check.Message, "require deployment-time checks") {
		t.Fatalf("incorrect host plan result: %#v", check)
	}
	generation.HostAddressIndex = generation.WorkloadGatewayIndex
	if check := hbnHostAddressPlanCheck(&profile, &generation); check.State != "failed" || check.ReasonCode != "HOST_ADDRESS_PLAN_INVALID" {
		t.Fatalf("gateway overlap was accepted: %#v", check)
	}
	if check := hbnHostAddressPlanCheck(&profile, nil); check.State != "unknown" || check.ReasonCode != "HOST_ADDRESS_PLAN_MISSING" {
		t.Fatalf("missing plan was accepted: %#v", check)
	}
}
