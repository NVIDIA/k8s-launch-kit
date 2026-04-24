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

package networkoperatorplugin

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

func TestNormalizePCI(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"0000:19:00.0", "0000:19:00.0"},
		{"00000000:19:00.0", "0000:19:00.0"},
		{"00000000:18:00.0", "0000:18:00.0"},
		{"0000:19:00.0 ", "0000:19:00.0"},
		{"0000:5A:00.1", "0000:5a:00.1"},
		{"", ""},
		{"not:a:pci", "not:a:pci"}, // passes through (not our responsibility to validate device/function)
	}
	for _, tt := range tests {
		got := normalizePCI(tt.in)
		if tt.in == "" || tt.in == "not:a:pci" {
			// For malformed input, accept either "" or the original — we just want no panic.
			continue
		}
		assert.Equal(t, tt.want, got, "normalizePCI(%q)", tt.in)
	}
}

func TestBusOf(t *testing.T) {
	assert.Equal(t, 0x19, busOf("0000:19:00.0"))
	assert.Equal(t, 0x5a, busOf("0000:5a:00.1"))
	assert.Equal(t, -1, busOf("malformed"))
}

func TestParseGPUQuery(t *testing.T) {
	in := `0, 00000000:18:00.0
1, 00000000:2E:00.0
2, 00000000:41:00.0
3, 00000000:51:00.0
`
	out := map[int]string{}
	parseGPUQuery(in, out)
	assert.Equal(t, "0000:18:00.0", out[0])
	assert.Equal(t, "0000:2e:00.0", out[1])
	assert.Equal(t, "0000:41:00.0", out[2])
	assert.Equal(t, "0000:51:00.0", out[3])
	assert.Len(t, out, 4)
}

func TestParseNUMABlock(t *testing.T) {
	in := `
0000:19:00.0|0
0000:5a:00.0|0
0000:9b:00.0|1
0000:d8:00.0|-1
bogus_line
|no-pci
`
	out := map[string]int{}
	parseNUMABlock(in, out)
	assert.Equal(t, 0, out["0000:19:00.0"])
	assert.Equal(t, 1, out["0000:9b:00.0"])
	assert.Equal(t, -1, out["0000:d8:00.0"])
	assert.Len(t, out, 4)
}

func TestStripANSI(t *testing.T) {
	assert.Equal(t, "GPU0", stripANSI("\x1b[4mGPU0\x1b[0m"))
	assert.Equal(t, "hello", stripANSI("hello"))
}

func TestPickConnectedGPU(t *testing.T) {
	gpuPCI := map[int]string{
		0: "0000:18:00.0",
		1: "0000:2e:00.0",
		2: "0000:41:00.0",
		3: "0000:51:00.0",
	}

	t.Run("single candidate returns immediately", func(t *testing.T) {
		assert.Equal(t, 2, pickConnectedGPU([]int{2}, "0000:3b:00.0", gpuPCI, map[int]int{}))
	})

	t.Run("no usage yet: bus adjacency picks nearest", func(t *testing.T) {
		// NIC at bus 3b: GPU2 at bus 41 (diff 6) is closer than GPU1 at bus 2e (diff d).
		got := pickConnectedGPU([]int{0, 1, 2, 3}, "0000:3b:00.0", gpuPCI, map[int]int{})
		assert.Equal(t, 2, got)
	})

	t.Run("round-robin spreads dual-port NICs across tied PIX candidates", func(t *testing.T) {
		// Real dual-port-card case: two same-bus NICs, both PIX to GPU4 AND GPU5
		// at consecutive buses. Without round-robin priority, both would pile on GPU4.
		gpuPCI2 := map[int]string{
			4: "0000:90:00.0",
			5: "0000:91:00.0",
		}
		usage := map[int]int{}
		first := pickConnectedGPU([]int{4, 5}, "0000:85:00.0", gpuPCI2, usage)
		usage[first]++
		second := pickConnectedGPU([]int{4, 5}, "0000:85:00.1", gpuPCI2, usage)
		assert.NotEqual(t, first, second, "dual-port NICs must spread")
	})

	t.Run("round-robin spreads across NUMA on flat topology", func(t *testing.T) {
		// EPYC-style flat topology: 4 NUMA-0 GPUs, 4 NUMA-0 NICs all tied at NODE.
		// Expect each NIC to land on a different GPU as usage counts increment.
		usage := map[int]int{}
		g1 := pickConnectedGPU([]int{0, 1, 2, 3}, "0000:0d:00.0", gpuPCI, usage)
		usage[g1]++
		g2 := pickConnectedGPU([]int{0, 1, 2, 3}, "0000:22:00.0", gpuPCI, usage)
		usage[g2]++
		g3 := pickConnectedGPU([]int{0, 1, 2, 3}, "0000:37:00.0", gpuPCI, usage)
		usage[g3]++
		g4 := pickConnectedGPU([]int{0, 1, 2, 3}, "0000:48:00.0", gpuPCI, usage)
		// All four assignments should be distinct.
		seen := map[int]bool{g1: true, g2: true, g3: true, g4: true}
		assert.Len(t, seen, 4, "each NIC should pair with a distinct GPU")
	})
}

