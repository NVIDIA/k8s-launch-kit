// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const ocpOperationManager = "l8k-ocp-operation"

func ocpOperationName(owner string) string {
	digest := sha256.Sum256([]byte(owner))
	return fmt.Sprintf("l8k-ocp-operation-%x", digest[:12])
}

// A create-only lock fences new l8k deployments and cleanup of the same logical
// deployment. There is no expiry or takeover: a crashed process leaves explicit
// recovery work, rather than allowing a paused process to resume after stealing.
// Callers acquire after read-only preparation and hold through all mutations and
// observation waits. Dry-run callers do not acquire a lock. External writers and
// older l8k binaries must be paused independently.
func acquireOCPOperation(ctx context.Context, c client.Client, namespace, owner, action string) (func() error, error) {
	if ctx == nil || c == nil || namespace == "" || owner == "" || (action != "deploy" && action != "clean") {
		return nil, fmt.Errorf("OpenShift operation locking requires context, client, namespace, owner and deploy/clean action")
	}
	object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: ocpOperationName(owner),
		Labels: map[string]string{"app.kubernetes.io/managed-by": ocpOperationManager},
	}, Data: map[string]string{"owner": owner, "action": action}}
	if err := c.Create(ctx, object, client.FieldOwner("l8k")); apierrors.IsAlreadyExists(err) {
		current := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
			return nil, fmt.Errorf("inspect existing OpenShift operation lock: %w", err)
		}
		if current.Labels["app.kubernetes.io/managed-by"] != ocpOperationManager || current.Data["owner"] != owner ||
			(current.Data["action"] != "deploy" && current.Data["action"] != "clean") || len(current.OwnerReferences) > 0 ||
			current.UID == "" || current.ResourceVersion == "" {
			return nil, fmt.Errorf("unexpected OpenShift operation lock %s; refusing to replace or adopt it", client.ObjectKeyFromObject(current))
		}
		return nil, fmt.Errorf("OpenShift %s operation lock %s is held (UID %s); wait for that operation, or after confirming its l8k process has stopped, remove this exact lock and retry; locks are never automatically taken over", current.Data["action"], client.ObjectKeyFromObject(current), current.UID)
	} else if err != nil {
		return nil, fmt.Errorf("acquire OpenShift operation lock %s: %w; inspect the lock before retrying an ambiguous create", client.ObjectKeyFromObject(object), err)
	}
	if object.UID == "" || object.ResourceVersion == "" {
		return nil, fmt.Errorf("OpenShift operation lock %s has no server-assigned UID/resourceVersion; inspect the partial acquisition before retrying", client.ObjectKeyFromObject(object))
	}
	uid, resourceVersion := object.UID, object.ResourceVersion
	var mutex sync.Mutex
	released := false
	return func() error {
		mutex.Lock()
		defer mutex.Unlock()
		if released {
			return nil
		}
		// Operation cancellation must not strand an otherwise owned lock. A
		// bounded independent context cannot broaden UID/RV deletion authority.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		err := c.Delete(releaseCtx, object, client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("release OpenShift operation lock %s (UID %s): %w; inspect the lock before retrying", client.ObjectKeyFromObject(object), uid, err)
		}
		released = true
		return nil
	}, nil
}
