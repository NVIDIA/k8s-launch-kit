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
	"strings"
	"testing"
	"time"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/nicconfigdaemon"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func bootstrapHealthPod(name, node string, status corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nicconfigdaemon.Namespace,
			OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Name: nicconfigdaemon.DaemonSetName}}},
		Spec:   corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "daemon"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{status}},
	}
}

func crashLoopDaemonStatus() corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: "daemon", RestartCount: 2,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 3}},
	}
}

func TestBootstrapDaemonObserverRefreshesFailuresAndIgnoresOtherPods(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	ready := corev1.ContainerStatus{Name: "daemon", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
	pod := bootstrapHealthPod("daemon-missing", "missing", ready)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0},
	}}}
	healthyPeer := bootstrapHealthPod("daemon-published", "published", crashLoopDaemonStatus())
	foreign := bootstrapHealthPod("foreign", "missing", crashLoopDaemonStatus())
	foreign.OwnerReferences[0].Name = "another-daemon"
	otherNamespace := bootstrapHealthPod("other-namespace", "missing", crashLoopDaemonStatus())
	otherNamespace.Namespace = "other"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, healthyPeer, foreign, otherNamespace).Build()
	var messages bytes.Buffer
	observe := newBootstrapDaemonObserver(c, ui.NewWithWriter(&messages))
	observe(context.Background(), []string{"missing"})
	assert.Empty(t, messages.String())

	pod.Status.ContainerStatuses = []corev1.ContainerStatus{crashLoopDaemonStatus()}
	require.NoError(t, c.Status().Update(context.Background(), pod))
	observe(context.Background(), []string{"missing"})
	observe(context.Background(), []string{"missing"})
	assert.Equal(t, 1, strings.Count(messages.String(), "waiting: CrashLoopBackOff"))
	assert.Contains(t, messages.String(), "on missing, pod daemon-missing, container daemon")
	assert.Contains(t, messages.String(), "last termination: Error (exit code 3)")
	assert.Contains(t, messages.String(), "--previous")
	assert.Contains(t, messages.String(), "--keep-namespace")
	assert.NotContains(t, messages.String(), "daemon-published")
	assert.NotContains(t, messages.String(), "foreign")
	assert.NotContains(t, messages.String(), "other-namespace")

	recovered := crashLoopDaemonStatus()
	recovered.Ready = true
	recovered.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{recovered}
	require.NoError(t, c.Status().Update(context.Background(), pod))
	observe(context.Background(), []string{"missing"})
	assert.Contains(t, messages.String(), "restarted; last termination: Error (exit code 3)")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{crashLoopDaemonStatus()}
	pod.Status.ContainerStatuses[0].RestartCount = recovered.RestartCount + 1
	require.NoError(t, c.Status().Update(context.Background(), pod))
	observe(context.Background(), []string{"missing"})
	assert.Equal(t, 2, strings.Count(messages.String(), "waiting: CrashLoopBackOff"), "a later recurrence after recovery is a new failure state")
}

func TestBootstrapDaemonWaitAllowsRecoveryAndReportsOnlyPendingPublishers(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, nicop.AddToScheme(scheme))
	deviceLists, podLists := 0, 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		bootstrapHealthPod("daemon-missing", "missing", crashLoopDaemonStatus()),
		bootstrapHealthPod("daemon-published", "published", crashLoopDaemonStatus()),
	).WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		switch out := list.(type) {
		case *nicop.NicDeviceList:
			deviceLists++
			out.Items = []nicop.NicDevice{{Status: nicop.NicDeviceStatus{Node: "published"}}, {Status: nicop.NicDeviceStatus{Node: "unrelated"}}}
			if deviceLists == 3 {
				out.Items = append(out.Items, nicop.NicDevice{Status: nicop.NicDeviceStatus{Node: "missing"}})
			}
			return nil
		case *corev1.PodList:
			podLists++
		}
		return c.List(ctx, list, opts...)
	}}).Build()
	var messages bytes.Buffer
	ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&messages))
	err := waitNicDevicesDiscoveredWithObserver(ctx, c, []string{"published", "missing"}, time.Millisecond, newBootstrapDaemonObserver(c, ui.FromContext(ctx)))
	require.NoError(t, err)
	assert.Equal(t, 3, deviceLists)
	assert.Equal(t, 2, podLists)
	assert.Equal(t, 1, strings.Count(messages.String(), "waiting: CrashLoopBackOff"))
	assert.Contains(t, messages.String(), "Discovered devices on 1/2 node(s)")
	assert.NotContains(t, messages.String(), "pod daemon-published")
}

func TestBootstrapDaemonObserverReportsReplacementPodWithSameName(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	pod := bootstrapHealthPod("daemon", "missing", crashLoopDaemonStatus())
	pod.UID = "original"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	var messages bytes.Buffer
	observe := newBootstrapDaemonObserver(c, ui.NewWithWriter(&messages))
	observe(context.Background(), []string{"missing"})
	require.NoError(t, c.Delete(context.Background(), pod))
	pod.ResourceVersion = ""
	pod.UID = "replacement"
	require.NoError(t, c.Create(context.Background(), pod))
	observe(context.Background(), []string{"missing"})
	assert.Equal(t, 2, strings.Count(messages.String(), "waiting: CrashLoopBackOff"), "a replacement pod is a new source even when its failure and name match")
}

