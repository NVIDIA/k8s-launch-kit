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
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestManifestRetryableErrorTimeout(t *testing.T) {
	interfaceTemplate := &unstructured.Unstructured{}
	interfaceTemplate.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "configuration.net.nvidia.com",
		Version: "v1alpha1",
		Kind:    "NicInterfaceNameTemplate",
	})
	assert.Equal(t, 5*time.Minute, manifestRetryableErrorTimeout(interfaceTemplate))

	configMap := &unstructured.Unstructured{}
	configMap.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	assert.Zero(t, manifestRetryableErrorTimeout(configMap))
	assert.Zero(t, manifestRetryableErrorTimeout(nil))
}

func TestPollUntilTerminal_InterfaceNameMismatchTimesOut(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   "configuration.net.nvidia.com",
		Version: "v1alpha1",
		Kind:    "NicInterfaceNameTemplate",
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName("nic-rename")

	registry := crstate.NewRegistry()
	registry.Register(gvk, func(context.Context, client.Client, *unstructured.Unstructured) (crstate.Result, error) {
		return crstate.Result{
			State:     crstate.StateError,
			Reason:    "worker-1/0000:05:00.0: interface name mismatch",
			Retryable: true,
		}, nil
	})

	err := pollUntilTerminalWithRetryableErrorTimeout(
		context.Background(), nil, registry, obj,
		"NicInterfaceNameTemplate/nic-rename", "", 10*time.Millisecond, time.Millisecond,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out after 10ms")
	assert.Contains(t, err.Error(), "interface name mismatch")
}

func TestPollUntilTerminal_InterfaceNameInitializationDoesNotUseMismatchTimeout(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   "configuration.net.nvidia.com",
		Version: "v1alpha1",
		Kind:    "NicInterfaceNameTemplate",
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName("nic-rename")

	registry := crstate.NewRegistry()
	registry.Register(gvk, func(context.Context, client.Client, *unstructured.Unstructured) (crstate.Result, error) {
		return crstate.Result{
			State:  crstate.StateInProgress,
			Reason: "waiting for nic-configuration-operator to discover devices",
		}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := pollUntilTerminalWithRetryableErrorTimeout(
		ctx, nil, registry, obj,
		"NicInterfaceNameTemplate/nic-rename", "", 10*time.Millisecond, time.Millisecond,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestPollUntilTerminal_ClearedInterfaceNameMismatchStopsMismatchTimeout(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   "configuration.net.nvidia.com",
		Version: "v1alpha1",
		Kind:    "NicInterfaceNameTemplate",
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName("nic-rename")

	validationCalls := 0
	registry := crstate.NewRegistry()
	registry.Register(gvk, func(context.Context, client.Client, *unstructured.Unstructured) (crstate.Result, error) {
		validationCalls++
		if validationCalls == 1 {
			return crstate.Result{
				State:     crstate.StateError,
				Reason:    "worker-1/0000:05:00.0: interface name mismatch",
				Retryable: true,
			}, nil
		}
		return crstate.Result{
			State:  crstate.StateInProgress,
			Reason: "worker-2: waiting for InterfaceNameApplied condition",
		}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := pollUntilTerminalWithRetryableErrorTimeout(
		ctx, nil, registry, obj,
		"NicInterfaceNameTemplate/nic-rename", "", 10*time.Millisecond, time.Millisecond,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Greater(t, validationCalls, 1)
}
