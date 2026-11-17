// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// verifyActivatedHostPrerequisites replaces no observation with a pre-reset
// value. Fresh complete host/network facts must pass before rebinding the
// activation receipt used by host configuration and runtime measurements.
func verifyActivatedHostPrerequisites(ctx context.Context, client *APIClient, prepared *preparedControlPlaneRun, control controlPlaneReceipts, workload workloadPreparationReceipt, activation activationStageReceipt, access config.Request, journal *executionJournal, timeout time.Duration, verify PrerequisiteVerifier) (PrerequisiteVerification, activationStageReceipt, error) {
	request, err := cloneRequestForPrerequisites(access)
	if err != nil {
		return PrerequisiteVerification{}, activation, fmt.Errorf("copy post-activation prerequisite request")
	}
	verification, err := verify(ctx, request, prepared.execution.plan, AfterActivation)
	if err != nil {
		return verification, activation, fmt.Errorf("post-activation prerequisite collection failed")
	}
	verification, err = assessOwnedResumePrerequisites(ctx, client, prepared, control, workload, verification, access, journal, timeout, AfterActivation)
	if err != nil {
		return verification, activation, err
	}
	if err = validatePrerequisiteVerification(prepared.execution.plan, verification); err != nil {
		return verification, activation, err
	}
	refreshed, err := rebindActivatedHostInventory(ctx, client, prepared, control, activation, verification.Inventory, access.Targets)
	return verification, refreshed, err
}

func rebindActivatedHostInventory(ctx context.Context, client activationStageClient, prepared *preparedControlPlaneRun, control controlPlaneReceipts, activation activationStageReceipt, observedInventory *inventory.Inventory, targets []config.Target) (activationStageReceipt, error) {
	current, err := cloneInventory(observedInventory)
	if err != nil {
		return activation, fmt.Errorf("copy post-activation host inventory")
	}
	deployment, err := retainedDeploymentObject(prepared.execution)
	if err != nil {
		return activation, err
	}
	selected, groups, err := bindActivationHosts(prepared.selected, control.enrollment, current, targets, deployment)
	if err != nil {
		return activation, err
	}
	if len(selected) != len(activation.selected) {
		return activation, fmt.Errorf("POST_ACTIVATION_HOST_CHANGED: selected membership changed")
	}
	for _, item := range selected {
		matches := 0
		for _, prior := range activation.selected {
			if prior.item.device.Serial == item.item.device.Serial && prior.host.ID == item.host.ID && prior.host.SystemUUID == item.host.SystemUUID && prior.host.InputRef == item.host.InputRef && prior.host.SSHEndpoint == item.host.SSHEndpoint && samePhysicalAdapter(prior.adapter, item.adapter) {
				matches++
			}
		}
		if matches != 1 {
			return activation, fmt.Errorf("POST_ACTIVATION_HOST_CHANGED: selected physical binding changed")
		}
	}
	observed, err := observeActivationScope(ctx, client, prepared.execution, deployment, activation.DeploymentUID, selected, activation.progress)
	if err != nil || !observed.complete || !EvaluateReadiness(observed.deployment).Ready || !activationCompletionReady(observed, activation.progress, selected) {
		return activation, fmt.Errorf("POST_ACTIVATION_SCOPE_CHANGED: current activation completion could not be proven")
	}
	activation.selected, activation.groups = selected, groups
	return activation, nil
}

func samePhysicalAdapter(before, after inventory.Adapter) bool {
	if before.Key != after.Key || before.SerialNumber != after.SerialNumber || before.PCIAddress != after.PCIAddress || before.VendorID != after.VendorID || before.DeviceID != after.DeviceID || len(before.PCIFunctions) != len(after.PCIFunctions) {
		return false
	}
	addresses := func(a inventory.Adapter) map[string]bool {
		result := map[string]bool{}
		for _, pf := range a.PCIFunctions {
			result[strings.ToLower(pf.Address)] = true
		}
		return result
	}
	return reflect.DeepEqual(addresses(before), addresses(after))
}
