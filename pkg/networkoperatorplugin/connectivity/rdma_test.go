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
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// realisticIbWriteBwOutput is a captured sample of perftest's
// `ib_write_bw -d <dev> -R -s 65536 --report_gbits <peer>` client
// output. Includes the banner, the column header, the summary row,
// and the trailing banner so parseIbWriteBwOutput's regex anchoring
// is exercised against realistic surrounding context.
const realisticIbWriteBwOutput = `---------------------------------------------------------------------------------------
                    RDMA_Write BW Test
 Dual-port       : OFF		Device         : mlx5_4
 Number of qps   : 1		Transport type : IB
 Connection type : RC		Using SRQ      : OFF
 PCIe relax order: ON
 ibv_wr* API     : ON
 TX depth        : 128
 CQ Moderation   : 100
 Mtu             : 1024[B]
 Link type       : Ethernet
 GID index       : 3
 Max inline data : 0[B]
 rdma_cm QPs	 : ON
 Data ex. method : rdma_cm
---------------------------------------------------------------------------------------
 local address: LID 0000 QPN 0x0094 PSN 0x4d2cb8
 GID: 00:00:00:00:00:00:00:00:00:00:255:255:10:00:00:01
 remote address: LID 0000 QPN 0x00a4 PSN 0xabcdef
 GID: 00:00:00:00:00:00:00:00:00:00:255:255:10:00:00:02
---------------------------------------------------------------------------------------
 #bytes     #iterations    BW peak[Gb/sec]    BW average[Gb/sec]   MsgRate[Mpps]
 65536      5000             194.39             193.21		   0.368459
---------------------------------------------------------------------------------------
`

func TestParseIbWriteBwOutput(t *testing.T) {
	t.Run("realistic summary row parses", func(t *testing.T) {
		peak, rate, ok := parseIbWriteBwOutput(realisticIbWriteBwOutput)
		require.True(t, ok)
		assert.InDelta(t, 194.39, peak, 0.001)
		assert.InDelta(t, 0.368459, rate, 0.0001)
	})

	t.Run("output without summary returns ok=false", func(t *testing.T) {
		// Output the server side prints before the client
		// finishes — no summary row, just the banner.
		short := `------------------------------------------------------
                    RDMA_Write BW Test
 Device         : mlx5_4
------------------------------------------------------
`
		peak, rate, ok := parseIbWriteBwOutput(short)
		assert.False(t, ok)
		assert.Equal(t, 0.0, peak)
		assert.Equal(t, 0.0, rate)
	})

	t.Run("malformed summary line is ignored", func(t *testing.T) {
		bad := ` 65536      5000    not_a_number    193.21    0.368
`
		_, _, ok := parseIbWriteBwOutput(bad)
		assert.False(t, ok)
	})

	t.Run("smaller message size sample", func(t *testing.T) {
		// perftest sometimes emits sub-1Gbps numbers — the
		// regex must accept them.
		sample := ` 2          1000           0.05               0.04                  3.125
`
		peak, rate, ok := parseIbWriteBwOutput(sample)
		require.True(t, ok)
		assert.InDelta(t, 0.05, peak, 0.001)
		assert.InDelta(t, 3.125, rate, 0.001)
	})
}

func TestParseIbWriteBwBatchResults(t *testing.T) {
	stdout := ibWriteBwBatchResultMarker + ` 0 0
` + ibWriteBwBatchStdoutMarker + ` 0
banner
 65536      5000             194.39             193.21		   0.368459
` + ibWriteBwBatchStderrMarker + ` 0
harmless warning
` + ibWriteBwBatchEndMarker + ` 0
` + ibWriteBwBatchResultMarker + ` 1 124
` + ibWriteBwBatchStdoutMarker + ` 1
timeout text
` + ibWriteBwBatchStderrMarker + ` 1
connection timed out
` + ibWriteBwBatchEndMarker + ` 1
`
	got := parseIbWriteBwBatchResults(stdout)
	require.Len(t, got, 2)
	assert.Equal(t, 0, got[0].rc)
	assert.Contains(t, got[0].stdout, "194.39")
	assert.Equal(t, "harmless warning", got[0].stderr)
	assert.Equal(t, 124, got[1].rc)
	assert.Contains(t, got[1].stdout, "timeout text")
	assert.Equal(t, "connection timed out", got[1].stderr)
}

