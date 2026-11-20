// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/resourceversion"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const mofedCleanupCheckpoint = "l8k-mofed-cleanup-checkpoint"
const mofedDriverLabel = "nvidia.com/ofed-driver"
const mofedDriverContainer = "mofed-container"

// withVerifiedMOFEDShutdown observes driver container shutdown while remove
// deletes the policies and their children with the Network Operator still alive.
// A durable checkpoint prevents a retry from mistaking lost terminal status for
// successful shutdown. It is removed only after the complete verification succeeds.
func withVerifiedMOFEDShutdown(ctx context.Context, kubeClient client.Client, namespace string, remove func(context.Context) (int, error)) (int, error) {
	if kubeClient == nil || remove == nil || namespace == "" {
		return 0, fmt.Errorf("verify MOFED shutdown: client, namespace and deletion callback are required")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := checkMOFEDCleanupTarget(ctx, kubeClient, namespace); err != nil {
		return 0, err
	}
	pods := &corev1.PodList{}
	if err := kubeClient.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return 0, fmt.Errorf("inventory MOFED pods: %w", err)
	}
	ds := &appsv1.DaemonSetList{}
	if err := kubeClient.List(ctx, ds, client.InNamespace(namespace), client.HasLabels{mofedDriverLabel}); err != nil {
		return 0, fmt.Errorf("inventory MOFED DaemonSets: %w", err)
	}
	observer := mofedShutdownObserver{pods: map[types.UID]*mofedShutdownPod{}, resourceVersion: pods.ResourceVersion}
	var initialObservationError error
	for i := range pods.Items {
		pod := &pods.Items[i]
		if isMOFEDPod(pod) {
			for _, status := range pod.Status.ContainerStatuses {
				if status.Name == mofedDriverContainer && status.State.Terminated != nil {
					initialObservationError = errors.Join(initialObservationError, fmt.Errorf("MOFED pod %s was already terminal at inventory; shutdown during this cleanup cannot be verified", pod.Name))
				}
			}
		}
		if err := observer.observe(pod, false); err != nil {
			initialObservationError = errors.Join(initialObservationError, err)
		}
	}
	hasDriver := len(observer.pods) != 0
	for i := range ds.Items {
		hasDriver = hasDriver || isMOFEDDaemonSet(&ds.Items[i])
	}
	if !hasDriver {
		intended, err := hasMOFEDPolicyIntent(ctx, kubeClient)
		if err != nil {
			return 0, err
		}
		if intended {
			return 0, fmt.Errorf("MOFED is configured in a live policy but no driver Pod or DaemonSet is observable in namespace %s; shutdown cannot be verified (check namespace, controller reconciliation and worker selection)", namespace)
		}
		return remove(ctx)
	}
	if pods.ResourceVersion == "" {
		return 0, fmt.Errorf("inventory MOFED pods: missing resource version; cannot establish continuous shutdown observation")
	}
	operationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watcher, ok := kubeClient.(client.WithWatch)
	if !ok {
		return 0, fmt.Errorf("verifying MOFED shutdown requires a Kubernetes client with pod watch support")
	}
	stream, err := watcher.Watch(operationCtx, &corev1.PodList{}, client.InNamespace(namespace), &client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: pods.ResourceVersion, AllowWatchBookmarks: true}})
	if err != nil {
		return 0, fmt.Errorf("watch MOFED shutdown before deletion: %w", err)
	}
	defer stream.Stop()
	checkpoint := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: mofedCleanupCheckpoint},
		Data: map[string]string{
			"state": "shutdown-unverified",
			"pods":  observer.inventory(),
			"recovery": "Verify driver recovery on affected hosts before manually removing this checkpoint. " +
				"Pod absence alone is not proof of successful shutdown. " +
				"The pods field is the initial inventory; additional driver pods may have appeared during teardown.",
		},
	}
	if err := kubeClient.Create(ctx, checkpoint); err != nil {
		return 0, fmt.Errorf("create MOFED cleanup checkpoint before deletion: %w", err)
	}
	if initialObservationError != nil {
		return 0, initialObservationError
	}
	type deletionResult struct {
		count int
		err   error
	}
	removed := make(chan deletionResult, 1)
	go func() {
		count, err := remove(operationCtx)
		removed <- deletionResult{count, err}
	}()
	var result deletionResult
	finished := false
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	// Wait for the callback after cancellation, so no cleanup mutation outlives
	// the guard's return. The callback is required to honor its supplied context.
	fail := func(cause error) (int, error) {
		cancel()
		if !finished {
			result = <-removed
			finished = true
		}
		return result.count, errors.Join(cause, result.err)
	}
	var fence string
	var fenceStarted time.Time
	for {
		select {
		case <-ctx.Done():
			return fail(fmt.Errorf("verify MOFED shutdown (watch fence %q, observed resource version %q): %w", fence, observer.resourceVersion, ctx.Err()))
		case event, ok := <-stream.ResultChan():
			if !ok {
				return fail(fmt.Errorf("MOFED shutdown watch ended before verification; terminal evidence may be missing"))
			}
			knownPods := len(observer.pods)
			if err := observer.event(event); err != nil {
				return fail(err)
			}
			if len(observer.pods) != knownPods {
				fence = ""
			}
		case result = <-removed:
			finished = true
			removed = nil
			if result.err != nil {
				return fail(result.err)
			}
		case <-ticker.C:
		}
		if !finished || !observer.complete() {
			continue
		}

		if fence == "" {
			empty, version, err := mofedResourcesAbsent(operationCtx, kubeClient, namespace, &observer)
			if err != nil {
				return fail(err)
			}
			if !empty {
				continue
			}
			if version == "" {
				return fail(fmt.Errorf("final MOFED pod inventory has no resource version; shutdown observation cannot be fenced"))
			}
			fence = version
			fenceStarted = time.Now()
		}
		caughtUp, err := mofedWatchReached(observer.resourceVersion, fence)
		if err != nil {
			return fail(err)
		}
		if !caughtUp {
			// Servers may omit optional bookmarks. An unbounded cleanup timeout must
			// not turn an unavailable observation fence into an endless silent wait.
			if time.Since(fenceStarted) >= 2*time.Minute {
				return fail(fmt.Errorf("MOFED shutdown watch did not reach final pod inventory resource version %s within 2m; successful shutdown remains unverified", fence))
			}
			continue
		}
		empty, _, err := mofedResourcesAbsent(operationCtx, kubeClient, namespace, &observer)
		if err != nil {
			return fail(err)
		}
		if !empty {
			fence = ""
			continue
		}

		if err := kubeClient.Delete(ctx, checkpoint, client.Preconditions{UID: &checkpoint.UID}); err != nil {
			return result.count, fmt.Errorf("remove verified MOFED cleanup checkpoint: %w", err)
		}
		return result.count, nil
	}
}

