// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

// PreparationStage identifies the input boundary that stopped an operation.
type PreparationStage string

const (
	GenerationInputs        PreparationStage = "generation-inputs"
	DeploymentBundle        PreparationStage = "deployment-bundle"
	DeploymentPlan          PreparationStage = "deployment-plan"
	DeploymentConfiguration PreparationStage = "deployment-configuration"
	DeploymentSettings      PreparationStage = "deployment-settings"
	DeploymentSelection     PreparationStage = "deployment-selection"
	DeploymentProvisioning  PreparationStage = "deployment-provisioning"
	DeploymentAccess        PreparationStage = "deployment-access"
)

// PreparationError retains the cause and input boundary without choosing CLI text.
type PreparationError struct {
	Stage PreparationStage
	Err   error
}

func (e *PreparationError) Error() string { return e.Err.Error() }
func (e *PreparationError) Unwrap() error { return e.Err }
