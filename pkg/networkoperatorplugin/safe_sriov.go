// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

const cleanSriovGroup = "sriovnetwork.openshift.io"
const cleanSriovDaemonContainer = "sriov-network-config-daemon"
const cleanSriovLogLimit = 8 * 1024 * 1024

func isCleanSriovIntent(ref cleanObjectRef, ns string) bool {
	if ref.Namespace != ns || ref.GVK.Group != cleanSriovGroup {
		return false
	}
	return (ref.GVK.Kind == "SriovNetworkNodePolicy" && ref.Name != "default") ||
		ref.GVK.Kind == "SriovNetworkPoolConfig"
}

// cleanSriovReset owns one retained baseline per NodeState. refresh extends
// those observations after upstream rail teardown without discarding identities.
type cleanSriovReset struct {
	namespace string
	stateGVK  schema.GroupVersionKind
	policies  []cleanObjectRef
	nodes     map[string]*cleanSriovNode
	readLogs  sriovDaemonLogReader
}

type cleanSriovNode struct {
	uid                 types.UID
	managedPFs          map[string]struct{}
	requiresProof       bool
	provedGeneration    int64
	attemptedGeneration int64
	nextLogRead         time.Time
}

func prepareSriovReset(ctx context.Context, c client.Client, refs []cleanObjectRef, ns string, readLogs sriovDaemonLogReader) (*cleanSriovReset, error) {
	reset := &cleanSriovReset{namespace: ns, nodes: make(map[string]*cleanSriovNode), readLogs: readLogs}
	if err := reset.refresh(ctx, c, refs); err != nil {
		return nil, err
	}
	return reset, nil
}

func (reset *cleanSriovReset) refresh(ctx context.Context, c client.Client, refs []cleanObjectRef) error {
	var policies []cleanObjectRef
	gvk := reset.stateGVK
	expected := make(map[string]struct{})
	for _, ref := range refs {
		if isCleanSriovIntent(ref, reset.namespace) {
			policies = append(policies, ref)
		}
		if ref.Namespace == reset.namespace && ref.GVK.Group == cleanSriovGroup && ref.GVK.Kind == "SriovNetworkNodeState" {
			gvk = ref.GVK
			expected[ref.Name] = struct{}{}
		}
	}
	if len(policies) == 0 && gvk.Empty() {
		reset.policies = nil
		return nil
	}
	if gvk.Empty() {
		gvk = schema.GroupVersionKind{Group: cleanSriovGroup, Version: "v1", Kind: "SriovNetworkNodeState"}
	}
	list, err := listCleanSriovStates(ctx, c, gvk, reset.namespace)
	if err != nil {
		return err
	}
	if len(policies) > 0 && len(list.Items) == 0 {
		return fmt.Errorf("cannot verify SR-IOV reset: configuration intent exists but no SriovNetworkNodeStates were found in namespace %s", reset.namespace)
	}
	for i := range list.Items {
		state := &list.Items[i]
		name := state.GetName()
		delete(expected, name)
		node := reset.nodes[name]
		if node == nil {
			node = &cleanSriovNode{uid: state.GetUID(), managedPFs: make(map[string]struct{})}
			reset.nodes[name] = node
		} else if node.uid != state.GetUID() {
			return fmt.Errorf("SriovNetworkNodeState %s/%s changed identity during cleanup", reset.namespace, name)
		}
		if err := node.capture(state); err != nil {
			return fmt.Errorf("inspect SriovNetworkNodeState %s/%s: %w", reset.namespace, name, err)
		}
		if node.requiresProof && reset.readLogs == nil {
			return fmt.Errorf("SriovNetworkNodeState %s/%s requires a Pod log reader to verify its applied generation", reset.namespace, name)
		}
	}
	for name := range expected {
		return fmt.Errorf("SriovNetworkNodeState %s/%s disappeared before reset baseline could be captured", reset.namespace, name)
	}
	reset.stateGVK = gvk
	reset.policies = policies
	return nil
}

