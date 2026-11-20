// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func isCleanRootPolicy(gvk schema.GroupVersionKind) bool {
	return gvk.Group == "mellanox.com" && (gvk.Kind == "NicClusterPolicy" || gvk.Kind == "NicNodePolicy")
}

const policyWorkloadCheckpoint = "l8k-policy-workload-cleanup"

// The policy controllers continue to create Deployments and DaemonSets while a
// foreground-deleting policy exists. Delete the root in the background and
// follow its observed workload UIDs through garbage collection instead.
type cleanPolicyWorkloads struct {
	checkpoint *corev1.ConfigMap // Data maps observed UIDs to kind/name, including vanished parents.
}

func loadPolicyWorkloads(ctx context.Context, c client.Client, ns string) (*cleanPolicyWorkloads, error) {
	key := client.ObjectKey{Namespace: ns, Name: policyWorkloadCheckpoint}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, key, cm); apierrors.IsNotFound(err) {
		cm.ObjectMeta = metav1.ObjectMeta{Name: key.Name, Namespace: ns, Labels: map[string]string{"app.kubernetes.io/managed-by": "l8k-clean"}}
		cm.Data = map[string]string{}
	} else if err != nil {
		return nil, fmt.Errorf("load policy workload checkpoint: %w", err)
	} else if cm.Labels["app.kubernetes.io/managed-by"] != "l8k-clean" || len(cm.Data) == 0 {
		return nil, fmt.Errorf("unexpected policy workload checkpoint %s; verify its ownership and contents", key)
	}
	return &cleanPolicyWorkloads{checkpoint: cm}, nil
}

func (owned cleanPolicyWorkloads) observeAndSave(ctx context.Context, c client.Client, ns string) ([]string, error) {
	var pending []string
	known := owned.checkpoint.Data
	previous := len(known)
	// Discover parents before their children. Keep parent UIDs even after
	// garbage collection removes them so their terminating pods remain visible.
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "apps", Version: "v1", Kind: "Deployment"},
		{Group: "apps", Version: "v1", Kind: "DaemonSet"},
		{Group: "apps", Version: "v1", Kind: "ReplicaSet"},
		{Version: "v1", Kind: "Pod"},
	} {
		objects, err := listCleanNetworkObjects(ctx, c, gvk, ns)
		if err != nil {
			return nil, err
		}
		for i := range objects {
			obj := &objects[i]
			owner := metav1.GetControllerOf(obj)
			knownParent := owner != nil && owner.UID != "" &&
				(known[string(owner.UID)] != "" || isCleanRootPolicy(schema.FromAPIVersionAndKind(owner.APIVersion, owner.Kind)))
			if knownParent || known[string(obj.GetUID())] != "" {
				if obj.GetUID() == "" {
					return nil, fmt.Errorf("policy-owned %s %s/%s has no UID", gvk.Kind, ns, obj.GetName())
				}
				known[string(obj.GetUID())] = gvk.Kind + "/" + obj.GetName()
				pending = append(pending, gvk.Kind+" "+obj.GetName())
			}
		}
	}
	if len(known) != previous {
		// Persist ancestry before issuing any root delete. A later invocation
		// can still recognize pods after their owning controllers disappear.
		var err error
		if owned.checkpoint.ResourceVersion == "" {
			err = c.Create(ctx, owned.checkpoint)
		} else {
			err = c.Update(ctx, owned.checkpoint)
		}
		if err != nil {
			return nil, fmt.Errorf("save observed policy workload UIDs: %w", err)
		}
	}
	sort.Strings(pending)
	return pending, nil
}

func (owned cleanPolicyWorkloads) wait(ctx context.Context, c client.Client, ns string, interval time.Duration) error {
	var pending []string
	err := wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		var err error
		pending, err = owned.observeAndSave(ctx, c, ns)
		return len(pending) == 0, err
	})
	if err != nil {
		return fmt.Errorf("wait for policy-owned workloads in %s (%s): %w", ns, strings.Join(pending, ", "), err)
	}
	if cm := owned.checkpoint; cm.ResourceVersion != "" {
		if err := c.Delete(ctx, cm, client.Preconditions{UID: &cm.UID, ResourceVersion: &cm.ResourceVersion}); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("remove completed policy workload checkpoint: %w", err)
		}
	}
	return nil
}
