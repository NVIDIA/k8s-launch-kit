// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/nicconfigdaemon"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDiscoverReadOnlyRequiresExistingDaemon(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, err := DiscoverReadOnly(context.Background(), c)

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotInstalled))
}

type errorListClient struct {
	client.Client
	err error
}

func (c errorListClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return c.err
}

type sequenceListClient struct {
	client.Client
	errors        []error
	persistentErr error
	items         []nicop.NicDevice
	calls         int
	afterCall     func(int)
}

func (c *sequenceListClient) List(_ context.Context, out client.ObjectList, _ ...client.ListOption) error {
	c.calls++
	if len(c.errors) > 0 {
		err := c.errors[0]
		c.errors = c.errors[1:]
		if c.afterCall != nil {
			c.afterCall(c.calls)
		}
		return err
	}
	if c.persistentErr != nil {
		if c.afterCall != nil {
			c.afterCall(c.calls)
		}
		return c.persistentErr
	}
	if list, ok := out.(*nicop.NicDeviceList); ok {
		list.Items = append([]nicop.NicDevice(nil), c.items...)
	}
	if c.afterCall != nil {
		c.afterCall(c.calls)
	}
	return nil
}

func TestWaitNicDevicesDiscoveredReturnsListError(t *testing.T) {
	listErr := errors.New("forbidden: cannot list NicDevice resources")
	c := errorListClient{
		Client: fake.NewClientBuilder().Build(),
		err:    listErr,
	}

	err := waitNicDevicesDiscovered(context.Background(), c, []string{"worker-0"})

	require.ErrorIs(t, err, listErr)
}

func TestWaitNicDevicesDiscoveredRetriesTransientListError(t *testing.T) {
	c := &sequenceListClient{
		Client: fake.NewClientBuilder().Build(),
		errors: []error{apierrors.NewServiceUnavailable("apiserver restarting")},
		items:  []nicop.NicDevice{{Status: nicop.NicDeviceStatus{Node: "worker-0"}}},
	}

	err := waitNicDevicesDiscoveredWithInterval(context.Background(), c, []string{"worker-0"}, time.Millisecond)

	require.NoError(t, err)
	assert.Equal(t, 2, c.calls)
}

func TestWaitNicDevicesDiscoveredRetriesUnexpectedEOF(t *testing.T) {
	c := &sequenceListClient{
		Client: fake.NewClientBuilder().Build(),
		errors: []error{fmt.Errorf("decode NicDevice response: %w", io.ErrUnexpectedEOF)},
		items:  []nicop.NicDevice{{Status: nicop.NicDeviceStatus{Node: "worker-0"}}},
	}

	err := waitNicDevicesDiscoveredWithInterval(context.Background(), c, []string{"worker-0"}, time.Millisecond)

	require.NoError(t, err)
	assert.Equal(t, 2, c.calls)
}

func TestWaitNicDevicesDiscoveredRetryCancellationPreservesLastError(t *testing.T) {
	listErr := apierrors.NewTooManyRequests("apiserver throttled", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &sequenceListClient{
		Client:        fake.NewClientBuilder().Build(),
		persistentErr: listErr,
		afterCall: func(calls int) {
			if calls == 2 {
				cancel()
			}
		},
	}

	err := waitNicDevicesDiscoveredWithInterval(ctx, c, []string{"worker-0"}, time.Millisecond)

	require.ErrorIs(t, err, listErr)
	assert.Equal(t, 2, c.calls)
}

func TestWaitNicDevicesDiscoveredSuccessfulListClearsRetryableError(t *testing.T) {
	listErr := apierrors.NewServiceUnavailable("apiserver restarting")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &sequenceListClient{
		Client: fake.NewClientBuilder().Build(),
		errors: []error{listErr},
		afterCall: func(calls int) {
			if calls == 2 {
				cancel()
			}
		},
	}

	err := waitNicDevicesDiscoveredWithInterval(ctx, c, []string{"worker-0"}, time.Millisecond)

	require.Error(t, err)
	assert.NotErrorIs(t, err, listErr)
	assert.Equal(t, 2, c.calls)
}

func TestWaitNicDevicesDiscoveredOrdinaryTimeoutDoesNotWrapOldError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &sequenceListClient{
		Client: fake.NewClientBuilder().Build(),
		afterCall: func(calls int) {
			if calls == 1 {
				cancel()
			}
		},
	}

	err := waitNicDevicesDiscoveredWithInterval(ctx, c, []string{"worker-0"}, time.Millisecond)

	// A successful list finds no devices, so this is an ordinary discovery timeout.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout waiting for NicDevice resources")
	assert.Equal(t, 1, c.calls)
}