func (node *cleanSriovNode) capture(state *unstructured.Unstructured) error {
	addresses, err := cleanSriovManagedPFs(state)
	if err != nil {
		return err
	}
	maps.Copy(node.managedPFs, addresses)
	// Reject malformed desired bridge entries before deleting a policy.
	if _, err := firstCleanSriovBridge(state, "spec"); err != nil {
		return err
	}
	spec, _, err := unstructured.NestedMap(state.Object, "spec")
	if err != nil {
		return err
	}
	// A never-configured node need not execute Apply. Later generation/spec
	// changes still escalate this node even when it was initially pristine.
	node.requiresProof = node.requiresProof || state.GetGeneration() != 1 || !cleanSriovEmpty(spec)
	return nil
}

// deletePoliciesAndWait removes configuration intent with its reconcilers alive.
// A retry still verifies remaining states after the policies have disappeared.
func (reset *cleanSriovReset) deletePoliciesAndWait(ctx context.Context, c client.Client, interval time.Duration, out ui.Output) (int, error) {
	if reset.stateGVK.Empty() {
		return 0, nil
	}
	deleted, err := deleteAllAndWait(ctx, c, reset.policies, interval, out, metav1.DeletePropagationForeground)
	if err != nil {
		return deleted, err
	}
	pending := "waiting for SR-IOV node reconciliation"
	err = wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		list, err := listCleanSriovStates(ctx, c, reset.stateGVK, reset.namespace)
		if err != nil {
			return false, err
		}
		seen := make(map[string]struct{}, len(list.Items))
		var reasons []string
		for i := range list.Items {
			state := &list.Items[i]
			name := state.GetName()
			node := reset.nodes[name]
			if node == nil || node.uid != state.GetUID() {
				return false, fmt.Errorf("SriovNetworkNodeState %s/%s appeared or changed identity during cleanup; reset proof is unavailable", reset.namespace, name)
			}
			seen[name] = struct{}{}
			reason, err := node.pending(ctx, c, state, reset.readLogs)
			if err != nil {
				return false, fmt.Errorf("SriovNetworkNodeState %s/%s: %w", reset.namespace, name, err)
			}
			if reason != "" {
				reasons = append(reasons, name+": "+reason)
			}
		}
		for name := range reset.nodes {
			if _, ok := seen[name]; !ok {
				reasons = append(reasons, name+": NodeState disappeared before reset could be verified")
			}
		}
		if len(reasons) == 0 {
			return true, nil
		}
		sort.Strings(reasons)
		next := strings.Join(reasons, "; ")
		if next != pending {
			out.Info("Waiting for SR-IOV reset: %s", next)
			pending = next
		}
		return false, nil
	})
	if err != nil {
		return deleted, fmt.Errorf("wait for SR-IOV reset (%s): %w", pending, err)
	}
	return deleted, nil
}

// observe checks the complete API postcondition without reading daemon logs.
// It is used both before and after the independently issued pods/log request.
func (node *cleanSriovNode) observe(state *unstructured.Unstructured) (string, error) {
	if err := node.capture(state); err != nil {
		return "", err
	}
	reason, err := cleanSriovStatePending(state)
	if err != nil || reason != "" {
		return reason, err
	}
	addresses, err := cleanSriovManagedPFs(state)
	if err != nil {
		return "", err
	}
	for _, address := range slices.Sorted(maps.Keys(node.managedPFs)) {
		if _, ok := addresses[address]; !ok {
			return "managed PF " + address + " missing from observed interfaces", nil
		}
	}
	// The daemon reports the bridges it manages, even after a policy is gone.
	bridge, err := firstCleanSriovBridge(state, "status")
	if err != nil {
		return "", err
	}
	if bridge != "" {
		return "managed bridge " + bridge + " is still observed", nil
	}
	return "", nil
}