func TestParseRPingBatchResultsKeepsPerCellOutput(t *testing.T) {
	output := rpingBatchResultMarker + ` 0 0
` + rpingBatchStdoutMarker + ` 0
ping data verified
` + rpingBatchStderrMarker + ` 0
` + rpingBatchEndMarker + ` 0
` + rpingBatchResultMarker + ` 1 1
` + rpingBatchStdoutMarker + ` 1
client output
` + rpingBatchStderrMarker + ` 1
RDMA_CM_EVENT_REJECTED
` + rpingBatchEndMarker + ` 1
`

	got := parseRPingBatchResults(output)

	require.Len(t, got, 2)
	assert.Equal(t, 0, got[0].rc)
	assert.Equal(t, "ping data verified", got[0].stdout)
	assert.Empty(t, got[0].stderr)
	assert.Equal(t, 1, got[1].rc)
	assert.Equal(t, "client output", got[1].stdout)
	assert.Equal(t, "RDMA_CM_EVENT_REJECTED", got[1].stderr)
}

func TestParseRDMAServerLogs(t *testing.T) {
	output := rdmaServerLogMarker + ` 1
server listening
connection rejected
` + rdmaServerLogEndMarker + ` 1
` + rdmaServerLogMarker + ` 3
empty peer response
` + rdmaServerLogEndMarker + ` 3
`

	got := parseRDMAServerLogs(output)

	require.Len(t, got, 2)
	assert.Contains(t, got[1], "connection rejected")
	assert.Equal(t, "empty peer response", got[3])
}

func TestRDMABatchClientCommandsDoNotRequireIPTooling(t *testing.T) {
	test := PingTest{
		SrcIP:       "192.168.0.10",
		DstIP:       "192.168.0.20",
		SrcIface:    "net1",
		SrcRDMADev:  "mlx5_1",
		DstRDMADev:  "mlx5_2",
		Expectation: ExpectRequired,
	}

	rpingCmd := rpingBatchClientCommand([]PingTest{test}, 5)
	assert.NotContains(t, rpingCmd, "route get")
	assert.NotContains(t, rpingCmd, "ipcmd")
	assert.NotContains(t, rpingCmd, "route get")
	assert.Contains(t, rpingCmd, `-p 9999`)
	assert.Contains(t, rpingCmd, rpingBatchStdoutMarker+" 0")
	assert.Contains(t, rpingCmd, rpingBatchStderrMarker+" 0")

	ibCmd := ibWriteBwBatchClientCommand([]PingTest{test}, 65536)
	assert.NotContains(t, ibCmd, "ipcmd")
	assert.NotContains(t, ibCmd, "route get")
	assert.Contains(t, ibCmd, ibWriteBwBatchResultMarker+" 0 $rc")
	assert.Contains(t, ibCmd, ibWriteBwBatchStdoutMarker+" 0")
	assert.Contains(t, ibCmd, ibWriteBwBatchStderrMarker+" 0")
}

func TestGPUDirectDMABufCommandsUseEndpointGPUIndices(t *testing.T) {
	test := PingTest{
		SrcIP: "192.168.0.10", DstIP: "192.168.0.20",
		SrcRDMADev: "mlx5_1", DstRDMADev: "mlx5_9",
		SrcGPUIndex: 4, DstGPUIndex: 7,
	}
	server := ibWriteBwBatchServerCommandMode([]PingTest{test}, 65536, true)
	client := ibWriteBwBatchClientCommandMode([]PingTest{test}, 65536, true)
	assert.Contains(t, server, "--use_cuda=7 --use_cuda_dmabuf")
	assert.NotContains(t, server, "--use_cuda=4")
	assert.Contains(t, client, "--use_cuda=4 --use_cuda_dmabuf")
	assert.NotContains(t, client, "--use_cuda=7")
}

func TestGPUDirectDMABufRejectsMissingTopologyWithoutGPUZeroFallback(t *testing.T) {
	err := gpudirectPreconditionError(PingTest{
		SrcGPUIndex: -1, DstGPUIndex: 3,
		SrcNode: "worker-a", DstNode: "worker-b",
		SrcRail: "rail-0", DstRail: "rail-0",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "src=-1 dst=3")
	assert.Contains(t, err.Error(), "worker-a")
	assert.NotContains(t, err.Error(), "assuming GPU0")
}

func TestRunGPUDirectDMABufBatchesFailsMissingTopologyBeforeExec(t *testing.T) {
	results := RunGPUDirectDMABufBatches(context.Background(), nil, nil, nil, []PingTest{{
		Kind:   GPUDirectDMABufSameRail,
		SrcPod: "pod-a", DstPod: "pod-b", SrcNode: "worker-a", DstNode: "worker-b",
		SrcRail: "rail-0", DstRail: "rail-0", SrcIP: "192.0.2.1", DstIP: "192.0.2.2",
		SrcRDMADev: "mlx5_0", DstRDMADev: "mlx5_1", SrcGPUIndex: -1, DstGPUIndex: 7,
	}}, 65536, 100)
	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "src=-1 dst=7")
	assert.Equal(t, 100.0, results[0].MinBandwidthGbps)
	assert.False(t, results[0].OK)
}

