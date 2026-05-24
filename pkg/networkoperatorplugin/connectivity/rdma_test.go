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
