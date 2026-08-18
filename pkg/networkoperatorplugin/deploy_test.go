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

func TestSniffKind(t *testing.T) {
	t.Run("NicClusterPolicy", func(t *testing.T) {
		doc := []byte(`apiVersion: mellanox.com/v1alpha1
kind: NicClusterPolicy
metadata:
  name: nic-cluster-policy`)
		assert.Equal(t, "NicClusterPolicy", sniffKind(doc))
	})

	t.Run("NicNodePolicy", func(t *testing.T) {
		doc := []byte(`apiVersion: mellanox.com/v1alpha1
kind: NicNodePolicy
metadata:
  name: pool-a`)
		assert.Equal(t, "NicNodePolicy", sniffKind(doc))
	})

	t.Run("other kind", func(t *testing.T) {
		doc := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: my-config`)
		assert.Equal(t, "ConfigMap", sniffKind(doc))
	})

	t.Run("invalid YAML returns empty", func(t *testing.T) {
		assert.Equal(t, "", sniffKind([]byte("not: [valid: yaml: {")))
	})

	t.Run("empty returns empty", func(t *testing.T) {
		assert.Equal(t, "", sniffKind([]byte("")))
	})
}

func TestContainsNicClusterPolicyKind(t *testing.T) {
	ncp := []byte(`kind: NicClusterPolicy`)
	nnp := []byte(`kind: NicNodePolicy`)
	other := []byte(`kind: ConfigMap`)

	assert.True(t, containsNicClusterPolicyKind(ncp))
	assert.False(t, containsNicClusterPolicyKind(nnp))
	assert.False(t, containsNicClusterPolicyKind(other))
}

func TestContainsNicNodePolicyKind(t *testing.T) {
	ncp := []byte(`kind: NicClusterPolicy`)
	nnp := []byte(`kind: NicNodePolicy`)
	other := []byte(`kind: ConfigMap`)

	assert.False(t, containsNicNodePolicyKind(ncp))
	assert.True(t, containsNicNodePolicyKind(nnp))
	assert.False(t, containsNicNodePolicyKind(other))
}

func TestSplitYAMLDocuments(t *testing.T) {
	t.Run("separates NCP and NNP docs", func(t *testing.T) {
		input := `---
kind: NicClusterPolicy
metadata:
  name: nic-cluster-policy
---
kind: NicNodePolicy
metadata:
  name: pool-a
---
kind: IPPool
metadata:
  name: my-pool`

		docs := splitYAMLDocuments(input)
		assert.Len(t, docs, 3)

		ncpCount, nnpCount, otherCount := 0, 0, 0
		for _, doc := range docs {
			b := []byte(doc)
			if containsNicClusterPolicyKind(b) {
				ncpCount++
			} else if containsNicNodePolicyKind(b) {
				nnpCount++
			} else {
				otherCount++
			}
		}
		assert.Equal(t, 1, ncpCount)
		assert.Equal(t, 1, nnpCount)
		assert.Equal(t, 1, otherCount)
	})

	t.Run("multiple NNP docs", func(t *testing.T) {
		input := `---
kind: NicNodePolicy
metadata:
  name: pool-a
---
kind: NicNodePolicy
metadata:
  name: pool-b`

		docs := splitYAMLDocuments(input)
		assert.Len(t, docs, 2)
		for _, doc := range docs {
			assert.True(t, containsNicNodePolicyKind([]byte(doc)))
		}
	})
}

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
