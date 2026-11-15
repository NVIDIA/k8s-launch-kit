// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

// executeNamespace establishes shared infrastructure without claiming ownership
// of an existing namespace or changing its labels, annotations, or policy.
// Later stages must continue to check this UID; a recreated namespace is a new
// boundary even if it has the same name.
func executeNamespace(ctx context.Context, client ResourceClient, name string, journal *executionJournal) (*unstructured.Unstructured, error) {
	if ctx == nil || client == nil || journal == nil || len(validation.IsDNS1123Label(name)) != 0 {
		return nil, fmt.Errorf("namespace execution requires context, client, report and a valid namespace name")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return nil, fmt.Errorf("namespace execution requires an execution deadline")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wanted := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": name},
	}}
	event := executionEvent{
		Stage: "management-namespaces", Operation: "create-or-verify", Subject: "Namespace/" + name,
		State: "attempted", ReasonCode: "NAMESPACE_INSPECTION_STARTED",
		Message: "Inspect the shared namespace and create it only if absent.",
	}
	if err := journal.record(event); err != nil {
		return nil, err
	}
	current, err := client.Get(ctx, wanted.DeepCopy())
	created := false
	if apierrors.IsNotFound(err) {
		if err := ctx.Err(); err != nil {
			return nil, namespaceFailure(journal, event, false, "NAMESPACE_EXECUTION_CANCELLED", "Namespace execution was cancelled before creation.", err)
		}
		current, err = client.Create(ctx, wanted.DeepCopy(), false)
		switch {
		case err == nil:
			created = true
		case apierrors.IsAlreadyExists(err):
			// A shared namespace created concurrently does not need adoption.
			current, err = client.Get(ctx, wanted.DeepCopy())
		default:
			return nil, namespaceFailure(journal, event, true, "NAMESPACE_CREATE_UNCERTAIN", "Namespace creation did not return a verified result; inspect live state before retrying.", err)
		}
	}
	if err != nil {
		return nil, namespaceFailure(journal, event, false, "NAMESPACE_READ_FAILED", "The shared namespace could not be read; no dependent operation may continue.", err)
	}
	if err := verifyNamespace(name, current); err != nil {
		return nil, namespaceFailure(journal, event, created, "NAMESPACE_NOT_USABLE", "The namespace lacks a stable identity, is not active, or is being deleted.", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, namespaceFailure(journal, event, created, "NAMESPACE_EXECUTION_CANCELLED", "Namespace execution was cancelled before completion.", err)
	}
	event.State, event.ReasonCode = "confirmed", "NAMESPACE_ACTIVE"
	event.Message = "The shared namespace is active; its existing metadata and policies were preserved."
	if created {
		event.Message = "The missing namespace was created and its active identity verified."
	}
	if err := journal.record(event); err != nil {
		return nil, err
	}
	return current.DeepCopy(), nil
}

func verifyNamespace(name string, current *unstructured.Unstructured) error {
	if current == nil || current.GetAPIVersion() != "v1" || current.GetKind() != "Namespace" || current.GetName() != name || current.GetNamespace() != "" || current.GetUID() == "" || current.GetResourceVersion() == "" || current.GetDeletionTimestamp() != nil {
		return fmt.Errorf("shared namespace identity is unavailable or invalid")
	}
	phase, found, err := unstructured.NestedString(current.Object, "status", "phase")
	if err != nil || !found || phase != "Active" {
		return fmt.Errorf("shared namespace is not active")
	}
	return nil
}

func namespaceFailure(journal *executionJournal, event executionEvent, uncertain bool, code, message string, cause error) error {
	event.State, event.ReasonCode, event.Message = "failed", code, message
	if uncertain {
		event.State = "uncertain"
	}
	return errors.Join(journal.record(event), reconcileError(code, message, safeAPIError("establish namespace", cause)))
}
