// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"

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

func (o *deploymentOperation) recordResetIntent(ctx context.Context, request ActivationRequest, phases map[types.UID]string, rounds map[types.UID]activationRebootRound) (operationReset, bool, error) {
	if err := o.writable(); err != nil {
		return operationReset{}, false, err
	}
	key := resetOperationKey(request, phases)
	if err := o.validateResetRequest(request, phases, rounds); err != nil {
		return operationReset{}, false, err
	}
	if prior, known := o.state.Resets[key]; known {
		// Completion closes this phase-bound logical request permanently. A
		// reappeared pending annotation cannot renew its ACK authority.
		if prior.Acknowledged {
			return operationReset{}, true, fmt.Errorf("RESET_ACTION_REQUIRED: reset request already acknowledged")
		}
		if !reflect.DeepEqual(copyOperationRequest(prior.Request), copyOperationRequest(request)) || len(prior.Rounds) != len(rounds) {
			return operationReset{}, true, o.bindingConflict("RESET_ACTION_REQUIRED: saved cohort changed")
		}
		for uid, current := range rounds {
			old, exists := prior.Rounds[uid]
			if !exists || !liveRound(old).sameRequest(current) {
				return operationReset{}, true, o.bindingConflict("RESET_ACTION_REQUIRED: pending round changed")
			}
		}
		return copyOperationReset(prior), true, nil
	}
	prior := operationReset{Request: copyOperationRequest(request), Rounds: map[types.UID]operationRound{}}
	for uid, round := range rounds {
		prior.Rounds[uid] = savedRound(round)
	}
	o.state.Resets[key] = prior
	return copyOperationReset(prior), false, o.save(ctx)
}
func (o *deploymentOperation) validateResetRequest(request ActivationRequest, phases map[types.UID]string, rounds map[types.UID]activationRebootRound) error {
	if o.state.DeploymentUID == "" || request.HostInputRef == "" || request.HostEndpoint == "" || !validUUID(request.HostSystemUUID) || request.HostAdapterSerial == "" || request.HostAdapterKey == "" || len(request.DPUNodes) == 0 || len(rounds) == 0 || len(phases) != len(rounds) {
		return fmt.Errorf("RESET_ACTION_REQUIRED: exact bound host request required")
	}
	seenNodes, seenDPUs := map[string]bool{}, map[types.UID]bool{}
	for _, node := range request.DPUNodes {
		child, known := o.state.Children["DPUNode/"+node.NodeName]
		if !known || child.UID != node.NodeUID || seenNodes[node.NodeName] || node.RebootMethod != "SystemLevelReset" || len(node.SelectedDPUs) == 0 {
			return fmt.Errorf("RESET_ACTION_REQUIRED: node cohort changed")
		}
		seenNodes[node.NodeName] = true
		for _, dpu := range node.SelectedDPUs {
			child, known := o.state.Children["DPU/"+dpu.DPUName]
			round, hasRound := rounds[dpu.DPUUID]
			enrolled := false
			for _, device := range o.state.Enrollment.Devices {
				if device.Serial == dpu.SerialNumber && device.DPUNodeUID == node.NodeUID {
					enrolled = true
				}
			}
			if !known || child.UID != dpu.DPUUID || !enrolled || seenDPUs[dpu.DPUUID] || dpu.HostAdapterKey == "" || dpu.AdapterSerial == "" || !hasRound || round.uid != dpu.DPUUID || round.generation != child.Generation || phases[dpu.DPUUID] != round.source || !validOperationRound(savedRound(round)) {
				return fmt.Errorf("RESET_ACTION_REQUIRED: DPU round cohort changed")
			}
			seenDPUs[dpu.DPUUID] = true
		}
	}
	if len(seenDPUs) != len(rounds) {
		return fmt.Errorf("RESET_ACTION_REQUIRED: foreign round")
	}
	return nil
}
func validOperationRound(round operationRound) bool {
	return round.UID != "" && round.Generation > 0 && supportedActivationRebootSourcePhase(round.Source) && !round.Requested.IsZero() && !round.Startup.IsZero() && validUUID(round.KernelBoot)
}
func (o *deploymentOperation) bindResetClaim(ctx context.Context, key string, uid types.UID) error {
	if err := o.writable(); err != nil {
		return err
	}
	value, known := o.state.Resets[key]
	if !known || value.ClaimUID != "" || value.Baseline != "" || value.Acknowledged || !validUUID(string(uid)) {
		return fmt.Errorf("RESET_CLAIM_CHANGED: claim receipt already bound or invalid")
	}
	value.ClaimUID = uid
	o.state.Resets[key] = value
	return o.save(ctx)
}
func (o *deploymentOperation) recordResetBaseline(ctx context.Context, key string, claimUID types.UID, observation ActivationObservation) error {
	if err := o.writable(); err != nil {
		return err
	}
	value, known := o.state.Resets[key]
	if !known || value.ClaimUID == "" || value.ClaimUID != claimUID || value.Baseline != "" || value.Acknowledged || observation.HostSystemUUID != value.Request.HostSystemUUID || observation.HostAdapterSerial != value.Request.HostAdapterSerial || !validUUID(observation.PreviousBootID) {
		return fmt.Errorf("RESET_BASELINE_INVALID: exact authenticated claim and host baseline required")
	}
	value.Baseline = observation.PreviousBootID
	o.state.Resets[key] = value
	return o.save(ctx)
}

