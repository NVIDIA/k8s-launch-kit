// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

const hbnEnrollmentAccessID = "dpf.hbn.enrollment-credentials"

// HBNEnrollmentAccessValidator checks local, already-resolved inputs for the
// supported BF4 crawler. It must not perform I/O, rotate credentials, or claim
// live authorization. Discovery accepts only its finite state/reason result;
// callback text and subjects never become public evidence.
type HBNEnrollmentAccessValidator func(config.Request, *inventory.Inventory) inventory.Check

func hbnEnrollmentAccessCheck(ctx context.Context, request config.Request, current *inventory.Inventory, validate HBNEnrollmentAccessValidator) inventory.Check {
	check := inventory.Check{ID: hbnEnrollmentAccessID, Subject: "selected DPU BMC enrollment inputs",
		Vantage: "executor", RequiredPhase: "deploy", State: "unknown"}
	set := func(state, reason, message string) inventory.Check {
		check.State, check.ReasonCode, check.Message = state, reason, message
		return check
	}
	if ctx == nil || ctx.Err() != nil {
		return set("unknown", "ENROLLMENT_CREDENTIALS_CHECK_CANCELLED", "Selected BMC crawler inputs were not validated before discovery was cancelled.")
	}
	if validate == nil {
		return set("unknown", "ENROLLMENT_CREDENTIALS_VALIDATOR_UNAVAILABLE", "The selected BF4 crawler credential compatibility validator is unavailable.")
	}
	result := validate(request, current)
	if ctx.Err() != nil {
		return set("unknown", "ENROLLMENT_CREDENTIALS_CHECK_CANCELLED", "Selected BMC crawler inputs were not validated before discovery was cancelled.")
	}
	if result.ID == hbnEnrollmentAccessID && result.Vantage == "executor" && result.RequiredPhase == "deploy" {
		switch {
		case result.State == "passed" && result.ReasonCode == "ENROLLMENT_CREDENTIALS_COMPATIBLE":
			return set("passed", result.ReasonCode, "Selected BF4 BMC inputs use admin and one common nonempty password for the DPF crawler; live Administrator authorization is checked separately.")
		case result.State == "failed" && result.ReasonCode == "ENROLLMENT_CREDENTIALS_INCOMPATIBLE":
			return set("failed", result.ReasonCode, "The current BF4 crawler requires exact selected BMC bindings, the admin username and the same nonempty password value on every selected DPU. Correct the protected credential references and rerun discovery.")
		case result.State == "unknown" && result.ReasonCode == "ENROLLMENT_IDENTITY_UNAVAILABLE":
			return set("unknown", result.ReasonCode, "Current selected DPU identities and exact BMC source bindings are required to validate crawler credential compatibility.")
		}
	}
	return set("unknown", "ENROLLMENT_CREDENTIALS_RESULT_INVALID", "The BF4 crawler credential validator returned an unsupported or incomplete result.")
}
