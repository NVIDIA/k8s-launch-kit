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

	"github.com/Mellanox/doca-driver-build/entrypoint/pkg/mofedmodules"
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

func TestClassifyDiscoveredModules(t *testing.T) {
	tests := []struct {
		name        string
		input       []string
		wantRDMA    []string
		wantStorage []string
	}{
		{
			name:        "empty input returns empty buckets",
			input:       nil,
			wantRDMA:    nil,
			wantStorage: nil,
		},
		{
			name:        "mlx5-prefixed modules silently dropped",
			input:       []string{"mlx5_vdpa", "mlx5_ib", "mlx5_core"},
			wantRDMA:    nil,
			wantStorage: nil,
		},
		{
			name:        "known storage modules routed to storage bucket",
			input:       []string{"ib_isert", "nvme_rdma", "ib_srpt"},
			wantRDMA:    nil,
			wantStorage: []string{"ib_isert", "nvme_rdma", "ib_srpt"},
		},
		{
			name:        "unknown modules routed to third-party RDMA bucket",
			input:       []string{"bnxt_re", "qedr", "ko2iblnd"},
			wantRDMA:    []string{"bnxt_re", "qedr", "ko2iblnd"},
			wantStorage: nil,
		},
		{
			name:        "mixed input splits correctly",
			input:       []string{"mlx5_core", "ib_srpt", "qedr", "mlx5_vdpa", "nvme_rdma", "siw"},
			wantRDMA:    []string{"qedr", "siw"},
			wantStorage: []string{"ib_srpt", "nvme_rdma"},
		},
		{
			name:        "ib_iser and ib_srp (initiator side) classify as storage via mofedmodules",
			input:       []string{"ib_iser", "ib_srp"},
			wantRDMA:    nil,
			wantStorage: []string{"ib_iser", "ib_srp"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRDMA, gotStorage := classifyDiscoveredModules(tt.input)
			assert.Equal(t, tt.wantRDMA, gotRDMA)
			assert.Equal(t, tt.wantStorage, gotStorage)
		})
	}
}

// TestKnownStorageModules_MatchesMofedmodules is a regression guard: the
// classification set MUST be derived exactly from the shared
// mofedmodules.DefaultStorageModules slice. If this assertion fails, the set
// built by buildKnownStorageModulesSet() has drifted from the canonical list
// in doca-driver-build — either because someone added a local override or
// because the shared list changed and this dep needs a bump.
func TestKnownStorageModules_MatchesMofedmodules(t *testing.T) {
	assert.Len(t, knownStorageModules, len(mofedmodules.DefaultStorageModules),
		"knownStorageModules size should match mofedmodules.DefaultStorageModules")
	for _, mod := range mofedmodules.DefaultStorageModules {
		assert.Truef(t, knownStorageModules[mod],
			"mofedmodules.DefaultStorageModules entry %q missing from knownStorageModules", mod)
	}
}