func TestWaitNicDevicesDiscoveredSucceedsWhenDevicesAlreadyExist(t *testing.T) {
	c := &sequenceListClient{
		Client: fake.NewClientBuilder().Build(),
		items:  []nicop.NicDevice{{Status: nicop.NicDeviceStatus{Node: "worker-0"}}},
	}

	err := waitNicDevicesDiscoveredWithInterval(context.Background(), c, []string{"worker-0"}, time.Millisecond)

	require.NoError(t, err)
	assert.Equal(t, 1, c.calls)
}

type timeoutOnlyError struct{}

func (timeoutOnlyError) Error() string { return "transport timeout" }
func (timeoutOnlyError) Timeout() bool { return true }

func TestRetryableNicDeviceListError(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		retry bool
	}{
		{name: "service unavailable", err: apierrors.NewServiceUnavailable("apiserver restarting"), retry: true},
		{name: "server timeout", err: apierrors.NewServerTimeout(schema.GroupResource{Group: "nic.nvidia.com", Resource: "nicdevices"}, "list", 1), retry: true},
		{name: "transport timeout", err: timeoutOnlyError{}, retry: true},
		{name: "temporary DNS failure", err: &net.DNSError{Err: "temporary failure", Name: "api.example", IsTemporary: true}, retry: true},
		{name: "permanent DNS failure", err: &net.DNSError{Err: "no such host", Name: "api.example", IsNotFound: true}, retry: false},
		{name: "connection reset", err: syscall.ECONNRESET, retry: true},
		{name: "connection refused", err: syscall.ECONNREFUSED, retry: true},
		{name: "broken pipe", err: syscall.EPIPE, retry: true},
		{name: "end of file", err: io.EOF, retry: true},
		{name: "unexpected end of file", err: io.ErrUnexpectedEOF, retry: true},
		{name: "connection aborted", err: syscall.ECONNABORTED, retry: true},
		{name: "forbidden", err: apierrors.NewForbidden(nicop.GroupVersion.WithResource("nicdevices").GroupResource(), "worker-0", errors.New("denied")), retry: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.retry, retryableNicDeviceListError(tt.err))
		})
	}
}

func TestReadOnlyPublisherNodesDistinguishesExcludedAndMissing(t *testing.T) {
	expectedNodes := []string{"with-device", "missing-device", "no-nic", "restricted-only", "unverified"}
	deviceNodes := map[string]bool{"with-device": true}
	probeResults := map[string]mellanoxNICProbeResult{
		"missing-device":  mellanoxNICProbePresent,
		"no-nic":          mellanoxNICProbeNone,
		"restricted-only": mellanoxNICProbeRestricted,
		"unverified":      mellanoxNICProbeUnverified,
	}

	selection, err := readOnlyPublisherNodes(expectedNodes, deviceNodes, probeResults)

	require.NoError(t, err)
	assert.Equal(t, []string{"with-device", "missing-device"}, selection.publisherNodes)
	assert.Equal(t, []string{"no-nic", "restricted-only"}, selection.excludedNodes)
	assert.Equal(t, []string{"unverified"}, selection.unverifiedNodes)
}

func TestReadOnlyPublisherNodesRejectsUnverifiedMissingDevice(t *testing.T) {
	selection, err := readOnlyPublisherNodes(
		[]string{"worker-without-device"}, nil, nil,
	)

	require.ErrorContains(t, err, "NIC publisher eligibility on node")
	assert.Empty(t, selection.publisherNodes)
	assert.Empty(t, selection.excludedNodes)
	assert.Empty(t, selection.unverifiedNodes)
}

