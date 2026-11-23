// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// applyOCPUnstructured preserves ordinary SSA updates while binding cleanup
// permission to an actual object incarnation. Only the OCP deployment adapter
// calls this function; input annotations cannot select it on Kubernetes.
func applyOCPUnstructured(ctx context.Context, c client.Client, obj *unstructured.Unstructured, dryRun bool) error {
	if obj == nil || c == nil {
		return fmt.Errorf("OpenShift apply requires an object and client")
	}
	opts := []client.ApplyOption{client.FieldOwner("l8k"), client.ForceOwnership}
	if dryRun {
		opts = append(opts, client.DryRunAll)
	}
	if !isOCPCleanKind(obj.GroupVersionKind()) || obj.GetAnnotations()[ocpCleanOwnerAnnotation] == "" {
		return c.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), opts...)
	}
	desired := obj.DeepCopy()
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(obj.GroupVersionKind())
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), live)
	if apierrors.IsNotFound(err) {
		if dryRun {
			// Preserve SSA admission/validation semantics. No marker is
			// persisted, so dry-run needs no object-incarnation binding.
			return c.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), opts...)
		}
		// Create is atomic. A competing creator causes AlreadyExists instead
		// of force-applying our ownership marker to its object.
		created := desired.DeepCopy()
		createOpts := []client.CreateOption{client.FieldOwner("l8k")}
		if err := c.Create(ctx, created, createOpts...); err != nil {
			return fmt.Errorf("create OpenShift resource without adoption: %w", err)
		}
		if created.GetUID() == "" || created.GetResourceVersion() == "" {
			return fmt.Errorf("created OpenShift resource has no UID/resourceVersion; inspect the partial deployment before retrying")
		}
		// An ambiguous patch result is deliberately not replayed. The owner
		// marker without a UID binding blocks subsequent apply and cleanup.
		if err := bindOCPCreatedUID(ctx, c, created); err != nil {
			return fmt.Errorf("bind created OpenShift resource UID (inspect partial deployment before retrying): %w", err)
		}
		live = created
	} else if err != nil {
		return fmt.Errorf("inspect OpenShift apply target: %w", err)
	} else {
		if live.GetUID() == "" || live.GetResourceVersion() == "" {
			return fmt.Errorf("existing OpenShift resource has no UID/resourceVersion")
		}
	}
	return applyPinnedOCPWithRetry(ctx, c, obj, desired, live, opts)
}

// A resourceVersion conflict can come solely from a controller status write.
// Refresh that version without widening the original authorization: incarnation,
// binding, spec and spec ownership must remain the same throughout the retry.
func applyPinnedOCPWithRetry(ctx context.Context, c client.Client, obj, desired, original *unstructured.Unstructured, opts []client.ApplyOption) error {
	if original.GetDeletionTimestamp() != nil {
		return fmt.Errorf("OpenShift apply target is terminating; refusing to apply its pinned incarnation")
	}
	originalOwners, err := ocpApplySpecOwners(original)
	if err != nil {
		return err
	}
	retained := desired.GetAnnotations()[ocpCleanRetainAnnotation] != "" || original.GetAnnotations()[ocpCleanRetainAnnotation] != ""
	if original.GetAnnotations()[ocpCleanOwnerAnnotation] == "" && validateOCPCleanLiveOwnership(ctx, c, desired, original) != nil {
		retained = true
	}
	applyConflict := false
	return retry.OnError(retry.DefaultRetry, func(err error) bool {
		return applyConflict && apierrors.IsConflict(err)
	}, func() error {
		applyConflict = false
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(original.GroupVersionKind())
		if err := c.Get(ctx, client.ObjectKeyFromObject(original), current); err != nil {
			return fmt.Errorf("refresh pinned OpenShift apply target: %w", err)
		}
		if current.GetDeletionTimestamp() != nil {
			return fmt.Errorf("OpenShift apply target is terminating; refusing to apply its pinned incarnation")
		}
		if current.GetUID() != original.GetUID() || current.GetResourceVersion() == "" {
			return fmt.Errorf("OpenShift apply target was replaced or has no resourceVersion; refusing to follow another incarnation")
		}
		if err := validateOCPBoundOwner(desired, current); err != nil {
			return err
		}
		for _, key := range []string{ocpCleanOwnerAnnotation, ocpCleanUIDAnnotation} {
			if current.GetAnnotations()[key] != original.GetAnnotations()[key] {
				return fmt.Errorf("OpenShift apply ownership binding changed while applying the pinned object")
			}
		}
		owners, err := ocpApplySpecOwners(current)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current.Object["spec"], original.Object["spec"]) ||
			!reflect.DeepEqual(owners, originalOwners) || !reflect.DeepEqual(current.GetOwnerReferences(), original.GetOwnerReferences()) {
			return fmt.Errorf("OpenShift apply target spec or its ownership changed; inspect the concurrent change before retrying")
		}
		if current.GetAnnotations()[ocpCleanRetainAnnotation] != "" ||
			(current.GetAnnotations()[ocpCleanOwnerAnnotation] == "" && validateOCPCleanLiveOwnership(ctx, c, desired, current) != nil) {
			// Historical adoption remains permitted. Once ambiguous, it must
			// never acquire cleanup permission on a subsequent retry.
			retained = true
		}
		// Apply may mutate its input even on error. Each attempt starts with
		// independent retained intent and a newly observed resourceVersion.
		candidate := desired.DeepCopy()
		annotations := candidate.GetAnnotations()
		annotations[ocpCleanUIDAnnotation] = string(original.GetUID())
		if retained {
			annotations[ocpCleanRetainAnnotation] = "true"
		}
		candidate.SetAnnotations(annotations)
		candidate.SetUID(original.GetUID())
		candidate.SetResourceVersion(current.GetResourceVersion())
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(candidate), opts...); err != nil {
			applyConflict = apierrors.IsConflict(err)
			return err
		}
		obj.Object = candidate.Object
		return nil
	})
}

