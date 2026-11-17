// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	enrollmentSecretName      = "bmc-shared-password"
	enrollmentSecretDataKey   = "password"
	enrollmentSecretType      = "Opaque"
	maxEnrollmentSecretBytes  = 1 << 20
	enrollmentSecretOperation = "credential-secret"
)

// enrollmentSecretReceipt contains only a stable reference to the shared BMC
// credential. Secret data is never returned to the caller or written to the
// execution journal.
type enrollmentSecretReceipt struct {
	Name            string
	Namespace       string
	UID             types.UID
	ResourceVersion string
	Created         bool
}

// executeEnrollmentSecret creates the shared DPF BMC credential when it is
// absent, or reuses an existing exact credential without changing ownership or
// metadata. A failed Create is uncertain and must be inspected before retry.
func executeEnrollmentSecret(ctx context.Context, client ResourceClient, namespace, password string, journal *executionJournal) (enrollmentSecretReceipt, error) {
	if ctx == nil || client == nil || journal == nil || len(validation.IsDNS1123Label(namespace)) != 0 || password == "" || len(password) > maxEnrollmentSecretBytes {
		return enrollmentSecretReceipt{}, fmt.Errorf("enrollment credential requires a bounded context, valid namespace, nonempty bounded password, client, and execution report")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return enrollmentSecretReceipt{}, fmt.Errorf("enrollment credential requires an execution deadline")
	}
	if err := ctx.Err(); err != nil {
		return enrollmentSecretReceipt{}, err
	}
	if !validSHA256Digest(journal.digest) {
		return enrollmentSecretReceipt{}, fmt.Errorf("enrollment credential report must identify a valid bundle digest")
	}

	event := executionEvent{
		Stage: generation.StageEnrollment, Operation: enrollmentSecretOperation,
		Subject: namespace + "/" + enrollmentSecretName, State: "attempted",
		ReasonCode: "ENROLLMENT_SECRET_INSPECTION_STARTED",
		Message:    "Inspect the selected namespace for the exact shared DPF BMC credential.",
	}
	if err := journal.record(event); err != nil {
		return enrollmentSecretReceipt{}, err
	}

	current, err := client.Get(ctx, secretIdentity(namespace))
	if err == nil {
		receipt, verifyErr := verifyEnrollmentSecret(current, namespace, password)
		if verifyErr != nil {
			return enrollmentSecretReceipt{}, enrollmentSecretFailure(journal, event, false, "ENROLLMENT_SECRET_CONFLICT", "An existing shared BMC credential does not match the required Secret contract; it was not modified.", verifyErr)
		}
		if err := ctx.Err(); err != nil {
			return enrollmentSecretReceipt{}, enrollmentSecretFailure(journal, event, false, "ENROLLMENT_SECRET_INSPECTION_CANCELED", "Credential inspection did not complete before cancellation.", err)
		}
		event.State, event.ReasonCode = "confirmed", "ENROLLMENT_SECRET_REUSED"
		event.Message = "The existing exact shared BMC credential was reused without adoption or modification."
		if err := journal.record(event); err != nil {
			return enrollmentSecretReceipt{}, err
		}
		return receipt, nil
	}
	if ctx.Err() != nil {
		return enrollmentSecretReceipt{}, enrollmentSecretFailure(journal, event, false, "ENROLLMENT_SECRET_INSPECTION_CANCELED", "Credential inspection did not complete before cancellation.", ctx.Err())
	}
	if !apierrors.IsNotFound(err) {
		return enrollmentSecretReceipt{}, enrollmentSecretFailure(journal, event, false, "ENROLLMENT_SECRET_READ_FAILED", "The shared BMC credential could not be inspected; no Secret was created.", err)
	}

	if err := ctx.Err(); err != nil {
		return enrollmentSecretReceipt{}, enrollmentSecretFailure(journal, event, false, "ENROLLMENT_SECRET_CREATE_CANCELED", "Credential creation was canceled before the create request.", err)
	}
	event.ReasonCode, event.Message = "ENROLLMENT_SECRET_CREATE_STARTED", "Create the missing shared DPF BMC credential; the password is not retained in the plan or report."
	if err := journal.record(event); err != nil {
		return enrollmentSecretReceipt{}, err
	}
	if err := ctx.Err(); err != nil {
		return enrollmentSecretReceipt{}, enrollmentSecretFailure(journal, event, false, "ENROLLMENT_SECRET_CREATE_CANCELED", "Credential creation was canceled before the create request.", err)
	}
	desired := enrollmentSecret(namespace, journal.digest, password)
	created, createErr := client.Create(ctx, desired, false)
	if createErr != nil {
		if apierrors.IsAlreadyExists(createErr) {
			return enrollmentSecretRace(ctx, client, namespace, password, journal, event)
		}
		return enrollmentSecretReceipt{}, enrollmentSecretFailure(journal, event, true, "ENROLLMENT_SECRET_CREATE_UNCERTAIN", "Credential creation returned an error; inspect the Secret before retrying.", createErr)
	}
	receipt, verifyErr := verifyEnrollmentSecret(created, namespace, password)
	if verifyErr != nil {
		return enrollmentSecretReceipt{}, enrollmentSecretFailure(journal, event, true, "ENROLLMENT_SECRET_CREATE_UNCERTAIN", "Credential creation returned no verifiable Secret identity; inspect the Secret before retrying.", verifyErr)
	}
	markedDigest, found, markErr := unstructured.NestedString(created.Object, "metadata", "annotations", BundleDigestAnnotation)
	if markErr != nil || !found || markedDigest != journal.digest {
		return enrollmentSecretReceipt{}, enrollmentSecretFailure(journal, event, true, "ENROLLMENT_SECRET_CREATE_UNCERTAIN", "Credential creation did not preserve its bundle ownership marker; inspect the Secret before retrying.", fmt.Errorf("created Secret ownership marker is unavailable"))
	}
	receipt.Created = true
	event.State, event.ReasonCode = "confirmed", "ENROLLMENT_SECRET_CREATED"
	event.Message = "The shared DPF BMC credential was created and its Secret identity verified."
	if err := journal.record(event); err != nil {
		return enrollmentSecretReceipt{}, err
	}
	if err := ctx.Err(); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func enrollmentSecret(namespace, bundleDigest, password string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": enrollmentSecretName, "namespace": namespace,
			"annotations": map[string]any{BundleDigestAnnotation: bundleDigest},
		},
		"type": enrollmentSecretType,
		"data": map[string]any{enrollmentSecretDataKey: base64.StdEncoding.EncodeToString([]byte(password))},
	}}
}

