// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"encoding/json"
	"fmt"

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
		if err := validateOCPBoundOwner(desired, live); err != nil {
			return err
		}
		if live.GetAnnotations()[ocpCleanRetainAnnotation] != "" ||
			(live.GetAnnotations()[ocpCleanOwnerAnnotation] == "" && validateOCPCleanLiveOwnership(ctx, c, desired, live) != nil) {
			// Historical deploy permits adoption. Preserve that behavior but
			// never turn an ambiguous adoption into later deletion permission.
			annotations := desired.GetAnnotations()
			annotations[ocpCleanRetainAnnotation] = "true"
			desired.SetAnnotations(annotations)
		}
	}
	annotations := desired.GetAnnotations()
	annotations[ocpCleanUIDAnnotation] = string(live.GetUID())
	desired.SetAnnotations(annotations)
	desired.SetUID(live.GetUID())
	desired.SetResourceVersion(live.GetResourceVersion())
	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(desired), opts...); err != nil {
		return err
	}
	obj.Object = desired.Object
	return nil
}

func bindOCPCreatedUID(ctx context.Context, c client.Client, created *unstructured.Unstructured) error {
	createdUID, owner := created.GetUID(), created.GetAnnotations()[ocpCleanOwnerAnnotation]
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(created.GroupVersionKind())
		if err := c.Get(ctx, client.ObjectKeyFromObject(created), current); err != nil {
			return err
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
