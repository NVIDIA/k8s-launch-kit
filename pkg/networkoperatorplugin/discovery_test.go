// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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

package networkoperatorplugin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCheckDaemonSetPodsReady_NoPodsFound(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, _, err := checkDaemonSetPodsReady(context.Background(), c, "custom-namespace", "nic-configuration-daemon")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no pods found for DaemonSet")
	assert.Contains(t, err.Error(), "custom-namespace")
	assert.Contains(t, err.Error(), "--network-operator-namespace")
}

func TestCheckDaemonSetPodsReady_PodsInCorrectNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nic-configuration-daemon-abc",
			Namespace: "network-operator",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "DaemonSet", Name: "nic-configuration-daemon"},
			},
		},
		Spec: corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	nodes, pods, err := checkDaemonSetPodsReady(context.Background(), c, "network-operator", "nic-configuration-daemon")
	require.NoError(t, err)
	assert.Equal(t, []string{"node-1"}, nodes)
	assert.Len(t, pods, 1)
}

func TestCheckDaemonSetPodsReady_PodNotReady(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nic-configuration-daemon-abc",
			Namespace: "nvidia-network-operator",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "DaemonSet", Name: "nic-configuration-daemon"},
			},
		},
		Spec: corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	_, _, err := checkDaemonSetPodsReady(context.Background(), c, "nvidia-network-operator", "nic-configuration-daemon")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not Ready")
}

func TestAlternateNamespace(t *testing.T) {
	assert.Equal(t, "network-operator", alternateNamespace("nvidia-network-operator"))
	assert.Equal(t, "nvidia-network-operator", alternateNamespace("network-operator"))
	assert.Equal(t, "", alternateNamespace("custom-namespace"))
}