func (node *cleanSriovNode) pending(ctx context.Context, c client.Client, state *unstructured.Unstructured, readLogs sriovDaemonLogReader) (string, error) {
	reason, err := node.observe(state)
	if err != nil || reason != "" {
		return reason, err
	}
	generation := state.GetGeneration()
	if !node.requiresProof || node.provedGeneration == generation {
		return "", nil
	}
	now := time.Now()
	if node.attemptedGeneration != generation || !now.Before(node.nextLogRead) {
		node.attemptedGeneration = generation
		node.nextLogRead = now.Add(5 * time.Second)
		latest, err := verifySriovDaemonGeneration(ctx, c, state, readLogs)
		if err != nil {
			return "", fmt.Errorf("verify generation %d daemon reconciliation: %w", generation, err)
		}
		if latest != nil {
			if reason, err := node.observe(latest); err != nil || reason != "" {
				return reason, err
			}
			node.provedGeneration = generation
			return "", nil
		}
	}
	return fmt.Sprintf("waiting for captured generation %d and successful reconcileID in current daemon logs (bounded to 10000 lines/8 MiB; evidence may be pending, rotated or truncated)", generation), nil
}

// Retain both intended and enumerated PF identities before deleting policies.
// A truncated inventory cannot establish that a previously managed PF reset.
func cleanSriovManagedPFs(state *unstructured.Unstructured) (map[string]struct{}, error) {
	addresses := make(map[string]struct{})
	for _, section := range []string{"spec", "status"} {
		interfaces, _, err := unstructured.NestedSlice(state.Object, section, "interfaces")
		if err != nil {
			return nil, err
		}
		for _, value := range interfaces {
			iface, ok := value.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("malformed %s interface", section)
			}
			external, _, err := unstructured.NestedBool(iface, "externallyManaged")
			if err != nil {
				return nil, err
			}
			if external {
				continue
			}
			address, _, err := unstructured.NestedString(iface, "pciAddress")
			if err != nil {
				return nil, err
			}
			if address == "" {
				return nil, fmt.Errorf("%s interface has no PCI address", section)
			}
			addresses[address] = struct{}{}
		}
	}
	return addresses, nil
}

func listCleanSriovStates(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, ns string) (*unstructured.UnstructuredList, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("list SriovNetworkNodeStates in namespace %s: %w", ns, err)
	}
	return list, nil
}

func cleanSriovStatePending(state *unstructured.Unstructured) (string, error) {
	spec, _, err := unstructured.NestedMap(state.Object, "spec")
	if err != nil {
		return "", err
	}
	if !cleanSriovEmpty(spec) {
		return "desired spec is not empty", nil
	}
	syncStatus, _, err := unstructured.NestedString(state.Object, "status", "syncStatus")
	if err != nil {
		return "", err
	}
	lastError, _, err := unstructured.NestedString(state.Object, "status", "lastSyncError")
	if err != nil {
		return "", err
	}
	if syncStatus == "Failed" {
		return "", fmt.Errorf("reset failed: %s", lastError)
	}
	if syncStatus != "Succeeded" || lastError != "" {
		return fmt.Sprintf("syncStatus=%q lastSyncError=%q", syncStatus, lastError), nil
	}
	for _, key := range []string{"sriovnetwork.openshift.io/current-state", "sriovnetwork.openshift.io/desired-state"} {
		if value, present := state.GetAnnotations()[key]; present && value != "Idle" {
			return fmt.Sprintf("drain annotation %s=%q has not returned to Idle", key, value), nil
		}
	}
	interfaces, _, err := unstructured.NestedSlice(state.Object, "status", "interfaces")
	if err != nil {
		return "", err
	}
	if len(interfaces) == 0 {
		return "no observed interfaces to verify reset", nil
	}
	for _, value := range interfaces {
		iface, ok := value.(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("malformed status interface")
		}
		address, _, err := unstructured.NestedString(iface, "pciAddress")
		if err != nil {
			return "", err
		}
		if address == "" {
			return "", fmt.Errorf("status interface has no PCI address")
		}
		external, _, err := unstructured.NestedBool(iface, "externallyManaged")
		if err != nil {
			return "", err
		}
		if external {
			continue
		}
		vfs, _, err := unstructured.NestedInt64(iface, "numVfs")
		if err != nil {
			return "", err
		}
		// Succeeded alone may belong to the previous nonempty desired spec.
		// The daemon refreshes host interfaces before publishing success.
		if vfs != 0 {
			return fmt.Sprintf("observed interface still has %d VFs", vfs), nil
		}
		observedVFs, _, err := unstructured.NestedSlice(iface, "Vfs")
		if err != nil {
			return "", err
		}
		if len(observedVFs) != 0 {
			return "observed interface still reports VF devices", nil
		}
	}
	return "", nil
}