// recordResetObservation reports whether the observation matched saved history.
// A match is neither fresh authority nor durable success: true can accompany a
// persistence error, so callers retain physical history but must stop effects.
// A nil owner preserves stage-only observations without a checkpoint contract.
func (o *deploymentOperation) recordResetObservation(ctx context.Context, key string, observation ActivationObservation) (bool, error) {
	if o == nil {
		return true, nil
	}
	if err := o.writable(); err != nil {
		return false, err
	}
	value, known := o.state.Resets[key]
	if !known || value.ClaimUID == "" || !validUUID(value.Baseline) || value.Baseline != observation.PreviousBootID || observation.HostSystemUUID != value.Request.HostSystemUUID || observation.HostAdapterSerial != value.Request.HostAdapterSerial || !observation.Reconnected || !validUUID(observation.CurrentBootID) || observation.CurrentBootID == value.Baseline || value.Observation.CurrentBootID != "" && value.Observation.CurrentBootID != observation.CurrentBootID {
		return false, fmt.Errorf("RESET_ACTION_REQUIRED: physical proof does not match saved baseline")
	}
	value.Observation = copyOperationObservation(observation)
	o.state.Resets[key] = value
	return true, o.save(ctx)
}
func validateResetACKHistory(value operationReset) error {
	physical := value.Observation
	if !validUUID(string(value.ClaimUID)) || !validUUID(value.Baseline) || physical.HostSystemUUID != value.Request.HostSystemUUID || physical.HostAdapterSerial != value.Request.HostAdapterSerial || physical.PreviousBootID != value.Baseline || !physical.Reconnected || !validUUID(physical.CurrentBootID) || physical.CurrentBootID == value.Baseline || len(value.Rounds) == 0 || len(value.ArmProofs) != len(value.Rounds) {
		return fmt.Errorf("RESET_ACTION_REQUIRED: complete physical and Arm proofs required")
	}
	for uid, prior := range value.Rounds {
		proof, found := value.ArmProofs[uid]
		if !found || uid != proof.UID || !validOperationRound(proof) || !liveRound(prior).sameRequest(liveRound(proof)) || !proof.Startup.After(prior.Startup) || !proof.Startup.After(prior.Requested) || proof.KernelBoot == prior.KernelBoot {
			return fmt.Errorf("RESET_ACTION_REQUIRED: Arm proof changed or incomplete")
		}
	}
	return nil
}
func (o *deploymentOperation) recordResetACKIntent(ctx context.Context, key string, proofs map[types.UID]activationRebootRound) error {
	if o == nil {
		return nil
	}
	if err := o.writable(); err != nil {
		return err
	}
	value, known := o.state.Resets[key]
	if !known {
		return fmt.Errorf("RESET_ACTION_REQUIRED: unknown reset intent")
	}
	retained := make(map[types.UID]operationRound, len(proofs))
	for uid, proof := range proofs {
		if saved, known := value.ArmProofs[uid]; known && !reflect.DeepEqual(saved, savedRound(proof)) {
			return o.bindingConflict("RESET_ACTION_REQUIRED: saved Arm proof changed")
		}
		retained[uid] = savedRound(proof)
	}
	value.ArmProofs, value.ACKIntent = retained, true
	if err := validateResetACKHistory(value); err != nil {
		return err
	}
	o.state.Resets[key] = value
	return o.save(ctx)
}
func (o *deploymentOperation) confirmResetAcknowledged(ctx context.Context, key string, completions []activationNodeProgress) error {
	if o == nil {
		return nil
	}
	if err := o.writable(); err != nil {
		return err
	}
	value, known := o.state.Resets[key]
	if !known || !value.ACKIntent || validateResetACKHistory(value) != nil || len(completions) != len(value.Request.DPUNodes) {
		return fmt.Errorf("RESET_ACTION_REQUIRED: exact ACK intent and completions required")
	}
	progress := maps.Clone(o.state.Progress)
	seen := map[string]bool{}
	for _, proof := range completions {
		p := savedProgress(proof)
		if err := o.validateProgress(p); err != nil {
			return err
		}
		var node *ActivationNode
		for i := range value.Request.DPUNodes {
			if value.Request.DPUNodes[i].NodeName == p.NodeName {
				node = &value.Request.DPUNodes[i]
			}
		}
		if node == nil || seen[p.NodeName] || !p.ResetAcknowledged || p.ReleasedVersion != "" || len(node.SelectedDPUs) != len(p.DPUs) {
			return fmt.Errorf("RESET_ACTION_REQUIRED: completion outside reset cohort")
		}
		for _, dpu := range node.SelectedDPUs {
			if p.DPUs[dpu.DPUName] != dpu.DPUUID {
				return fmt.Errorf("RESET_ACTION_REQUIRED: completed DPU changed")
			}
		}
		if prior, exists := progress[p.NodeName]; exists && !sameProgressCohort(prior, p) {
			return o.bindingConflict("RESET_ACTION_REQUIRED: completion bindings changed")
		}
		seen[p.NodeName] = true
		progress[p.NodeName] = p
	}
	value.Acknowledged = true
	o.state.Resets[key], o.state.Progress = value, progress
	return o.save(ctx)
}

