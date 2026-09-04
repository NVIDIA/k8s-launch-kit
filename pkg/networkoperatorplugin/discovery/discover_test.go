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

package discovery

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGPUDirectAvailableOnAllGroupNodes(t *testing.T) {
	groups := []config.ClusterConfig{
		{WorkerNodes: []string{"node-a", "node-b"}},
		{WorkerNodes: []string{"node-c"}},
	}
	node := func(name, quantity string) corev1.Node {
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceName("example.com/gpu"): resource.MustParse(quantity)},
		}}
	}
	nodes := []corev1.Node{node("node-a", "8"), node("node-b", "1"), node("node-c", "2")}
	assert.True(t, gpuDirectAvailableOnAllGroupNodes(groups, nodes, "example.com/gpu"))
	nodes[1].Status.Allocatable[corev1.ResourceName("example.com/gpu")] = resource.MustParse("0")
	assert.False(t, gpuDirectAvailableOnAllGroupNodes(groups, nodes, "example.com/gpu"))
	assert.False(t, gpuDirectAvailableOnAllGroupNodes(groups, nodes[:2], "example.com/gpu"))
	assert.False(t, gpuDirectAvailableOnAllGroupNodes(nil, nodes, "example.com/gpu"))

	t.Run("merged bucket requires its largest topology prefix on every worker", func(t *testing.T) {
		rail0 := 0
		mergedGroups := []config.ClusterConfig{
			{GPUType: "same-gpu", WorkerNodes: []string{"node-a"}, PFs: []config.PFConfig{{
				Traffic: "east-west", Rail: &rail0, ConnectedGPU: "GPU1",
			}}},
			{GPUType: "same-gpu", WorkerNodes: []string{"node-c"}, PFs: []config.PFConfig{{
				Traffic: "east-west", Rail: &rail0, ConnectedGPU: "GPU7",
			}}},
		}
		limitedNodes := []corev1.Node{node("node-a", "2"), node("node-c", "8")}
		assert.False(t, gpuDirectAvailableOnAllGroupNodes(mergedGroups, limitedNodes, "example.com/gpu"))
		limitedNodes[0].Status.Allocatable[corev1.ResourceName("example.com/gpu")] = resource.MustParse("8")
		assert.True(t, gpuDirectAvailableOnAllGroupNodes(mergedGroups, limitedNodes, "example.com/gpu"))

		mergedGroups[1].GPUType = "different-gpu"
		limitedNodes[0].Status.Allocatable[corev1.ResourceName("example.com/gpu")] = resource.MustParse("2")
		assert.True(t, gpuDirectAvailableOnAllGroupNodes(mergedGroups, limitedNodes, "example.com/gpu"),
			"separate render buckets must retain their own request sizes")
	})
}

func TestCheckDaemonSetPodsReady_NoPodsFound(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	_, err := checkDaemonSetPodsReady(context.Background(), c, "custom-namespace", "nic-configuration-daemon")
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

	st, err := checkDaemonSetPodsReady(context.Background(), c, "network-operator", "nic-configuration-daemon")
	require.NoError(t, err)
	assert.Equal(t, []string{"node-1"}, st.readyNodes)
	assert.Len(t, st.readyPods, 1)
	assert.Equal(t, 1, st.total)
	assert.Equal(t, 1, st.ready)
	assert.Equal(t, 0, st.stuck)
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

	// A not-Ready pod no longer errors — the pod exists, so the caller keeps
	// polling based on the counts. It's counted as neither ready nor stuck
	// (no terminal waiting reason).
	st, err := checkDaemonSetPodsReady(context.Background(), c, "nvidia-network-operator", "nic-configuration-daemon")
	require.NoError(t, err)
	assert.Equal(t, 1, st.total)
	assert.Equal(t, 0, st.ready)
	assert.Equal(t, 0, st.stuck)
	assert.Empty(t, st.readyNodes)
}

func TestCheckDaemonSetPodsReady_StuckPodCounted(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	readyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nic-configuration-daemon-ready",
			Namespace: "nvidia-k8s-launch-kit",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "DaemonSet", Name: "nic-configuration-daemon"},
			},
		},
		Spec: corev1.PodSpec{NodeName: "node-ready"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	// A pod stuck on ImagePullBackOff on an unrelated node — must be counted
	// as stuck so the wait can proceed instead of blocking until timeout.
	stuckPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nic-configuration-daemon-stuck",
			Namespace: "nvidia-k8s-launch-kit",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "DaemonSet", Name: "nic-configuration-daemon"},
			},
		},
		Spec: corev1.PodSpec{NodeName: "node-stuck"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(readyPod, stuckPod).Build()

	st, err := checkDaemonSetPodsReady(context.Background(), c, "nvidia-k8s-launch-kit", "nic-configuration-daemon")
	require.NoError(t, err)
	assert.Equal(t, 2, st.total)
	assert.Equal(t, 1, st.ready)
	assert.Equal(t, 1, st.stuck)
	assert.Equal(t, []string{"node-ready"}, st.readyNodes)
}