// Check prior shutdown evidence and the namespace boundary before any
// destructive stage, and again when acquiring the driver observation.
func checkMOFEDCleanupTarget(ctx context.Context, kubeClient client.Client, namespace string) error {
	checkpoint := &corev1.ConfigMap{}
	checkpointKey := types.NamespacedName{Namespace: namespace, Name: mofedCleanupCheckpoint}
	if err := kubeClient.Get(ctx, checkpointKey, checkpoint); err == nil {
		return fmt.Errorf("MOFED cleanup checkpoint %s/%s remains from an incomplete attempt; verify driver recovery on the affected hosts before an administrator removes it", namespace, mofedCleanupCheckpoint)
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read MOFED cleanup checkpoint: %w", err)
	}
	return rejectOtherNamespaceMOFED(ctx, kubeClient, namespace)
}

type mofedShutdownPod struct {
	name         string
	node         string
	restartCount int32
	containerID  string
	proved       bool
	deleted      bool
}

type mofedShutdownObserver struct {
	pods            map[types.UID]*mofedShutdownPod
	resourceVersion string
}

func isMOFEDPod(pod *corev1.Pod) bool {
	if _, ok := pod.Labels[mofedDriverLabel]; !ok {
		return false
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != "DaemonSet" || owner.APIVersion != appsv1.SchemeGroupVersion.String() || owner.UID == "" {
		return false
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == mofedDriverContainer {
			return true
		}
	}
	return false
}

func isMOFEDDaemonSet(ds *appsv1.DaemonSet) bool {
	if _, ok := ds.Labels[mofedDriverLabel]; !ok {
		return false
	}
	for _, container := range ds.Spec.Template.Spec.Containers {
		if container.Name == mofedDriverContainer {
			return true
		}
	}
	return false
}

func (o *mofedShutdownObserver) observe(pod *corev1.Pod, deleted bool) error {
	tracked := o.pods[pod.UID]
	if tracked == nil {
		if !isMOFEDPod(pod) {
			return nil
		}
		if pod.UID == "" {
			return fmt.Errorf("MOFED pod %s has no UID; shutdown cannot be verified", pod.Name)
		}
		tracked = &mofedShutdownPod{name: pod.Name, node: pod.Spec.NodeName}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == mofedDriverContainer {
				tracked.restartCount = status.RestartCount
				tracked.containerID = status.ContainerID
			}
		}
		o.pods[pod.UID] = tracked
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != mofedDriverContainer {
			continue
		}
		if status.RestartCount != tracked.restartCount || (tracked.containerID != "" && status.ContainerID != tracked.containerID) {
			return fmt.Errorf("MOFED pod %s on node %s changed driver container instance during cleanup; shutdown evidence is incomplete", pod.Name, tracked.node)
		}
		if tracked.containerID == "" {
			tracked.containerID = status.ContainerID
		}
		terminated := status.State.Terminated
		if terminated == nil || pod.DeletionTimestamp == nil {
			continue
		}
		if tracked.containerID == "" {
			return fmt.Errorf("MOFED pod %s has no current driver container identity; shutdown cannot be verified", pod.Name)
		}
		if terminated.ExitCode != 0 || terminated.Signal != 0 {
			return fmt.Errorf("MOFED pod %s on node %s failed shutdown: exit code %d, signal %d (%s)", pod.Name, tracked.node, terminated.ExitCode, terminated.Signal, terminated.Reason)
		}
		// deletionTimestamp is the graceful deletion deadline, not its start;
		// a normal shutdown usually finishes before that timestamp.
		if terminated.FinishedAt.IsZero() {
			return fmt.Errorf("MOFED pod %s has no driver termination completion timestamp", pod.Name)
		}
		tracked.proved = true
	}
	if deleted {
		if !tracked.proved {
			return fmt.Errorf("MOFED pod %s on node %s disappeared without successful driver shutdown evidence", pod.Name, tracked.node)
		}
		tracked.deleted = true
	}
	return nil
}