func TestProbeMissingReadOnlyPublishersRequiresRESTConfig(t *testing.T) {
	results, err := probeMissingReadOnlyPublishers(
		context.Background(), nil, "network-operator", []string{"worker-0"}, nil, nil, nil, false,
	)

	require.ErrorContains(t, err, "pass WithReadOnlyRESTConfig")
	assert.Nil(t, results)
}

func TestProbeMissingReadOnlyPublishersCanExcludeWithoutRESTConfig(t *testing.T) {
	results, err := probeMissingReadOnlyPublishers(
		context.Background(), nil, "network-operator", []string{"worker-0"}, nil, nil, nil, true,
	)

	require.NoError(t, err)
	assert.Equal(t, mellanoxNICProbeUnverified, results["worker-0"])
}

func TestProbeMissingReadOnlyPublishersDoesNotSuppressProbeErrors(t *testing.T) {
	probeErr := errors.New("pods/exec is forbidden")
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "daemon-worker-0", Namespace: "network-operator"},
		Spec: corev1.PodSpec{
			NodeName:   "worker-0",
			Containers: []corev1.Container{{Name: "daemon"}},
		},
	}
	results, err := probeMissingReadOnlyPublishers(
		context.Background(), &rest.Config{}, "network-operator", []string{"worker-0"},
		[]corev1.Pod{pod}, nil,
		func(context.Context, *rest.Config, string, string, string, []string) (string, error) {
			return "", probeErr
		}, true,
	)

	require.ErrorIs(t, err, probeErr)
	assert.Nil(t, results)
}

func TestReadOnlyNodeSelectorsSupportNICOnlyWorkers(t *testing.T) {
	nodes := []string{"worker-0", "worker-1"}
	labels := map[string]map[string]string{
		"worker-0": {readOnlyNICOperatorLabel: "true"},
		"worker-1": {readOnlyNICOperatorLabel: "true"},
	}

	var selected map[string]string
	for _, candidate := range readOnlyNodeSelectors(nodes, labels) {
		if selectorMatchesOnlyNodes(candidate, nodes, labels) {
			selected = candidate
			break
		}
	}

	assert.Equal(t, map[string]string{readOnlyNICOperatorLabel: "true"}, selected)
}

func TestEnsureReadOnlyNodeSelectorsRepairsUnsafeMultiGroupSelectors(t *testing.T) {
	groups := []config.ClusterConfig{
		{Identifier: "group-a", WorkerNodes: []string{"worker-a"}, NodeSelector: map[string]string{"pool": "shared"}},
		{Identifier: "group-b", WorkerNodes: []string{"worker-b"}, NodeSelector: map[string]string{"pool": "shared"}},
	}
	labels := map[string]map[string]string{
		"worker-a": {config.MachineLabelKey: "machine-a", "pool": "shared", "kubernetes.io/hostname": "worker-a"},
		"worker-b": {config.MachineLabelKey: "machine-b", "pool": "shared", "kubernetes.io/hostname": "worker-b"},
		"worker-c": {config.MachineLabelKey: "machine-c", "pool": "shared", "kubernetes.io/hostname": "worker-c"},
	}

	require.NoError(t, ensureReadOnlyNodeSelectors(groups, labels))
	assert.Equal(t, map[string]string{config.MachineLabelKey: "machine-a"}, groups[0].NodeSelector)
	assert.Equal(t, map[string]string{config.MachineLabelKey: "machine-b"}, groups[1].NodeSelector)
}

func TestEnsureReadOnlyNodeSelectorsRejectsSelectorMatchingOutsideNode(t *testing.T) {
	groups := []config.ClusterConfig{{
		Identifier:   "group-a",
		WorkerNodes:  []string{"worker-a", "worker-b"},
		NodeSelector: map[string]string{"pool": "shared"},
	}}
	labels := map[string]map[string]string{
		"worker-a": {config.MachineLabelKey: "same", "pool": "shared"},
		"worker-b": {config.MachineLabelKey: "same", "pool": "shared"},
		"worker-c": {config.MachineLabelKey: "same", "pool": "shared"},
	}

	err := ensureReadOnlyNodeSelectors(groups, labels)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not derive a safe node selector")
}

