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

package connectivity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const exampleDaemonSetManifest = `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: sriov-test
  namespace: "nvidia-network-operator"
spec:
  selector:
    matchLabels:
      app: sriov-test
  template:
    metadata:
      labels:
        app: sriov-test
    spec:
      containers:
      - name: test-container
        image: nvcr.io/nvidia/doca/doca:3.3.0-full-rt-host
`

// writeExampleDS drops a single example DaemonSet manifest into a temp dir and
// returns the dir, mimicking what `l8k generate` leaves on disk.
func writeExampleDS(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "60-example-daemonset.yaml"),
		[]byte(exampleDaemonSetManifest), 0o600))
	return dir
}

// TestLoadExampleDaemonSetsUsesManifestNamespace pins the namespace contract
// the connectivity matrix relies on: each test DaemonSet is applied into
// whatever namespace its rendered manifest carries. With --network-namespaces,
// `l8k generate` emits one example DaemonSet per network namespace (each with
// its namespace baked in), so validate fans out across them automatically
// without any namespace flag of its own.
func TestLoadExampleDaemonSetsUsesManifestNamespace(t *testing.T) {
	objs, refs, err := LoadExampleDaemonSets(writeExampleDS(t), true)
	require.NoError(t, err)
	require.Len(t, objs, 1)
	require.Len(t, refs, 1)
	require.Equal(t, "nvidia-network-operator", objs[0].GetNamespace())
	require.Equal(t, "nvidia-network-operator", refs[0].Namespace)
	require.Equal(t, "test-container", refs[0].RDMAContainer)
	require.Equal(t, "netshoot", refs[0].ICMPContainer)
	containers, ok, err := unstructured.NestedSlice(objs[0].Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, containers, 2)
	sidecar, ok := containers[1].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, []interface{}{"/bin/bash", "-c", icmpTestCommand}, sidecar["command"])
}

func TestLoadExampleDaemonSetsSkipsICMPSidecarWhenDisabled(t *testing.T) {
	objs, refs, err := LoadExampleDaemonSets(writeExampleDS(t), false)
	require.NoError(t, err)
	require.Len(t, objs, 1)
	require.Len(t, refs, 1)
	require.Equal(t, "test-container", refs[0].RDMAContainer)
	require.Equal(t, "test-container", refs[0].ICMPContainer)
	containers, ok, err := unstructured.NestedSlice(objs[0].Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, containers, 1)
}