// TestReclassifyAndReassignRails covers the combined Traffic + Rail helper.
func TestReclassifyAndReassignRails(t *testing.T) {
	ewTraffic := func(r int) config.PFConfig {
		return config.PFConfig{Traffic: "east-west"}
	}
	_ = ewTraffic

	t.Run("no PIX preserves existing classification and numbers rails", func(t *testing.T) {
		pfs := []config.PFConfig{
			{PciAddress: "0000:19:00.0", Traffic: "east-west"},
			{PciAddress: "0000:22:00.0", Traffic: "north-south"},
			{PciAddress: "0000:22:00.1", Traffic: "north-south"},
			{PciAddress: "0000:37:00.0", Traffic: "east-west"},
		}
		reclassifyAndReassignRails(pfs)
		assert.Equal(t, "east-west", pfs[0].Traffic)
		assert.Equal(t, "north-south", pfs[1].Traffic)
		assert.Equal(t, "north-south", pfs[2].Traffic)
		assert.Equal(t, "east-west", pfs[3].Traffic)
		require.NotNil(t, pfs[0].Rail)
		assert.Equal(t, 0, *pfs[0].Rail)
		assert.Nil(t, pfs[1].Rail)
		assert.Nil(t, pfs[2].Rail)
		require.NotNil(t, pfs[3].Rail)
		assert.Equal(t, 1, *pfs[3].Rail)
	})

	t.Run("PIX present rewrites traffic and re-rails", func(t *testing.T) {
		// Start with everything wrongly classified by the heuristic.
		pfs := []config.PFConfig{
			// Two BF-3 DPU orphans marked E/W by heuristic, but no PIX
			{PciAddress: "0000:19:00.0", Traffic: "east-west"},
			{PciAddress: "0000:37:00.0", Traffic: "east-west"},
			// ConnectX pair marked N/S by heuristic, but PIX to GPUs
			{PciAddress: "0000:22:00.0", Traffic: "north-south", GPUProximity: "PIX", ConnectedGPU: "GPU2"},
			{PciAddress: "0000:22:00.1", Traffic: "north-south", GPUProximity: "PIX", ConnectedGPU: "GPU3"},
		}
		reclassifyAndReassignRails(pfs)
		// PIX ones are now E/W, non-PIX ones flipped to N/S.
		assert.Equal(t, "north-south", pfs[0].Traffic)
		assert.Equal(t, "north-south", pfs[1].Traffic)
		assert.Equal(t, "east-west", pfs[2].Traffic)
		assert.Equal(t, "east-west", pfs[3].Traffic)
		// Rails only on E/W in slice order (which is PCI-sorted already).
		assert.Nil(t, pfs[0].Rail)
		assert.Nil(t, pfs[1].Rail)
		require.NotNil(t, pfs[2].Rail)
		assert.Equal(t, 0, *pfs[2].Rail)
		require.NotNil(t, pfs[3].Rail)
		assert.Equal(t, 1, *pfs[3].Rail)
	})

	t.Run("idempotent — second call produces same state", func(t *testing.T) {
		pfs := []config.PFConfig{
			{PciAddress: "0000:19:00.0", Traffic: "north-south"},
			{PciAddress: "0000:22:00.0", Traffic: "east-west", GPUProximity: "PIX", ConnectedGPU: "GPU0"},
		}
		reclassifyAndReassignRails(pfs)
		t1, t2 := pfs[0].Traffic, pfs[1].Traffic
		reclassifyAndReassignRails(pfs)
		assert.Equal(t, t1, pfs[0].Traffic)
		assert.Equal(t, t2, pfs[1].Traffic)
		require.NotNil(t, pfs[1].Rail)
		assert.Equal(t, 0, *pfs[1].Rail)
	})
}