func TestReadOnlyNodeSelectorsTryLaterUniqueCandidate(t *testing.T) {
	nodes := []string{"worker-0", "worker-1"}
	labels := map[string]map[string]string{
		"worker-0": {config.MachineLabelKey: "shared", config.GPULabelKey: "group"},
		"worker-1": {config.MachineLabelKey: "shared", config.GPULabelKey: "group"},
		"worker-2": {config.MachineLabelKey: "shared", config.GPULabelKey: "other"},
	}

	var selected map[string]string
	for _, candidate := range readOnlyNodeSelectors(nodes, labels) {
		if selectorMatchesOnlyNodes(candidate, nodes, labels) {
			selected = candidate
			break
		}
	}

	assert.Equal(t, map[string]string{config.GPULabelKey: "group"}, selected)
}

func TestReadOnlyNodeSelectorsFailWhenNoCandidateIsSafe(t *testing.T) {
	nodes := []string{"worker-0", "worker-1"}
	labels := map[string]map[string]string{
		"worker-0": {config.MachineLabelKey: "shared", config.GPULabelKey: "group"},
		"worker-1": {config.MachineLabelKey: "shared", config.GPULabelKey: "group"},
		"worker-2": {config.MachineLabelKey: "shared", config.GPULabelKey: "group"},
	}

	for _, candidate := range readOnlyNodeSelectors(nodes, labels) {
		assert.False(t, selectorMatchesOnlyNodes(candidate, nodes, labels))
	}
}

func TestFindDaemonSetNamespacePrefersNetworkOperator(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	owner := []metav1.OwnerReference{{Kind: "DaemonSet", Name: nicconfigdaemon.DaemonSetName}}
	operatorPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "operator-daemon", Namespace: "nvidia-network-operator", OwnerReferences: owner,
	}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	launchKitPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "launch-kit-daemon", Namespace: nicconfigdaemon.Namespace, OwnerReferences: owner,
	}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(operatorPod, launchKitPod).Build()

	namespace, err := findDaemonSetNamespace(context.Background(), c, nicconfigdaemon.DaemonSetName, "")

	require.NoError(t, err)
	assert.Equal(t, "nvidia-network-operator", namespace)
}

func TestFindDaemonSetNamespaceUsesConfiguredNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	owner := []metav1.OwnerReference{{Kind: "DaemonSet", Name: nicconfigdaemon.DaemonSetName}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "custom-daemon", Namespace: "custom-network-operator", OwnerReferences: owner,
	}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	namespace, err := findDaemonSetNamespace(context.Background(), c, nicconfigdaemon.DaemonSetName, "custom-network-operator")

	require.NoError(t, err)
	assert.Equal(t, "custom-network-operator", namespace)
}

func TestFindDaemonSetNamespaceSkipsUnreadyPreferredNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	owner := []metav1.OwnerReference{{Kind: "DaemonSet", Name: nicconfigdaemon.DaemonSetName}}
	unready := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "operator-daemon", Namespace: "nvidia-network-operator", OwnerReferences: owner,
	}}
	ready := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "bootstrap-daemon", Namespace: nicconfigdaemon.Namespace, OwnerReferences: owner,
	}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(unready, ready).Build()

	namespace, err := findDaemonSetNamespace(context.Background(), c, nicconfigdaemon.DaemonSetName, "")

	require.NoError(t, err)
	assert.Equal(t, nicconfigdaemon.Namespace, namespace)
}

func TestWaitForDaemonSetPodsReturnsFallbackNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	owner := []metav1.OwnerReference{{Kind: "DaemonSet", Name: nicconfigdaemon.DaemonSetName}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "fallback-daemon", Namespace: "network-operator", OwnerReferences: owner,
		},
		Spec: corev1.PodSpec{NodeName: "worker-0"},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	nodes, pods, namespace, err := waitForDaemonSetPods(
		context.Background(), c, ui.NewSilent(), "nvidia-network-operator", nicconfigdaemon.DaemonSetName, time.Second,
	)

	require.NoError(t, err)
	assert.Equal(t, []string{"worker-0"}, nodes)
	assert.Len(t, pods, 1)
	assert.Equal(t, "network-operator", namespace)
}