func (o *mofedShutdownObserver) event(event watch.Event) error {
	if event.Object != nil {
		object, err := meta.Accessor(event.Object)
		if err != nil {
			return fmt.Errorf("read MOFED watch resource version: %w", err)
		}
		if object.GetResourceVersion() != "" {
			o.resourceVersion = object.GetResourceVersion()
		}
	}
	if event.Type == watch.Error {
		return fmt.Errorf("MOFED shutdown watch failed: %w", apierrors.FromObject(event.Object))
	}
	if event.Type == watch.Bookmark {
		return nil
	}
	if event.Type != watch.Added && event.Type != watch.Modified && event.Type != watch.Deleted {
		return fmt.Errorf("MOFED shutdown watch returned unexpected event %q", event.Type)
	}
	pod, ok := event.Object.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("MOFED shutdown watch returned unexpected object %T", event.Object)
	}
	return o.observe(pod, event.Type == watch.Deleted)
}

func (o *mofedShutdownObserver) complete() bool {
	for _, pod := range o.pods {
		if !pod.deleted {
			return false
		}
	}
	return true
}

func (o *mofedShutdownObserver) inventory() string {
	var names []string
	for uid, pod := range o.pods {
		names = append(names, fmt.Sprintf("%s uid=%s node=%s", pod.name, uid, pod.node))
	}
	sort.Strings(names)
	return strings.Join(names, "\n")
}