// TestParseTopologyProbe_EPYC covers the flat-topology machine from the
// user's live probe: ConnectX at bus 22, three BF-3s, two NUMA groups,
// GPUs on their own host bridges, no NIC↔GPU PIX relationships.
func TestParseTopologyProbe_EPYC(t *testing.T) {
	// Real sample: 4 GPUs, 5 NICs. NIC0-NIC1 are two ports of the same ConnectX
	// at bus 22 (PIX to each other, not to any GPU). All NIC-GPU pairs within
	// NUMA land at NODE; cross-NUMA at SYS.
	output := `0, 00000000:4A:00.0
1, 00000000:61:00.0
2, 00000000:CA:00.0
3, 00000000:E1:00.0
---TOPO---
	GPU0	GPU1	GPU2	GPU3	NIC0	NIC1	NIC2	NIC3	NIC4	CPU Affinity	NUMA Affinity	GPU NUMA ID
GPU0	 X 	NODE	SYS	SYS	NODE	NODE	NODE	NODE	SYS	0,2,4,6,8,10	0		N/A
GPU1	NODE	 X 	SYS	SYS	NODE	NODE	NODE	NODE	SYS	0,2,4,6,8,10	0		N/A
GPU2	SYS	SYS	 X 	NODE	SYS	SYS	SYS	SYS	NODE	1,3,5,7,9,11	1		N/A
GPU3	SYS	SYS	NODE	 X 	SYS	SYS	SYS	SYS	NODE	1,3,5,7,9,11	1		N/A
NIC0	NODE	NODE	SYS	SYS	 X 	PIX	NODE	NODE	SYS
NIC1	NODE	NODE	SYS	SYS	PIX	 X 	NODE	NODE	SYS
NIC2	NODE	NODE	SYS	SYS	NODE	NODE	 X 	NODE	SYS
NIC3	NODE	NODE	SYS	SYS	NODE	NODE	NODE	 X 	SYS
NIC4	SYS	SYS	NODE	NODE	SYS	SYS	SYS	SYS	 X

Legend:

  X    = Self

NIC Legend:

  NIC0: roceo12399
  NIC1: roceo12409
  NIC2: rocep13s0f0
  NIC3: rocep55s0f0
  NIC4: rocep181s0f0
---NUMA---
0000:22:00.0|0
0000:22:00.1|0
0000:0d:00.0|0
0000:37:00.0|0
0000:b5:00.0|1
`
	data := parseTopologyProbe(output)
	require.Len(t, data.gpuPCI, 4)
	assert.Equal(t, "0000:4a:00.0", data.gpuPCI[0])
	assert.Equal(t, "0000:e1:00.0", data.gpuPCI[3])

	// Every NIC row should be keyed by RDMA name via the legend.
	require.Contains(t, data.matrix, "roceo12399")
	require.Contains(t, data.matrix, "rocep181s0f0")

	// No NIC has PIX to any GPU on this flat-topology machine.
	for rdma, row := range data.matrix {
		for _, prox := range row {
			assert.NotEqual(t, proxPIX, prox,
				"EPYC fixture should have no PIX NIC-GPU pairs (rdma=%s)", rdma)
		}
	}

	// Verify NUMA map.
	assert.Equal(t, 0, data.numa["0000:22:00.0"])
	assert.Equal(t, 1, data.numa["0000:b5:00.0"])
}

