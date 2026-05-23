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
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// updateGolden re-writes the checked-in golden file when set. Run with
// `go test ./pkg/networkoperatorplugin/connectivity/ -update-golden`
// after intentional template changes.
var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/verify-report.golden.html with the current renderer output")

// fixtureData is the deterministic input the renderer is exercised
// against. Includes one of every state badge, an IN-PROGRESS row with
// expandable Details, a passing rail and a failing rail in the matrix,
// and one cross-rail canary, so the golden covers the whole template
// surface.
func fixtureData() ReportData {
	return ReportData{
		Cluster: ClusterInfo{
			L8kVersion:        "v0.0.0-test",
			GeneratedAt:       time.Date(2026, 5, 23, 11, 30, 0, 0, time.UTC),
			KubeContext:       "kubernetes-admin@kubernetes",
			APIServerVersion:  "v1.35.0",
			OperatorNamespace: "network-operator",
		},
		Profile: ProfileInfo{
			Fabric:         "ethernet",
			DeploymentType: "sriov",
			Multirail:      true,
		},
		Nodes: []NodeInfo{
			{Name: "node-a", MachineLabel: "vendor-a-h200", GpuLabel: "h200", Role: "control-plane,worker"},
			{Name: "node-b", MachineLabel: "vendor-a-h200", GpuLabel: "h200", Role: "worker"},
		},
		NodeGroups: []NodeGroupInfo{
			{
				Identifier: "vendor-a-h200", MachineType: "vendor-a", GPUType: "h200",
				LinkType:     "Ethernet",
				NodeSelector: map[string]string{"nvidia.kubernetes-launch-kit.machine": "vendor-a-h200"},
				WorkerNodes:  []string{"node-a", "node-b"},
				SriovCapable: true, RdmaCapable: true,
				EastWestPFs: []PFInfo{
					{PciAddress: "0000:08:00.0", DeviceID: "1021", Rail: "0", Traffic: "east-west",
						NetworkInterface: "eth_r0", RdmaDevice: "rdma_r0", PartNumber: "MCX713106AE", NumaNode: "0", ConnectedGPU: "0"},
					{PciAddress: "0000:08:00.1", DeviceID: "1021", Rail: "1", Traffic: "east-west",
						NetworkInterface: "eth_r1", RdmaDevice: "rdma_r1", PartNumber: "MCX713106AE", NumaNode: "0", ConnectedGPU: "1"},
				},
				PresetDeviations: []PresetDeviation{
					{Field: "deviceID", Expected: "1023", Got: "1021", Detail: "expected ConnectX-8 (1023), found ConnectX-7 (1021)"},
				},
				PresetApplied: true,
			},
		},
		Release: &networkoperatorplugin.VersionCheck{
			SelectedRelease: "26.4",
			ExpectedVersion: "v26.4.0-beta.9",
			DeployedRelease: &networkoperatorplugin.HelmReleaseInfo{
				Name: "network-operator", ChartName: "network-operator",
				ChartVersion: "26.4.0-beta.9", AppVersion: "v26.4.0-beta.9",
				Revision: 1, Status: "deployed",
			},
			Match: true,
		},
		Manifests: []networkoperatorplugin.ValidationResult{
			{
				Kind: "NicClusterPolicy", APIVersion: "mellanox.com/v1alpha1",
				Name: "nic-cluster-policy", SourceFile: "10-nicclusterpolicy.yaml",
				State:  crstate.StateSuccess,
				Reason: "ready — ready: 12/12",
				Details: map[string]string{
					"multus":      "ready",
					"ofed-driver": "ready",
				},
				LiveYAML: "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: nic-cluster-policy\nstatus:\n  state: ready\n",
				Found:    true,
			},
			{
				Kind: "SriovNetworkNodePolicy", APIVersion: "sriovnetwork.openshift.io/v1",
				Name: "ethernet-sriov-rail-0", Namespace: "network-operator",
				SourceFile: "40-sriovnetworknodepolicy.yaml",
				State:      crstate.StateInProgress,
				Reason:     "node-b: syncStatus=InProgress",
				Details: map[string]string{
					"node-a": "Succeeded",
					"node-b": "InProgress",
				},
				Found: true,
			},
			{
				Kind: "MacvlanNetwork", APIVersion: "mellanox.com/v1alpha1",
				Name: "macvlan-net", SourceFile: "30-macvlannetwork.yaml",
				State:  crstate.StateError,
				Reason: "controller reports error: image pull failed",
			},
			{
				Kind: "IPPool", APIVersion: "nv-ipam.nvidia.com/v1alpha1",
				Name: "rail-0-pool", Namespace: "network-operator",
				SourceFile: "20-ippool.yaml",
				State:      crstate.StateNotDeployed,
				Reason:     "not found in cluster",
				Missing:    true,
			},
		},
		Matrix: &MatrixResult{
			PingResults: []PingResult{
				{
					Test: PingTest{Kind: PingSameRail,
						SrcNode: "node-a", DstNode: "node-b",
						Rail: "rail-0", SrcRail: "rail-0", DstRail: "rail-0"},
					OK: true, PacketLoss: 0, RTTAvgMs: 0.12,
				},
				{
					Test: PingTest{Kind: PingSameRail,
						SrcNode: "node-b", DstNode: "node-a",
						Rail: "rail-0", SrcRail: "rail-0", DstRail: "rail-0"},
					OK: true, PacketLoss: 0, RTTAvgMs: 0.15,
				},
				{
					Test: PingTest{Kind: PingSameRail,
						SrcNode: "node-a", DstNode: "node-b",
						Rail: "rail-1", SrcRail: "rail-1", DstRail: "rail-1"},
					OK: true, PacketLoss: 0, RTTAvgMs: 0.18,
				},
				{
					Test: PingTest{Kind: PingSameRail,
						SrcNode: "node-b", DstNode: "node-a",
						Rail: "rail-1", SrcRail: "rail-1", DstRail: "rail-1"},
					OK: false, PacketLoss: 100,
				},
				{
					Test: PingTest{Kind: PingCrossRail,
						SrcNode: "node-a", DstNode: "node-b",
						Rail: "rail-0→rail-1", SrcRail: "rail-0", DstRail: "rail-1"},
					OK: true, PacketLoss: 0, RTTAvgMs: 0.20,
				},
			},
			Summary: MatrixSummary{TotalTests: 5, Passed: 4, Failed: 1},
		},
		Warnings: []string{
			"SriovNetworkNodePolicy/ethernet-sriov-rail-0 is in-progress on 1/2 nodes — re-run later.",
			"IPPool/rail-0-pool not found — l8k generate was not run before validate.",
		},
	}
}

