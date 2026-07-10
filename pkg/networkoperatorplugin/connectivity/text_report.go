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
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// RenderMatrixText prints the matrix as a per-rail src×dst grid plus a
// cross-rail canary list. Output flows through uiOutput.Info() so it
// lands on stdout in text mode and on stderr (with no JSON
// contamination) in JSON mode — JSON consumers still read the
// structured MatrixResult that the validate CLI marshals to stdout.
//
// Cells:
//   - "—"      src equals dst (self-test, not run)
//   - "·"      no test for this (src,dst,rail) — shared-rail set
//     didn't include both endpoints
//   - rping       — "✓" / "✗" / "✗ ERR"
//   - ib_write_bw — "✓ 194.4 Gbps" / "✗ ERR"
//
// ANSI color is applied only when uiOutput.IsTTY() — keeps log files
// and CI pipelines free of escape sequences.
func RenderMatrixText(uiOutput ui.Output, result *MatrixResult) {
	if result == nil || result.Skipped != nil {
		return
	}
	if len(result.PingResults) == 0 {
		return
	}

	tty := uiOutput.IsTTY()
	// Group per (rail, kind family). One grid is rendered for
	// every (rail, family) bucket that has at least one result, in
	// the test-execution order so the reader sees rping
	// (QP-establishment canary) before ib_write_bw (bandwidth).
	byRail, byCross, nodes, rails := groupResultsByKind(result.PingResults)

	families := []kindFamily{familyRPing, familyIbBw}
	for _, rail := range rails {
		for _, fam := range families {
			grid, ok := byRail[rail][fam]
			if !ok {
				continue
			}
			uiOutput.Info("")
			uiOutput.Info("Rail %s — %s:", rail, familyTitle(fam))
			for _, line := range renderRailGrid(nodes, grid, fam, tty) {
				uiOutput.Info("%s", line)
			}
		}
	}

	for _, fam := range families {
		cross, ok := byCross[fam]
		if !ok || len(cross) == 0 {
			continue
		}
		uiOutput.Info("")
		uiOutput.Info("Cross-rail canary — %s:", familyTitle(fam))
		for _, line := range renderCrossRailList(cross, fam, tty) {
			uiOutput.Info("%s", line)
		}
	}
}

// kindFamily collapses the four PingTestKind values down to the two
// families the renderer cares about: rping and ib_write_bw. The
// same-rail / cross-rail axis is handled separately (per-rail grid vs
// cross-rail list).
type kindFamily int

const (
	familyRPing kindFamily = iota
	familyIbBw
)

func kindFamilyOf(k PingTestKind) kindFamily {
	if k.IsRDMABw() {
		return familyIbBw
	}
	return familyRPing
}

func familyTitle(f kindFamily) string {
	if f == familyIbBw {
		return "RDMA bandwidth (ib_write_bw)"
	}
	return "RDMA ping (rping)"
}

// groupResultsByKind indexes results by (rail, family, src) → dst →
// result for the per-rail grids, and by family → []result for the
// cross-rail canary lists. Returns the deterministic node and rail
// orderings so callers don't have to sort again. Node names are used
// as the axis labels because they're what operators recognize
// ("worker-03" vs the DaemonSet-generated pod suffix); the underlying
// pod name is still carried on the PingTest for the SPDY exec path.
func groupResultsByKind(results []PingResult) (byRail map[string]map[kindFamily]map[string]map[string]*PingResult, byCross map[kindFamily][]*PingResult, nodes []string, rails []string) {
	byRail = map[string]map[kindFamily]map[string]map[string]*PingResult{}
	byCross = map[kindFamily][]*PingResult{}
	nodeSet := map[string]struct{}{}
	railSet := map[string]struct{}{}

	for i := range results {
		r := &results[i]
		src := axisLabel(r.Test.SrcNode, r.Test.SrcPod)
		dst := axisLabel(r.Test.DstNode, r.Test.DstPod)
		nodeSet[src] = struct{}{}
		nodeSet[dst] = struct{}{}
		fam := kindFamilyOf(r.Test.Kind)
		if r.Test.Kind.IsCrossRail() {
			byCross[fam] = append(byCross[fam], r)
			continue
		}
		if _, ok := byRail[r.Test.Rail]; !ok {
			byRail[r.Test.Rail] = map[kindFamily]map[string]map[string]*PingResult{}
			railSet[r.Test.Rail] = struct{}{}
		}
		if _, ok := byRail[r.Test.Rail][fam]; !ok {
			byRail[r.Test.Rail][fam] = map[string]map[string]*PingResult{}
		}
		if _, ok := byRail[r.Test.Rail][fam][src]; !ok {
			byRail[r.Test.Rail][fam][src] = map[string]*PingResult{}
		}
		byRail[r.Test.Rail][fam][src][dst] = r
	}

	nodes = make([]string, 0, len(nodeSet))
	for n := range nodeSet {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	rails = make([]string, 0, len(railSet))
	for r := range railSet {
		rails = append(rails, r)
	}
	sort.Strings(rails)
	return
}

// axisLabel returns the preferred display name for one endpoint of a
// ping test. The kubelet sometimes hasn't populated Pod.Spec.NodeName
// yet at scheduling time, so we fall back to the pod name to avoid
// merging unrelated endpoints under the same empty label.
func axisLabel(node, pod string) string {
	if node != "" {
		return node
	}
	return pod
}

// renderRailGrid produces the lines for one rail's src×dst grid, with
// columns aligned via text/tabwriter. Axis labels are node names
// (with pod-name fallback for endpoints whose NodeName wasn't set).
// fam selects per-kind cell formatting (ICMP shows loss + RTT,
// rping shows ✓/✗, ib_write_bw shows ✓ N Gbps).
func renderRailGrid(nodes []string, table map[string]map[string]*PingResult, fam kindFamily, tty bool) []string {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)

	// Header: blank corner + each dst node (short form).
	fmt.Fprintf(tw, "  src \\ dst\t")
	for _, dst := range nodes {
		fmt.Fprintf(tw, "%s\t", shortPodName(dst))
	}
	fmt.Fprintln(tw)

	for _, src := range nodes {
		fmt.Fprintf(tw, "  %s\t", shortPodName(src))
		for _, dst := range nodes {
			fmt.Fprintf(tw, "%s\t", cellFor(src, dst, table[src][dst], fam, tty))
		}
		fmt.Fprintln(tw)
	}
	tw.Flush()

	return trailingTrim(strings.Split(buf.String(), "\n"))
}

