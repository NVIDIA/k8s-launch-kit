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
	"fmt"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkPod builds a TestPod fixture with synthetic IPs *and* an RDMA
// device per rail. RDMA stages require both sides of a pair to have an RDMA
// device for the rail, so the test fixture populates RDMADevsByRail alongside
// IP/interface data.
func mkPod(name string, rails ...string) TestPod {
	tp := TestPod{
		Name:                  name,
		IPsByRail:             map[string]string{},
		RDMADevsByRail:        map[string]string{},
		InterfacesByRail:      map[string]string{},
		GPUIndicesByRail:      map[string]int{},
		GPUPCIAddressesByRail: map[string]string{},
	}
	for i, rail := range rails {
		tp.IPsByRail[rail] = fakeIP(name, rail)
		tp.RDMADevsByRail[rail] = fakeRDMADev(name, rail)
		tp.InterfacesByRail[rail] = fakeIface(i)
		tp.GPUIndicesByRail[rail] = i + 2
		tp.GPUPCIAddressesByRail[rail] = fmt.Sprintf("0000:%02x:00.0", i+1)
		tp.RailOrder = append(tp.RailOrder, rail)
	}
	return tp
}

func fakeIP(pod, rail string) string {
	// Cheap, stable, human-readable IPs for assertions.
	switch pod {
	case "pod-a":
		switch rail {
		case "rail-0":
			return "10.0.0.1"
		case "rail-1":
			return "10.0.1.1"
		case "rail-2":
			return "10.0.2.1"
		}
	case "pod-b":
		switch rail {
		case "rail-0":
			return "10.0.0.2"
		case "rail-1":
			return "10.0.1.2"
		case "rail-2":
			return "10.0.2.2"
		}
	case "pod-c":
		switch rail {
		case "rail-0":
			return "10.0.0.3"
		case "rail-1":
			return "10.0.1.3"
		}
	}
	return "0.0.0.0"
}

// fakeRDMADev returns a stable mlx5-style device name per (pod, rail).
// The test only needs the strings to be non-empty for Plan() to emit
// tests; the actual device names are never opened.
func fakeRDMADev(pod, rail string) string {
	return "mlx5_" + pod + "_" + rail
}

func fakeIface(idx int) string {
	return fmt.Sprintf("net%d", idx+1)
}

func TestPlan_SoftSkipOnFewerThan2Pods(t *testing.T) {
	t.Run("zero pods", func(t *testing.T) {
		plan := Plan(nil)
		require.NotNil(t, plan.Skip)
		assert.Contains(t, plan.Skip.Reason, "0 schedulable")
		assert.Empty(t, plan.RDMASameRail)
	})
	t.Run("one pod", func(t *testing.T) {
		plan := Plan([]TestPod{mkPod("pod-a", "rail-0", "rail-1")})
		require.NotNil(t, plan.Skip)
		assert.Contains(t, plan.Skip.Reason, "1 schedulable")
	})
}

func TestNormalizeChecks(t *testing.T) {
	assert.Equal(t, []Check{CheckICMP, CheckRPing, CheckIBWriteBW}, normalizeChecks(nil))
	assert.Empty(t, normalizeChecks([]Check{}))
	assert.Equal(t, []Check{CheckIBWriteBW, CheckRPing},
		normalizeChecks([]Check{CheckIBWriteBW, "", CheckRPing, CheckIBWriteBW}))
}

func TestPlan_TwoPodsOneRail(t *testing.T) {
	plan := Plan([]TestPod{
		mkPod("pod-a", "rail-0"),
		mkPod("pod-b", "rail-0"),
	})
	require.Nil(t, plan.Skip)

	// Same-rail: 2 pods × 1 ordered pair each direction × 1 rail = 2
	// rping tests and 2 ib_write_bw tests.
	assert.Len(t, plan.RDMASameRail, 2)
	assert.Len(t, plan.RDMABwSameRail, 2)
	assert.Len(t, plan.GPUDirectDMABufSameRail, 2)
	assert.Len(t, plan.ICMPSameRail, 2)
	// Cross-rail canary requires ≥2 rails per pod — skipped here.
	assert.Empty(t, plan.RDMACrossRail)
	assert.Empty(t, plan.RDMABwCrossRail)
	assert.Empty(t, plan.GPUDirectDMABufCrossRail)
	assert.Empty(t, plan.ICMPCrossRail)

	// Check direction coverage: both A→B and B→A appear in the rping
	// slice.
	dirs := map[string]bool{}
	for _, t := range plan.RDMASameRail {
		dirs[t.SrcPod+"→"+t.DstPod] = true
	}
	assert.True(t, dirs["pod-a→pod-b"])
	assert.True(t, dirs["pod-b→pod-a"])
	// Every emitted test carries the per-side RDMA device name so
	// ib_write_bw can pass `-d <dev>`.
	for _, tt := range plan.RDMABwSameRail {
		assert.NotEmpty(t, tt.SrcRDMADev, "%+v", tt)
		assert.NotEmpty(t, tt.DstRDMADev, "%+v", tt)
		assert.NotEmpty(t, tt.SrcIface, "%+v", tt)
		assert.NotEmpty(t, tt.DstIface, "%+v", tt)
	}
}

