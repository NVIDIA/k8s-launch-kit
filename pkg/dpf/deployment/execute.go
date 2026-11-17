// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type nativeObjectReconciler interface {
	BundleDigest() string
	Ensure(context.Context, ResourceClient, *unstructured.Unstructured) (*unstructured.Unstructured, Disposition, error)
	Verify(*unstructured.Unstructured, *unstructured.Unstructured) error
}

// executeNativeObject joins durable attempt recording, create-or-verify, and
// current-intent readiness. It is a stage building block, not a deployment
// success predicate. Its caller owns prerequisite checks and the additional
// tenant, selected-device, artifact and data-plane barriers.
func executeNativeObject(ctx context.Context, client ResourceClient, reconciler nativeObjectReconciler,
	desired *unstructured.Unstructured, stage string, journal *executionJournal, wait bool, interval time.Duration,
) (*unstructured.Unstructured, error) {
	if ctx == nil || client == nil || reconciler == nil || desired == nil || stage == "" || journal == nil {
		return nil, fmt.Errorf("native execution requires context, client, reconciler, desired resource, stage and report")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reconciler.BundleDigest() == "" || reconciler.BundleDigest() != journal.digest {
		return nil, fmt.Errorf("execution report and reconciler must identify the same retained bundle")
	}
	if _, bounded := ctx.Deadline(); !bounded || interval <= 0 {
		return nil, fmt.Errorf("native execution requires a deadline and positive polling interval")
	}
	object := desired.DeepCopy()
	event := executionEvent{
		Stage: stage, Operation: "create-or-verify", Subject: object.GetKind() + "/" + object.GetNamespace() + "/" + object.GetName(),
		State: "attempted", ReasonCode: "RESOURCE_RECONCILIATION_STARTED",
		Message: "Inspect the exact desired resource and create it only if absent. An interrupted attempt requires fresh inspection.",
	}
	if err := journal.record(event); err != nil {
		return nil, err
	}
	current, disposition, err := reconciler.Ensure(ctx, client, object.DeepCopy())
	if err != nil {
		event.State = "failed"
		if disposition == DispositionUncertain || disposition == DispositionCreated {
			event.State = "uncertain"
		}
		event.ReasonCode = "RESOURCE_RECONCILIATION_FAILED"
		event.Message = "The operation did not complete. Resources may have been created; inspect live state before retrying."
		var diagnostic *ReconciliationError
		if errors.As(err, &diagnostic) && diagnostic != nil {
			event.ReasonCode, event.Message = diagnostic.ReasonCode, diagnostic.Message
			return nil, errors.Join(journal.record(event), diagnostic)
		}
		return nil, errors.Join(journal.record(event), safeAPIError("reconcile desired resource", err))
	}
	if current == nil || current.GetUID() == "" {
		event.State, event.ReasonCode = "uncertain", "RESOURCE_RECEIPT_INCOMPLETE"
		event.Message = "The operation returned no established resource identity; no dependent operation may continue."
		return nil, errors.Join(journal.record(event), fmt.Errorf("resource receipt lacks an established identity"))
	}
	event.State, event.ReasonCode = "confirmed", "RESOURCE_INTENT_VERIFIED"
	switch disposition {
	case DispositionCreated:
		event.Message = "The resource was created and its returned intent and ownership were verified."
	case DispositionExisting, DispositionExistingAfterRace:
		event.Message = "The existing resource matches this bundle's intent and ownership."
	default:
		event.State, event.ReasonCode = "uncertain", "RESOURCE_RECEIPT_INVALID"
		event.Message = "The operation returned an unsupported result; no dependent operation may continue."
		return nil, errors.Join(journal.record(event), fmt.Errorf("unsupported reconciliation result"))
	}
	if err := journal.record(event); err != nil {
		return nil, err
	}
	if !wait {
		return current.DeepCopy(), nil
	}
	return waitNativeReady(ctx, client, object, current.GetUID(), reconciler.Verify, interval, func(state Readiness, terminal bool) error {
		event.Operation, event.State = "readiness", "waiting"
		if terminal {
			event.State = "failed"
		}
		if terminal && state.Ready {
			event.State = "ready"
		}
		event.ReasonCode, event.Message = state.ReasonCode, state.Message
		return journal.record(event)
	})
}
