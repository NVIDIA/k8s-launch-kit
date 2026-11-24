// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ocpPolicyWorkloadClean struct {
	mu          sync.Mutex
	namespace   string
	retainEmpty bool
	roots       []cleanObjectRef
	checkpoint  *corev1.ConfigMap
}

func prepareOCPWorkloadClean(ctx context.Context, c client.Client, ns string, refs []cleanObjectRef, retainEmpty bool) (*ocpPolicyWorkloadClean, error) {
	var roots []cleanObjectRef
	var identities []string
	for _, ref := range refs {
		if isCleanRootPolicy(ref.GVK) {
			roots = append(roots, ref)
			identities = append(identities, ref.GVK.Group+"/"+ref.GVK.Kind+"/"+ref.Name)
		}
	}
	sort.Strings(identities)
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(identities, "\n"))))[:16]
	checkpoint := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "l8k-ocp-clean-" + digest}, Data: map[string]string{}}
	if len(roots) == 0 {
		return &ocpPolicyWorkloadClean{namespace: ns, roots: roots, checkpoint: checkpoint, retainEmpty: retainEmpty}, nil
	}
	key := client.ObjectKey{Namespace: ns, Name: "l8k-ocp-clean-" + digest}
	err := c.Get(ctx, key, checkpoint)
	if apierrors.IsNotFound(err) {
		checkpoint.ObjectMeta = metav1.ObjectMeta{Namespace: ns, Name: key.Name, Labels: map[string]string{"app.kubernetes.io/managed-by": "l8k-clean"}}
		checkpoint.Data = map[string]string{}
	} else if err != nil {
		return nil, fmt.Errorf("read scoped workload checkpoint: %w", err)
	} else if checkpoint.Labels["app.kubernetes.io/managed-by"] != "l8k-clean" || len(checkpoint.Data) == 0 {
		return nil, fmt.Errorf("unexpected scoped workload checkpoint %s", key)
	}
	result := &ocpPolicyWorkloadClean{namespace: ns, roots: roots, checkpoint: checkpoint, retainEmpty: retainEmpty}
	if _, err := result.observe(ctx, c); err != nil {
		return nil, err
	}
	return result, nil
}

func (owned *ocpPolicyWorkloadClean) rootOwner(object metav1.Object) bool {
	owner := metav1.GetControllerOf(object)
	if owner == nil {
		return false
	}
	gvk := schema.FromAPIVersionAndKind(owner.APIVersion, owner.Kind)
	for _, ref := range owned.roots {
		if gvk.Group == ref.GVK.Group && gvk.Kind == ref.GVK.Kind && owner.Name == ref.Name && (ref.UID == "" || ref.UID == owner.UID) {
			return true
		}
	}
	return false
}

func (owned *ocpPolicyWorkloadClean) observe(ctx context.Context, c client.Client) ([]string, error) {
	if len(owned.roots) == 0 {
		return nil, nil
	}
	owned.mu.Lock()
	defer owned.mu.Unlock()
	known := owned.checkpoint.Data
	previous := len(known)
	var pending []string
	// Retain parent UIDs after garbage collection. A retry must still recognize
	// Pods whose DaemonSet/ReplicaSet disappeared before this invocation.
	for _, gvk := range []schema.GroupVersionKind{{Group: "apps", Version: "v1", Kind: "Deployment"}, {Group: "apps", Version: "v1", Kind: "DaemonSet"}, {Group: "apps", Version: "v1", Kind: "ReplicaSet"}, {Version: "v1", Kind: "Pod"}} {
		objects, err := listCleanNetworkObjects(ctx, c, gvk, owned.namespace)
		if err != nil {
			return nil, err
		}
		for i := range objects {
			object := &objects[i]
			owner := metav1.GetControllerOf(object)
			if !owned.rootOwner(object) && known[string(object.GetUID())] == "" && (owner == nil || known[string(owner.UID)] == "") {
				continue
			}
			if object.GetUID() == "" {
				return nil, fmt.Errorf("selected policy-owned %s %s has no UID", gvk.Kind, object.GetName())
			}
			known[string(object.GetUID())] = gvk.Kind + "/" + object.GetName()
			pending = append(pending, gvk.Kind+" "+object.GetName())
		}
	}
	if previous != len(known) || (owned.retainEmpty && owned.checkpoint.ResourceVersion == "") {
		// Persist an empty observation too: absent roots on a later retry still
		// need evidence that ancestry was inventoried before root deletion.
		known["_ancestry-recorded"] = "true"
		var err error
		if owned.checkpoint.ResourceVersion == "" {
			err = c.Create(ctx, owned.checkpoint)
		} else {
			err = c.Update(ctx, owned.checkpoint)
		}
		if err != nil {
			return nil, fmt.Errorf("save selected policy workload ancestry: %w", err)
		}
	}
	return pending, nil
}

