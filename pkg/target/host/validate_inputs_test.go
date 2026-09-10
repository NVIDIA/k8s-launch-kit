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

package host

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/connectivity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestHasDeploymentValidationInputs(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "60-example-daemonset.yaml"), []byte("example"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cluster-config.yaml"), []byte("config"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("notes"), 0o600))

	hasInputs, err := hasDeploymentValidationInputs(dir)
	require.NoError(t, err)
	assert.False(t, hasInputs)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "values.yaml"), []byte("operator: {}"), 0o600))
	hasInputs, err = hasDeploymentValidationInputs(dir)
	require.NoError(t, err)
	assert.True(t, hasInputs)

	require.NoError(t, os.Remove(filepath.Join(dir, "values.yaml")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "20-sriov-network.yaml"), []byte("kind: SriovNetwork"), 0o600))
	hasInputs, err = hasDeploymentValidationInputs(dir)
	require.NoError(t, err)
	assert.True(t, hasInputs)
}

func TestValidateConnectivityDaemonSets(t *testing.T) {
	validObject := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "DaemonSet",
		}}
	}
	validRef := connectivity.DaemonSetRef{
		Namespace:     "default",
		Name:          "connectivity-test",
		RDMAContainer: "test-container",
		ICMPContainer: "netshoot",
		SourceFile:    "60-example-daemonset.yaml",
	}
	checks := []connectivity.Check{connectivity.CheckICMP, connectivity.CheckRPing}

	t.Run("accepts complete workload", func(t *testing.T) {
		assert.NoError(t, validateConnectivityDaemonSets(
			[]*unstructured.Unstructured{validObject()}, []connectivity.DaemonSetRef{validRef}, checks))
	})

	t.Run("requires workload", func(t *testing.T) {
		assert.ErrorContains(t, validateConnectivityDaemonSets(nil, nil, checks), "no example test DaemonSet")
	})

	t.Run("requires apps v1", func(t *testing.T) {
		object := validObject()
		object.SetAPIVersion("extensions/v1beta1")
		assert.ErrorContains(t, validateConnectivityDaemonSets(
			[]*unstructured.Unstructured{object}, []connectivity.DaemonSetRef{validRef}, checks), "apps/v1")
	})

	t.Run("requires namespace", func(t *testing.T) {
		ref := validRef
		ref.Namespace = ""
		assert.ErrorContains(t, validateConnectivityDaemonSets(
			[]*unstructured.Unstructured{validObject()}, []connectivity.DaemonSetRef{ref}, checks), "metadata.namespace")
	})

	t.Run("requires route helper", func(t *testing.T) {
		ref := validRef
		ref.ICMPContainer = ""
		assert.ErrorContains(t, validateConnectivityDaemonSets(
			[]*unstructured.Unstructured{validObject()}, []connectivity.DaemonSetRef{ref}, checks), "netshoot")
	})

	t.Run("requires RDMA container", func(t *testing.T) {
		ref := validRef
		ref.RDMAContainer = ""
		assert.ErrorContains(t, validateConnectivityDaemonSets(
			[]*unstructured.Unstructured{validObject()}, []connectivity.DaemonSetRef{ref}, checks), "RDMA tools")
	})

	t.Run("rejects duplicate targets", func(t *testing.T) {
		assert.ErrorContains(t, validateConnectivityDaemonSets(
			[]*unstructured.Unstructured{validObject(), validObject()},
			[]connectivity.DaemonSetRef{validRef, validRef}, checks), "more than once")
	})
}
