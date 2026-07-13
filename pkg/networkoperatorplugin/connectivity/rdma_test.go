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
banner
 65536      5000             194.39             193.21		   0.368459
` + ibWriteBwBatchEndMarker + ` 0
` + ibWriteBwBatchResultMarker + ` 1 124
timeout text
` + ibWriteBwBatchEndMarker + ` 1
`
	got := parseIbWriteBwBatchResults(stdout)
	require.Len(t, got, 2)
	assert.Equal(t, 0, got[0].rc)
	assert.Contains(t, got[0].stdout, "194.39")
	assert.Equal(t, 124, got[1].rc)
	assert.Contains(t, got[1].stdout, "timeout text")
}

func TestParseRDMABatchRouteResults(t *testing.T) {
	tests := []PingTest{
		{SrcIP: "192.168.0.10", DstIP: "192.168.0.20"},
		{SrcIP: "192.168.4.10", DstIP: "192.168.0.20"},
	}
	stdout := rdmaBatchRouteResultMarker + ` 0 0
192.168.0.20 from 192.168.0.10 dev net1 src 192.168.0.10
` + rdmaBatchRouteEndMarker + ` 0
` + rdmaBatchRouteResultMarker + ` 1 127
` + routeSkipMarker + `
` + rdmaBatchRouteEndMarker + ` 1
`
	got := parseRDMABatchRouteResults(stdout, tests)
	require.Len(t, got, 2)
	assert.True(t, got[0].OK)
	assert.Equal(t, "net1", got[0].Dev)
	assert.Equal(t, "ip -o route get 192.168.0.20 from 192.168.0.10", got[0].Command)
	assert.False(t, got[1].OK)
	assert.Equal(t, "ip command not found in validation container", got[1].Err)
	assert.False(t, routeMismatch(got[1], PingTest{SrcIface: "net2"}))
}

func TestRDMABatchClientCommandsIncludeSourceRouteGuard(t *testing.T) {
	test := PingTest{
		SrcIP:       "192.168.0.10",
		DstIP:       "192.168.0.20",
		SrcIface:    "net1",
		SrcRDMADev:  "mlx5_1",
		Expectation: ExpectRequired,
	}

	rpingCmd := rpingBatchClientCommand([]PingTest{test}, 5)
	assert.Contains(t, rpingCmd, rdmaBatchRouteResultMarker)
	assert.Contains(t, rpingCmd, "ipcmd")
	assert.Contains(t, rpingCmd, "route get")
	assert.Contains(t, rpingCmd, rpingBatchResultMarker+" 0 201")
	assert.Contains(t, rpingCmd, `if [ "$route_ok" = "1" ]; then run_with_timeout`)
	assert.Contains(t, rpingCmd, `-p 9999`)
	assert.NotContains(t, rpingCmd, "continue")

	ibCmd := ibWriteBwBatchClientCommand([]PingTest{test}, 65536)
	assert.Contains(t, ibCmd, rdmaBatchRouteResultMarker)
	assert.Contains(t, ibCmd, "route get")
	assert.Contains(t, ibCmd, ibWriteBwBatchResultMarker+" 0 201")
	assert.Contains(t, ibCmd, `if [ "$route_ok" = "1" ]; then run_with_timeout`)
	assert.NotContains(t, ibCmd, "continue")
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
}
