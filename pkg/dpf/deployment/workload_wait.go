// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
)

func waitWorkloadResource(ctx context.Context, client workloadStageClient, reconciler nativeObjectReconciler, tenant *tenantClient,
	spec workloadResourceSpec, uid types.UID, journal *executionJournal, interval time.Duration,
) (types.UID, error) {
	if ctx == nil || client == nil || reconciler == nil || tenant == nil || spec.object == nil || uid == "" || journal == nil || interval <= 0 {
		return "", workloadStageFailure(journal, "WORKLOAD_WAIT_INPUT_INVALID", "A bounded workload readiness wait is incomplete.", nil)
	}
	last := result("WORKLOAD_STATUS_UNOBSERVED", "The DPF controller has not reported current workload reconciliation.")
	childUID := types.UID("")
	for {
		if err := ctx.Err(); err != nil {
			return childUID, workloadStageFailure(journal, "WORKLOAD_READINESS_CANCELED", "Workload prerequisites did not reconcile before cancellation.", err)
		}
		current, err := client.Get(ctx, spec.object.DeepCopy())
		if err != nil {
			if retryableRead(err) {
				last = result("WORKLOAD_RESOURCE_READ_RETRY", "The API is temporarily unavailable; workload reconciliation remains unverified.")
			} else {
				return childUID, workloadStageFailure(journal, "WORKLOAD_RESOURCE_READ_FAILED", "A retained workload prerequisite could not be refreshed.", safeAPIError("read workload prerequisite", err))
			}
		} else if current == nil || current.GetUID() != uid {
			return childUID, workloadStageFailure(journal, "WORKLOAD_RESOURCE_REPLACED", "A retained workload prerequisite was replaced while waiting.", nil)
		} else if err := reconciler.Verify(spec.object, current); err != nil {
			return childUID, workloadStageFailure(journal, "WORKLOAD_RESOURCE_DRIFTED", "A retained workload prerequisite no longer matches this bundle.", nil)
		} else {
			last = workloadResourceReadiness(current, spec.mode)
			if terminalWorkloadReadiness(last) {
				return childUID, workloadStageFailure(journal, last.ReasonCode, "A DPF workload prerequisite reported an invalid or terminal state.", nil)
			}
			if last.Ready && spec.mode != workloadTemplateReadiness {
				child, readErr := client.getTenantWorkloadChild(ctx, tenant, spec.object)
				if readErr != nil {
					if apierrors.IsNotFound(readErr) || retryableRead(readErr) {
						last = result("WORKLOAD_TENANT_CHILD_PENDING", "The controller-reported tenant child has not appeared yet.")
					} else {
						return childUID, workloadStageFailure(journal, "WORKLOAD_TENANT_CHILD_UNAVAILABLE", "The selected tenant child object could not be completely inspected.", safeAPIError("read selected tenant child", readErr))
					}
				} else if child == nil {
					return childUID, workloadStageFailure(journal, "WORKLOAD_TENANT_CHILD_INVALID", "The selected tenant child observation is empty.", nil)
				} else if err := verifyWorkloadChild(current, child, tenant.expected); err != nil {
					return childUID, workloadStageFailure(journal, "WORKLOAD_TENANT_CHILD_MISMATCH", workloadChildVerificationMessage("The controller-created tenant child does not match the retained parent resource.", err), nil)
				} else if childUID != "" && childUID != child.GetUID() {
					return childUID, workloadStageFailure(journal, "WORKLOAD_TENANT_CHILD_REPLACED", "The selected tenant child was replaced while waiting.", nil)
				} else {
					childUID = child.GetUID()
				}
			}
			if last.Ready {
				final, finalErr := client.Get(ctx, spec.object.DeepCopy())
				if finalErr != nil {
					return childUID, workloadStageFailure(journal, "WORKLOAD_RESOURCE_READ_FAILED", "The workload prerequisite could not be re-read before recording readiness.", safeAPIError("recheck workload prerequisite", finalErr))
				}
				if final == nil || final.GetUID() != uid || reconciler.Verify(spec.object, final) != nil {
					return childUID, workloadStageFailure(journal, "WORKLOAD_RESOURCE_CHANGED", "The workload prerequisite changed before readiness was confirmed.", nil)
				}
				last = workloadResourceReadiness(final, spec.mode)
				if last.Ready && spec.mode != workloadTemplateReadiness {
					finalChild, finalChildErr := client.getTenantWorkloadChild(ctx, tenant, spec.object)
					var verificationErr error
					if finalChildErr == nil && finalChild != nil && finalChild.GetUID() == childUID {
						verificationErr = verifyWorkloadChild(final, finalChild, tenant.expected)
					}
					if finalChildErr != nil || finalChild == nil || finalChild.GetUID() != childUID || verificationErr != nil {
						return childUID, workloadStageFailure(journal, "WORKLOAD_TENANT_CHILD_CHANGED", workloadChildVerificationMessage("The selected tenant child changed before readiness was confirmed.", verificationErr), safeAPIError("recheck tenant workload child", finalChildErr))
					}
				}
				if last.Ready {
					if err := client.VerifyIdentity(ctx); err != nil {
						return childUID, workloadStageFailure(journal, "MANAGEMENT_IDENTITY_CHANGED", "Management cluster identity changed while waiting for workload reconciliation.", err)
					}
					if spec.mode != workloadTemplateReadiness {
						if err := tenant.VerifyIdentity(ctx); err != nil {
							return childUID, workloadStageFailure(journal, "TENANT_IDENTITY_CHANGED", "Selected tenant API identity changed while verifying its controller child.", err)
						}
					}
					if err := ctx.Err(); err != nil {
						return childUID, workloadStageFailure(journal, "WORKLOAD_READINESS_CANCELED", "Workload preparation was canceled before readiness was recorded.", err)
					}
					if err := journal.record(executionEvent{Stage: generation.StageWorkload, Operation: "readiness", Subject: spec.object.GetKind() + "/" + spec.object.GetNamespace() + "/" + spec.object.GetName(), State: "ready",
						ReasonCode: last.ReasonCode, Message: last.Message}); err != nil {
						return childUID, err
					}
					if err := ctx.Err(); err != nil {
						return childUID, workloadStageFailure(journal, "WORKLOAD_READINESS_CANCELED", "Workload readiness was canceled before the receipt could be issued.", err)
					}
					return childUID, nil
				}
			}
		}
		if err := journal.record(executionEvent{Stage: generation.StageWorkload, Operation: "readiness", Subject: spec.object.GetKind() + "/" + spec.object.GetNamespace() + "/" + spec.object.GetName(), State: "waiting",
			ReasonCode: last.ReasonCode, Message: last.Message}); err != nil {
			return childUID, err
		}
		if err := waitInterval(ctx, interval); err != nil {
			return childUID, workloadStageFailure(journal, "WORKLOAD_READINESS_TIMEOUT", "Retained workload prerequisites did not converge before the deadline.", err)
		}
	}
}