type deadlineRecordingClient struct {
	client.Client
	deadline time.Time
}

func (c *deadlineRecordingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if deadline, ok := ctx.Deadline(); ok {
		c.deadline = deadline
	}
	return c.Client.List(ctx, list, opts...)
}

type deadlineNodeListClient struct {
	client.Client
	nodeListCalls int
}

func (c *deadlineNodeListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.NodeList); ok {
		c.nodeListCalls++
		<-ctx.Done()
		return ctx.Err()
	}
	return c.Client.List(ctx, list, opts...)
}

func TestWaitForDaemonSetPodsAppliesTimeoutWithParentDeadline(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ready-daemon", Namespace: "network-operator",
			OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Name: nicconfigdaemon.DaemonSetName}},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	c := &deadlineRecordingClient{Client: base}
	parentDeadline := time.Now().Add(time.Hour)
	parentCtx, cancel := context.WithDeadline(context.Background(), parentDeadline)
	defer cancel()

	_, _, _, err := waitForDaemonSetPods(
		parentCtx, c, ui.NewSilent(), "network-operator", nicconfigdaemon.DaemonSetName, time.Minute,
	)

	require.NoError(t, err)
	assert.True(t, c.deadline.Before(parentDeadline), "the daemon wait should retain its own timeout")
}

func TestWaitForDaemonSetPodsDoesNotReturnPartialSuccessAfterParentDeadline(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	owner := []metav1.OwnerReference{{Kind: "DaemonSet", Name: nicconfigdaemon.DaemonSetName}}
	ready := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-daemon", Namespace: "network-operator", OwnerReferences: owner},
		Spec:       corev1.PodSpec{NodeName: "ready-node"},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-daemon", Namespace: "network-operator", OwnerReferences: owner},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ready, pending).Build()
	parentCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(25*time.Millisecond))
	defer cancel()

	_, _, _, err := waitForDaemonSetPods(
		parentCtx, c, ui.NewSilent(), "network-operator", nicconfigdaemon.DaemonSetName, time.Minute,
	)

	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestWaitForDaemonSetPodsPreservesPartialResultsOnItsOwnTimeout(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	owner := []metav1.OwnerReference{{Kind: "DaemonSet", Name: nicconfigdaemon.DaemonSetName}}
	ready := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-daemon", Namespace: "network-operator", OwnerReferences: owner},
		Spec:       corev1.PodSpec{NodeName: "ready-node"},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-daemon", Namespace: "network-operator", OwnerReferences: owner},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ready, pending).Build()

	nodes, pods, namespace, err := waitForDaemonSetPods(
		context.Background(), c, ui.NewSilent(), "network-operator", nicconfigdaemon.DaemonSetName, 20*time.Millisecond,
	)

	require.NoError(t, err)
	require.Len(t, pods, 1)
	assert.Equal(t, []string{"ready-daemon"}, []string{pods[0].Name})
	assert.Equal(t, []string{"ready-node"}, nodes)
	assert.Equal(t, "network-operator", namespace)
}

func TestWaitForDaemonSetPodsPreservesReadyPodsWhenNodeLookupTimesOut(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	owner := []metav1.OwnerReference{{Kind: "DaemonSet", Name: nicconfigdaemon.DaemonSetName}}
	ready := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-daemon", Namespace: "network-operator", OwnerReferences: owner},
		Spec:       corev1.PodSpec{NodeName: "ready-node"},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-daemon", Namespace: "network-operator", OwnerReferences: owner},
		Spec:       corev1.PodSpec{NodeName: "pending-node"},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ready, pending).Build()
	c := &deadlineNodeListClient{Client: base}

	nodes, pods, namespace, err := waitForDaemonSetPods(
		context.Background(), c, ui.NewSilent(), "network-operator", nicconfigdaemon.DaemonSetName, 20*time.Millisecond,
	)

	require.NoError(t, err)
	assert.Equal(t, 1, c.nodeListCalls)
	assert.Equal(t, []string{"ready-node"}, nodes)
	require.Len(t, pods, 1)
	assert.Equal(t, "ready-daemon", pods[0].Name)
	assert.Equal(t, "network-operator", namespace)
}

