// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// waitTenantReady polls only the selected tenant's controller-owned sources.
// It does not open a tenant API client or perform resource effects.
func waitTenantReady(
	ctx context.Context,
	resources resourceReader,
	sources tenantSourceReader,
	expected TenantReadinessExpected,
	desiredDPUCluster *unstructured.Unstructured,
	establishedUID types.UID,
	reconciler nativeObjectReconciler,
	journal *executionJournal,
	interval time.Duration,
) (TenantReadinessObserved, error) {
	if ctx == nil || resources == nil || sources == nil || desiredDPUCluster == nil || establishedUID == "" || reconciler == nil || journal == nil || interval <= 0 {
		return TenantReadinessObserved{}, fmt.Errorf("tenant readiness wait requires its clients, retained DPUCluster, UID, reconciler, report and positive interval")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return TenantReadinessObserved{}, fmt.Errorf("tenant readiness wait requires an execution deadline")
	}
	if err := ctx.Err(); err != nil {
		return TenantReadinessObserved{}, err
	}
	if err := validateTenantExpectation(expected); err != nil {
		return TenantReadinessObserved{}, fmt.Errorf("invalid retained tenant inputs")
	}
	if desiredDPUCluster.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" || desiredDPUCluster.GetKind() != "DPUCluster" || desiredDPUCluster.GetName() != expected.Name || desiredDPUCluster.GetNamespace() != expected.Namespace {
		return TenantReadinessObserved{}, fmt.Errorf("retained DPUCluster does not match the selected tenant identity")
	}
	if reconciler.BundleDigest() == "" || reconciler.BundleDigest() != journal.digest {
		return TenantReadinessObserved{}, fmt.Errorf("execution report and tenant reconciler must identify the same retained bundle")
	}
	desired := desiredDPUCluster.DeepCopy()
	event := executionEvent{
		Stage: generation.StageTenant, Operation: "tenant-readiness", Subject: "DPUCluster/" + expected.Namespace + "/" + expected.Name,
		State: "attempted", ReasonCode: "TENANT_READINESS_STARTED",
		Message: "Wait for the exact retained DPUCluster and its owned Kamaji sources; no tenant API connection or resource effect starts here.",
	}
	if err := journal.record(event); err != nil {
		return TenantReadinessObserved{}, err
	}
	last := TenantReadiness{State: TenantReadinessPending, ReasonCode: "TENANT_SOURCE_UNOBSERVED", Message: "Tenant controller sources have not yet reported readiness."}
	for {
		if err := ctx.Err(); err != nil {
			return TenantReadinessObserved{}, finishTenantWait(journal, expected, last, err)
		}
		current, err := resources.Get(ctx, desired.DeepCopy())
		if err != nil {
			if ctx.Err() != nil {
				return TenantReadinessObserved{}, finishTenantWait(journal, expected, last, ctx.Err())
			}
			if apierrors.IsNotFound(err) {
				return TenantReadinessObserved{}, failTenantWait(journal, expected, "TENANT_DPUCLUSTER_MISSING", "The selected DPUCluster disappeared; it will not be recreated by this wait.", fmt.Errorf("selected DPUCluster is missing"))
			}
			if retryableRead(err) {
				last = tenantPending("TENANT_DPUCLUSTER_READ_RETRY", "The management API is temporarily unavailable or throttled; selected DPUCluster readiness remains unverified.")
				if err := recordTenantWait(journal, expected, last, false); err != nil {
					return TenantReadinessObserved{}, err
				}
				if err := waitInterval(ctx, interval); err != nil {
					return TenantReadinessObserved{}, finishTenantWait(journal, expected, last, err)
				}
				continue
			}
			code := "TENANT_DPUCLUSTER_READ_FAILED"
			message := "The selected DPUCluster could not be read; no replacement will be created."
			if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
				code, message = "TENANT_DPUCLUSTER_READ_FORBIDDEN", "The caller cannot inspect the selected DPUCluster."
			}
			return TenantReadinessObserved{}, failTenantWait(journal, expected, code, message, safeAPIError("read selected DPUCluster", err))
		}
		if current == nil || current.GetUID() != establishedUID {
			return TenantReadinessObserved{}, failTenantWait(journal, expected, "TENANT_DPUCLUSTER_REPLACED", "The selected DPUCluster UID changed; inspect the replacement before continuing.", fmt.Errorf("selected DPUCluster identity changed"))
		}
		if current.GetDeletionTimestamp() != nil {
			return TenantReadinessObserved{}, failTenantWait(journal, expected, "TENANT_DPUCLUSTER_DELETING", "The selected DPUCluster is being deleted.", fmt.Errorf("selected DPUCluster is deleting"))
		}
		if err := reconciler.Verify(desired.DeepCopy(), current.DeepCopy()); err != nil {
			return TenantReadinessObserved{}, failTenantWait(journal, expected, tenantVerificationCode(err), "The selected DPUCluster no longer matches retained intent and ownership.", nil)
		}
		snapshot, err := sources.observeTenantSources(ctx, expected)
		if err != nil {
			if ctx.Err() != nil {
				return TenantReadinessObserved{}, finishTenantWait(journal, expected, last, ctx.Err())
			}
			if apierrors.IsNotFound(err) {
				last = tenantPending("TENANT_SOURCE_NOT_OBSERVED", "A required controller-owned tenant source has not been created yet.")
			} else if retryableRead(err) {
				last = tenantPending("TENANT_SOURCE_READ_RETRY", "The management API is temporarily unavailable or throttled; tenant readiness remains unverified.")
			} else {
				code, message := "TENANT_SOURCE_READ_FAILED", "A required tenant source could not be completely inspected."
				if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
					code, message = "TENANT_SOURCE_READ_FORBIDDEN", "The caller cannot inspect all required tenant sources."
				}
				return TenantReadinessObserved{}, failTenantWait(journal, expected, code, message, safeAPIError("observe tenant sources", err))
			}
			if err := recordTenantWait(journal, expected, last, false); err != nil {
				return TenantReadinessObserved{}, err
			}
			if err := waitInterval(ctx, interval); err != nil {
				return TenantReadinessObserved{}, finishTenantWait(journal, expected, last, err)
			}
			continue
		}
		if snapshot.observed.DPUCluster == nil || snapshot.observed.DPUCluster.GetUID() != establishedUID {
			return TenantReadinessObserved{}, failTenantWait(journal, expected, "TENANT_DPUCLUSTER_REPLACED", "The DPUCluster observed with tenant sources is missing or has a different UID.", fmt.Errorf("tenant source chain changed"))
		}
		if snapshot.observed.DPUCluster.GetDeletionTimestamp() != nil {
			return TenantReadinessObserved{}, failTenantWait(journal, expected, "TENANT_DPUCLUSTER_DELETING", "The selected DPUCluster is being deleted.", fmt.Errorf("selected DPUCluster is deleting"))
		}
		if err := reconciler.Verify(desired.DeepCopy(), snapshot.observed.DPUCluster.DeepCopy()); err != nil {
			return TenantReadinessObserved{}, failTenantWait(journal, expected, tenantVerificationCode(err), "The DPUCluster read with tenant sources no longer matches retained intent and ownership.", nil)
		}
		last = EvaluateTenantReadiness(expected, snapshot.observed)
		if last.State == TenantReadinessConflict || last.State == TenantReadinessInvalid {
			return TenantReadinessObserved{}, failTenantWait(journal, expected, last.ReasonCode, last.Message, fmt.Errorf("tenant source state is %s", last.State))
		}
		if !last.Ready {
			if err := recordTenantWait(journal, expected, last, false); err != nil {
				return TenantReadinessObserved{}, err
			}
			if err := waitInterval(ctx, interval); err != nil {
				return TenantReadinessObserved{}, finishTenantWait(journal, expected, last, err)
			}
			continue
		}
		// The sources may have converged while the selected DPUCluster changed.
		// Do one last fresh exact-UID and intent check before returning success.
		if err := ctx.Err(); err != nil {
			return TenantReadinessObserved{}, finishTenantWait(journal, expected, last, err)
		}
		final, err := resources.Get(ctx, desired.DeepCopy())
		if err != nil {
			if ctx.Err() != nil {
				return TenantReadinessObserved{}, finishTenantWait(journal, expected, last, ctx.Err())
			}
			if apierrors.IsNotFound(err) {
				return TenantReadinessObserved{}, failTenantWait(journal, expected, "TENANT_DPUCLUSTER_MISSING", "The selected DPUCluster disappeared before tenant readiness was confirmed.", fmt.Errorf("selected DPUCluster is missing"))
			}
			if retryableRead(err) {
				last = tenantPending("TENANT_DPUCLUSTER_READ_RETRY", "The final DPUCluster identity recheck was temporarily unavailable.")
				if err := recordTenantWait(journal, expected, last, false); err != nil {
					return TenantReadinessObserved{}, err
				}
				if err := waitInterval(ctx, interval); err != nil {
					return TenantReadinessObserved{}, finishTenantWait(journal, expected, last, err)
				}
				continue
			}
			return TenantReadinessObserved{}, failTenantWait(journal, expected, "TENANT_DPUCLUSTER_READ_FAILED", "The final DPUCluster identity recheck failed.", safeAPIError("recheck selected DPUCluster", err))
		}
		if final == nil || final.GetUID() != establishedUID || final.GetDeletionTimestamp() != nil {
			return TenantReadinessObserved{}, failTenantWait(journal, expected, "TENANT_DPUCLUSTER_REPLACED", "The DPUCluster changed before tenant readiness was confirmed.", fmt.Errorf("selected DPUCluster identity changed"))
		}
		if err := reconciler.Verify(desired.DeepCopy(), final.DeepCopy()); err != nil {
			return TenantReadinessObserved{}, failTenantWait(journal, expected, tenantVerificationCode(err), "The final DPUCluster read no longer matches retained intent and ownership.", nil)
		}
		if err := ctx.Err(); err != nil {
			return TenantReadinessObserved{}, finishTenantWait(journal, expected, last, err)
		}
		// The final read must still report convergence, not merely unchanged
		// identity and spec with an older successful status retained elsewhere.
		snapshot.observed.DPUCluster = final.DeepCopy()
		last = EvaluateTenantReadiness(expected, snapshot.observed)
		if last.State == TenantReadinessConflict || last.State == TenantReadinessInvalid {
			return TenantReadinessObserved{}, failTenantWait(journal, expected, last.ReasonCode, last.Message, nil)
		}
		if !last.Ready {
			if err := recordTenantWait(journal, expected, last, false); err != nil {
				return TenantReadinessObserved{}, err
			}
			if err := waitInterval(ctx, interval); err != nil {
				return TenantReadinessObserved{}, finishTenantWait(journal, expected, last, err)
			}
			continue
		}
		if err := recordTenantWait(journal, expected, last, true); err != nil {
			return TenantReadinessObserved{}, err
		}
		if err := ctx.Err(); err != nil {
			return TenantReadinessObserved{}, finishTenantWait(journal, expected, last, err)
		}
		return cloneTenantReadinessObserved(snapshot.observed), nil
	}
}