func mofedResourcesAbsent(ctx context.Context, kubeClient client.Client, namespace string, observer *mofedShutdownObserver) (bool, string, error) {
	sets := &appsv1.DaemonSetList{}
	if err := kubeClient.List(ctx, sets, client.InNamespace(namespace), client.HasLabels{mofedDriverLabel}); err != nil {
		return false, "", fmt.Errorf("verify MOFED DaemonSet deletion: %w", err)
	}
	for i := range sets.Items {
		if isMOFEDDaemonSet(&sets.Items[i]) {
			return false, "", nil
		}
	}
	pods := &corev1.PodList{}
	if err := kubeClient.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return false, "", fmt.Errorf("verify MOFED pod deletion: %w", err)
	}
	absent := true
	for i := range pods.Items {
		pod := &pods.Items[i]
		if observer.pods[pod.UID] != nil || isMOFEDPod(pod) {
			if err := observer.observe(pod, false); err != nil {
				return false, "", err
			}
			absent = false
		}
	}
	return absent, pods.ResourceVersion, nil
}

// The policies removed by clean are cluster scoped. A namespace-limited observer
// cannot verify the driver teardown caused by deleting a policy in another
// installation, so discover that mismatch before issuing any deletion.
func rejectOtherNamespaceMOFED(ctx context.Context, kubeClient client.Client, namespace string) error {
	pods := &corev1.PodList{}
	if err := kubeClient.List(ctx, pods, client.HasLabels{mofedDriverLabel}); err != nil {
		return fmt.Errorf("discover MOFED pods across namespaces: %w", err)
	}
	for i := range pods.Items {
		if pod := &pods.Items[i]; pod.Namespace != namespace && isMOFEDPod(pod) {
			return fmt.Errorf("MOFED driver pod %s/%s is outside cleanup namespace %s; refusing cluster-scoped policy deletion without shutdown coverage", pod.Namespace, pod.Name, namespace)
		}
	}
	sets := &appsv1.DaemonSetList{}
	if err := kubeClient.List(ctx, sets, client.HasLabels{mofedDriverLabel}); err != nil {
		return fmt.Errorf("discover MOFED DaemonSets across namespaces: %w", err)
	}
	for i := range sets.Items {
		if ds := &sets.Items[i]; ds.Namespace != namespace && isMOFEDDaemonSet(ds) {
			return fmt.Errorf("MOFED driver DaemonSet %s/%s is outside cleanup namespace %s; refusing cluster-scoped policy deletion without shutdown coverage", ds.Namespace, ds.Name, namespace)
		}
	}
	return nil
}

func hasMOFEDPolicyIntent(ctx context.Context, kubeClient client.Client) (bool, error) {
	for _, kind := range []string{"NicClusterPolicy", "NicNodePolicy"} {
		policies := &unstructured.UnstructuredList{}
		policies.SetGroupVersionKind(schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: kind + "List"})
		if err := kubeClient.List(ctx, policies); err != nil {
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				continue
			}
			return false, fmt.Errorf("inspect %s driver intent: %w", kind, err)
		}
		for i := range policies.Items {
			value, present, err := unstructured.NestedFieldNoCopy(policies.Items[i].Object, "spec", "ofedDriver")
			if err != nil {
				return false, fmt.Errorf("inspect %s %s driver intent: %w", kind, policies.Items[i].GetName(), err)
			}
			if present && value != nil {
				return true, nil
			}
		}
	}
	return false, nil
}

// Core/v1 Pods use the API server's comparable storage resource versions. Keep
// equality valid for opaque versions, but never guess their relative order.
func mofedWatchReached(observed, fence string) (bool, error) {
	if observed == fence {
		return true, nil
	}
	comparison, err := resourceversion.CompareResourceVersion(observed, fence)
	if err != nil {
		return false, fmt.Errorf("compare MOFED watch resource version %q with final inventory %q: %w", observed, fence, err)
	}
	return comparison >= 0, nil
}
