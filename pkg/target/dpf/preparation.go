// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"errors"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/app"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
)

func preparationError(err error) error {
	var preparation *app.PreparationError
	if !errors.As(err, &preparation) {
		return err
	}
	var message, suggestion string
	switch preparation.Stage {
	case app.GenerationInputs:
		message, suggestion = "invalid DPF generation inputs", "Correct inventory or supported overrides and rerun discovery; catalog gaps require a supported Launch Kit release"
	case app.DeploymentBundle:
		message, suggestion = "invalid DPF deployment bundle", "Regenerate the complete bundle and retain its metadata"
	case app.DeploymentPlan:
		message, suggestion = "invalid retained DPF deployment plan", "Regenerate and review the bundle before opening access"
	case app.DeploymentConfiguration:
		message, suggestion = "invalid DPF deployment configuration", "Supply the discovery output with --inventory dpf-discovered.yaml"
	case app.DeploymentSettings:
		message, suggestion = "cannot restore DPF deployment settings", "Regenerate and review the bundle"
	case app.DeploymentSelection:
		message, suggestion = "DPF deployment selection conflicts with the bundle", "Use the generated selection or regenerate and review the bundle"
	case app.DeploymentProvisioning:
		message, suggestion = "cannot resolve retained DPF provisioning", "Use the validated bundle's supported recipe"
	case app.DeploymentAccess:
		message, suggestion = "invalid DPF deployment access", "Correct the site configuration and access references"
	default:
		return err
	}
	return apperrors.NewValidationError(message, preparation.Err, suggestion)
}