// API serialization includes empty system and bridges objects. Scalar values,
// including false and zero, are still explicit desired configuration.
func cleanSriovEmpty(value interface{}) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case map[string]interface{}:
		for _, child := range typed {
			if !cleanSriovEmpty(child) {
				return false
			}
		}
		return true
	case []interface{}:
		return len(typed) == 0
	default:
		return false
	}
}

// Validate all entries and return a stable bridge name for progress messages.
func firstCleanSriovBridge(state *unstructured.Unstructured, section string) (string, error) {
	bridges, _, err := unstructured.NestedSlice(state.Object, section, "bridges", "ovs")
	if err != nil {
		return "", err
	}
	var first string
	for _, value := range bridges {
		bridge, ok := value.(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("malformed %s bridge", section)
		}
		name, _, err := unstructured.NestedString(bridge, "name")
		if err != nil {
			return "", err
		}
		if name == "" {
			return "", fmt.Errorf("%s bridge has no name", section)
		}
		if first == "" || name < first {
			first = name
		}
	}
	return first, nil
}

// sriovDaemonLogReader reads a bounded, timestamped current-container log. The
// caller supplies the NodeState lifetime boundary, never a guessed release/image.
type sriovDaemonLogReader func(context.Context, string, string, string, metav1.Time) ([]byte, error)

func newSriovDaemonLogReader(config *rest.Config) sriovDaemonLogReader {
	if config == nil {
		return nil
	}
	return func(ctx context.Context, namespace, pod, container string, since metav1.Time) ([]byte, error) {
		c, err := coreclient.NewForConfig(config)
		if err != nil {
			return nil, err
		}
		tail, limit := int64(10000), int64(cleanSriovLogLimit)
		stream, err := c.Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
			Container: container, SinceTime: &since, Timestamps: true, TailLines: &tail, LimitBytes: &limit,
		}).Stream(ctx)
		if err != nil {
			return nil, err
		}
		defer stream.Close()
		data, err := io.ReadAll(io.LimitReader(stream, cleanSriovLogLimit+1))
		if err != nil {
			return nil, err
		}
		if len(data) > cleanSriovLogLimit {
			data = data[:cleanSriovLogLimit]
		}
		return data, nil
	}
}

type cleanSriovDaemonIdentity struct {
	uid          types.UID
	containerID  string
	restartCount int32
	startedAt    metav1.Time
}

func sriovDaemonIdentity(pod *corev1.Pod) (cleanSriovDaemonIdentity, error) {
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" || owner.Name != cleanSriovDaemonContainer || owner.UID == "" {
		return cleanSriovDaemonIdentity{}, fmt.Errorf("pod %s/%s is not owned by the SR-IOV config daemon DaemonSet", pod.Namespace, pod.Name)
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != cleanSriovDaemonContainer {
			continue
		}
		if pod.UID == "" || status.ContainerID == "" || status.State.Running == nil || status.State.Running.StartedAt.IsZero() {
			break
		}
		return cleanSriovDaemonIdentity{uid: pod.UID, containerID: status.ContainerID, restartCount: status.RestartCount, startedAt: status.State.Running.StartedAt}, nil
	}
	return cleanSriovDaemonIdentity{}, fmt.Errorf("daemon Pod %s/%s has no stable running container identity", pod.Namespace, pod.Name)
}

