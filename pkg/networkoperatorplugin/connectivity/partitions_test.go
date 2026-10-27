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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"github.com/stretchr/testify/require"
)

func TestPartitionMembership(t *testing.T) {
	for _, tc := range []struct {
		name, output, warning string
		invalid               bool
	}{
		{"limited default", "mlx5_0 1\n0x7fff\n0x0000", "only limited-membership P_Keys (0x7fff)", false},
		{"full default", "mlx5_0 1 0xffff 0x0000", "", false},
		{"mixed", "mlx5_0 1 0x7fff 0x8001", "", false},
		{"other partition", "mlx5_0 2 0x0001", "only limited-membership P_Keys (0x0001)", false},
		{"empty slots", "mlx5_0 1 0x0000 0x8000", "no usable P_Key entries", false},
		{"Ethernet", "not-infiniband", "", false},
		{"empty", "", "", true},
		{"missing table", "mlx5_0 1", "", true},
		{"malformed", "mlx5_0 1 0x7fff garbage", "", true},
		{"malformed after full", "mlx5_0 1 0xffff garbage", "", true},
		{"invalid port", "mlx5_0 0 0x7fff", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warning, err := partitionWarning(tc.output)
			if tc.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.warning == "" {
				require.Empty(t, warning)
			} else {
				require.Contains(t, warning, tc.warning)
			}
		})
	}
}

func TestPartitionProbeSysfs(t *testing.T) {
	for _, tc := range []struct {
		name                                  string
		ethernet, ambiguous, missing, partial bool
	}{
		{name: "selected second port"}, {name: "Ethernet", ethernet: true},
		{name: "ambiguous device", ambiguous: true}, {name: "missing table", missing: true}, {name: "partial table", partial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(path, value string) {
				path = filepath.Join(root, path)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
				require.NoError(t, os.WriteFile(path, []byte(value), 0600))
			}
			write("class/net/net1/type", "32\n")
			write("class/net/net1/dev_port", "1\n")
			require.NoError(t, os.MkdirAll(filepath.Join(root, "class/net/net1/device/infiniband/mlx5_0"), 0755))
			write("class/infiniband/mlx5_0/ports/1/link_layer", "InfiniBand\n")
			write("class/infiniband/mlx5_0/ports/1/pkeys/0", "0xffff\n")
			write("class/infiniband/mlx5_0/ports/2/link_layer", "InfiniBand\n")
			if !tc.missing {
				write("class/infiniband/mlx5_0/ports/2/pkeys/0", "0x7fff\n")
			}
			if tc.partial {
				require.NoError(t, os.Symlink("absent", filepath.Join(root, "class/infiniband/mlx5_0/ports/2/pkeys/1")))
			}
			if tc.ethernet {
				write("class/net/net1/type", "1\n")
			}
			if tc.ambiguous {
				require.NoError(t, os.MkdirAll(filepath.Join(root, "class/net/net1/device/infiniband/mlx5_1"), 0755))
			}
			out, err := exec.Command("/bin/sh", "-c", partitionProbe, "test-pkeys", root, "net1").CombinedOutput()
			if tc.ambiguous || tc.missing || tc.partial {
				require.Error(t, err)
				require.NotEmpty(t, out)
				return
			}
			require.NoError(t, err, string(out))
			warning, err := partitionWarning(string(out))
			require.NoError(t, err)
			if tc.ethernet {
				require.Empty(t, warning)
			} else {
				require.Contains(t, warning, "mlx5_0 port 2")
				require.Contains(t, warning, "0x7fff")
				require.NotContains(t, warning, "0xffff")
			}
		})
	}
}

func TestPartitionWarningsRetainEvidenceAndReports(t *testing.T) {
	pod := TestPod{Name: "test-pod", Namespace: "test-ns", Node: "worker", InterfacesByRail: map[string]string{"rail-1": "net2", "rail-0": "net1"}}
	calls := 0
	warnings := collectPartitionWarnings(context.Background(), pod, func(ctx context.Context, command []string) (kubeclient.ExecResult, error) {
		_, bounded := ctx.Deadline()
		require.True(t, bounded)
		calls++
		if calls == 1 {
			require.Equal(t, "net1", command[len(command)-1])
			return kubeclient.ExecResult{Stdout: "mlx5_0 1 0x7fff"}, nil
		}
		return kubeclient.ExecResult{Stderr: "permission denied"}, fmt.Errorf("exec [/bin/sh -c %s] (stderr: permission denied): failed", partitionProbe)
	})
	require.Len(t, warnings, 2)
	require.Contains(t, warnings[0], "Node worker pod test-ns/test-pod rail rail-0 interface net1")
	require.Contains(t, warnings[0], "Two limited members cannot communicate")
	require.Contains(t, warnings[1], "unverified")
	require.Contains(t, warnings[1], "permission denied")
	require.NotContains(t, warnings[1], "set -eu")
	require.Equal(t, 1, strings.Count(warnings[1], "permission denied"))
	matrix := &MatrixResult{PartitionWarnings: warnings}
	encoded, err := json.Marshal(matrix)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "PartitionWarnings")
	require.NotContains(t, string(encoded), "set -eu")
	var html bytes.Buffer
	require.NoError(t, RenderHTML(&html, ReportData{Matrix: matrix}))
	require.Contains(t, html.String(), "only limited-membership")
	require.Contains(t, html.String(), "permission denied")
	require.NotContains(t, html.String(), "set -eu")
	require.Equal(t, 0, matrix.Summary.Failed)
}

func TestPartitionProbeFailureBoundsStderr(t *testing.T) {
	stderr := "\n" + strings.Repeat("é", 130) + "\n" + strings.Repeat("x", 4096)
	pod := TestPod{InterfacesByRail: map[string]string{"rail-0": "net1"}}
	warnings := collectPartitionWarnings(context.Background(), pod, func(context.Context, []string) (kubeclient.ExecResult, error) {
		return kubeclient.ExecResult{Stderr: stderr}, errors.New("opaque exec failure")
	})
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "pod exec failed: ")
	require.NotContains(t, warnings[0], "opaque exec failure")
	require.NotContains(t, warnings[0], strings.Repeat("x", 4096))
	require.True(t, utf8.ValidString(warnings[0]))
	require.Less(t, len(warnings[0]), 400)
}

func TestPartitionProbeArgumentsAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	iface := "net'$(touch unexpected)"
	pod := TestPod{InterfacesByRail: map[string]string{"rail-0": iface, "rail-1": "../outside"}}
	calls := 0
	warnings := collectPartitionWarnings(ctx, pod, func(ctx context.Context, command []string) (kubeclient.ExecResult, error) {
		calls++
		require.Equal(t, iface, command[len(command)-1])
		require.False(t, strings.Contains(command[2], iface))
		return kubeclient.ExecResult{}, ctx.Err()
	})
	require.Equal(t, 1, calls)
	require.Len(t, warnings, 2)
	require.Contains(t, warnings[0], "context canceled")
	require.Contains(t, warnings[1], "invalid interface name")
}
