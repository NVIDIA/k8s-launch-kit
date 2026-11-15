// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func savedRound(r activationRebootRound) operationRound {
	return operationRound{UID: r.uid, Generation: r.generation, Source: r.source, Requested: r.requested, Startup: r.startup, KernelBoot: r.kernelBoot}
}
func liveRound(r operationRound) activationRebootRound {
	return activationRebootRound{uid: r.UID, generation: r.Generation, source: r.Source, requested: r.Requested, startup: r.Startup, kernelBoot: r.KernelBoot}
}
func resetOperationKey(request ActivationRequest, phases map[types.UID]string) string {
	// Match the immutable claim's phase-bound logical identity, never an attempt.
	return request.HostSystemUUID + "/" + inventory.Digest("dpf-reset-phase-v1", phases)
}

// prepareOperationReset recognizes an existing one-shot claim before any
// submission branch. It never replaces one or renews its physical permission.
func prepareOperationReset(ctx context.Context, operation *deploymentOperation, client ResourceClient, deployment *unstructured.Unstructured, digest string, request *ActivationRequest, phases map[types.UID]string, rounds map[types.UID]activationRebootRound, reboot ExternalRebooter) (map[types.UID]activationRebootRound, *ActivationObservation, error) {
	if operation == nil {
		return rounds, nil, nil
	}
	key := resetOperationKey(*request, phases)
	desired, identityDigest, err := activationRebootGuardObject(deployment, digest, *request, phases)
	if err != nil {
		return nil, nil, err
	}
	prior, known := operation.state.Resets[key]
	if known {
		if len(prior.Rounds) != len(rounds) {
			return nil, nil, fmt.Errorf("RESET_ACTION_REQUIRED: saved cohort changed")
		}
		for uid, current := range rounds {
			old, exists := prior.Rounds[uid]
			if !exists || !liveRound(old).sameRequest(current) {
				return nil, nil, fmt.Errorf("RESET_ACTION_REQUIRED: pending round changed")
			}
		}
	} else {
		prior = operationReset{Request: *request, Rounds: map[types.UID]operationRound{}}
		for uid, round := range rounds {
			prior.Rounds[uid] = savedRound(round)
		}
		operation.state.Resets[key] = prior
		if err := operation.save(ctx); err != nil {
			return nil, nil, err
		}
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
	request.recordClaim = func(uid types.UID) error {
		value := operation.state.Resets[key]
		if value.ClaimUID != "" || !validUUID(string(uid)) {
			return fmt.Errorf("RESET_CLAIM_CHANGED: claim receipt already bound or invalid")
		}
		value.ClaimUID = uid
		operation.state.Resets[key] = value
		return operation.save(ctx)
	}
	request.BeforeSubmit = func(observation ActivationObservation) error {
		if observation.HostSystemUUID != request.HostSystemUUID || observation.HostAdapterSerial != request.HostAdapterSerial || !validUUID(observation.PreviousBootID) {
			return fmt.Errorf("RESET_BASELINE_INVALID: authenticated host baseline required")
		}
		claim, err := client.Get(ctx, desired.DeepCopy())
		if err != nil || !activationRebootGuardMatches(claim, desired, identityDigest) || operation.state.Resets[key].ClaimUID == "" || claim.GetUID() != operation.state.Resets[key].ClaimUID {
			return fmt.Errorf("RESET_CLAIM_CHANGED: cannot persist baseline")
		}
		value := operation.state.Resets[key]
		if value.Baseline != "" {
			return fmt.Errorf("RESET_ACTION_REQUIRED: baseline already recorded")
		}
		value.Baseline = observation.PreviousBootID
		value.ClaimUID = claim.GetUID()
		operation.state.Resets[key] = value
		return operation.save(ctx)
	}
	return rounds, nil, nil
}

func (o *deploymentOperation) saveResetObservation(ctx context.Context, key string, observation ActivationObservation) error {
	if o == nil {
		return nil
	}
	value, ok := o.state.Resets[key]
	if !ok || value.Baseline != observation.PreviousBootID || !validUUID(observation.CurrentBootID) || observation.CurrentBootID == value.Baseline {
		return fmt.Errorf("RESET_ACTION_REQUIRED: physical proof does not match saved baseline")
	}
	value.Observation = observation
	o.state.Resets[key] = value
	return o.save(ctx)
}

func (o *deploymentOperation) saveResetProofs(ctx context.Context, key string, proofs map[types.UID]activationRebootRound) error {
	if o == nil {
		return nil
	}
	value := o.state.Resets[key]
	value.ArmProofs = map[types.UID]operationRound{}
	for uid, proof := range proofs {
		value.ArmProofs[uid] = savedRound(proof)
	}
	value.ACKIntent = true
	o.state.Resets[key] = value
	return o.save(ctx)
}

func (o *deploymentOperation) finishReset(ctx context.Context, key string, progress map[string]activationNodeProgress) error {
	if o == nil {
		return nil
	}
	value := o.state.Resets[key]
	value.Acknowledged = true
	o.state.Resets[key] = value
	return o.saveProgress(ctx, progress)
}

// resolveCompletedResets handles a lost ACK/checkpoint response only from saved
// physical/current-Arm proofs and current exact ready controller advancement.
func (o *deploymentOperation) resolveCompletedResets(ctx context.Context, observed activationObservation, reboot ExternalRebooter) error {
	if o == nil {
		return nil
	}
	for key, reset := range o.state.Resets {
		if reset.Acknowledged {
			continue
		}
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
		observer, ok := reboot.(ExternalRebootObserver)
		if !ok {
			return fmt.Errorf("RESET_ACTION_REQUIRED: observation backend unavailable")
		}
		host, err := observer.ObserveExternal(ctx, reset.Request, reset.Baseline)
		if err != nil || host.CurrentBootID != reset.Observation.CurrentBootID {
			return fmt.Errorf("RESET_ACTION_REQUIRED: current host boot proof changed")
		}
		for _, node := range reset.Request.DPUNodes {
			for _, selected := range node.SelectedDPUs {
				dpu := observed.dpus[selected.DPUName]
				proof, exists := reset.ArmProofs[selected.DPUUID]
				if !exists || dpu == nil || dpu.GetUID() != selected.DPUUID || dpu.GetGeneration() != proof.Generation || !EvaluateReadiness(dpu).Ready || !conditionReadiness(dpu, "NodeEffectRemoved", "DPU_STATUS_STALE", true).Ready {
					return fmt.Errorf("RESET_ACTION_REQUIRED: current completed DPU proof unavailable")
				}
				boot, _, _ := unstructured.NestedString(dpu.Object, "status", "agentStatus", "lastObservedPendingNvconfig", "bootID")
				startup, _, _ := unstructured.NestedString(dpu.Object, "status", "agentStatus", "lastStartupTime")
				at, err := time.Parse(time.RFC3339Nano, startup)
				if err != nil || !at.Equal(proof.Startup) || boot != proof.KernelBoot {
					return fmt.Errorf("RESET_ACTION_REQUIRED: current Arm proof changed")
				}
			}
		}
		reset.Acknowledged = true
		o.state.Resets[key] = reset
		if err := o.save(ctx); err != nil {
			return err
		}
	}
	return nil
}