func TestBootstrapDaemonObserverBoundsInspectionAndHonorsCancellation(t *testing.T) {
	for _, parentHasDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "five second bound", true: "shorter parent deadline"}[parentHasDeadline], func(t *testing.T) {
			parentCtx, cancel := context.WithCancel(context.Background())
			if parentHasDeadline {
				cancel()
				parentCtx, cancel = context.WithTimeout(context.Background(), time.Second)
			}
			defer cancel()
			started := time.Now()
			inspections := 0
			c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				inspections++
				deadline, ok := ctx.Deadline()
				require.True(t, ok, "health inspection must have its own bound")
				if parentHasDeadline {
					parentDeadline, _ := parentCtx.Deadline()
					assert.Equal(t, parentDeadline, deadline)
				} else {
					assert.WithinDuration(t, started.Add(5*time.Second), deadline, 100*time.Millisecond)
				}
				cancel()
				<-ctx.Done()
				return ctx.Err()
			}}).Build()
			var messages bytes.Buffer
			newBootstrapDaemonObserver(c, ui.NewWithWriter(&messages))(parentCtx, []string{"missing"})
			assert.Equal(t, 1, inspections)
			assert.Contains(t, messages.String(), "context canceled")
			assert.Contains(t, messages.String(), "daemon health is unverified")
		})
	}
}

func TestBootstrapDaemonInspectionErrorDoesNotMaskDiscoveryError(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, nicop.AddToScheme(scheme))
	primary := errors.New("cannot list NicDevice resources")
	deviceLists := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
		if _, ok := list.(*corev1.PodList); ok {
			return errors.New("pod health forbidden")
		}
		deviceLists++
		if deviceLists == 3 {
			return primary
		}
		return nil
	}}).Build()
	var messages bytes.Buffer
	ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&messages))
	err := waitNicDevicesDiscoveredWithObserver(ctx, c, []string{"missing"}, time.Millisecond, newBootstrapDaemonObserver(c, ui.FromContext(ctx)))
	require.ErrorIs(t, err, primary)
	assert.Equal(t, 1, strings.Count(messages.String(), "pod health forbidden"))
	assert.Contains(t, messages.String(), "daemon health is unverified")
	assert.NotContains(t, messages.String(), "Found")
}

func TestNicDeviceWaitWithoutObserverDoesNotInspectPods(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, nicop.AddToScheme(scheme))
	deviceLists, podLists := 0, 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
		if out, ok := list.(*nicop.NicDeviceList); ok {
			deviceLists++
			if deviceLists == 2 {
				out.Items = []nicop.NicDevice{{Status: nicop.NicDeviceStatus{Node: "worker"}}}
			}
			return nil
		}
		podLists++
		return errors.New("unexpected pod inspection")
	}}).Build()
	require.NoError(t, waitNicDevicesDiscoveredWithInterval(context.Background(), c, []string{"worker"}, time.Millisecond))
	assert.Equal(t, 2, deviceLists)
	assert.Zero(t, podLists)
}

func TestNicDeviceWaitTimeoutNamesMissingPublishers(t *testing.T) {
	for _, retryError := range []bool{false, true} {
		t.Run(map[bool]string{false: "successful list", true: "retryable list error"}[retryError], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			listErr := apierrors.NewServiceUnavailable("API restarting")
			c := &sequenceListClient{
				Client:    fake.NewClientBuilder().Build(),
				items:     []nicop.NicDevice{{Status: nicop.NicDeviceStatus{Node: "published"}}},
				afterCall: func(int) { cancel() },
			}
			if retryError {
				c.persistentErr = listErr
			}
			err := waitNicDevicesDiscoveredWithInterval(ctx, c, []string{"worker-b", "published", "worker-a"}, time.Millisecond)
			require.Error(t, err)
			if retryError {
				assert.ErrorIs(t, err, listErr)
				assert.Contains(t, err.Error(), "missing: published, worker-a, worker-b")
			} else {
				assert.Contains(t, err.Error(), "missing: worker-a, worker-b")
				assert.NotContains(t, err.Error(), "missing: published")
			}
		})
	}
}

func TestDaemonContainerFailureReportsCurrentTermination(t *testing.T) {
	status := corev1.ContainerStatus{Name: "daemon", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137},
	}}
	assert.Equal(t, "terminated: OOMKilled (exit code 137)", daemonContainerFailure(status))
	status = crashLoopDaemonStatus()
	status.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	assert.Equal(t, "restarted; last termination: Error (exit code 3)", daemonContainerFailure(status))
	status.Name = "completed-init"
	status.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0}}
	assert.Empty(t, daemonContainerFailure(status), "successful init completion is not a current failure, even after an earlier retry")
}

func TestFilterNodesWithNICsReportsUnverifiedNodes(t *testing.T) {
	for _, missingPod := range []bool{false, true} {
		t.Run(map[bool]string{false: "probe fails", true: "missing pod"}[missingPod], func(t *testing.T) {
			var messages bytes.Buffer
			pods := []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "daemon"}, Spec: corev1.PodSpec{NodeName: "worker", Containers: []corev1.Container{{Name: "daemon"}}}}}
			if missingPod {
				pods = nil
			}
			withNICs, restricted := filterNodesWithNICs(context.Background(), nil, nicconfigdaemon.Namespace, []string{"worker"}, pods, ui.NewWithWriter(&messages),
				func(context.Context, *rest.Config, string, string, string, []string) (string, error) {
					return "", errors.New("container is restarting")
				})
			assert.Empty(t, withNICs)
			assert.Empty(t, restricted)
			assert.Contains(t, messages.String(), "worker")
			assert.Contains(t, messages.String(), "unverified node")
			assert.Contains(t, messages.String(), "inventory may be incomplete")
			assert.NotContains(t, messages.String(), "host-restricted")
			if !missingPod {
				assert.Contains(t, messages.String(), "daemon pod daemon")
				assert.Contains(t, messages.String(), "container is restarting")
			}
		})
	}
}