func TestPopulateGPUTopologyUsesNodeRailAndPlane(t *testing.T) {
	rail0, rail1 := 0, 1
	groups := []config.ClusterConfig{{
		WorkerNodes: []string{"node-a"},
		PFs: []config.PFConfig{
			{Traffic: "east-west", Rail: &rail0, PciAddress: "0000:01:00.0", ConnectedGPU: "GPU4", ConnectedGPUPCIAddress: "0000:41:00.0"},
			{Traffic: "east-west", Rail: &rail0, PciAddress: "0000:01:00.1", ConnectedGPU: "GPU5", ConnectedGPUPCIAddress: "0000:51:00.0"},
			{Traffic: "east-west", Rail: &rail1, PciAddress: "0000:02:00.0", ConnectedGPU: "GPU7", ConnectedGPUPCIAddress: "0000:71:00.0"},
		},
	}}
	pod := TestPod{Node: "node-a", RailOrder: []string{"rail-0-plane-0", "rail-0-plane-1", "rail-1"}}
	PopulateGPUTopology(&pod, groups)
	assert.Equal(t, 4, pod.GPUIndicesByRail["rail-0-plane-0"])
	assert.Equal(t, 5, pod.GPUIndicesByRail["rail-0-plane-1"])
	assert.Equal(t, 7, pod.GPUIndicesByRail["rail-1"])
	assert.Equal(t, "0000:71:00.0", pod.GPUPCIAddressesByRail["rail-1"])
}

func TestPlanGPUDirectUsesEndpointSpecificGPUIndices(t *testing.T) {
	src := mkPod("pod-a", "rail-0")
	dst := mkPod("pod-b", "rail-0")
	src.GPUIndicesByRail["rail-0"] = 4
	src.GPUPCIAddressesByRail["rail-0"] = "0000:41:00.0"
	dst.GPUIndicesByRail["rail-0"] = 7
	dst.GPUPCIAddressesByRail["rail-0"] = "0000:71:00.0"

	plan := Plan([]TestPod{src, dst})
	require.Len(t, plan.GPUDirectDMABufSameRail, 2)
	forward := plan.GPUDirectDMABufSameRail[0]
	assert.Equal(t, "pod-a", forward.SrcPod)
	assert.Equal(t, "pod-b", forward.DstPod)
	assert.Equal(t, 4, forward.SrcGPUIndex)
	assert.Equal(t, 7, forward.DstGPUIndex)
	assert.Equal(t, "0000:41:00.0", forward.SrcGPUPCIAddress)
	assert.Equal(t, "0000:71:00.0", forward.DstGPUPCIAddress)
}

func TestPopulateGPUTopologyDoesNotFallbackForAmbiguousRail(t *testing.T) {
	rail0 := 0
	groups := []config.ClusterConfig{{WorkerNodes: []string{"node-a"}, PFs: []config.PFConfig{
		{Traffic: "east-west", Rail: &rail0, PciAddress: "0000:01:00.0", ConnectedGPU: "GPU1"},
		{Traffic: "east-west", Rail: &rail0, PciAddress: "0000:02:00.0", ConnectedGPU: "GPU2"},
	}}}
	pod := TestPod{Node: "node-a", RailOrder: []string{"rail-0"}}
	PopulateGPUTopology(&pod, groups)
	assert.NotContains(t, pod.GPUIndicesByRail, "rail-0")
}

func TestPopulateGPUTopologyDoesNotChooseFirstAmbiguousNodeGroup(t *testing.T) {
	rail0 := 0
	groups := []config.ClusterConfig{
		{WorkerNodes: []string{"node-a"}, PFs: []config.PFConfig{{
			Traffic: "east-west", Rail: &rail0, PciAddress: "0000:01:00.0", ConnectedGPU: "GPU1",
		}}},
		{WorkerNodes: []string{"node-a"}, PFs: []config.PFConfig{{
			Traffic: "east-west", Rail: &rail0, PciAddress: "0000:02:00.0", ConnectedGPU: "GPU6",
		}}},
	}
	pod := TestPod{Node: "node-a", RailOrder: []string{"rail-0"}}
	PopulateGPUTopology(&pod, groups)
	assert.NotContains(t, pod.GPUIndicesByRail, "rail-0")
}

func TestPlan_TwoPodsTwoRails_IncludesCrossRailCanary(t *testing.T) {
	plan := Plan([]TestPod{
		mkPod("pod-a", "rail-0", "rail-1"),
		mkPod("pod-b", "rail-0", "rail-1"),
	})
	require.Nil(t, plan.Skip)

	// Same-rail: 2 pods × 1 ordered pair each direction × 2 rails = 4
	// per family.
	assert.Len(t, plan.RDMASameRail, 4)
	assert.Len(t, plan.RDMABwSameRail, 4)
	assert.Len(t, plan.ICMPSameRail, 4)
	// Quick cross-rail canary: one deterministic pod pair for every
	// ordered rail mapping = 2 per family for two rails.
	assert.Len(t, plan.RDMACrossRail, 2)
	assert.Len(t, plan.RDMABwCrossRail, 2)
	assert.Len(t, plan.ICMPCrossRail, 2)

	// Each cross-rail test should ping rail-0 → rail-1 (the first two
	// rails by sorted order). Concrete IP assertions catch
	// off-by-one in railOrder indexing.
	for _, c := range plan.RDMACrossRail {
		assert.Equal(t, ExpectObserve, c.Expectation)
		assert.NotEqual(t, c.SrcRail, c.DstRail)
	}
}