func TestEligibleDiscoveryNodeNamesFiltersNotReadyAndUnschedulable(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		testNode("ready-b", corev1.ConditionTrue, false),
		testNode("ready-a", corev1.ConditionTrue, false),
		testNode("not-ready", corev1.ConditionFalse, false),
		testNode("cordoned", corev1.ConditionTrue, true),
		testNode("not-ready-cordoned", corev1.ConditionFalse, true),
		testNodeWithoutReadyCondition("missing-ready-condition", false),
	).Build()

	got, excluded, err := eligibleDiscoveryNodeNames(context.Background(), c)
	require.NoError(t, err)
	assert.Equal(t, []string{"ready-a", "ready-b"}, got)
	assert.Equal(t, 3, excluded.notReady)
	assert.Equal(t, 1, excluded.unschedulable)
}

func TestEligibleDiscoveryNodeNamesNoEligibleNodes(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		testNode("not-ready", corev1.ConditionFalse, false),
		testNode("cordoned", corev1.ConditionTrue, true),
	).Build()

	got, excluded, err := eligibleDiscoveryNodeNames(context.Background(), c)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, 1, excluded.notReady)
	assert.Equal(t, 1, excluded.unschedulable)
}

func testNode(name string, ready corev1.ConditionStatus, unschedulable bool) *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: unschedulable},
	}
	node.Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: ready},
	}
	return node
}

func testNodeWithoutReadyCondition(name string, unschedulable bool) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: unschedulable},
	}
}

func TestPodStuck(t *testing.T) {
	stuck := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
		},
	}}
	assert.True(t, podStuck(stuck))

	progressing := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}},
		},
	}}
	assert.False(t, podStuck(progressing))

	running := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{
			{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		},
	}}
	assert.False(t, podStuck(running))
}

func TestAlternateNamespace(t *testing.T) {
	assert.Equal(t, "network-operator", alternateNamespace("nvidia-network-operator"))
	assert.Equal(t, "nvidia-network-operator", alternateNamespace("network-operator"))
	assert.Equal(t, "", alternateNamespace("custom-namespace"))
}

func TestParseMachineTypeFromDMI(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"normal value", "DGXH100 880G\n", "DGXH100-880G"},
		{"trailing whitespace", "  DGXH100 880G  \n\n", "DGXH100-880G"},
		{"no spaces", "DGX-A100\n", "DGX-A100"},
		{"empty", "", ""},
		{"whitespace only", "  \n", ""},
		{"multiple spaces", "ThinkSystem SR680a V3\n", "ThinkSystem-SR680a-V3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseMachineTypeFromDMI(tt.raw))
		})
	}
}

func TestParseGPUProductFromNvidiaSmi(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name: "single GPU",
			output: `GPU 00000000:E1:00.0
    Product Name                          : NVIDIA H100 NVL
    Product Brand                         : NVIDIA
`,
			want: "NVIDIA-H100-NVL",
		},
		{
			name: "multiple GPUs returns first",
			output: `GPU 00000000:E1:00.0
    Product Name                          : NVIDIA H100 NVL
GPU 00000000:E2:00.0
    Product Name                          : NVIDIA A100 80GB
`,
			want: "NVIDIA-H100-NVL",
		},
		{
			name:   "no product name",
			output: "Driver Version: 550.90\nCUDA Version: 12.4\n",
			want:   "",
		},
		{
			name:   "empty output",
			output: "",
			want:   "",
		},
		{
			name:   "extra whitespace",
			output: "    Product Name                          : NVIDIA H100 NVL   \n",
			want:   "NVIDIA-H100-NVL",
		},
		{
			name:   "SXM variant",
			output: "    Product Name                          : NVIDIA H100 80GB HBM3\n",
			want:   "NVIDIA-H100-80GB-HBM3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseGPUProductFromNvidiaSmi(tt.output))
		})
	}
}

