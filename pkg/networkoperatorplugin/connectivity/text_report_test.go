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
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureOutput is a minimal ui.Output that records every message in
// the order it was emitted so tests can assert on exact lines.
type captureOutput struct{ lines []string }

func (c *captureOutput) Info(format string, args ...interface{}) {
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}
func (c *captureOutput) Success(format string, args ...interface{}) {
	c.lines = append(c.lines, "SUCCESS: "+fmt.Sprintf(format, args...))
}
func (c *captureOutput) Warning(format string, args ...interface{}) {
	c.lines = append(c.lines, "WARNING: "+fmt.Sprintf(format, args...))
}
func (c *captureOutput) Error(format string, args ...interface{}) {
	c.lines = append(c.lines, "ERROR: "+fmt.Sprintf(format, args...))
}
func (c *captureOutput) StartProgress(message string) ui.Progress {
	return &captureProgress{out: c, msg: message}
}
func (c *captureOutput) Header(text string)           {}
func (c *captureOutput) Section(text string)          { c.lines = append(c.lines, "SECTION: "+text) }
func (c *captureOutput) Confirm(string) (bool, error) { return true, nil }
func (c *captureOutput) IsTTY() bool                  { return false }

type captureProgress struct {
	out *captureOutput
	msg string
}

func (p *captureProgress) Update(string)    {}
func (p *captureProgress) Success(m string) { p.out.lines = append(p.out.lines, "PROGRESS_OK: "+m) }
func (p *captureProgress) Fail(m string)    { p.out.lines = append(p.out.lines, "PROGRESS_FAIL: "+m) }

// Helpers synthesize RDMA results so the renderer's per-(rail, family)
// grouping and node-label fallback are exercised.

func rpingResult(src, dst, rail string, ok bool) PingResult {
	return PingResult{
		Test: PingTest{
			Kind:    RDMAPingSameRail,
			SrcPod:  src + "-pod",
			DstPod:  dst + "-pod",
			SrcNode: src,
			DstNode: dst,
			Rail:    rail,
			SrcRail: rail,
			DstRail: rail,
		},
		OK: ok,
	}
}

func ibBwResult(src, dst, rail string, ok bool, bwGbps float64) PingResult {
	return PingResult{
		Test: PingTest{
			Kind:    RDMABwSameRail,
			SrcPod:  src + "-pod",
			DstPod:  dst + "-pod",
			SrcNode: src,
			DstNode: dst,
			Rail:    rail,
			SrcRail: rail,
			DstRail: rail,
		},
		OK:            ok,
		BandwidthGbps: bwGbps,
	}
}

func crossRpingResult(src, dst, srcRail, dstRail string, ok bool) PingResult {
	return PingResult{
		Test: PingTest{
			Kind:    RDMAPingCrossRail,
			SrcPod:  src + "-pod",
			DstPod:  dst + "-pod",
			SrcNode: src,
			DstNode: dst,
			Rail:    srcRail + "→" + dstRail,
			SrcRail: srcRail,
			DstRail: dstRail,
		},
		OK: ok,
	}
}

func TestCellBodyExpectationLabels(t *testing.T) {
	assert.Equal(t, "connected", cellBody(&PingResult{
		Expectation: ExpectObserve,
		ObservedOK:  true,
	}, familyRPing))
	assert.Equal(t, "not connected", cellBody(&PingResult{
		Expectation: ExpectObserve,
		ObservedOK:  false,
	}, familyRPing))
	assert.Equal(t, "✓ blocked", cellBody(&PingResult{
		Expectation: ExpectForbidden,
		OK:          true,
		ObservedOK:  false,
	}, familyRPing))
	assert.Equal(t, "✗ connected", cellBody(&PingResult{
		Expectation: ExpectForbidden,
		OK:          false,
		ObservedOK:  true,
	}, familyRPing))
	assert.Equal(t, "✗ ERR", cellBody(&PingResult{
		Expectation: ExpectForbidden,
		OK:          false,
		ObservedOK:  false,
		Err:         fmt.Errorf("api exec failed"),
	}, familyRPing))
}

func TestRenderMatrixText_RailGridAndCrossRail(t *testing.T) {
	result := &MatrixResult{
		PingResults: []PingResult{
			// Rail rail-0: 2 nodes, both rping pass
			rpingResult("worker-1", "worker-2", "rail-0", true),
			rpingResult("worker-2", "worker-1", "rail-0", true),
			ibBwResult("worker-1", "worker-2", "rail-0", true, 194.4),
			ibBwResult("worker-2", "worker-1", "rail-0", true, 193.8),
			// Rail rail-1: one rping direction fails
			rpingResult("worker-1", "worker-2", "rail-1", true),
			rpingResult("worker-2", "worker-1", "rail-1", false),
			// Cross-rail canaries
			crossRpingResult("worker-1", "worker-2", "rail-0", "rail-1", true),
			crossRpingResult("worker-2", "worker-1", "rail-0", "rail-1", false),
		},
	}

	out := &captureOutput{}
	RenderMatrixText(out, result)

	joined := strings.Join(out.lines, "\n")

	// Sanity: both rails rendered. The kind family appears in the
	// header so we look for the "Rail <name> — RDMA ping" prefix.
	assert.Contains(t, joined, "Rail rail-0 — RDMA ping (rping):")
	assert.Contains(t, joined, "Rail rail-1 — RDMA ping (rping):")
	assert.Contains(t, joined, "Rail rail-0 — RDMA bandwidth (ib_write_bw):")
	// Cross-rail section rendered.
	assert.Contains(t, joined, "Cross-rail canary — RDMA ping (rping):")
	// Header row of the grid.
	assert.Contains(t, joined, "src \\ dst")
	// Axis labels must be node names, NOT pod names.
	assert.Contains(t, joined, "worker-1")
	assert.Contains(t, joined, "worker-2")
	assert.NotContains(t, joined, "worker-1-pod")
	// Sample cells: rping ✓ in non-TTY mode is just plain text.
	assert.Contains(t, joined, "✓")
	// ib_write_bw cell shows formatted Gbps.
	assert.Contains(t, joined, "194.4 Gbps")
	// Failure cell shows ✗ for rping.
	assert.Contains(t, joined, "✗")
	// Self-pairs are dashes.
	selfDash := 0
	for _, l := range out.lines {
		if strings.Contains(l, "worker-1") && strings.Contains(l, "—") {
			selfDash++
		}
	}
	assert.GreaterOrEqual(t, selfDash, 2, "expected self-pair dashes on worker-1 row across both rails")
}