func TestPlan_ThreePodsThreeRails_FullMatrix(t *testing.T) {
	plan := PlanWithOptions([]TestPod{
		mkPod("pod-a", "rail-0", "rail-1", "rail-2"),
		mkPod("pod-b", "rail-0", "rail-1", "rail-2"),
		mkPod("pod-c", "rail-0", "rail-1"), // shorter rail list — exercise asymmetric case
	}, ModeFull, "")

	require.Nil(t, plan.Skip)

	// Same-rail tests: for each ordered pair (i ≠ j) and for each
	// rail both endpoints have:
	//   - pod-a ↔ pod-b: 3 rails × 2 dirs = 6
	//   - pod-a ↔ pod-c: 2 rails × 2 dirs = 4
	//   - pod-b ↔ pod-c: 2 rails × 2 dirs = 4
	// Total = 14 per family (rping + ib_write_bw each).
	assert.Len(t, plan.RDMASameRail, 14)
	assert.Len(t, plan.RDMABwSameRail, 14)
	assert.Len(t, plan.ICMPSameRail, 14)

	// Full cross-rail: every ordered pod pair x every source rail x
	// every destination rail, excluding same rail.
	assert.Len(t, plan.RDMACrossRail, 28)
	assert.Len(t, plan.RDMABwCrossRail, 28)
	assert.Len(t, plan.ICMPCrossRail, 28)
	for _, c := range plan.RDMACrossRail {
		assert.Equal(t, ExpectObserve, c.Expectation)
	}
}

func TestPlan_StrictCrossRailExpectationFollowsRouting(t *testing.T) {
	pods := []TestPod{
		mkPod("pod-a", "rail-0", "rail-1"),
		mkPod("pod-b", "rail-0", "rail-1"),
	}
	source := PlanWithOptions(pods, ModeStrict, "source-based")
	require.NotEmpty(t, source.RDMACrossRail)
	assert.Equal(t, ExpectRequired, source.RDMACrossRail[0].Expectation)

	destination := PlanWithOptions(pods, ModeStrict, "destination-based")
	require.NotEmpty(t, destination.RDMACrossRail)
	assert.Equal(t, ExpectForbidden, destination.RDMACrossRail[0].Expectation)
}

func TestPlan_StableOrderingAcrossRuns(t *testing.T) {
	pods := []TestPod{
		mkPod("pod-b", "rail-0", "rail-1"),
		mkPod("pod-a", "rail-0", "rail-1"),
		mkPod("pod-c", "rail-0", "rail-1"),
	}
	plan1 := Plan(pods)
	plan2 := Plan(pods)
	require.Equal(t, plan1.RDMASameRail, plan2.RDMASameRail)
	require.Equal(t, plan1.RDMACrossRail, plan2.RDMACrossRail)
	require.Equal(t, plan1.RDMABwSameRail, plan2.RDMABwSameRail)
	require.Equal(t, plan1.RDMABwCrossRail, plan2.RDMABwCrossRail)
	// First test should always be from pod-a (sorted-by-name).
	assert.Equal(t, "pod-a", plan1.RDMASameRail[0].SrcPod)
}

func TestPlan_SkipsPairsWithoutSharedRail(t *testing.T) {
	plan := Plan([]TestPod{
		mkPod("pod-a", "rail-0"),
		mkPod("pod-b", "rail-1"),
	})
	require.Nil(t, plan.Skip)
	// No shared rail → zero same-rail tests.
	assert.Empty(t, plan.RDMASameRail)
	assert.Empty(t, plan.RDMABwSameRail)
	// Each pod only has 1 rail → no cross-rail canary either.
	assert.Empty(t, plan.RDMACrossRail)
	assert.Empty(t, plan.RDMABwCrossRail)
}

func TestPlan_SkipsPairsWithoutRDMADevices(t *testing.T) {
	// Pod with an IP but no RDMA device for the shared rail —
	// Plan() drops those silently since RunIbWriteBw needs -d <dev>.
	a := mkPod("pod-a", "rail-0")
	b := mkPod("pod-b", "rail-0")
	delete(b.RDMADevsByRail, "rail-0")
	plan := Plan([]TestPod{a, b})
	require.Nil(t, plan.Skip)
	assert.Empty(t, plan.RDMASameRail, "no RDMA test should be emitted for the missing-device pair")
	assert.Empty(t, plan.RDMABwSameRail)
	assert.Len(t, plan.ICMPSameRail, 2, "ICMP still only needs IP and interface")
}