func TestWaitForDaemonSetPodsSkipsPodsOnNotReadyNodes(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	owner := []metav1.OwnerReference{{Kind: "DaemonSet", Name: nicconfigdaemon.DaemonSetName}}
	ready := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-daemon", Namespace: "network-operator", OwnerReferences: owner},
		Spec:       corev1.PodSpec{NodeName: "ready-node"},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-daemon", Namespace: "network-operator", OwnerReferences: owner},
		Spec:       corev1.PodSpec{NodeName: "not-ready-node"},
	}
	notReadyNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "not-ready-node"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ready, pending, notReadyNode).Build()

	st, err := checkDaemonSetPodsReady(context.Background(), c, "network-operator", nicconfigdaemon.DaemonSetName)
	require.NoError(t, err)
	require.NoError(t, excludePodsOnNotReadyNodes(context.Background(), c, &st))

	assert.Equal(t, 2, st.total)
	assert.Equal(t, 1, st.ready)
	assert.Equal(t, 1, st.notReadyNodePods)
	assert.True(t, st.canProceed())
	assert.Equal(t, st.total, st.ready+st.stuck+st.notReadyNodePods,
		"a pod on a NotReady node should not keep the readiness wait open")
}

type daemonNamespaceFallbackClient struct {
	client.Client
	primaryPod  corev1.Pod
	fallback    []corev1.Pod
	primaryCall int
}

func (c *daemonNamespaceFallbackClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	pods, ok := list.(*corev1.PodList)
	if !ok {
		return c.Client.List(ctx, list, opts...)
	}
	listOpts := &client.ListOptions{}
	for _, opt := range opts {
		opt.ApplyToList(listOpts)
	}
	switch listOpts.Namespace {
	case "nvidia-network-operator":
		c.primaryCall++
		if c.primaryCall == 1 {
			pods.Items = []corev1.Pod{c.primaryPod}
		}
	case "network-operator":
		pods.Items = append([]corev1.Pod(nil), c.fallback...)
	}
	return nil
}

func TestDiscoverReadOnlyProbesInSelectedFallbackNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, nicop.AddToScheme(scheme))
	owner := []metav1.OwnerReference{{Kind: "DaemonSet", Name: nicconfigdaemon.DaemonSetName}}
	readyPod := func(name, namespace, node string) corev1.Pod {
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, OwnerReferences: owner},
			Spec:       corev1.PodSpec{NodeName: node},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}}},
		}
	}
	primaryPod := readyPod("primary-daemon", "nvidia-network-operator", "primary-worker")
	fallbackPods := []corev1.Pod{
		readyPod("fallback-worker-0", "network-operator", "worker-0"),
		readyPod("fallback-worker-1", "network-operator", "worker-1"),
	}
	device := &nicop.NicDevice{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1-device", Namespace: "default"},
		Status: nicop.NicDeviceStatus{
			Node: "worker-1", Type: "1023", PartNumber: "pn-test",
			Ports: []nicop.NicDevicePortSpec{{PCI: "0000:18:00.0", RdmaInterface: "mlx5_0", NetworkInterface: "eth0"}},
		},
	}
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-0"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}},
		device,
	).Build()
	c := &daemonNamespaceFallbackClient{Client: baseClient, primaryPod: primaryPod, fallback: fallbackPods}
	var execNamespace string

	_, err := DiscoverReadOnly(context.Background(), c,
		WithReadOnlyRESTConfig(&rest.Config{}),
		withReadOnlyPodExec(func(_ context.Context, _ *rest.Config, namespace, _, _ string, _ []string) (string, error) {
			execNamespace = namespace
			return "present\n", nil
		}),
	)

	require.ErrorContains(t, err, "NicDevice resources are missing for eligible publisher nodes")
	assert.Equal(t, "network-operator", execNamespace)
}

