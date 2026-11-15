// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/redfish"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const activationResetSafetyMaxAge = 2 * time.Minute

// ErrActivationResetSafetyExpired means no reset may be submitted using the
// expired BMC observation. Existing one-shot claims remain blocking.
var ErrActivationResetSafetyExpired = errors.New("ACTIVATION_RESET_SAFETY_EXPIRED: selected BMC reset-safety proof expired before action submission")

// VerifyResetSafetyFresh checks only local attempt timing, never BMC wall time.
// The native rebooter must repeat this check at its command submission boundary.
func (r ActivationRequest) VerifyResetSafetyFresh() error {
	now := time.Now()
	if r.ResetSafetyExpiresAt.IsZero() || r.ResetSafetyExpiresAt.After(now.Add(activationResetSafetyMaxAge)) {
		return fmt.Errorf("ACTIVATION_RESET_SAFETY_UNVERIFIED: a bounded locally observed reset-safety expiry is required")
	}
	if !now.Before(r.ResetSafetyExpiresAt) {
		return ErrActivationResetSafetyExpired
	}
	return nil
}

// activationRebootRound is private evidence for one actual pending request.
// The immutable claim remains phase-bound to prevent replay; this finer fence
// prevents serving or acknowledging a changed request during that attempt.
type activationRebootRound struct {
	uid        types.UID
	generation int64
	source     string
	requested  time.Time
	startup    time.Time
	kernelBoot string
}

func readActivationRebootRound(dpu *unstructured.Unstructured) (activationRebootRound, error) {
	if dpu == nil || dpu.GetUID() == "" || !dpu.GetDeletionTimestamp().IsZero() {
		return activationRebootRound{}, fmt.Errorf("current DPU identity is unavailable")
	}
	source, err := activationRebootSourcePhase(dpu)
	if err != nil {
		return activationRebootRound{}, err
	}
	text := func(path ...string) string {
		value, _, _ := unstructured.NestedString(dpu.Object, path...)
		return value
	}
	if text("status", "phase") != "Rebooting" || text("status", "rebootStatus", "phase") != "Pending" ||
		text("status", "rebootStatus", "reason") != activationManualWaitReason || text("status", "rebootStatus", "method") != "SystemLevelReset" {
		return activationRebootRound{}, fmt.Errorf("current DPU does not have the exact external reset request")
	}
	requested, requestErr := time.Parse(time.RFC3339Nano, text("status", "rebootStatus", "lastTransitionTime"))
	startup, startupErr := time.Parse(time.RFC3339Nano, text("status", "agentStatus", "lastStartupTime"))
	kernel := text("status", "agentStatus", "lastObservedPendingNvconfig", "bootID")
	if requestErr != nil || requested.IsZero() || requested.After(time.Now()) {
		return activationRebootRound{}, fmt.Errorf("pending-request transition time is missing, malformed or future")
	}
	if startupErr != nil || startup.IsZero() || startup.After(time.Now()) {
		return activationRebootRound{}, fmt.Errorf("current agent startup time is missing, malformed or future")
	}
	if !validUUID(kernel) {
		return activationRebootRound{}, fmt.Errorf("current Arm kernel boot identity is missing or malformed")
	}
	return activationRebootRound{uid: dpu.GetUID(), generation: dpu.GetGeneration(), source: source, requested: requested, startup: startup, kernelBoot: kernel}, nil
}

func (r activationRebootRound) sameRequest(current activationRebootRound) bool {
	return r.uid == current.uid && r.generation == current.generation && r.source == current.source && r.requested.Equal(current.requested)
}

type activationRebootBMC struct {
	selected enrollmentBMC
	startup  time.Time
}

