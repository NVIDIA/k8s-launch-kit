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

func TestRDMAPlan(t *testing.T) {
	mk := func(name string, rails ...string) TestPod {
		tp := mkPod(name, rails...)
		tp.RDMADevsByRail = map[string]string{}
		for _, r := range rails {
			tp.RDMADevsByRail[r] = "mlx5_" + r // stub dev name
		}
		return tp
	}

	t.Run("emits rping and ib_write_bw variants per ICMP pair", func(t *testing.T) {
		plan := Plan([]TestPod{
			mk("pod-a", "rail-0", "rail-1"),
			mk("pod-b", "rail-0", "rail-1"),
		})
		require.Nil(t, plan.Skip)
		RDMAPlan([]TestPod{
			mk("pod-a", "rail-0", "rail-1"),
			mk("pod-b", "rail-0", "rail-1"),
		}, &plan)

		// 2 pods × 1 ordered pair each dir × 2 rails = 4
		assert.Len(t, plan.RDMASameRail, 4)
		assert.Len(t, plan.RDMABwSameRail, 4)
		// Cross-rail canaries: 2 ordered pod pairs = 2 each.
		assert.Len(t, plan.RDMACrossRail, 2)
		assert.Len(t, plan.RDMABwCrossRail, 2)

		// Every emitted test must carry both RDMA device names —
		// the runner needs them for `-d <dev>`.
		for _, set := range [][]PingTest{plan.RDMASameRail, plan.RDMABwSameRail, plan.RDMACrossRail, plan.RDMABwCrossRail} {
			for _, tst := range set {
				assert.NotEmpty(t, tst.SrcRDMADev, "src device must be set on %+v", tst)
				assert.NotEmpty(t, tst.DstRDMADev, "dst device must be set on %+v", tst)
			}
		}
	})

	t.Run("pods missing RDMA device on a rail get that rail skipped", func(t *testing.T) {
		pods := []TestPod{
			mk("pod-a", "rail-0", "rail-1"),
			mk("pod-b", "rail-0", "rail-1"),
		}
		// Drop pod-b's rail-1 device — the matrix should still
		// produce rail-0 RDMA tests but skip rail-1.
		delete(pods[1].RDMADevsByRail, "rail-1")

		plan := Plan(pods)
		require.Nil(t, plan.Skip)
		RDMAPlan(pods, &plan)

		// rail-0 only: 2 ordered pairs = 2 same-rail tests
		// per kind.
		assert.Len(t, plan.RDMASameRail, 2)
		assert.Len(t, plan.RDMABwSameRail, 2)
		// Cross-rail canary uses src.rail[0] → dst.rail[1].
		// pod-a→pod-b skipped (pod-b missing rail-1 device);
		// pod-b→pod-a still works (pod-b has rail-0,
		// pod-a has rail-1). One direction only.
		assert.Len(t, plan.RDMACrossRail, 1)
		assert.Len(t, plan.RDMABwCrossRail, 1)
		assert.Equal(t, "pod-b", plan.RDMACrossRail[0].SrcPod)
		assert.Equal(t, "pod-a", plan.RDMACrossRail[0].DstPod)
	})

	t.Run("skip block from ICMP plan is preserved", func(t *testing.T) {
		plan := Plan([]TestPod{mk("solo", "rail-0")})
		require.NotNil(t, plan.Skip)
		RDMAPlan([]TestPod{mk("solo", "rail-0")}, &plan)
		// Soft-skipped ICMP plan stays soft-skipped — no RDMA
		// tests scheduled.
		assert.Empty(t, plan.RDMASameRail)
		assert.Empty(t, plan.RDMABwSameRail)
	})
}

func TestPingTestKind_Predicates(t *testing.T) {
	assert.True(t, PingSameRail.IsICMP())
	assert.True(t, PingCrossRail.IsICMP())
	assert.False(t, RDMAPingSameRail.IsICMP())
	assert.False(t, RDMABwSameRail.IsICMP())

	assert.True(t, RDMAPingSameRail.IsRDMAPing())
	assert.True(t, RDMAPingCrossRail.IsRDMAPing())
	assert.False(t, RDMABwSameRail.IsRDMAPing())

	assert.True(t, RDMABwSameRail.IsRDMABw())
	assert.True(t, RDMABwCrossRail.IsRDMABw())

	assert.True(t, PingCrossRail.IsCrossRail())
	assert.True(t, RDMAPingCrossRail.IsCrossRail())
	assert.True(t, RDMABwCrossRail.IsCrossRail())
	assert.False(t, PingSameRail.IsCrossRail())
	assert.False(t, RDMAPingSameRail.IsCrossRail())
	assert.False(t, RDMABwSameRail.IsCrossRail())
}