const goldenPath = "testdata/verify-report.golden.html"

func TestRenderHTML_Golden(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, RenderHTML(&buf, fixtureData()))

	if *updateGolden {
		require.NoError(t, os.MkdirAll(filepath.Dir(goldenPath), 0o755))
		require.NoError(t, os.WriteFile(goldenPath, buf.Bytes(), 0o644))
		t.Logf("rewrote golden %s (%d bytes)", goldenPath, buf.Len())
		return
	}

	want, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "missing golden file %s — run with -update-golden once to seed it", goldenPath)

	got := buf.Bytes()
	if !bytes.Equal(got, want) {
		// Surface a short diff hint without dumping the whole
		// rendered HTML into the test log.
		t.Fatalf("rendered HTML does not match %s\n"+
			"  want %d bytes\n"+
			"  got  %d bytes\n"+
			"re-run with `-update-golden` if the change is intentional",
			goldenPath, len(want), len(got))
	}
}

func TestRenderHTML_HandlesNilOptionalFields(t *testing.T) {
	data := ReportData{
		Cluster: ClusterInfo{L8kVersion: "v0", GeneratedAt: time.Now()},
		// Profile, Nodes, Release, Manifests, Matrix, Warnings all
		// zero / nil — renderer must produce a valid document.
	}
	var buf bytes.Buffer
	require.NoError(t, RenderHTML(&buf, data))
	assert.Contains(t, buf.String(), "<html")
	assert.Contains(t, buf.String(), "Connectivity testing was not run")
}
