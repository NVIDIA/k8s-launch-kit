// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/app"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// hbnEnrollmentAccessCheck joins current source selection and the deployment
// binder without reopening credential references or rendering another plan.
func hbnEnrollmentAccessCheck(request config.Request, current *inventory.Inventory) inventory.Check {
	check := inventory.Check{ID: "dpf.hbn.enrollment-credentials", Vantage: "executor", RequiredPhase: "deploy",
		State: "unknown", ReasonCode: "ENROLLMENT_IDENTITY_UNAVAILABLE"}
	if request.HBN == nil || request.Software == nil {
		return check
	}
	selected, err := app.ResolveDevices(request.Input.Site, current, *request.HBN)
	if err != nil {
		return check
	}
	if err := deployment.ValidateEnrollmentBMCs(request.Software.Version, selected, request.Targets); err != nil {
		check.State, check.ReasonCode = "failed", "ENROLLMENT_CREDENTIALS_INCOMPATIBLE"
		return check
	}
	check.State, check.ReasonCode = "passed", "ENROLLMENT_CREDENTIALS_COMPATIBLE"
	return check
}