func verifySriovDaemonGeneration(ctx context.Context, c client.Client, state *unstructured.Unstructured, readLogs sriovDaemonLogReader) (*unstructured.Unstructured, error) {
	if state.GetUID() == "" || state.GetCreationTimestamp().Time.IsZero() || state.GetGeneration() <= 0 {
		return nil, fmt.Errorf("NodeState UID, lifetime and generation are required to correlate daemon logs")
	}
	if readLogs == nil {
		return nil, fmt.Errorf("pod log reader is unavailable")
	}
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(state.GetNamespace()), client.MatchingLabels{"app": cleanSriovDaemonContainer}); err != nil {
		return nil, fmt.Errorf("list SR-IOV config daemon Pods: %w", err)
	}
	var selected *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != state.GetName() {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("multiple config daemon Pods found for node %s; cannot bind generation evidence", state.GetName())
		}
		selected = pod
	}
	if selected == nil {
		return nil, fmt.Errorf("no config daemon Pod found for node %s", state.GetName())
	}
	identity, err := sriovDaemonIdentity(selected)
	if err != nil {
		return nil, err
	}
	data, err := readLogs(ctx, selected.Namespace, selected.Name, cleanSriovDaemonContainer, state.GetCreationTimestamp())
	if err != nil {
		return nil, fmt.Errorf("read current daemon Pod %s/%s logs: %w", selected.Namespace, selected.Name, err)
	}
	current := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(selected), current); err != nil {
		return nil, fmt.Errorf("recheck daemon Pod identity: %w", err)
	}
	after, err := sriovDaemonIdentity(current)
	if err != nil {
		return nil, err
	}
	if identity != after || current.Spec.NodeName != state.GetName() {
		return nil, fmt.Errorf("daemon Pod/container changed while reading generation evidence")
	}
	latest := objectForCleanRef(cleanObjectRef{GVK: state.GroupVersionKind(), Namespace: state.GetNamespace(), Name: state.GetName()})
	if err := c.Get(ctx, client.ObjectKeyFromObject(state), latest); err != nil {
		return nil, fmt.Errorf("recheck NodeState lifetime after reading daemon logs: %w", err)
	}
	if latest.GetUID() != state.GetUID() || latest.GetGeneration() != state.GetGeneration() {
		return nil, fmt.Errorf("NodeState identity or generation changed while reading daemon evidence")
	}
	// Released daemons log success before publishing their final status. The
	// matching log is apply evidence; the caller must validate this fresh API
	// observation against every retained host/drain postcondition as well.
	if !sriovLogProvesGeneration(data, state, identity.startedAt.Time) {
		return nil, nil
	}
	return latest, nil
}

// Released daemons emit controller-runtime JSON fields after a console prefix,
// or a complete JSON record. The capture and success must share a reconcileID.
func sriovLogProvesGeneration(data []byte, state *unstructured.Unstructured, containerStart time.Time) bool {
	captured := make(map[string]bool)
	boundary := state.GetCreationTimestamp().Time
	if containerStart.After(boundary) {
		boundary = containerStart
	}
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := string(raw)
		timestamp, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil || at.Before(boundary) {
			continue
		}
		start := strings.IndexByte(rest, '{')
		if start < 0 {
			continue
		}
		var fields map[string]interface{}
		decoder := json.NewDecoder(strings.NewReader(rest[start:]))
		decoder.UseNumber()
		if decoder.Decode(&fields) != nil {
			continue
		}
		id, _ := fields["reconcileID"].(string)
		name, _ := fields["name"].(string)
		namespace, _ := fields["namespace"].(string)
		if id == "" || name != state.GetName() || namespace != state.GetNamespace() {
			continue
		}
		message, _ := fields["msg"].(string)
		if message == "" {
			message, _ = fields["message"].(string)
		}
		prefix := strings.TrimSpace(rest[:start])
		if message == "new generation" || strings.HasSuffix(prefix, "new generation") {
			number, ok := fields["generation"].(json.Number)
			generation, err := number.Int64()
			captured[id] = ok && err == nil && generation == state.GetGeneration()
			continue
		}
		if (message == "sync succeeded" || strings.HasSuffix(prefix, "sync succeeded")) && captured[id] {
			return true
		}
	}
	return false
}