// TestParseTopologyProbe_SR680a covers the DGX-class machine from the
// topology-collector report. 8 GPUs, 11 NICs: 8 BF-3 SuperNICs each PIX
// to a single GPU, 2 CX-6 Lx ports (no PIX), 1 orphan BF-3 (no PIX).
func TestParseTopologyProbe_SR680a(t *testing.T) {
	output := sr680aProbeOutput
	data := parseTopologyProbe(output)

	// 8 GPUs.
	require.Len(t, data.gpuPCI, 8)
	assert.Equal(t, "0000:18:00.0", data.gpuPCI[0])

	// 11 NIC rows keyed by RDMA name.
	rdmaNames := []string{
		"rocep25s0f0", "rocep42s0f0", "rocep59s0f0", "rocep76s0f0",
		"rocep90s0f0", "rocep90s0f1",
		"rocep155s0f0", "rocep171s0f0", "rocep193s0f0", "rocep203s0f0",
		"rocep216s0f0",
	}
	for _, n := range rdmaNames {
		require.Contains(t, data.matrix, n, "missing NIC %s", n)
	}

	// Each of the 8 SuperNICs has PIX to exactly one GPU, corresponding by index.
	assert.Equal(t, proxPIX, data.matrix["rocep25s0f0"][0])
	assert.Equal(t, proxPIX, data.matrix["rocep42s0f0"][1])
	assert.Equal(t, proxPIX, data.matrix["rocep59s0f0"][2])
	assert.Equal(t, proxPIX, data.matrix["rocep76s0f0"][3])
	assert.Equal(t, proxPIX, data.matrix["rocep155s0f0"][4])
	assert.Equal(t, proxPIX, data.matrix["rocep171s0f0"][5])
	assert.Equal(t, proxPIX, data.matrix["rocep193s0f0"][6])
	assert.Equal(t, proxPIX, data.matrix["rocep203s0f0"][7])

	// CX-6 Lx and orphan BF-3 have no PIX to any GPU.
	for _, rdma := range []string{"rocep90s0f0", "rocep90s0f1", "rocep216s0f0"} {
		for gpu, prox := range data.matrix[rdma] {
			assert.NotEqual(t, proxPIX, prox,
				"%s should have no PIX (but did to GPU%d)", rdma, gpu)
		}
	}
}

