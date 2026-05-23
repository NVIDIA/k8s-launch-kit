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

package crstate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	sriovAPIVersion = sriovGroup + "/" + sriovVersion
)

// sriovPolicyManifest builds the policy CR a deploy would apply. The
// returned *unstructured already has spec.nodeSelector / spec.numVfs /
// spec.nicSelector.pfNames populated when non-empty.
func sriovPolicyManifest(name, ns string, nodeSelector map[string]string, numVfs int64, pfNames []string) *unstructured.Unstructured {
	obj := makeUnstructured(sriovAPIVersion, sriovKindNetworkNodePolicy, ns, name, nil)
	if len(nodeSelector) > 0 {
		nsRaw := make(map[string]interface{}, len(nodeSelector))
		for k, v := range nodeSelector {
			nsRaw[k] = v
		}
		_ = unstructured.SetNestedMap(obj.Object, nsRaw, "spec", "nodeSelector")
	}
	_ = unstructured.SetNestedField(obj.Object, numVfs, "spec", "numVfs")
	if len(pfNames) > 0 {
		raw := make([]interface{}, len(pfNames))
		for i, n := range pfNames {
			raw[i] = n
		}
		_ = unstructured.SetNestedSlice(obj.Object, raw, "spec", "nicSelector", "pfNames")
	}
	return obj
}

// sriovNodeState builds a live SriovNetworkNodeState in `ns`, named for
// `node`, with the given syncStatus and per-PF interfaces.
func sriovNodeState(node, ns, syncStatus string, lastSyncErr string, ifaces []map[string]interface{}) *unstructured.Unstructured {
	obj := makeUnstructured(sriovAPIVersion, sriovKindNetworkNodeState, ns, node, nil)
	if syncStatus != "" {
		_ = unstructured.SetNestedField(obj.Object, syncStatus, "status", "syncStatus")
	}
	if lastSyncErr != "" {
		_ = unstructured.SetNestedField(obj.Object, lastSyncErr, "status", "lastSyncError")
	}
	if len(ifaces) > 0 {
		raw := make([]interface{}, len(ifaces))
		for i, m := range ifaces {
			raw[i] = m
		}
		_ = unstructured.SetNestedSlice(obj.Object, raw, "status", "interfaces")
	}
	return obj
}

func iface(name, pci string, numVfs int64) map[string]interface{} {
	return map[string]interface{}{
		"name":       name,
		"pciAddress": pci,
		"numVfs":     numVfs,
	}
}

func TestSriovValidator_NotDeployed(t *testing.T) {
	manifest := sriovPolicyManifest("p", "ns", nil, 0, nil)
	c := newClient(t)
	res, err := sriovNetworkNodePolicyValidator(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateNotDeployed, res.State)
}

func TestSriovValidator_NodeSelectorMatchesNoNodes(t *testing.T) {
	manifest := sriovPolicyManifest("p", "ns", map[string]string{"role": "absent"}, 8, []string{"eth_r0"})
	live := manifest.DeepCopy()
	c := newClient(t, live, node("worker-1", map[string]string{"role": "other"}))
	res, err := sriovNetworkNodePolicyValidator(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateError, res.State)
	assert.Contains(t, res.Reason, "spec.nodeSelector matched no nodes")
}

func TestSriovValidator_AllNodesSucceeded_PFsMatch(t *testing.T) {
	manifest := sriovPolicyManifest("p", "ns", map[string]string{"role": "worker"}, 8, []string{"eth_r0", "eth_r1"})
	live := manifest.DeepCopy()
	state := sriovNodeState("worker-1", "ns", sriovSyncStatusSucceeded, "", []map[string]interface{}{
		iface("eth_r0", "0000:1a:00.0", 8),
		iface("eth_r1", "0000:1b:00.0", 8),
	})
	c := newClient(t, live, state, node("worker-1", map[string]string{"role": "worker"}))
	res, err := sriovNetworkNodePolicyValidator(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateSuccess, res.State)
}

