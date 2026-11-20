// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	BundleDigestAnnotation   = "nvidia.kubernetes-launch-kit.dpf-bundle-digest"
	IntentDigestAnnotation   = "nvidia.kubernetes-launch-kit.dpf-object-digest"
	maxOwnershipDigestLength = len("sha256:") + 64
)

// ReconciliationError carries a bounded, safe diagnostic while preserving
// causes for errors.Is/As classification.
type ReconciliationError struct {
	ReasonCode string
	Message    string
	cause      error
}

func (e *ReconciliationError) Error() string {
	if e == nil {
		return "DPF resource reconciliation failed"
	}
	return e.ReasonCode + ": " + e.Message
}

func (e *ReconciliationError) Unwrap() error { return e.cause }

func reconcileError(code, message string, cause error) error {
	return &ReconciliationError{ReasonCode: code, Message: message, cause: cause}
}

// ResourceClient is the narrow portion of APIClient used by create-or-verify.
// Create must not update or adopt an existing object.
type ResourceClient interface {
	Get(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error)
	Create(context.Context, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error)
}

type Disposition string

const (
	DispositionCreated           Disposition = "created"
	DispositionExisting          Disposition = "existing"
	DispositionExistingAfterRace Disposition = "existing-after-create-race"
	DispositionUncertain         Disposition = "uncertain"
)

// Reconciler verifies DPF objects against the CRD schemas retained from the
// selected operator chart and one immutable bundle identity.
type Reconciler struct {
	*desiredVerifier
}

// newReconciler binds explicitly selected immutable rules and metadata.
func newReconciler(rules *controllercontract.Rules, bundleDigest string, policy controllercontract.MetadataPolicy) (*Reconciler, error) {
	verifier, err := newDesiredVerifier(rules, bundleDigest, policy)
	if err != nil {
		return nil, err
	}
	return &Reconciler{desiredVerifier: verifier}, nil
}

// BundleDigest returns the immutable bundle identity bound to this reconciler.
func (r *Reconciler) BundleDigest() string {
	if r == nil {
		return ""
	}
	return r.desiredVerifier.BundleDigest()
}

// Ensure reads before creating. AlreadyExists races are resolved by a fresh
// read and the same verifier; no update, patch, adoption, or delete is issued.
// If Create succeeds but response verification fails, DispositionCreated and
// the returned object identify a possible partial write; callers must not retry
// this as an unobserved create.
func (r *Reconciler) Ensure(ctx context.Context, client ResourceClient, desired *unstructured.Unstructured) (*unstructured.Unstructured, Disposition, error) {
	if ctx == nil || client == nil || r == nil {
		return nil, "", fmt.Errorf("context, reconciler, and resource client are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	marked, err := r.markedDesired(desired)
	if err != nil {
		return nil, "", reconcileError("DESIRED_OBJECT_INVALID", "desired DPF resource failed validation", err)
	}
	current, err := client.Get(ctx, desired.DeepCopy())
	if err == nil {
		if err := r.Verify(desired, current); err != nil {
			return current, DispositionExisting, err
		}
		return current, DispositionExisting, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, "", reconcileError("RESOURCE_READ_FAILED", "cannot read the existing DPF resource", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	created, createErr := client.Create(ctx, marked, false)
	if createErr != nil {
		if !apierrors.IsAlreadyExists(createErr) {
			return nil, DispositionUncertain, reconcileError("RESOURCE_CREATE_UNCERTAIN", "create returned an error; the API may have committed the resource, re-read before retry", createErr)
		}
		current, err = client.Get(ctx, desired.DeepCopy())
		if err != nil {
			return nil, DispositionExistingAfterRace, reconcileError("RESOURCE_RACE_READ_FAILED", "cannot re-read the resource after a create race", err)
		}
		if err := r.Verify(desired, current); err != nil {
			return current, DispositionExistingAfterRace, reconcileError("RESOURCE_CONFLICT", "resource created concurrently does not match the retained bundle", err)
		}
		return current, DispositionExistingAfterRace, nil
	}
	if created == nil {
		return nil, DispositionUncertain, reconcileError("RESOURCE_CREATE_RESPONSE_MISSING", "create succeeded without an object response; re-read before retry", nil)
	}
	if err := r.Verify(desired, created); err != nil {
		return created, DispositionCreated, reconcileError("RESOURCE_POST_CREATE_CONFLICT", postCreateConflictMessage(err), err)
	}
	return created, DispositionCreated, nil
}

// Verify uses the same pure desired/live comparison as read-only observation.
func (r *Reconciler) Verify(desired, current *unstructured.Unstructured) error {
	if r == nil {
		return reconcileError("RECONCILER_INVALID", "reconciler is required", nil)
	}
	return r.desiredVerifier.Verify(desired, current)
}

// Keep post-create ambiguity while naming only a verifier-owned finite category.
// Messages, causes and arbitrary error strings may contain private API content.
func postCreateConflictMessage(err error) string {
	const prefix = "resource was created but response verification failed"
	const suffix = "; re-read before retry"
	var verification *ReconciliationError
	if errors.As(err, &verification) && verification != nil {
		switch verification.ReasonCode {
		case "RECONCILER_INVALID", "DESIRED_OBJECT_INVALID", "RESOURCE_IDENTITY_MISMATCH", "RESOURCE_UNAVAILABLE",
			"RESOURCE_OWNERSHIP_CONFLICT", "RESOURCE_METADATA_CONFLICT", "RESOURCE_SCHEMA_INVALID", "RESOURCE_CONTENT_CONFLICT":
			return prefix + " (" + verification.ReasonCode + ")" + suffix
		}
	}
	return prefix + suffix
}

func objectDigest(object *unstructured.Unstructured) (string, error) {
	data, err := json.Marshal(object.Object)
	if err != nil {
		return "", fmt.Errorf("encode desired object for ownership digest: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validSHA256Digest(value string) bool {
	if len(value) != maxOwnershipDigestLength || !strings.HasPrefix(value, "sha256:") || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func jsonEqual(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && string(left) == string(right)
}