func TestDiscoverReadOnlyDoesNotMutateCluster(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, nicop.AddToScheme(scheme))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nic-configuration-daemon-worker-0",
			Namespace: "nvidia-network-operator",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "DaemonSet", Name: nicconfigdaemon.DaemonSetName},
			},
		},
		Spec: corev1.PodSpec{NodeName: "worker-0"},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Labels: map[string]string{
		"nvidia.com/gpu.product": "NVIDIA-H100",
	}}}
	device := &nicop.NicDevice{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0-device", Namespace: "default"},
		Status: nicop.NicDeviceStatus{
			Node:       "worker-0",
			Type:       "1023",
			PartNumber: "pn-test",
			Ports: []nicop.NicDevicePortSpec{{
				PCI:              "0000:18:00.0",
				RdmaInterface:    "mlx5_0",
				NetworkInterface: "eth0",
			}},
		},
	}
	stale := &nicop.NicDevice{ObjectMeta: metav1.ObjectMeta{Name: "stale-device", Namespace: "default"},
		Status: nicop.NicDeviceStatus{Node: "removed-worker", Type: "1023"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, node, device, stale).Build()

	cfg, err := DiscoverReadOnly(context.Background(), c)

	require.NoError(t, err)
	require.Len(t, cfg.ClusterConfig, 1)
	assert.Equal(t, "worker-0", cfg.ClusterConfig[0].WorkerNodes[0])
	assert.Equal(t, map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"},
		cfg.ClusterConfig[0].NodeSelector)
	assert.Equal(t, map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, node.Labels,
		"read-only discovery must not write Launch Kit labels")
	assert.False(t, hasNamespace(t, c, nicconfigdaemon.Namespace),
		"read-only discovery must not create or delete the daemon namespace")
}

func TestDiscoverReadOnlyCanExcludeUnverifiedPublishers(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, nicop.AddToScheme(scheme))
	owner := []metav1.OwnerReference{{Kind: "DaemonSet", Name: nicconfigdaemon.DaemonSetName}}
	readyPod := func(name, node string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       "nvidia-network-operator",
				OwnerReferences: owner,
			},
			Spec: corev1.PodSpec{NodeName: node},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}}},
		}
	}
	device := &nicop.NicDevice{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0-device", Namespace: "default"},
		Status: nicop.NicDeviceStatus{
			Node:       "worker-0",
			Type:       "1023",
			PartNumber: "pn-test",
			Ports: []nicop.NicDevicePortSpec{{
				PCI:              "0000:18:00.0",
				RdmaInterface:    "mlx5_0",
				NetworkInterface: "eth0",
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		readyPod("daemon-worker-0", "worker-0"),
		readyPod("daemon-worker-1", "worker-1"),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Labels: map[string]string{
			"nvidia.com/gpu.product": "NVIDIA-H100",
		}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}},
		device,
	).Build()
	var output bytes.Buffer
	ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&output))

	_, err := DiscoverReadOnly(ctx, c)
	require.ErrorContains(t, err, "pass WithReadOnlyRESTConfig")

	cfg, err := DiscoverReadOnly(ctx, c, WithReadOnlyAssumeUnverifiedExcluded())

	require.NoError(t, err)
	require.Len(t, cfg.ClusterConfig, 1)
	assert.Equal(t, []string{"worker-0"}, cfg.ClusterConfig[0].WorkerNodes)
	assert.Contains(t, output.String(), "unverified")
}