func secretIdentity(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": enrollmentSecretName, "namespace": namespace},
	}}
}

func verifyEnrollmentSecret(secret *unstructured.Unstructured, namespace, password string) (enrollmentSecretReceipt, error) {
	if secret == nil || secret.GetAPIVersion() != "v1" || secret.GetKind() != "Secret" || secret.GetName() != enrollmentSecretName || secret.GetNamespace() != namespace || secret.GetUID() == "" || secret.GetResourceVersion() == "" || secret.GetDeletionTimestamp() != nil {
		return enrollmentSecretReceipt{}, fmt.Errorf("secret identity is missing, deleting, or does not match the selected namespace")
	}
	owners, ownersFound, ownersErr := unstructured.NestedSlice(secret.Object, "metadata", "ownerReferences")
	if ownersErr != nil || ownersFound && len(owners) != 0 {
		return enrollmentSecretReceipt{}, fmt.Errorf("shared credential Secret has an unexpected owner")
	}
	secretType, found, err := unstructured.NestedString(secret.Object, "type")
	if err != nil || !found || secretType != enrollmentSecretType {
		return enrollmentSecretReceipt{}, fmt.Errorf("shared credential Secret type is not Opaque")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(secret.Object, "stringData"); found {
		return enrollmentSecretReceipt{}, fmt.Errorf("shared credential Secret contains unexpected stringData")
	}
	data, found, err := unstructured.NestedStringMap(secret.Object, "data")
	if err != nil || !found || len(data) != 1 {
		return enrollmentSecretReceipt{}, fmt.Errorf("shared credential Secret data keys do not match the required contract")
	}
	encoded, found := data[enrollmentSecretDataKey]
	if !found {
		return enrollmentSecretReceipt{}, fmt.Errorf("shared credential Secret password key is missing")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || string(decoded) != password {
		return enrollmentSecretReceipt{}, fmt.Errorf("shared credential Secret password does not match the selected credential")
	}
	return enrollmentSecretReceipt{Name: secret.GetName(), Namespace: secret.GetNamespace(), UID: secret.GetUID(), ResourceVersion: secret.GetResourceVersion()}, nil
}

func enrollmentSecretRace(ctx context.Context, client ResourceClient, namespace, password string, journal *executionJournal, event executionEvent) (enrollmentSecretReceipt, error) {
	if err := ctx.Err(); err != nil {
		return enrollmentSecretReceipt{}, enrollmentSecretFailure(journal, event, false, "ENROLLMENT_SECRET_RACE_UNVERIFIED", "A concurrent credential create was reported, but its result could not be verified before cancellation.", err)
	}
	current, err := client.Get(ctx, secretIdentity(namespace))
	if err != nil {
		return enrollmentSecretReceipt{}, enrollmentSecretFailure(journal, event, false, "ENROLLMENT_SECRET_RACE_UNVERIFIED", "A concurrent credential create was reported, but the Secret could not be verified.", err)
	}
	receipt, err := verifyEnrollmentSecret(current, namespace, password)
	if err != nil {
		return enrollmentSecretReceipt{}, enrollmentSecretFailure(journal, event, false, "ENROLLMENT_SECRET_RACE_CONFLICT", "A concurrent credential Secret does not match the required contract; it was not modified.", err)
	}
	if err := ctx.Err(); err != nil {
		return enrollmentSecretReceipt{}, enrollmentSecretFailure(journal, event, false, "ENROLLMENT_SECRET_RACE_UNVERIFIED", "A concurrent credential create was reported, but verification was canceled.", err)
	}
	event.State, event.ReasonCode = "confirmed", "ENROLLMENT_SECRET_REUSED_AFTER_RACE"
	event.Message = "A concurrent exact shared BMC credential was verified and reused without modification."
	if err := journal.record(event); err != nil {
		return enrollmentSecretReceipt{}, err
	}
	return receipt, nil
}

func enrollmentSecretFailure(journal *executionJournal, event executionEvent, uncertain bool, code, message string, cause error) error {
	event.State, event.ReasonCode, event.Message = "failed", code, message
	if uncertain {
		event.State = "uncertain"
	}
	var safeCause error
	if cause != nil {
		switch {
		case errors.Is(cause, context.Canceled):
			safeCause = context.Canceled
		case errors.Is(cause, context.DeadlineExceeded):
			safeCause = context.DeadlineExceeded
		default:
			safeCause = safeAPIError("enrollment credential", cause)
		}
	}
	return errors.Join(journal.record(event), reconcileError(code, message, safeCause))
}