// operationArmBoot is a bounded observed value, not a freshness capability.
type operationArmBoot struct {
	Generation int64
	Startup    time.Time
	KernelBoot string
}

func (o *deploymentOperation) confirmResetControllerAdvance(ctx context.Context, key string, host ActivationObservation, nodes map[string]activationObjectReceipt, armBoots map[types.UID]operationArmBoot) error {
	if err := o.writable(); err != nil {
		return err
	}
	value, known := o.state.Resets[key]
	if !known || !value.ACKIntent || validateResetACKHistory(value) != nil || host.HostSystemUUID != value.Request.HostSystemUUID || host.HostAdapterSerial != value.Request.HostAdapterSerial || !host.Reconnected || host.PreviousBootID != value.Baseline || host.CurrentBootID != value.Observation.CurrentBootID {
		return fmt.Errorf("RESET_ACTION_REQUIRED: controller advance differs from saved physical proofs")
	}
	if len(nodes) != len(value.Request.DPUNodes) || len(armBoots) != len(value.Rounds) {
		return fmt.Errorf("RESET_ACTION_REQUIRED: complete controller identity and Arm observations required")
	}
	for _, node := range value.Request.DPUNodes {
		current, known := nodes[node.NodeName]
		prior := o.state.Children["DPUNode/"+node.NodeName]
		if !known || current.Name != node.NodeName || current.UID != node.NodeUID || current.Generation != prior.Generation {
			return fmt.Errorf("RESET_ACTION_REQUIRED: saved node replaced or changed")
		}
		for _, selected := range node.SelectedDPUs {
			current, known := armBoots[selected.DPUUID]
			saved, proved := value.ArmProofs[selected.DPUUID]
			if !known || !proved || current.Generation != saved.Generation || !current.Startup.Equal(saved.Startup) || current.KernelBoot != saved.KernelBoot {
				return fmt.Errorf("RESET_ACTION_REQUIRED: current Arm observation differs from saved proof")
			}
		}
	}

	value.Acknowledged = true
	o.state.Resets[key] = value
	return o.save(ctx)
}