func (c *APIClient) verifyActivationRebootBMCs(ctx context.Context, selected []activationRebootBMC, timeout time.Duration, journal *executionJournal) (time.Time, error) {
	if len(selected) == 0 || journal == nil {
		return time.Time{}, fmt.Errorf("reset safety requires selected devices and an owned journal")
	}
	var oldest time.Time
	for _, item := range selected {
		event := executionEvent{Stage: generation.StageDeployment, Operation: "verify-reset-safety", Subject: item.selected.device.Serial,
			State: "attempted", ReasonCode: "ACTIVATION_RESET_SAFETY_STARTED", Message: "Read the selected current-round BMC identity, Administrator, Arm shutdown, complete tasks and qualified firmware copy state before a host reset."}
		if err := journal.record(event); err != nil {
			return time.Time{}, err
		}
		observed, err := (redfish.Collector{}).ObserveResetSafety(ctx, item.selected.target, timeout, redfish.ResetSafetyRequest{
			SerialNumber: item.selected.device.Serial, PSID: item.selected.device.PSID, NotBefore: item.startup,
		})
		if err != nil {
			var cause error
			if errors.Is(err, context.Canceled) {
				cause = context.Canceled
			} else if errors.Is(err, context.DeadlineExceeded) {
				cause = context.DeadlineExceeded
			}
			return time.Time{}, activationFailure(journal, "ACTIVATION_RESET_SAFETY_UNVERIFIED", "The selected DPU is not verified safe for host reset. "+safeBMCDiagnostics(err), cause)
		}
		if oldest.IsZero() || observed.ObservedAt.Before(oldest) {
			oldest = observed.ObservedAt
		}
		event.State, event.ReasonCode = "confirmed", "ACTIVATION_RESET_SAFE_NIC_COPY_NOT_EXPOSED"
		event.Message = fmt.Sprintf("Same selected DPU is Off with nonbooting progress at %s; local observation %s expires %s; %d complete successful tasks and four idle board-copy targets were verified. NIC slots use qualified Unknown/NA and do not expose independent programming-idle state; concurrent external hardware writers must be excluded.", observed.BootStateTime.UTC().Format(time.RFC3339Nano), observed.ObservedAt.UTC().Format(time.RFC3339Nano), observed.ObservedAt.Add(activationResetSafetyMaxAge).UTC().Format(time.RFC3339Nano), observed.TaskCount)
		if err := journal.record(event); err != nil {
			return time.Time{}, err
		}
	}
	if oldest.IsZero() || time.Since(oldest) >= activationResetSafetyMaxAge {
		return time.Time{}, activationFailure(journal, "ACTIVATION_RESET_SAFETY_STALE", "The complete same-host cohort could not be verified within the bounded reset-safety window; no reset was requested.", nil)
	}
	return oldest.Add(activationResetSafetyMaxAge), nil
}

// waitActivationAgentReboots never sends another physical action. The same
// request must remain pending while both agent startup and current kernel boot
// identity advance. initialBootID deliberately is not a current-boot signal.
func waitActivationAgentReboots(ctx context.Context, client activationStageClient, execution *managementExecution, deployment *unstructured.Unstructured,
	deploymentUID types.UID, selected []activationSelectedDevice, indexes []int, rounds map[types.UID]activationRebootRound, interval time.Duration,
) (activationObservation, error) {
	for {
		current, err := observeActivationScope(ctx, client, execution, deployment, deploymentUID, selected)
		if err != nil || !current.complete {
			if ctx.Err() != nil {
				return activationObservation{}, ctx.Err()
			}
			return activationObservation{}, fmt.Errorf("selected cohort changed after host reset")
		}
		_, actionable, err := selectedHostRebootPending(current, selected, indexes)
		if err != nil || !actionable {
			return activationObservation{}, fmt.Errorf("selected external request changed after host reset")
		}
		ready := true
		for _, index := range indexes {
			dpu := current.dpus[selected[index].dpuName]
			now, err := readActivationRebootRound(dpu)
			before, found := rounds[now.uid]
			if err != nil || !found || !before.sameRequest(now) {
				return activationObservation{}, fmt.Errorf("selected DPU or reboot round changed after host reset")
			}
			if !now.startup.After(before.startup) || !now.startup.After(before.requested) || now.kernelBoot == before.kernelBoot {
				ready = false
			}
		}
		if ready {
			return current, nil
		}
		if err := activationWait(ctx, interval); err != nil {
			return activationObservation{}, err
		}
	}
}
