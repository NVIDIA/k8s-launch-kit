// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

func waitWorkloadResource(rules *controllercontract.Rules, ctx context.Context, client identityResourceReader, reconciler nativeObjectReconciler, tenant tenantVerificationReader,
	spec workloadResourceSpec, uid types.UID, journal *executionJournal, interval time.Duration,
) (types.UID, error) {
	if rules.Validate() != nil || ctx == nil || client == nil || reconciler == nil || tenant == nil || spec.object == nil || uid == "" || journal == nil || interval <= 0 {
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
			last = rules.WorkloadReadiness(current, spec.mode)
			if controllercontract.TerminalWorkloadReadiness(last) {
				return childUID, workloadStageFailure(journal, last.ReasonCode, "A DPF workload prerequisite reported an invalid or terminal state.", nil)
			}
			if last.Ready && spec.mode != controllercontract.WorkloadTemplate {
				child, readErr := tenant.getWorkloadChild(ctx, spec.object)
				if readErr != nil {
					if apierrors.IsNotFound(readErr) || retryableRead(readErr) {
						last = result("WORKLOAD_TENANT_CHILD_PENDING", "The controller-reported tenant child has not appeared yet.")
					} else {
						return childUID, workloadStageFailure(journal, "WORKLOAD_TENANT_CHILD_UNAVAILABLE", "The selected tenant child object could not be completely inspected.", safeAPIError("read selected tenant child", readErr))
					}
				} else if child == nil {
					return childUID, workloadStageFailure(journal, "WORKLOAD_TENANT_CHILD_INVALID", "The selected tenant child observation is empty.", nil)
				} else if err := verifyWorkloadChild(current, child, tenant.expectation()); err != nil {
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
				last = rules.WorkloadReadiness(final, spec.mode)
				if last.Ready && spec.mode != controllercontract.WorkloadTemplate {
					finalChild, finalChildErr := tenant.getWorkloadChild(ctx, spec.object)
					var verificationErr error
					if finalChildErr == nil && finalChild != nil && finalChild.GetUID() == childUID {
						verificationErr = verifyWorkloadChild(final, finalChild, tenant.expectation())
					}
					if finalChildErr != nil || finalChild == nil || finalChild.GetUID() != childUID || verificationErr != nil {
						return childUID, workloadStageFailure(journal, "WORKLOAD_TENANT_CHILD_CHANGED", workloadChildVerificationMessage("The selected tenant child changed before readiness was confirmed.", verificationErr), safeAPIError("recheck tenant workload child", finalChildErr))
					}
				}
				if last.Ready {
					if err := client.VerifyIdentity(ctx); err != nil {
						diagnostic := managementIdentityDiagnostic(ctx, err)
						return childUID, workloadStageFailure(journal, diagnostic.ReasonCode, diagnostic.Message+" No workload-readiness receipt was issued.", diagnostic)
					}
					if spec.mode != controllercontract.WorkloadTemplate {
						if err := tenant.VerifyIdentity(ctx); err != nil {
							diagnostic := tenantIdentityDiagnostic(ctx, err)
							return childUID, workloadStageFailure(journal, diagnostic.ReasonCode, diagnostic.Message+" No workload-readiness receipt was issued.", diagnostic)
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