func TestParseGPUProductFromSysfs(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{"H200 SXM", "0x2335\n", "NVIDIA-H200-SXM-141GB"},
		{"H200 NVL", "0x233b\n", "NVIDIA-H200-NVL"},
		{"L40S", "0x26b9\n", "NVIDIA-L40S"},
		{"uppercase hex", "0x233B\n", "NVIDIA-H200-NVL"},
		{"no 0x prefix", "2335\n", "NVIDIA-H200-SXM-141GB"},
		{"extra whitespace", "   0x2335   \n\n", "NVIDIA-H200-SXM-141GB"},
		{"multiple lines, first wins", "0x2335\n0x26b9\n", "NVIDIA-H200-SXM-141GB"},
		{"empty", "", ""},
		{"whitespace only", "  \n", ""},
		{"unknown device", "0xffff\n", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseGPUProductFromSysfs(tt.output))
		})
	}
}

func TestParseMellanoxNICProbeResult(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   mellanoxNICProbeResult
	}{
		{"present marker", "present\n", mellanoxNICProbePresent},
		{"present with whitespace", "  present  \n", mellanoxNICProbePresent},
		{"restricted marker", "restricted\n", mellanoxNICProbeRestricted},
		{"restricted with whitespace", "  restricted  \n", mellanoxNICProbeRestricted},
		{"present wins over restricted", "restricted\npresent\n", mellanoxNICProbePresent},
		{"empty", "", mellanoxNICProbeNone},
		{"whitespace only", "  \n\t", mellanoxNICProbeNone},
		{"unexpected output", "mlxprivhost warning\n", mellanoxNICProbeNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseMellanoxNICProbeResult(tt.output))
		})
	}
}

func TestSysfsMellanoxNICPresentCmd(t *testing.T) {
	type fakePCIDevice struct {
		name     string
		vendorID string
		deviceID string
	}

	tests := []struct {
		name           string
		devices        []fakePCIDevice
		mlxprivhostRun string
		want           string
	}{
		{
			name: "no NVIDIA NIC",
			devices: []fakePCIDevice{
				{name: "0000:01:00.0", vendorID: "0x10de", deviceID: "0x2335"},
			},
			mlxprivhostRun: "exit 1",
			want:           "",
		},
		{
			name: "regular NVIDIA NIC",
			devices: []fakePCIDevice{
				{name: "0000:01:00.0", vendorID: "0x15b3", deviceID: "0x1023"},
			},
			mlxprivhostRun: "exit 1",
			want:           "present",
		},
		{
			name: "restricted BF3",
			devices: []fakePCIDevice{
				{name: "0000:01:00.0", vendorID: "0x15b3", deviceID: "0xa2dc"},
			},
			mlxprivhostRun: "printf 'level: restricted\\n'",
			want:           "restricted",
		},
		{
			name: "privileged BF3",
			devices: []fakePCIDevice{
				{name: "0000:01:00.0", vendorID: "0x15b3", deviceID: "0xa2dc"},
			},
			mlxprivhostRun: "printf 'level: privileged\\n'",
			want:           "present",
		},
		{
			name: "trust query failure is fail open",
			devices: []fakePCIDevice{
				{name: "0000:01:00.0", vendorID: "0x15b3", deviceID: "0xa2dc"},
			},
			mlxprivhostRun: "exit 1",
			want:           "present",
		},
		{
			name: "all known BlueField device IDs restricted",
			devices: []fakePCIDevice{
				{name: "0000:01:00.0", vendorID: "0x15b3", deviceID: "0xa2d6"},
				{name: "0000:02:00.0", vendorID: "0x15b3", deviceID: "0xa2d9"},
				{name: "0000:03:00.0", vendorID: "0x15b3", deviceID: "0xa2dc"},
				{name: "0000:04:00.0", vendorID: "0x15b3", deviceID: "0xa2df"},
			},
			mlxprivhostRun: "printf 'level: restricted\\n'",
			want:           "restricted",
		},
		{
			name: "restricted BF3 plus regular NIC",
			devices: []fakePCIDevice{
				{name: "0000:01:00.0", vendorID: "0x15b3", deviceID: "0xa2dc"},
				{name: "0000:02:00.0", vendorID: "0x15b3", deviceID: "0x1023"},
			},
			mlxprivhostRun: "printf 'level: restricted\\n'",
			want:           "present",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pciRoot := t.TempDir()
			for _, device := range tt.devices {
				deviceDir := filepath.Join(pciRoot, device.name)
				require.NoError(t, os.MkdirAll(deviceDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(deviceDir, "vendor"), []byte(device.vendorID+"\n"), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(deviceDir, "device"), []byte(device.deviceID+"\n"), 0o644))
			}

			mlxprivhost := filepath.Join(t.TempDir(), "mlxprivhost")
			require.NoError(t, os.WriteFile(mlxprivhost,
				[]byte("#!/bin/sh\n"+tt.mlxprivhostRun+"\n"), 0o755))

			//nolint:gosec // fixed test command and script; temp paths are positional parameters
			cmd := exec.Command("/bin/sh", "-c", sysfsMellanoxNICPresentCmd,
				"nic-probe", pciRoot, mlxprivhost)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "probe output: %s", output)
			assert.Equal(t, tt.want, strings.TrimSpace(string(output)))
		})
	}
}