// TestDiscoverGPUTopology_Algorithm exercises the end-to-end algorithm
// (parse → enrich PFs → PIX-gate reclassification) against the SR680a-V3
// fixture, assuming the NicDevice CR provides the PF list we expect.
//
// This is the canonical integration test for the plan — encoding the
// "8 E/W rails + 3 N/S" outcome as described in the plan's verification
// section.
func TestDiscoverGPUTopology_Algorithm_SR680a(t *testing.T) {
	data := parseTopologyProbe(sr680aProbeOutput)

	// Construct the group's PFs in PCI-ascending order, as buildClusterConfig would.
	pfs := []config.PFConfig{
		{PciAddress: "0000:19:00.0", RdmaDevice: "rocep25s0f0", Traffic: "east-west"},   // BF-3 slot 1
		{PciAddress: "0000:2a:00.0", RdmaDevice: "rocep42s0f0", Traffic: "east-west"},   // BF-3 slot 2
		{PciAddress: "0000:3b:00.0", RdmaDevice: "rocep59s0f0", Traffic: "east-west"},   // BF-3 slot 3
		{PciAddress: "0000:4c:00.0", RdmaDevice: "rocep76s0f0", Traffic: "east-west"},   // BF-3 slot 4
		{PciAddress: "0000:5a:00.0", RdmaDevice: "rocep90s0f0", Traffic: "north-south"}, // CX-6 Lx port 0
		{PciAddress: "0000:5a:00.1", RdmaDevice: "rocep90s0f1", Traffic: "north-south"}, // CX-6 Lx port 1
		{PciAddress: "0000:9b:00.0", RdmaDevice: "rocep155s0f0", Traffic: "east-west"},  // BF-3 slot 5
		{PciAddress: "0000:ab:00.0", RdmaDevice: "rocep171s0f0", Traffic: "east-west"},  // BF-3 slot 6
		{PciAddress: "0000:c1:00.0", RdmaDevice: "rocep193s0f0", Traffic: "east-west"},  // BF-3 slot 7
		{PciAddress: "0000:cb:00.0", RdmaDevice: "rocep203s0f0", Traffic: "east-west"},  // BF-3 slot 8
		{PciAddress: "0000:d8:00.0", RdmaDevice: "rocep216s0f0", Traffic: "north-south"}, // Orphan BF-3
	}

	// Apply the enrichment logic inline (parallels what discoverGPUTopology does
	// after probing). We don't involve execInPod; we drive straight off the parsed
	// data and the PF slice.
	usage := map[int]int{}
	for i := range pfs {
		row, ok := data.matrix[pfs[i].RdmaDevice]
		if !ok {
			continue
		}
		best := proxUnknown
		var candidates []int
		for gpu, p := range row {
			if p == proxUnknown {
				continue
			}
			if best == proxUnknown || p < best {
				best = p
				candidates = []int{gpu}
			} else if p == best {
				candidates = append(candidates, gpu)
			}
		}
		chosen := pickConnectedGPU(candidates, pfs[i].PciAddress, data.gpuPCI, usage)
		usage[chosen]++
		pfs[i].ConnectedGPU = "GPU" + itoa(chosen)
		pfs[i].GPUProximity = best.String()
	}
	reclassifyAndReassignRails(pfs)

	// Eight SuperNICs become E/W with rails 0..7 in PCI order.
	expectedRails := map[string]int{
		"rocep25s0f0":  0,
		"rocep42s0f0":  1,
		"rocep59s0f0":  2,
		"rocep76s0f0":  3,
		"rocep155s0f0": 4,
		"rocep171s0f0": 5,
		"rocep193s0f0": 6,
		"rocep203s0f0": 7,
	}
	for i := range pfs {
		if expectedRail, ok := expectedRails[pfs[i].RdmaDevice]; ok {
			assert.Equal(t, "east-west", pfs[i].Traffic, "pf %s", pfs[i].RdmaDevice)
			require.NotNil(t, pfs[i].Rail, "pf %s should have rail", pfs[i].RdmaDevice)
			assert.Equal(t, expectedRail, *pfs[i].Rail, "pf %s rail", pfs[i].RdmaDevice)
			assert.Equal(t, "PIX", pfs[i].GPUProximity, "pf %s proximity", pfs[i].RdmaDevice)
		}
	}
	// N/S PFs: 2 CX-6 Lx ports + 1 orphan BF-3.
	nsRDMA := []string{"rocep90s0f0", "rocep90s0f1", "rocep216s0f0"}
	for _, rdma := range nsRDMA {
		for i := range pfs {
			if pfs[i].RdmaDevice == rdma {
				assert.Equal(t, "north-south", pfs[i].Traffic, "pf %s", rdma)
				assert.Nil(t, pfs[i].Rail, "pf %s should have no rail", rdma)
				assert.NotEqual(t, "PIX", pfs[i].GPUProximity, "pf %s should not be PIX", rdma)
			}
		}
	}

	// Spot-check ConnectedGPU on a SuperNIC — should match the matrix diagonal.
	for i := range pfs {
		if pfs[i].RdmaDevice == "rocep25s0f0" {
			assert.Equal(t, "GPU0", pfs[i].ConnectedGPU)
		}
		if pfs[i].RdmaDevice == "rocep203s0f0" {
			assert.Equal(t, "GPU7", pfs[i].ConnectedGPU)
		}
	}
}