func TestRunGPUDirectDMABufBatchesPreservesTopologyErrorInMixedBatch(t *testing.T) {
	tests := []PingTest{
		{
			Kind: GPUDirectDMABufSameRail, SrcPod: "pod-a", DstPod: "pod-b",
			SrcNode: "worker-a", DstNode: "worker-b", SrcRail: "rail-0", DstRail: "rail-0",
			SrcIP: "192.0.2.1", DstIP: "192.0.2.2", SrcRDMADev: "mlx5_0", DstRDMADev: "mlx5_1",
			SrcGPUIndex: -1, DstGPUIndex: 7,
		},
		{
			Kind: GPUDirectDMABufSameRail, SrcPod: "pod-a", DstPod: "pod-b",
			SrcNode: "worker-a", DstNode: "worker-b", SrcRail: "rail-1", DstRail: "rail-1",
			SrcIP: "198.51.100.1", DstIP: "198.51.100.2", SrcRDMADev: "mlx5_2", DstRDMADev: "mlx5_3",
			SrcGPUIndex: 4, DstGPUIndex: 7,
		},
	}
	results := RunGPUDirectDMABufBatches(context.Background(), nil, nil, nil, tests, 65536, 100)
	require.Len(t, results, 2)
	require.Error(t, results[0].Err)
	assert.Contains(t, results[0].Err.Error(), "src=-1 dst=7")
	require.Error(t, results[1].Err)
	assert.Contains(t, results[1].Err.Error(), "no namespace/container lookup")
}

func TestGPUDirectResultJSONIncludesFamilyAndError(t *testing.T) {
	data, err := json.Marshal(PingResult{
		Test: PingTest{Kind: GPUDirectDMABufSameRail, SrcGPUIndex: 4, DstGPUIndex: 7},
		Err:  fmt.Errorf("DMA-BUF unavailable"),
	})
	require.NoError(t, err)
	assert.Contains(t, string(data), `"Family":"gpudirect_dmabuf"`)
	assert.Contains(t, string(data), `"Error":"DMA-BUF unavailable"`)
	assert.Contains(t, string(data), `"SrcGPUIndex":4`)
	assert.Contains(t, string(data), `"DstGPUIndex":7`)
}

func TestRPingBatchServerCommandRunsOneListenerPerTest(t *testing.T) {
	tests := []PingTest{
		{DstIP: "192.168.0.20"},
		{DstIP: "192.168.0.20"},
		{DstIP: "192.168.4.20"},
	}

	cmd := rpingBatchServerCommand(tests)

	assert.Contains(t, cmd, "pkill rping")
	assert.Contains(t, cmd, "rping -s -a \"192.168.0.20\" -p 9999")
	assert.Contains(t, cmd, "rping -s -a \"192.168.0.20\" -p 10000")
	assert.Contains(t, cmd, "rping -s -a \"192.168.4.20\" -p 10001")
	assert.Equal(t, 3, strings.Count(cmd, "rping -s -a "))
	assert.Equal(t, 3, strings.Count(cmd, "nohup rping"))
}

func TestPingTestKind_Predicates(t *testing.T) {
	assert.True(t, RDMAPingSameRail.IsRDMAPing())
	assert.True(t, RDMAPingCrossRail.IsRDMAPing())
	assert.False(t, RDMABwSameRail.IsRDMAPing())

	assert.True(t, RDMABwSameRail.IsRDMABw())
	assert.True(t, RDMABwCrossRail.IsRDMABw())
	assert.False(t, RDMAPingSameRail.IsRDMABw())

	assert.True(t, RDMAPingCrossRail.IsCrossRail())
	assert.True(t, RDMABwCrossRail.IsCrossRail())
	assert.False(t, RDMAPingSameRail.IsCrossRail())
	assert.False(t, RDMABwSameRail.IsCrossRail())
	assert.True(t, GPUDirectDMABufSameRail.IsGPUDirectDMABuf())
	assert.True(t, GPUDirectDMABufCrossRail.IsCrossRail())
	assert.False(t, GPUDirectDMABufSameRail.IsRDMABw())
}