func TestRenderMatrixText_SkippedSkipsRender(t *testing.T) {
	out := &captureOutput{}
	RenderMatrixText(out, &MatrixResult{Skipped: &MatrixSkip{Reason: "no pods"}})
	assert.Empty(t, out.lines, "skipped matrix must not emit grid lines")
}

func TestRenderMatrixText_EmptyResultsIsNoOp(t *testing.T) {
	out := &captureOutput{}
	RenderMatrixText(out, &MatrixResult{})
	assert.Empty(t, out.lines)
}

func TestShortPodName(t *testing.T) {
	t.Run("short name unchanged", func(t *testing.T) {
		assert.Equal(t, "sriov-test-abc", shortPodName("sriov-test-abc"))
	})
	t.Run("long name keeps tail", func(t *testing.T) {
		// 30-char name should be truncated and keep the 8-char DS-hash tail.
		long := "really-long-sriov-test-name-7tf9g"
		out := shortPodName(long)
		require.LessOrEqual(t, len(out), 24+2) // 24 visible chars + ellipsis rune
		assert.True(t, strings.HasSuffix(out, "ame-7tf9g") || strings.HasSuffix(out, "me-7tf9g"),
			"output %q must end with the tail of %q", out, long)
		assert.Contains(t, out, "…")
	})
}

func TestCellFor(t *testing.T) {
	t.Run("self pair is dash", func(t *testing.T) {
		assert.Equal(t, "—", cellFor("pod-a", "pod-a", nil, familyRPing, false))
	})
	t.Run("missing result is bullet", func(t *testing.T) {
		assert.Equal(t, "·", cellFor("pod-a", "pod-b", nil, familyRPing, false))
	})
	t.Run("rping OK is bare checkmark", func(t *testing.T) {
		r := PingResult{OK: true}
		assert.Equal(t, "✓", cellFor("a", "b", &r, familyRPing, false))
	})
	t.Run("rping fail is bare X", func(t *testing.T) {
		r := PingResult{OK: false}
		assert.Equal(t, "✗", cellFor("a", "b", &r, familyRPing, false))
	})
	t.Run("ib_write_bw OK shows Gbps", func(t *testing.T) {
		r := PingResult{OK: true, BandwidthGbps: 194.39}
		assert.Equal(t, "✓ 194.4 Gbps", cellFor("a", "b", &r, familyIbBw, false))
	})
	t.Run("ib_write_bw fail with no bandwidth is ERR", func(t *testing.T) {
		r := PingResult{OK: false}
		assert.Equal(t, "✗ ERR", cellFor("a", "b", &r, familyIbBw, false))
	})
	t.Run("ib_write_bw fail below threshold shows observed bandwidth", func(t *testing.T) {
		r := PingResult{OK: false, BandwidthGbps: 194.39, MinBandwidthGbps: 800}
		assert.Equal(t, "✗ 194.4 Gbps (< 800.0)", cellFor("a", "b", &r, familyIbBw, false))
	})
	t.Run("ib_write_bw OK but zero bandwidth is ERR", func(t *testing.T) {
		// Defensive: OK=true with zero bandwidth shouldn't happen
		// in practice, but if it does the cell renders as ERR
		// since a "0 Gbps" link is broken even if perftest exited
		// cleanly.
		r := PingResult{OK: true, BandwidthGbps: 0}
		assert.Equal(t, "✗ ERR", cellFor("a", "b", &r, familyIbBw, false))
	})
	t.Run("TTY mode wraps in ANSI green for OK", func(t *testing.T) {
		r := PingResult{OK: true}
		got := cellFor("a", "b", &r, familyRPing, true)
		assert.Contains(t, got, "\033[32m")
		assert.Contains(t, got, "\033[0m")
	})
}

func TestKindFamilyOf(t *testing.T) {
	assert.Equal(t, familyRPing, kindFamilyOf(RDMAPingSameRail))
	assert.Equal(t, familyRPing, kindFamilyOf(RDMAPingCrossRail))
	assert.Equal(t, familyIbBw, kindFamilyOf(RDMABwSameRail))
	assert.Equal(t, familyIbBw, kindFamilyOf(RDMABwCrossRail))
}