// Ignore timestamps, status ownership and metadata-only ownership changes, while
// comparing all managers' exact spec claims independently of entry ordering.
func ocpApplySpecOwners(obj *unstructured.Unstructured) ([]string, error) {
	var owners []string
	for _, entry := range obj.GetManagedFields() {
		if entry.Subresource != "" || entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(entry.FieldsV1.GetRawBytes(), &fields); err != nil {
			return nil, fmt.Errorf("inspect OpenShift apply field ownership: %w", err)
		}
		spec, present := fields["f:spec"]
		if !present {
			continue
		}
		encoded, err := json.Marshal([]any{entry.Manager, entry.Operation, entry.APIVersion, entry.FieldsType, spec})
		if err != nil {
			return nil, err
		}
		owners = append(owners, string(encoded))
	}
	sort.Strings(owners)
	return owners, nil
}

func bindOCPCreatedUID(ctx context.Context, c client.Client, created *unstructured.Unstructured) error {
	createdUID, owner := created.GetUID(), created.GetAnnotations()[ocpCleanOwnerAnnotation]
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(created.GroupVersionKind())
		if err := c.Get(ctx, client.ObjectKeyFromObject(created), current); err != nil {
			return err
		}
		if current.GetDeletionTimestamp() != nil {
			return fmt.Errorf("created OpenShift apply target is terminating; refusing UID binding")
		}
		annotations := current.GetAnnotations()
		if current.GetUID() != createdUID || annotations[ocpCleanOwnerAnnotation] != owner || annotations[ocpCleanRetainAnnotation] != "" {
			return fmt.Errorf("created OpenShift object was replaced or its ownership changed before UID binding")
		}
		if bound := annotations[ocpCleanUIDAnnotation]; bound != "" {
			if bound != string(createdUID) {
				return fmt.Errorf("created OpenShift object acquired a different UID binding")
			}
			created.Object = current.Object
			return nil
		}
		// Explicit UID and resourceVersion constrain this metadata-only merge.
		// Status writes can advance resourceVersion immediately after Create;
		// retry only a conclusive conflict while retaining the original UID.
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
			"uid": string(createdUID), "resourceVersion": current.GetResourceVersion(),
			"annotations": map[string]any{ocpCleanUIDAnnotation: string(createdUID)},
		}})
		if err != nil {
			return err
		}
		if err := c.Patch(ctx, current, client.RawPatch(types.MergePatchType, patch), client.FieldOwner("l8k")); err != nil {
			return err
		}
		created.Object = current.Object
		return nil
	})
}