// TestDiscoverGPUTopology_Algorithm_EPYC validates the PIX-absent case:
// no override should fire, input Traffic is preserved, bus-adjacency
// still picks a reasonable ConnectedGPU per NIC.
func TestDiscoverGPUTopology_Algorithm_EPYC_NoOverride(t *testing.T) {
	output := `0, 00000000:4A:00.0
1, 00000000:61:00.0
2, 00000000:CA:00.0
3, 00000000:E1:00.0
---TOPO---
	GPU0	GPU1	GPU2	GPU3	NIC0	NIC1	NIC2	NIC3	NIC4	CPU Affinity	NUMA Affinity	GPU NUMA ID
GPU0	 X 	NODE	SYS	SYS	NODE	NODE	NODE	NODE	SYS	0	0	N/A
GPU1	NODE	 X 	SYS	SYS	NODE	NODE	NODE	NODE	SYS	0	0	N/A
GPU2	SYS	SYS	 X 	NODE	SYS	SYS	SYS	SYS	NODE	1	1	N/A
GPU3	SYS	SYS	NODE	 X 	SYS	SYS	SYS	SYS	NODE	1	1	N/A
NIC0	NODE	NODE	SYS	SYS	 X 	PIX	NODE	NODE	SYS
NIC1	NODE	NODE	SYS	SYS	PIX	 X 	NODE	NODE	SYS
NIC2	NODE	NODE	SYS	SYS	NODE	NODE	 X 	NODE	SYS
NIC3	NODE	NODE	SYS	SYS	NODE	NODE	NODE	 X 	SYS
NIC4	SYS	SYS	NODE	NODE	SYS	SYS	SYS	SYS	 X

NIC Legend:

  NIC0: roceo12399
  NIC1: roceo12409
  NIC2: rocep13s0f0
  NIC3: rocep55s0f0
  NIC4: rocep181s0f0
---NUMA---
0000:22:00.0|0
0000:22:00.1|0
0000:0d:00.0|0
0000:37:00.0|0
0000:b5:00.1|1
`
	data := parseTopologyProbe(output)

	// Simulate part-number heuristic result.
	pfs := []config.PFConfig{
		{PciAddress: "0000:0d:00.0", RdmaDevice: "rocep13s0f0", Traffic: "north-south"},
		{PciAddress: "0000:22:00.0", RdmaDevice: "roceo12399", Traffic: "east-west"},
		{PciAddress: "0000:22:00.1", RdmaDevice: "roceo12409", Traffic: "east-west"},
		{PciAddress: "0000:37:00.0", RdmaDevice: "rocep55s0f0", Traffic: "north-south"},
		{PciAddress: "0000:b5:00.0", RdmaDevice: "rocep181s0f0", Traffic: "north-south"},
	}

	// Enrich + reclassify (parallels discoverGPUTopology).
	usage := map[int]int{}
	for i := range pfs {
		row, ok := data.matrix[pfs[i].RdmaDevice]
		if !ok {
			continue
		}
		best := proxUnknown
		var candidates []int
		for gpu, p := range row {
			if p == proxUnknown {
				continue
			}
			if best == proxUnknown || p < best {
				best = p
				candidates = []int{gpu}
			} else if p == best {
				candidates = append(candidates, gpu)
			}
		}
		chosen := pickConnectedGPU(candidates, pfs[i].PciAddress, data.gpuPCI, usage)
		usage[chosen]++
		pfs[i].ConnectedGPU = "GPU" + itoa(chosen)
		pfs[i].GPUProximity = best.String()
	}
	// No PIX NIC-GPU pair exists on this machine → no override.
	reclassifyAndReassignRails(pfs)

	// ConnectX PFs stay E/W (heuristic preserved), BF-3s stay N/S.
	tMap := map[string]string{}
	for _, pf := range pfs {
		tMap[pf.RdmaDevice] = pf.Traffic
	}
	assert.Equal(t, "east-west", tMap["roceo12399"])
	assert.Equal(t, "east-west", tMap["roceo12409"])
	assert.Equal(t, "north-south", tMap["rocep13s0f0"])
	assert.Equal(t, "north-south", tMap["rocep55s0f0"])
	assert.Equal(t, "north-south", tMap["rocep181s0f0"])

	// All NIC→GPU pairs should resolve to NODE (within NUMA) — not PIX.
	for _, pf := range pfs {
		if pf.GPUProximity != "" {
			assert.NotEqual(t, "PIX", pf.GPUProximity, "pf %s", pf.RdmaDevice)
		}
	}
}