func (owned *ocpPolicyWorkloadClean) removeCheckpoint(ctx context.Context, c client.Client) error {
	if owned.checkpoint.ResourceVersion == "" {
		return nil
	}
	if err := c.Delete(ctx, owned.checkpoint, client.Preconditions{UID: &owned.checkpoint.UID, ResourceVersion: &owned.checkpoint.ResourceVersion}); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("remove selected policy workload checkpoint: %w", err)
	}
	return nil
}

func (owned *ocpPolicyWorkloadClean) mofedCheckpoint() string {
	return owned.checkpoint.Name + "-mofed"
}

func (owned *ocpPolicyWorkloadClean) mofedScope(ctx context.Context, c client.Client, refs []cleanObjectRef) *mofedCleanupScope {
	return &mofedCleanupScope{
		checkpointName:   owned.mofedCheckpoint(),
		includeDaemonSet: func(ds *appsv1.DaemonSet) bool { return owned.rootOwner(ds) },
		includePod: func(pod *corev1.Pod) (bool, error) {
			parent := metav1.GetControllerOf(pod)
			if parent == nil {
				return false, nil
			}
			owned.mu.Lock()
			known := owned.checkpoint.Data[string(parent.UID)] != "" || owned.checkpoint.Data[string(pod.UID)] != ""
			owned.mu.Unlock()
			if known {
				return true, nil
			}
			// A controller can create a last DaemonSet while its policy is deleting.
			// Resolve that parent explicitly rather than selecting drivers by label.
			ds := &appsv1.DaemonSet{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: parent.Name}, ds); err != nil {
				if apierrors.IsNotFound(err) {
					return false, fmt.Errorf("cannot attribute MOFED pod %s/%s after its DaemonSet disappeared; selected-driver shutdown remains unverified", pod.Namespace, pod.Name)
				}
				return false, fmt.Errorf("inspect MOFED pod parent: %w", err)
			}
			if ds.UID != parent.UID {
				return false, fmt.Errorf("MOFED pod %s/%s parent was replaced; shutdown scope remains unverified", pod.Namespace, pod.Name)
			}
			return owned.rootOwner(ds), nil
		},
		hasIntent: func(ctx context.Context) (bool, error) {
			for _, ref := range refs {
				if !isCleanRootPolicy(ref.GVK) {
					continue
				}
				policy, err := ocpDesired(ref)
				if err != nil {
					return false, err
				}
				driver, present, err := unstructured.NestedFieldNoCopy(policy.Object, "spec", "ofedDriver")
				if err != nil {
					return false, err
				}
				if present && driver != nil {
					return true, nil
				}
			}
			return false, nil
		},
	}
}

// A cluster-scoped policy can be observed by another operator installation.
// Reject selected drivers outside this observer's namespace without requiring
// independent foreign installations to shut down.
func (owned *ocpPolicyWorkloadClean) checkDriverNamespaces(ctx context.Context, c client.Client) error {
	sets := &appsv1.DaemonSetList{}
	if err := c.List(ctx, sets, client.HasLabels{mofedDriverLabel}); err != nil {
		return fmt.Errorf("inspect selected MOFED driver namespaces: %w", err)
	}
	for i := range sets.Items {
		ds := &sets.Items[i]
		if ds.Namespace != owned.namespace && isMOFEDDaemonSet(ds) && owned.rootOwner(ds) {
			return fmt.Errorf("selected policy owns MOFED DaemonSet %s/%s outside cleanup namespace %s; shutdown coverage is unavailable", ds.Namespace, ds.Name, owned.namespace)
		}
	}
	return nil
}