func TestFindDaemonPod(t *testing.T) {
	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "daemon-pod-1"},
			Spec:       corev1.PodSpec{NodeName: "node-1"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "daemon-pod-2"},
			Spec:       corev1.PodSpec{NodeName: "node-2"},
		},
	}

	t.Run("pod found on matching node", func(t *testing.T) {
		result := findDaemonPod([]string{"node-2", "node-3"}, pods)
		require.NotNil(t, result)
		assert.Equal(t, "daemon-pod-2", result.Name)
	})

	t.Run("no matching node", func(t *testing.T) {
		result := findDaemonPod([]string{"node-5"}, pods)
		assert.Nil(t, result)
	})

	t.Run("empty nodes list", func(t *testing.T) {
		result := findDaemonPod([]string{}, pods)
		assert.Nil(t, result)
	})

	t.Run("empty pods list", func(t *testing.T) {
		result := findDaemonPod([]string{"node-1"}, []corev1.Pod{})
		assert.Nil(t, result)
	})
}

// TestClassifyByPartNumberFrequency_BFSuperNICTriggersOOBRule reproduces the
// cluster-config the user reported: 6 PFs, three part numbers each appearing
// twice. After Stage 1 (BF3 SuperNIC explicit-EW pin + ns-product-ids DPU
// match), the heuristic must:
//   - keep the BF3 DPU PFs north-south,
//   - keep the BF3 SuperNIC PFs east-west,
//   - flip the unpinned ConnectX-6 Lx PFs (used as OOB) to north-south.
func TestClassifyByPartNumberFrequency_BFSuperNICTriggersOOBRule(t *testing.T) {
	bfDPU := nodePFEntry{
		pfFingerprint: pfFingerprint{DeviceID: "a2dc", PciAddress: "0000:0d:00.0"},
		IsNorthSouth:  true, // pinned by ns-product-ids match
		PartNumber:    "900-9D3B6-00CV-AA0",
	}
	bfSuperNIC := nodePFEntry{
		pfFingerprint:      pfFingerprint{DeviceID: "a2dc", PciAddress: "0000:37:00.0"},
		IsExplicitEastWest: true, // pinned by BF3 SuperNIC rule
		PartNumber:         "900-9D3D4-00EN-HA0",
	}
	oobNIC := nodePFEntry{
		pfFingerprint: pfFingerprint{DeviceID: "101f", PciAddress: "0000:22:00.0"},
		PartNumber:    "0DN78C",
	}

	ni := &nodeInfo{pfs: []nodePFEntry{
		bfDPU, bfDPU, bfSuperNIC, bfSuperNIC, oobNIC, oobNIC,
	}}
	classifyByPartNumberFrequency(map[string]*nodeInfo{"node-1": ni})

	for i, pf := range ni.pfs {
		switch pf.PartNumber {
		case "900-9D3B6-00CV-AA0":
			assert.True(t, pf.IsNorthSouth, "BF3 DPU PF[%d] should remain north-south", i)
			assert.False(t, pf.IsExplicitEastWest)
		case "900-9D3D4-00EN-HA0":
			assert.False(t, pf.IsNorthSouth, "BF3 SuperNIC PF[%d] must NOT be flipped to north-south", i)
			assert.True(t, pf.IsExplicitEastWest)
		case "0DN78C":
			assert.True(t, pf.IsNorthSouth, "ConnectX-6 Lx OOB PF[%d] should be flipped to north-south", i)
			assert.False(t, pf.IsExplicitEastWest)
		}
	}
}