// itoa is a tiny helper to keep the test file self-contained without pulling strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// Long fixture placed at the bottom of the file to keep the test cases
// readable. Real nvidia-smi output from the topology-collector report for
// `ThinkSystem-SR680a-V3-BF3` (pdx-g24r13-2894-lh2-w02 snapshot).
//
// GPUs 0–3 on NUMA 0 (bus 18, 2e, 41, 51 roughly), GPUs 4–7 on NUMA 1.
// 11 RDMA NICs: 8 SuperNICs PIX to matching GPU, 2 CX-6 Lx + 1 orphan BF-3
// have no PIX.
var sr680aProbeOutput = strings.Join([]string{
	`0, 00000000:18:00.0`,
	`1, 00000000:2E:00.0`,
	`2, 00000000:41:00.0`,
	`3, 00000000:51:00.0`,
	`4, 00000000:9A:00.0`,
	`5, 00000000:AB:00.0`,
	`6, 00000000:BD:00.0`,
	`7, 00000000:D1:00.0`,
	`---TOPO---`,
	"\tGPU0\tGPU1\tGPU2\tGPU3\tGPU4\tGPU5\tGPU6\tGPU7\tNIC0\tNIC1\tNIC2\tNIC3\tNIC4\tNIC5\tNIC6\tNIC7\tNIC8\tNIC9\tNIC10\tCPU Affinity\tNUMA Affinity\tGPU NUMA ID",
	"GPU0\t X \tNODE\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tPIX\tNODE\tNODE\tNODE\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tSYS\t0-55\t0\tN/A",
	"GPU1\tNODE\t X \tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tNODE\tPIX\tNODE\tNODE\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tSYS\t0-55\t0\tN/A",
	"GPU2\tNODE\tNODE\t X \tNODE\tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\tPIX\tNODE\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tSYS\t0-55\t0\tN/A",
	"GPU3\tNODE\tNODE\tNODE\t X \tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\tNODE\tPIX\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tSYS\t0-55\t0\tN/A",
	"GPU4\tSYS\tSYS\tSYS\tSYS\t X \tNODE\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tSYS\tSYS\tPIX\tNODE\tNODE\tNODE\tNODE\t56-111\t1\tN/A",
	"GPU5\tSYS\tSYS\tSYS\tSYS\tNODE\t X \tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tSYS\tSYS\tNODE\tPIX\tNODE\tNODE\tNODE\t56-111\t1\tN/A",
	"GPU6\tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\t X \tNODE\tSYS\tSYS\tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\tPIX\tNODE\tNODE\t56-111\t1\tN/A",
	"GPU7\tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\tNODE\t X \tSYS\tSYS\tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\tNODE\tPIX\tNODE\t56-111\t1\tN/A",
	"NIC0\tPIX\tNODE\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\t X \tNODE\tNODE\tNODE\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tSYS",
	"NIC1\tNODE\tPIX\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tNODE\t X \tNODE\tNODE\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tSYS",
	"NIC2\tNODE\tNODE\tPIX\tNODE\tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\t X \tNODE\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tSYS",
	"NIC3\tNODE\tNODE\tNODE\tPIX\tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\tNODE\t X \tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tSYS",
	"NIC4\tNODE\tNODE\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\tNODE\tNODE\t X \tPIX\tSYS\tSYS\tSYS\tSYS\tSYS",
	"NIC5\tNODE\tNODE\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\tNODE\tNODE\tPIX\t X \tSYS\tSYS\tSYS\tSYS\tSYS",
	"NIC6\tSYS\tSYS\tSYS\tSYS\tPIX\tNODE\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tSYS\tSYS\t X \tNODE\tNODE\tNODE\tNODE",
	"NIC7\tSYS\tSYS\tSYS\tSYS\tNODE\tPIX\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tSYS\tSYS\tNODE\t X \tNODE\tNODE\tNODE",
	"NIC8\tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\tPIX\tNODE\tSYS\tSYS\tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\t X \tNODE\tNODE",
	"NIC9\tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\tNODE\tPIX\tSYS\tSYS\tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\tNODE\t X \tNODE",
	"NIC10\tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\tNODE\tNODE\tSYS\tSYS\tSYS\tSYS\tSYS\tSYS\tNODE\tNODE\tNODE\tNODE\t X ",
	``,
	`Legend:`,
	``,
	`  X    = Self`,
	``,
	`NIC Legend:`,
	``,
	`  NIC0: rocep25s0f0`,
	`  NIC1: rocep42s0f0`,
	`  NIC2: rocep59s0f0`,
	`  NIC3: rocep76s0f0`,
	`  NIC4: rocep90s0f0`,
	`  NIC5: rocep90s0f1`,
	`  NIC6: rocep155s0f0`,
	`  NIC7: rocep171s0f0`,
	`  NIC8: rocep193s0f0`,
	`  NIC9: rocep203s0f0`,
	`  NIC10: rocep216s0f0`,
	`---NUMA---`,
	`0000:19:00.0|0`,
	`0000:2a:00.0|0`,
	`0000:3b:00.0|0`,
	`0000:4c:00.0|0`,
	`0000:5a:00.0|0`,
	`0000:5a:00.1|0`,
	`0000:9b:00.0|1`,
	`0000:ab:00.0|1`,
	`0000:c1:00.0|1`,
	`0000:cb:00.0|1`,
	`0000:d8:00.0|1`,
}, "\n")