func TestSriovValidator_SucceededButPFMissing_SilentFailure(t *testing.T) {
	// This is the silent-failure case the plan calls out: the SR-IOV
	// operator reports Succeeded because pfNames matched zero
	// interfaces (typically because NicInterfaceNameTemplate udev rules
	// failed to apply).
	manifest := sriovPolicyManifest("p", "ns", map[string]string{"role": "worker"}, 8, []string{"eth_r0", "eth_r1"})
	live := manifest.DeepCopy()
	state := sriovNodeState("worker-1", "ns", sriovSyncStatusSucceeded, "", []map[string]interface{}{
		iface("eth_r0", "0000:1a:00.0", 8),
		// eth_r1 missing
	})
	c := newClient(t, live, state, node("worker-1", map[string]string{"role": "worker"}))
	res, err := sriovNetworkNodePolicyValidator(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateError, res.State)
	assert.Contains(t, res.Reason, "missing")
	assert.Contains(t, res.Reason, "eth_r1")
}

func TestSriovValidator_SucceededButNumVfsMismatch(t *testing.T) {
	manifest := sriovPolicyManifest("p", "ns", map[string]string{"role": "worker"}, 8, []string{"eth_r0"})
	live := manifest.DeepCopy()
	state := sriovNodeState("worker-1", "ns", sriovSyncStatusSucceeded, "", []map[string]interface{}{
		iface("eth_r0", "0000:1a:00.0", 0), // expected 8
	})
	c := newClient(t, live, state, node("worker-1", map[string]string{"role": "worker"}))
	res, err := sriovNetworkNodePolicyValidator(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateError, res.State)
	assert.Contains(t, res.Reason, "numVfs=8")
	assert.Contains(t, res.Reason, "found 0")
}

func TestSriovValidator_NodeFailed(t *testing.T) {
	manifest := sriovPolicyManifest("p", "ns", map[string]string{"role": "worker"}, 8, []string{"eth_r0"})
	live := manifest.DeepCopy()
	state := sriovNodeState("worker-1", "ns", sriovSyncStatusFailed, "kernel rejected request", nil)
	c := newClient(t, live, state, node("worker-1", map[string]string{"role": "worker"}))
	res, err := sriovNetworkNodePolicyValidator(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateError, res.State)
	assert.Contains(t, res.Reason, "kernel rejected request")
}

func TestSriovValidator_PartialProgress(t *testing.T) {
	// Two nodes: one Succeeded with matching PFs, one InProgress.
	// Aggregate should report in-progress (no errors), preserving the
	// per-node breakdown in Details.
	manifest := sriovPolicyManifest("p", "ns", map[string]string{"role": "worker"}, 8, []string{"eth_r0"})
	live := manifest.DeepCopy()
	c := newClient(t,
		live,
		sriovNodeState("worker-1", "ns", sriovSyncStatusSucceeded, "", []map[string]interface{}{
			iface("eth_r0", "0000:1a:00.0", 8),
		}),
		sriovNodeState("worker-2", "ns", sriovSyncStatusInProgress, "", nil),
		node("worker-1", map[string]string{"role": "worker"}),
		node("worker-2", map[string]string{"role": "worker"}),
	)
	res, err := sriovNetworkNodePolicyValidator(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateInProgress, res.State)
	assert.Contains(t, res.Details["worker-1"], "Succeeded")
	assert.Contains(t, res.Details["worker-2"], "InProgress")
}

func TestSriovValidator_RootDevicesSelector(t *testing.T) {
	// When nicSelector uses rootDevices rather than pfNames the
	// validator cross-checks against PCI addresses.
	manifest := sriovPolicyManifest("p", "ns", map[string]string{"role": "worker"}, 8, nil)
	roots := []interface{}{"0000:1a:00.0", "0000:1b:00.0"}
	_ = unstructured.SetNestedSlice(manifest.Object, roots, "spec", "nicSelector", "rootDevices")
	live := manifest.DeepCopy()

	c := newClient(t, live,
		sriovNodeState("worker-1", "ns", sriovSyncStatusSucceeded, "", []map[string]interface{}{
			iface("eth_r0", "0000:1a:00.0", 8),
			// rootDevice 0000:1b:00.0 missing
		}),
		node("worker-1", map[string]string{"role": "worker"}),
	)
	res, err := sriovNetworkNodePolicyValidator(context.Background(), c, manifest)
	require.NoError(t, err)
	assert.Equal(t, StateError, res.State)
	assert.Contains(t, res.Reason, "0000:1b:00.0")
}

// Make sure helpers compile even when unused by Go's strictness.
var _ client.Client = nil
