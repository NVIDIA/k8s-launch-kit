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

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/nvidia/k8s-launch-kit/pkg/presetmatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// updateGolden re-writes the checked-in golden file when set. Run with
// `go test ./pkg/networkoperatorplugin/connectivity/ -update-golden`
// after intentional template changes.
var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/k8s-launch-kit-validation-report.golden.html with the current renderer output")

// fixtureData is the deterministic input the renderer is exercised
// against. Includes one of every state badge, an IN-PROGRESS row with
// expandable Details, a passing rail and a failing rail in the matrix,
// and one cross-rail result, so the golden covers the whole template
// surface.
func fixtureData() ReportData {
	return ReportData{
		Verdict: OverallVerdict{
			Pass: false,
			Reasons: []string{
				"1 connectivity test(s) failed in the connectivity matrix",
				`The detected platform topology does not match the certified topology for ACME-vendor-a-h200 server type (see Node groups section for the per-device diff)`,
			},
		},
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
				// Actual = discovered hardware.
				EastWestPFs: []PFInfo{
					// deviceID drift from the certified topology
					// at this PCI — row tinted.
					{PciAddress: "0000:08:00.0", DeviceID: "1021", Rail: "0", Traffic: "east-west",
						NetworkInterface: "eth_r0", RdmaDevice: "rdma_r0", PartNumber: "MCX713106AE", NumaNode: "0", ConnectedGPU: "0",
						Mismatched: true},
					// Matches the certified topology — clean row.
					{PciAddress: "0000:08:00.1", DeviceID: "1023", Rail: "1", Traffic: "east-west",
						NetworkInterface: "eth_r1", RdmaDevice: "rdma_r1", PartNumber: "MCX713106AE", NumaNode: "0", ConnectedGPU: "1"},
					// Cluster has this PCI but the certified
					// topology doesn't — row tinted.
					{PciAddress: "0000:08:00.2", DeviceID: "1023", Rail: "2", Traffic: "east-west",
						NetworkInterface: "eth_r2", RdmaDevice: "rdma_r2", PartNumber: "MCX713106AE", NumaNode: "0", ConnectedGPU: "2",
						Mismatched: true},
				},
				// Expected = certified topology from the matched preset.
				ExpectedEastWestPFs: []PFInfo{
					// Same PCI as Actual[0] but the expected
					// deviceID — tinted in this table too because
					// the actual deviceID drifts.
					{PciAddress: "0000:08:00.0", DeviceID: "1023", Rail: "0", Traffic: "east-west",
						NetworkInterface: "eth_r0", RdmaDevice: "rdma_r0", PartNumber: "MCX713106AE", NumaNode: "0", ConnectedGPU: "0",
						Mismatched: true},
					// Matches the cluster — clean row.
					{PciAddress: "0000:08:00.1", DeviceID: "1023", Rail: "1", Traffic: "east-west",
						NetworkInterface: "eth_r1", RdmaDevice: "rdma_r1", PartNumber: "MCX713106AE", NumaNode: "0", ConnectedGPU: "1"},
					// Certified topology expects this PCI but the
					// cluster doesn't have it — tinted.
					{PciAddress: "0000:08:00.3", DeviceID: "1023", Rail: "3", Traffic: "east-west",
						NetworkInterface: "eth_r3", RdmaDevice: "rdma_r3", PartNumber: "MCX713106AE", NumaNode: "0", ConnectedGPU: "3",
						Mismatched: true},
				},
				PresetDeviations: []PresetDeviation{
					{Field: "deviceID", Expected: "1023@0000:08:00.0", Got: "1021@0000:08:00.0", Detail: "device ID at PCI address differs from preset"},
					{Field: "pciAddress", Got: "0000:08:00.2", Detail: "discovered PCI address not present in preset"},
					{Field: "pciAddress", Expected: "0000:08:00.3", Detail: "preset PCI address not present on discovered hardware"},
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
		ComponentCheck: &networkoperatorplugin.ComponentVersionCheck{
			ExpectedComponent: "network-operator-v26.4.0-beta.9",
			ExpectedDOCA:      "doca3.4.0-26.04-0.8.4.0-0",
			Components: []networkoperatorplugin.ComponentVersionResult{
				{Source: "NicClusterPolicy/nic-cluster-policy", Section: "nvIpam",
					Expected: "network-operator-v26.4.0-beta.9", Actual: "network-operator-v26.4.0-beta.9", Match: true, Kind: "component"},
				{Source: "NicClusterPolicy/nic-cluster-policy", Section: "secondaryNetwork.multus",
					Expected: "network-operator-v26.4.0-beta.9", Actual: "network-operator-v26.4.0-beta.7", Match: false, Kind: "component"},
				{Source: "NicNodePolicy/nicnodepolicy-h200", Section: "ofedDriver",
					Expected: "doca3.4.0-26.04-0.8.4.0-0", Actual: "doca3.4.0-26.04-0.8.4.0-0", Match: true, Kind: "doca"},
			},
			AllMatch: false,
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
				// rping: same-rail, both directions, rail-0 (both pass)
				{
					Test: PingTest{Kind: RDMAPingSameRail,
						SrcNode: "node-a", DstNode: "node-b",
						Rail: "rail-0", SrcRail: "rail-0", DstRail: "rail-0"},
					OK: true,
				},
				{
					Test: PingTest{Kind: RDMAPingSameRail,
						SrcNode: "node-b", DstNode: "node-a",
						Rail: "rail-0", SrcRail: "rail-0", DstRail: "rail-0"},
					OK: true,
				},
				// rping: same-rail, rail-1 (one direction fails)
				{
					Test: PingTest{Kind: RDMAPingSameRail,
						SrcNode: "node-a", DstNode: "node-b",
						Rail: "rail-1", SrcRail: "rail-1", DstRail: "rail-1"},
					OK: true,
				},
				{
					Test: PingTest{Kind: RDMAPingSameRail,
						SrcNode: "node-b", DstNode: "node-a",
						Rail: "rail-1", SrcRail: "rail-1", DstRail: "rail-1"},
					OK: false,
				},
				// rping: cross-rail observation
				{
					Test: PingTest{Kind: RDMAPingCrossRail,
						SrcNode: "node-a", DstNode: "node-b",
						Rail: "rail-0→rail-1", SrcRail: "rail-0", DstRail: "rail-1",
						SrcIface: "net1", DstIface: "net2",
						Expectation: ExpectObserve},
					OK: true, ObservedOK: true, Expectation: ExpectObserve,
				},
				// ib_write_bw: same-rail rail-0 both directions pass with bandwidth
				{
					Test: PingTest{Kind: RDMABwSameRail,
						SrcNode: "node-a", DstNode: "node-b",
						Rail: "rail-0", SrcRail: "rail-0", DstRail: "rail-0"},
					OK: true, BandwidthGbps: 194.4,
				},
				{
					Test: PingTest{Kind: RDMABwSameRail,
						SrcNode: "node-b", DstNode: "node-a",
						Rail: "rail-0", SrcRail: "rail-0", DstRail: "rail-0"},
					OK: true, BandwidthGbps: 193.8,
				},
				// ib_write_bw: cross-rail observation passes with bandwidth
				{
					Test: PingTest{Kind: RDMABwCrossRail,
						SrcNode: "node-a", DstNode: "node-b",
						Rail: "rail-0→rail-1", SrcRail: "rail-0", DstRail: "rail-1",
						SrcIface: "net1", DstIface: "net2",
						Expectation: ExpectObserve},
					OK: true, ObservedOK: true, Expectation: ExpectObserve, BandwidthGbps: 187.6,
				},
			},
			Summary: MatrixSummary{TotalTests: 8, Passed: 7, Failed: 1},
		},
		Warnings: []string{
			"SriovNetworkNodePolicy/ethernet-sriov-rail-0 is in-progress on 1/2 nodes — re-run later.",
			"IPPool/rail-0-pool not found — l8k generate was not run before validate.",
		},
		PresetMatches: []presetmatch.Result{
			{
				Group:        "vendor-a-h200",
				MachineType:  "vendor-a",
				GPUType:      "h200",
				Manufacturer: "ACME",
				Status:       presetmatch.StatusDeviation,
				PresetName:   "vendor-a/h200",
				Reason:       "1 deviation(s) from matched preset",
				Deviations: []config.PresetDeviationEntry{
					{Field: "deviceID", Expected: "1023", Got: "1021", Detail: "expected ConnectX-8 (1023), found ConnectX-7 (1021)"},
				},
			},
		},
	}
}

const goldenPath = "testdata/k8s-launch-kit-validation-report.golden.html"

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