// renderCrossRailList prints the cross-rail canary tests as a simple
// "src.rail → dst.rail: result" list — a 2D grid doesn't fit the
// asymmetric (srcRail, dstRail) shape neatly. Same node-name fallback
// as the per-rail grid: prefer Pod.Spec.NodeName, fall back to the
// pod name.
func renderCrossRailList(results []*PingResult, fam kindFamily, tty bool) []string {
	type row struct {
		src, dst string
		r        *PingResult
	}
	rows := make([]row, len(results))
	for i, r := range results {
		rows[i] = row{
			src: axisLabel(r.Test.SrcNode, r.Test.SrcPod),
			dst: axisLabel(r.Test.DstNode, r.Test.DstPod),
			r:   r,
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].src != rows[j].src {
			return rows[i].src < rows[j].src
		}
		return rows[i].dst < rows[j].dst
	})

	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	for _, row := range rows {
		left := fmt.Sprintf("  %s [%s]\t→ %s [%s]\t",
			shortPodName(row.src), row.r.Test.SrcRail,
			shortPodName(row.dst), row.r.Test.DstRail)
		fmt.Fprintf(tw, "%s%s\n", left, cellDetail(row.r, fam, tty))
	}
	tw.Flush()
	return trailingTrim(strings.Split(buf.String(), "\n"))
}

// cellFor renders one src×dst grid cell. self-pairs render as "—",
// missing pairs as "·" (the rail set didn't pair these two pods —
// rare; e.g. one pod's multus annotation was missing this rail).
func cellFor(src, dst string, r *PingResult, fam kindFamily, tty bool) string {
	if src == dst {
		return "—"
	}
	if r == nil {
		return "·"
	}
	return cellDetail(r, fam, tty)
}

// cellDetail formats a single result into its terse cell representation
// + optional ANSI color when TTY. The body shape depends on the
// kind family:
//
//   - rping:       ✓ / ✗
//   - ib_write_bw: ✓ 194.4 Gbps / ✗ ERR
func cellDetail(r *PingResult, fam kindFamily, tty bool) string {
	body := cellBody(r, fam)
	if tty {
		if r.OK {
			return "\033[32m" + body + "\033[0m"
		}
		return "\033[31m" + body + "\033[0m"
	}
	return body
}

func cellBody(r *PingResult, fam kindFamily) string {
	if fam == familyIbBw {
		if r.OK && r.BandwidthGbps > 0 {
			return fmt.Sprintf("✓ %.1f Gbps", r.BandwidthGbps)
		}
		if r.BandwidthGbps > 0 && r.MinBandwidthGbps > 0 {
			return fmt.Sprintf("✗ %.1f Gbps (< %.1f)", r.BandwidthGbps, r.MinBandwidthGbps)
		}
		if r.BandwidthGbps > 0 {
			return fmt.Sprintf("✗ %.1f Gbps", r.BandwidthGbps)
		}
		return "✗ ERR"
	}
	// rping
	if r.OK {
		return "✓"
	}
	return "✗"
}

// shortPodName trims a long pod name to keep grid columns from blowing
// out — DaemonSet pods often have a 5-char random suffix after a long
// app name. We keep the leading portion + the last 5 chars (the hash)
// so duplicates remain distinguishable.
func shortPodName(name string) string {
	const max = 24
	if len(name) <= max {
		return name
	}
	// Keep tail (DS hash) so duplicates stay distinguishable.
	const tail = 8
	head := max - tail - 1
	if head < 1 {
		return name[:max]
	}
	return name[:head] + "…" + name[len(name)-tail:]
}

// trailingTrim drops trailing empty lines tabwriter leaves behind so
// the table doesn't double-space when emitted via uiOutput.Info.
func trailingTrim(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
