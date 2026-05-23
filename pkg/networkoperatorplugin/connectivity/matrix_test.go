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

func mkPod(name string, rails ...string) TestPod {
	tp := TestPod{
		Name:      name,
		IPsByRail: map[string]string{},
	}
	for i, rail := range rails {
		// Synthesize an IP deterministic per (pod, rail) so failed
		// assertions are readable: pod-A rail-0 → "10.0.<podIdx>.<railIdx>"
		_ = i
		tp.IPsByRail[rail] = fakeIP(name, rail)
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

func TestPlan_SoftSkipOnFewerThan2Pods(t *testing.T) {
	t.Run("zero pods", func(t *testing.T) {
		plan := Plan(nil)
		require.NotNil(t, plan.Skip)
		assert.Contains(t, plan.Skip.Reason, "0 schedulable")
		assert.Empty(t, plan.SameRail)
	})
	t.Run("one pod", func(t *testing.T) {
		plan := Plan([]TestPod{mkPod("pod-a", "rail-0", "rail-1")})
		require.NotNil(t, plan.Skip)
		assert.Contains(t, plan.Skip.Reason, "1 schedulable")
	})
}

func TestPlan_TwoPodsOneRail(t *testing.T) {
	plan := Plan([]TestPod{
		mkPod("pod-a", "rail-0"),
		mkPod("pod-b", "rail-0"),
	})
	require.Nil(t, plan.Skip)

	// Same-rail: 2 pods × 1 ordered pair each direction × 1 rail = 2 tests.
	assert.Len(t, plan.SameRail, 2)
	// Cross-rail canary requires ≥2 rails per pod — skipped here.
	assert.Empty(t, plan.CrossRail)

	// Check direction coverage: both A→B and B→A appear.
	dirs := map[string]bool{}
	for _, t := range plan.SameRail {
		dirs[t.SrcPod+"→"+t.DstPod] = true
	}
	assert.True(t, dirs["pod-a→pod-b"])
	assert.True(t, dirs["pod-b→pod-a"])
}

func TestPlan_TwoPodsTwoRails_IncludesCrossRailCanary(t *testing.T) {
	plan := Plan([]TestPod{
		mkPod("pod-a", "rail-0", "rail-1"),
		mkPod("pod-b", "rail-0", "rail-1"),
	})
	require.Nil(t, plan.Skip)

	// Same-rail: 2 pods × 1 ordered pair each direction × 2 rails = 4.
	assert.Len(t, plan.SameRail, 4)
	// Cross-rail canary: one per ordered pod pair = 2.
	assert.Len(t, plan.CrossRail, 2)

	// Each cross-rail test should ping rail-0 → rail-1 (the first two
	// rails by sorted order). Concrete IP assertions catch
	// off-by-one in railOrder indexing.
	for _, c := range plan.CrossRail {
		assert.Equal(t, "rail-0", c.SrcRail)
		assert.Equal(t, "rail-1", c.DstRail)
		assert.Equal(t, "rail-0→rail-1", c.Rail)
	}
}

func TestPlan_ThreePodsThreeRails_FullMatrix(t *testing.T) {
	plan := Plan([]TestPod{
		mkPod("pod-a", "rail-0", "rail-1", "rail-2"),
		mkPod("pod-b", "rail-0", "rail-1", "rail-2"),
		mkPod("pod-c", "rail-0", "rail-1"), // shorter rail list — exercise asymmetric case
	})

	require.Nil(t, plan.Skip)

	// Same-rail tests: for each ordered pair (i ≠ j) and for each
	// rail both endpoints have:
	//   - pod-a ↔ pod-b: 3 rails × 2 dirs = 6
	//   - pod-a ↔ pod-c: 2 rails × 2 dirs = 4
	//   - pod-b ↔ pod-c: 2 rails × 2 dirs = 4
	// Total = 14.
	assert.Len(t, plan.SameRail, 14)

	// Cross-rail canary requires ≥2 rails on both endpoints. All
	// three pods qualify (pod-c has 2), so every ordered pair gets
	// one canary: 3 × 2 = 6.
	assert.Len(t, plan.CrossRail, 6)
}

func TestPlan_StableOrderingAcrossRuns(t *testing.T) {
	pods := []TestPod{
		mkPod("pod-b", "rail-0", "rail-1"),
		mkPod("pod-a", "rail-0", "rail-1"),
		mkPod("pod-c", "rail-0", "rail-1"),
	}
	plan1 := Plan(pods)
	plan2 := Plan(pods)
	require.Equal(t, plan1.SameRail, plan2.SameRail)
	require.Equal(t, plan1.CrossRail, plan2.CrossRail)
	// First test should always be from pod-a (sorted-by-name).
	assert.Equal(t, "pod-a", plan1.SameRail[0].SrcPod)
}

func TestPlan_SkipsPairsWithoutSharedRail(t *testing.T) {
	plan := Plan([]TestPod{
		mkPod("pod-a", "rail-0"),
		mkPod("pod-b", "rail-1"),
	})
	require.Nil(t, plan.Skip)
	// No shared rail → zero same-rail tests.
	assert.Empty(t, plan.SameRail)
	// Each pod only has 1 rail → no cross-rail canary either.
	assert.Empty(t, plan.CrossRail)
}
