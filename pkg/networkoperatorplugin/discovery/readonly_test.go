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
	"context"
	"errors"
	"testing"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/nicconfigdaemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

func hasNamespace(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	ns := &corev1.Namespace{}
	return c.Get(context.Background(), client.ObjectKey{Name: name}, ns) == nil
}