// TestClassifyByPartNumberFrequency_NoExplicitEWFallsBackToFrequency verifies
// the legacy 5+-PF heuristic still runs when no BF3 SuperNIC is present.
func TestClassifyByPartNumberFrequency_NoExplicitEWFallsBackToFrequency(t *testing.T) {
	majority := nodePFEntry{
		pfFingerprint: pfFingerprint{DeviceID: "1023"},
		PartNumber:    "AAA-major",
	}
	minority := nodePFEntry{
		pfFingerprint: pfFingerprint{DeviceID: "1023"},
		PartNumber:    "BBB-minor",
	}

	ni := &nodeInfo{pfs: []nodePFEntry{
		majority, majority, majority, majority, minority,
	}}
	classifyByPartNumberFrequency(map[string]*nodeInfo{"node-1": ni})

	for _, pf := range ni.pfs {
		if pf.PartNumber == "AAA-major" {
			assert.False(t, pf.IsNorthSouth, "majority part number stays east-west")
		} else {
			assert.True(t, pf.IsNorthSouth, "minority part number flipped to north-south")
		}
	}
}

// TestClassifyByPartNumberFrequency_DPUExcludedFromTiebreak verifies that PFs
// already pinned north-south (via ns-product-ids) don't poison the alphabetic
// tiebreak in the legacy heuristic.
func TestClassifyByPartNumberFrequency_DPUExcludedFromTiebreak(t *testing.T) {
	// Without exclusion, the DPU part number "AAA-dpu" would win the
	// alphabetic tiebreak and the real EW NICs would get reclassified as
	// north-south.
	dpu := nodePFEntry{
		pfFingerprint: pfFingerprint{DeviceID: "a2dc"},
		IsNorthSouth:  true,
		PartNumber:    "AAA-dpu",
	}
	ew := nodePFEntry{
		pfFingerprint: pfFingerprint{DeviceID: "1023"},
		PartNumber:    "BBB-ew",
	}
	mgmt := nodePFEntry{
		pfFingerprint: pfFingerprint{DeviceID: "101f"},
		PartNumber:    "CCC-mgmt",
	}

	ni := &nodeInfo{pfs: []nodePFEntry{
		dpu, dpu, ew, ew, ew, ew, mgmt,
	}}
	classifyByPartNumberFrequency(map[string]*nodeInfo{"node-1": ni})

	for _, pf := range ni.pfs {
		switch pf.PartNumber {
		case "AAA-dpu":
			assert.True(t, pf.IsNorthSouth)
		case "BBB-ew":
			assert.False(t, pf.IsNorthSouth, "EW majority must stay east-west")
		case "CCC-mgmt":
			assert.True(t, pf.IsNorthSouth, "minority flipped to north-south")
		}
	}
}

// --- parsePortFabricVerdict tests ---

func TestParsePortFabricVerdict_InfiniBand(t *testing.T) {
	linkType, raw := parsePortFabricVerdict("InfiniBand\n")
	assert.Equal(t, "InfiniBand", linkType)
	assert.Equal(t, "InfiniBand", raw)
}

func TestParsePortFabricVerdict_Ethernet(t *testing.T) {
	linkType, raw := parsePortFabricVerdict("Ethernet\n")
	assert.Equal(t, "Ethernet", linkType)
	assert.Equal(t, "Ethernet", raw)
}

func TestParsePortFabricVerdict_DownPortStillResolvesByLinkLayer(t *testing.T) {
	// Old behaviour required ACTIVE state; the new probe reads
	// only the configured link_layer file, which gives a verdict
	// regardless of runtime state. This is what unblocks
	// discovery on freshly provisioned clusters where the switch
	// isn't plugged in yet.
	linkType, _ := parsePortFabricVerdict("Ethernet\n")
	assert.Equal(t, "Ethernet", linkType)
}

func TestParsePortFabricVerdict_EmptyOutput(t *testing.T) {
	linkType, raw := parsePortFabricVerdict("")
	assert.Equal(t, "", linkType)
	assert.Equal(t, "", raw)
}

func TestParsePortFabricVerdict_UnrecognisedValue(t *testing.T) {
	linkType, raw := parsePortFabricVerdict("Foo\n")
	assert.Equal(t, "", linkType)
	assert.Equal(t, "Foo", raw)
}

func TestParsePortFabricVerdict_TrimsWhitespace(t *testing.T) {
	linkType, _ := parsePortFabricVerdict("  Ethernet  \n")
	assert.Equal(t, "Ethernet", linkType)
}

func TestNormalizeLinkLayer(t *testing.T) {
	cases := map[string]string{
		"Ethernet":    "Ethernet",
		"ethernet":    "Ethernet",
		"  ETHERNET ": "Ethernet",
		"InfiniBand":  "InfiniBand",
		"infiniband":  "InfiniBand",
		"INFINIBAND":  "InfiniBand",
		"":            "",
		"Foo":         "",
	}
	for in, want := range cases {
		assert.Equalf(t, want, normalizeLinkLayer(in), "normalizeLinkLayer(%q)", in)
	}
}
