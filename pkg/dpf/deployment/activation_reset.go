// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// prepareOperationReset recognizes an existing one-shot claim before any
// submission branch. It never replaces one or renews its physical permission.
func prepareOperationReset(ctx context.Context, operation *deploymentOperation, client resourceReader, deployment *unstructured.Unstructured, digest string, request *ActivationRequest, phases map[types.UID]string, rounds map[types.UID]activationRebootRound, reboot ExternalRebooter) (map[types.UID]activationRebootRound, *ActivationObservation, error) {
	if operation == nil {
		return rounds, nil, nil
	}
	key := resetOperationKey(*request, phases)
	desired, identityDigest, err := activationRebootGuardObject(deployment, digest, *request, phases)
	if err != nil {
		return nil, nil, err
	}
	prior, known, err := operation.recordResetIntent(ctx, *request, phases, rounds)
	if err != nil {
		return nil, nil, err
	}

	claim, readErr := client.Get(ctx, desired.DeepCopy())
	if readErr == nil {
		if !known || !activationRebootGuardMatches(claim, desired, identityDigest) || prior.ClaimUID != "" && prior.ClaimUID != claim.GetUID() {
			return nil, nil, fmt.Errorf("RESET_ACTION_REQUIRED: existing claim has no exact saved operation binding")
		}
		observer, ok := reboot.(ExternalRebootObserver)
		if !ok || !validUUID(prior.Baseline) {
			return nil, nil, fmt.Errorf("RESET_ACTION_REQUIRED: claim exists without recoverable host baseline; no submission permitted")
		}
		observation, err := observer.ObserveExternal(ctx, *request, prior.Baseline)
		if err != nil {
			return nil, nil, err
		}
		if prior.Observation.CurrentBootID != "" && observation.CurrentBootID != prior.Observation.CurrentBootID {
			return nil, nil, fmt.Errorf("RESET_ACTION_REQUIRED: an additional host boot was observed")
		}
		restored := map[types.UID]activationRebootRound{}
		for uid, round := range prior.Rounds {
			restored[uid] = liveRound(round)
		}
		return restored, &observation, nil
	}
	if !apierrors.IsNotFound(readErr) || prior.ClaimUID != "" || prior.Baseline != "" {
		return nil, nil, fmt.Errorf("RESET_ACTION_REQUIRED: claim unavailable/missing; it may not be recreated")
	}
	// The native adapter must call this after authenticating the selected host and
	// before command submission. A stale checkpoint writer fails before its action.
	request.recordClaim = func(uid types.UID) error { return operation.bindResetClaim(ctx, key, uid) }
	request.BeforeSubmit = func(observation ActivationObservation) error {
		claim, err := client.Get(ctx, desired.DeepCopy())
		saved, _ := operation.reset(key)
		if err != nil || !activationRebootGuardMatches(claim, desired, identityDigest) || saved.ClaimUID == "" || claim.GetUID() != saved.ClaimUID {
			return fmt.Errorf("RESET_CLAIM_CHANGED: cannot persist baseline")
		}
		return operation.recordResetBaseline(ctx, key, claim.GetUID(), observation)
	}

	return rounds, nil, nil
}

// resolveCompletedResets handles a lost ACK/checkpoint response only from saved
// physical/current-Arm proofs and current exact ready controller advancement.
func resolveCompletedResets(rules *controllercontract.Rules, ctx context.Context, operation *deploymentOperation, client resourceReader, deployment *unstructured.Unstructured, digest string, observed activationObservation, reboot ExternalRebooter) error {
	if err := rules.Validate(); err != nil {
		return err
	}
	if operation == nil {
		return nil
	}
	if err := operation.writable(); err != nil {
		return err
	}
	for key, reset := range operation.pendingResets() {
		pending := false
		for _, node := range reset.Request.DPUNodes {
			current := observed.nodes[node.NodeName]
			if current == nil || current.GetUID() != node.NodeUID {
				return fmt.Errorf("RESET_ACTION_REQUIRED: saved node replaced")
			}
			if current.GetAnnotations()[activationExternalRebootKey] == "true" {
				pending = true
			}
		}
		if pending {
			continue
		}
		if !reset.ACKIntent || !reset.Observation.Reconnected || len(reset.ArmProofs) != len(reset.Rounds) || len(reset.Rounds) == 0 {
			return fmt.Errorf("RESET_ACTION_REQUIRED: absent request does not prove acknowledged reset history")
		}
		phases := make(map[types.UID]string, len(reset.Rounds))
		for uid, round := range reset.Rounds {
			phases[uid] = round.Source
		}
		desired, identityDigest, err := activationRebootGuardObject(deployment, digest, reset.Request, phases)
		if err != nil {
			return err
		}
		claim, err := client.Get(ctx, desired)
		if err != nil || claim == nil || reset.ClaimUID == "" || claim.GetUID() != reset.ClaimUID || !activationRebootGuardMatches(claim, desired, identityDigest) {
			return fmt.Errorf("RESET_ACTION_REQUIRED: exact saved claim unavailable or changed")
		}

		observer, ok := reboot.(ExternalRebootObserver)
		if !ok {
			return fmt.Errorf("RESET_ACTION_REQUIRED: observation backend unavailable")
		}
		host, err := observer.ObserveExternal(ctx, reset.Request, reset.Baseline)
		if err != nil || host.CurrentBootID != reset.Observation.CurrentBootID {
			return fmt.Errorf("RESET_ACTION_REQUIRED: current host boot proof changed")
		}

		nodes := make(map[string]activationObjectReceipt, len(reset.Request.DPUNodes))
		boots := make(map[types.UID]operationArmBoot, len(reset.Rounds))
		for _, node := range reset.Request.DPUNodes {
			current := observed.nodes[node.NodeName]
			if _, present := current.GetAnnotations()[activationExternalRebootKey]; present {
				return fmt.Errorf("RESET_ACTION_REQUIRED: request still present")
			}
			nodes[node.NodeName] = activationObjectReceipt{Name: current.GetName(), UID: current.GetUID(), Generation: current.GetGeneration()}
			for _, selected := range node.SelectedDPUs {
				dpu := observed.dpus[selected.DPUName]
				proof, exists := reset.ArmProofs[selected.DPUUID]
				if !exists || dpu == nil || dpu.GetUID() != selected.DPUUID || dpu.GetGeneration() != proof.Generation || !rules.ResourceReadiness(dpu).Ready || !rules.NodeEffectRemoved(dpu).Ready {
					return fmt.Errorf("RESET_ACTION_REQUIRED: current completed DPU proof unavailable")
				}
				boot, _, _ := unstructured.NestedString(dpu.Object, "status", "agentStatus", "lastObservedPendingNvconfig", "bootID")
				startup, _, _ := unstructured.NestedString(dpu.Object, "status", "agentStatus", "lastStartupTime")
				at, err := time.Parse(time.RFC3339Nano, startup)
				if err != nil || !at.Equal(proof.Startup) || boot != proof.KernelBoot {
					return fmt.Errorf("RESET_ACTION_REQUIRED: current Arm proof changed")
				}
				boots[dpu.GetUID()] = operationArmBoot{Generation: dpu.GetGeneration(), Startup: at, KernelBoot: boot}
			}
		}

		if err := operation.confirmResetControllerAdvance(ctx, key, host, nodes, boots); err != nil {
			return err
		}
	}
	return nil
}