func workloadResourceReadiness(object *unstructured.Unstructured, mode workloadCheckMode) Readiness {
	if object == nil || object.GetDeletionTimestamp() != nil {
		return result("WORKLOAD_RESOURCE_DELETING", "The retained workload prerequisite is unavailable or deleting.")
	}
	generation, ok := metadataGeneration(object)
	if !ok {
		return result("WORKLOAD_GENERATION_INVALID", "The workload resource generation is missing or invalid.")
	}
	observed, found, err := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
	if err != nil || !found || observed < generation || observed == 0 {
		return result("WORKLOAD_STATUS_STALE", "The DPF controller has not observed the current workload generation.")
	}
	if observed > generation {
		return result("WORKLOAD_STATUS_INVALID", "The DPF controller observed a future workload generation.")
	}
	var conditions []string
	switch mode {
	case workloadTemplateReadiness:
		conditions = []string{"Ready", "DPUServiceTemplateReconciled"}
	case workloadIPAMReadiness:
		conditions = []string{"DPUIPAMObjectReconciled"}
	case workloadInterfaceReadiness:
		conditions = []string{"ServiceInterfaceSetReconciled"}
	default:
		return result("WORKLOAD_STATUS_UNSUPPORTED", "No readiness condition is defined for this workload resource.")
	}
	for _, conditionType := range conditions {
		state := conditionReadiness(object, conditionType, "WORKLOAD_CONDITION_STALE", true)
		if !state.Ready {
			if state.ReasonCode == "RESOURCE_NOT_READY" {
				condition, conditionState := findCondition(object, conditionType)
				if !conditionState.Ready || condition == nil {
					return conditionState
				}
				if condition.reason != "Pending" {
					return withControllerReason(result("RESOURCE_TERMINAL_FAILURE", fmt.Sprintf("The %s condition failed reconciliation.", conditionType)), condition.reason)
				}
				state.ReasonCode = "WORKLOAD_RECONCILIATION_PENDING"
			}
			return state
		}
	}
	return ready("The DPF controller reported the required current-generation workload reconciliation condition.")
}

func terminalWorkloadReadiness(state Readiness) bool {
	switch state.ReasonCode {
	case "RESOURCE_TERMINAL_FAILURE", "RESOURCE_DELETING", "RESOURCE_STATUS_INVALID", "RESOURCE_STATUS_AMBIGUOUS", "WORKLOAD_GENERATION_INVALID", "WORKLOAD_STATUS_INVALID", "WORKLOAD_STATUS_UNSUPPORTED":
		return true
	default:
		return false
	}
}
