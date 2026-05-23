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
//   - "—"      src equals dst (self-ping, not run)
//   - "·"      no test for this (src,dst,rail) — shared-rail set
//     didn't include both endpoints
//   - "✓ 0%"   ping passed; packet loss percentage when non-zero,
//     RTT when present (e.g. "✓ 0% 0.5ms")
//   - "✗ 100%" ping failed; "✗ ERR" if the exec itself errored
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
	byRail, crossRail, nodesSorted, railsSorted := groupResults(result.PingResults)

	for _, rail := range railsSorted {
		uiOutput.Info("")
		uiOutput.Info("Rail %s:", rail)
		for _, line := range renderRailGrid(nodesSorted, byRail[rail], tty) {
			uiOutput.Info("%s", line)
		}
	}

	if len(crossRail) > 0 {
		uiOutput.Info("")
		uiOutput.Info("Cross-rail canary:")
		for _, line := range renderCrossRailList(crossRail, tty) {
			uiOutput.Info("%s", line)
		}
	}
}

// groupResults indexes ping results by rail and source node for the
// per-rail grid, plus a flat slice for the cross-rail canary. Returns
// the deterministic node and rail orderings so callers don't have to
// sort again. Node names are used as the axis labels because they're
// what operators recognize ("worker-03" vs the DaemonSet-generated
// pod suffix like "sriov-test-7t8h9"); the underlying pod name is
// still carried on the PingTest for the SPDY exec path.
func groupResults(results []PingResult) (byRail map[string]map[string]map[string]*PingResult, crossRail []*PingResult, nodes []string, rails []string) {
	byRail = map[string]map[string]map[string]*PingResult{}
	nodeSet := map[string]struct{}{}
	railSet := map[string]struct{}{}

	for i := range results {
		r := &results[i]
		src := axisLabel(r.Test.SrcNode, r.Test.SrcPod)
		dst := axisLabel(r.Test.DstNode, r.Test.DstPod)
		nodeSet[src] = struct{}{}
		nodeSet[dst] = struct{}{}
		if r.Test.Kind == PingCrossRail {
			crossRail = append(crossRail, r)
			continue
		}
		if _, ok := byRail[r.Test.Rail]; !ok {
			byRail[r.Test.Rail] = map[string]map[string]*PingResult{}
			railSet[r.Test.Rail] = struct{}{}
		}
		if _, ok := byRail[r.Test.Rail][src]; !ok {
			byRail[r.Test.Rail][src] = map[string]*PingResult{}
		}
		byRail[r.Test.Rail][src][dst] = r
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
func renderRailGrid(nodes []string, table map[string]map[string]*PingResult, tty bool) []string {
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
			fmt.Fprintf(tw, "%s\t", cellFor(src, dst, table[src][dst], tty))
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
func renderCrossRailList(results []*PingResult, tty bool) []string {
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
		fmt.Fprintf(tw, "%s%s\n", left, cellDetail(row.r, tty))
	}
	tw.Flush()
	return trailingTrim(strings.Split(buf.String(), "\n"))
}

// cellFor renders one src×dst grid cell. self-pairs render as "—",
// missing pairs as "·" (the rail set didn't pair these two pods —
// rare; e.g. one pod's multus annotation was missing this rail).
func cellFor(src, dst string, r *PingResult, tty bool) string {
	if src == dst {
		return "—"
	}
	if r == nil {
		return "·"
	}
	return cellDetail(r, tty)
}

// cellDetail formats a single result into its terse cell representation
// + optional ANSI color when TTY.
func cellDetail(r *PingResult, tty bool) string {
	var body string
	switch {
	case r.OK:
		body = fmt.Sprintf("✓ %d%%", r.PacketLoss)
		if r.RTTAvgMs > 0 {
			body = fmt.Sprintf("%s %.1fms", body, r.RTTAvgMs)
		}
	case r.PacketLoss >= 0:
		body = fmt.Sprintf("✗ %d%%", r.PacketLoss)
	default:
		body = "✗ ERR"
	}
	if tty {
		if r.OK {
			return "\033[32m" + body + "\033[0m"
		}
		return "\033[31m" + body + "\033[0m"
	}
	return body
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