func tenantPending(code, message string) TenantReadiness {
	return TenantReadiness{State: TenantReadinessPending, ReasonCode: code, Message: message}
}

func recordTenantWait(journal *executionJournal, expected TenantReadinessExpected, result TenantReadiness, terminal bool) error {
	state := "waiting"
	if terminal {
		state = "ready"
		if !result.Ready {
			state = "failed"
		}
	}
	return journal.record(executionEvent{
		Stage: generation.StageTenant, Operation: "tenant-readiness", Subject: "DPUCluster/" + expected.Namespace + "/" + expected.Name,
		State: state, ReasonCode: result.ReasonCode, Message: result.Message,
	})
}

func failTenantWait(journal *executionJournal, expected TenantReadinessExpected, code, message string, cause error) error {
	readiness := TenantReadiness{State: TenantReadinessConflict, ReasonCode: code, Message: message}
	recordErr := recordTenantWait(journal, expected, readiness, true)
	if cause == nil {
		cause = fmt.Errorf("%s", code)
	}
	return errors.Join(recordErr, cause)
}

func finishTenantWait(journal *executionJournal, expected TenantReadinessExpected, last TenantReadiness, cause error) error {
	code, message := "TENANT_READINESS_TIMEOUT", "Tenant readiness was not established before the execution deadline."
	if errors.Is(cause, context.Canceled) {
		code, message = "TENANT_READINESS_CANCELED", "The tenant readiness wait was canceled."
	}
	if last.ReasonCode != "" {
		message += " Last check: " + last.ReasonCode + "."
	}
	readiness := TenantReadiness{State: TenantReadinessConflict, ReasonCode: code, Message: message}
	recordErr := recordTenantWait(journal, expected, readiness, true)
	return errors.Join(recordErr, fmt.Errorf("%s: %w", code, cause))
}

func tenantVerificationCode(err error) string {
	var diagnostic *ReconciliationError
	if errors.As(err, &diagnostic) && diagnostic != nil && diagnostic.ReasonCode != "" {
		return diagnostic.ReasonCode
	}
	return "TENANT_DPUCLUSTER_INTENT_CHANGED"
}

func cloneTenantReadinessObserved(in TenantReadinessObserved) TenantReadinessObserved {
	out := in
	if in.DPUCluster != nil {
		out.DPUCluster = in.DPUCluster.DeepCopy()
	}
	if in.TenantControlPlane != nil {
		out.TenantControlPlane = in.TenantControlPlane.DeepCopy()
	}
	if in.Service != nil {
		out.Service = in.Service.DeepCopy()
	}
	out.Keepalived = make([]*unstructured.Unstructured, len(in.Keepalived))
	for index, object := range in.Keepalived {
		if object != nil {
			out.Keepalived[index] = object.DeepCopy()
		}
	}
	return out
}