func readOnlyClientWithNoSafeGroupSelectors(t *testing.T) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, nicop.AddToScheme(scheme))

	objects := make([]client.Object, 0, 9)
	for _, node := range []string{"worker-a", "worker-b", "worker-outlier"} {
		objects = append(objects,
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node, Labels: map[string]string{
				"nvidia.com/gpu.product": "NVIDIA-H100",
			}}},
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nic-configuration-daemon-" + node,
					Namespace: "nvidia-network-operator",
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "DaemonSet",
						Name: nicconfigdaemon.DaemonSetName,
					}},
				},
				Spec: corev1.PodSpec{NodeName: node},
				Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
					Type: corev1.PodReady, Status: corev1.ConditionTrue,
				}}},
			},
		)
	}
	objects = append(objects,
		&nicop.NicDevice{ObjectMeta: metav1.ObjectMeta{Name: "worker-a-device"}, Status: nicop.NicDeviceStatus{
			Node: "worker-a", Type: "1023", Ports: []nicop.NicDevicePortSpec{{PCI: "0000:18:00.0"}},
		}},
		&nicop.NicDevice{ObjectMeta: metav1.ObjectMeta{Name: "worker-b-device"}, Status: nicop.NicDeviceStatus{
			Node: "worker-b", Type: "1023", Ports: []nicop.NicDevicePortSpec{{PCI: "0000:18:00.0"}},
		}},
		&nicop.NicDevice{ObjectMeta: metav1.ObjectMeta{Name: "worker-outlier-device"}, Status: nicop.NicDeviceStatus{
			Node: "worker-outlier", Type: "1017", Ports: []nicop.NicDevicePortSpec{{PCI: "0000:51:00.0"}},
		}},
	)

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestDiscoverReadOnlyRequiresOptInForMissingSafeSelector(t *testing.T) {
	t.Run("strict by default", func(t *testing.T) {
		c := readOnlyClientWithNoSafeGroupSelectors(t)

		_, err := DiscoverReadOnly(context.Background(), c)

		require.ErrorContains(t, err, `could not derive a safe node selector for discovered group`)
	})

	t.Run("returns inventory when opted in", func(t *testing.T) {
		c := readOnlyClientWithNoSafeGroupSelectors(t)
		var warnings bytes.Buffer
		ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&warnings))

		cfg, err := DiscoverReadOnly(ctx, c, WithReadOnlyAllowMissingNodeSelector())

		require.NoError(t, err)
		require.Len(t, cfg.ClusterConfig, 2)
		groupsByWorkers := make(map[string]config.ClusterConfig, len(cfg.ClusterConfig))
		for _, group := range cfg.ClusterConfig {
			groupsByWorkers[strings.Join(group.WorkerNodes, ",")] = group
		}
		for i, workers := range []string{"worker-a,worker-b", "worker-outlier"} {
			group, ok := groupsByWorkers[workers]
			require.True(t, ok, "expected discovered group with workers %s", workers)
			assert.Empty(t, group.NodeSelector)
			assert.Contains(t, warnings.String(), fmt.Sprintf("group %q", group.Identifier))
			assert.Equal(t, fmt.Sprintf("group-%d", i), group.Identifier)
		}
		assert.Equal(t, 2, strings.Count(warnings.String(), "WorkerNodes is authoritative"))
	})
}

func hasNamespace(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	ns := &corev1.Namespace{}
	return c.Get(context.Background(), client.ObjectKey{Name: name}, ns) == nil
}

func TestDiscoverReadOnlyAssignsVRRailsFromNetworkBayStatus(t *testing.T) {
	for _, missingStatus := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing-status-%t", missingStatus), func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, nicop.AddToScheme(scheme))
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a", Labels: map[string]string{"nvidia.com/gpu.machine": "VR"}}}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "daemon-worker-a", Namespace: "nvidia-network-operator",
					OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Name: nicconfigdaemon.DaemonSetName}},
				},
				Spec: corev1.PodSpec{NodeName: "worker-a"},
				Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				}},
			}
			devices := vrRailTestDevices("worker-a")
			if missingStatus {
				devices[0].Status.NetworkBay = nil
			}
			objects := []client.Object{node, pod}
			for i := range devices {
				objects = append(objects, &devices[i])
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			cfg, err := DiscoverReadOnly(context.Background(), c)
			if missingStatus {
				require.ErrorContains(t, err, "no NetworkBay status")
				require.Nil(t, cfg)
				return
			}
			require.NoError(t, err)
			require.Len(t, cfg.ClusterConfig, 1)
			assertVRTestRails(t, cfg.ClusterConfig[0])
		})
	}
}
