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
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"sort"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/nvidia/k8s-launch-kit/pkg/presetmatch"
)

// reportTemplate is the embedded HTML body the validate CLI's
// `--report-path` writes after a connectivity run completes. Inlined
// into a single file (CSS in <style>, layout in semantic HTML5) so
// the report is self-contained — drop the file in a chat, attach it
// to a ticket, open it offline, and it renders identically. No JS,
// no external assets.
//
//go:embed report.html.tmpl
var reportTemplate string

// OverallVerdict captures the high-level pass/fail outcome rendered
// as a prominent banner at the top of the report. PASS means every
// gating check succeeded (Helm release version, component versions,
// manifest state, connectivity matrix). Preset deviations are
// informational and do NOT downgrade PASS — they're surfaced
// separately in the Node groups section.
type OverallVerdict struct {
	// Pass is true when every gating check succeeded.
	Pass bool
	// Reasons lists each individual gating check that failed (one
	// short line per failure). Empty when Pass.
	Reasons []string
	// Notes lists informational items that don't gate (preset
	// deviations, in-progress manifests when --wait wasn't used,
	// matrix soft-skip). Surfaced in the banner subtitle when
	// non-empty.
	Notes []string
}

// ReportData is the input to RenderHTML. The validate CLI populates
// it from values it has already computed (Manifests, Matrix) plus a
// few small lookups (cluster API version, node labels).
type ReportData struct {
	// Verdict is the overall pass/fail outcome rendered as a
	// prominent banner at the top of the report. Computed by the
	// caller (CLI) from the same inputs that drive the exit code.
	Verdict OverallVerdict
	Cluster        ClusterInfo
	Profile        ProfileInfo
	NodeGroups     []NodeGroupInfo
	Nodes          []NodeInfo
	Release        *networkoperatorplugin.VersionCheck
	// ComponentCheck cross-references the live NCP+NNP component
	// versions against the embedded release catalog. Surfaced as a
	// sub-table under "Network Operator release" in the HTML
	// report. Nil when the check couldn't run.
	ComponentCheck *networkoperatorplugin.ComponentVersionCheck
	// PresetMatches carries one Result per cluster group from
	// pkg/presetmatch — surfaced under the "Node groups" section.
	// Empty when validate ran without a usable cluster-config.yaml.
	PresetMatches []presetmatch.Result
	Manifests     []networkoperatorplugin.ValidationResult
	Matrix        *MatrixResult
	// Warnings is a flat list of one-line strings rendered as a
	// bulleted rollup at the bottom of the report — typically the
	// "in-progress manifest re-run later" notes and the matrix
	// soft-skip reason when no test pods were schedulable.
	Warnings []string
}

// NodeGroupInfo mirrors one `clusterConfig[]` entry — what `l8k
// discover` bucketed nodes into, plus the PF inventory used to
// render manifests. Surfacing this in the report lets operators
// see at a glance which east-west devices the run was generated
// against, whether a preset matched, and what the rail topology
// looks like.
type NodeGroupInfo struct {
	Identifier       string
	MachineType      string
	GPUType          string
	LinkType         string
	NodeSelector     map[string]string
	WorkerNodes      []string
	SriovCapable     bool
	RdmaCapable      bool
	IbCapable        bool
	PresetApplied    bool
	PresetDeviations []PresetDeviation
	// EastWestPFs / NorthSouthPFs are the *actual* (discovered)
	// PFs on the cluster. ExpectedEastWestPFs / ExpectedNorthSouthPFs
	// mirror the layout but carry the certified topology's PFs from
	// the matched preset (nil when no preset matched). When both are
	// populated the report renders them as paired Actual / Expected
	// sub-tables with mismatched rows highlighted in each.
	EastWestPFs           []PFInfo
	NorthSouthPFs         []PFInfo
	ExpectedEastWestPFs   []PFInfo
	ExpectedNorthSouthPFs []PFInfo
	// PFCountMismatch is non-nil when the discovered PF count
	// differs from the certified topology. Surfaced inline in the
	// East-west PFs Actual header.
	PFCountMismatch *PFCountMismatch
}

// PFInfo is one row in a node group's PF table. The fields mirror
// PFConfig but as strings so the template doesn't need helpers for
// "—" fallbacks on nil pointers.
//
// Mismatched is true when this row diverges from its counterpart in
// the other table (the Actual table flags PCIs whose deviceID drifts
// from the certified topology or PCIs the certified topology
// doesn't list; the Expected table flags PCIs whose deviceID drifts
// or PCIs the cluster doesn't actually have). Rendered as a tinted
// row in both tables — the operator scans down and the diff is
// obvious without inline annotations.
type PFInfo struct {
	PciAddress       string
	DeviceID         string
	Rail             string
	Traffic          string
	NetworkInterface string
	RdmaDevice       string
	PSID             string
	PartNumber       string
	NumaNode         string
	ConnectedGPU     string
	GPUProximity     string
	Mismatched       bool
}

// PFCountMismatch is set on NodeGroupInfo when the discovered PF
// count differs from the certified topology — rendered next to the
// "East-west PFs (N)" header rather than as a separate row.
type PFCountMismatch struct {
	Expected int
	Got      int
}

// PresetDeviation is one row in a node group's deviation list —
// surfaces `cluster-config.yaml`'s presetDeviation entries verbatim.
type PresetDeviation struct {
	Field    string
	Expected string
	Got      string
	Detail   string
}

// ClusterInfo identifies the run: which l8k binary, when, against
// which kube context, talking to which API server.
type ClusterInfo struct {
	L8kVersion       string
	GeneratedAt      time.Time
	KubeContext      string
	APIServerVersion string
	// OperatorNamespace is the namespace where the Network Operator
	// chart was installed (and where validate looked for Helm
	// release Secrets).
	OperatorNamespace string
}

// ProfileInfo describes the deployment shape the report is verifying.
// SpectrumX is non-nil only when the profile opted into Spectrum-X.
type ProfileInfo struct {
	Fabric         string
	DeploymentType string
	Multirail      bool
	SpectrumX      *ProfileSpectrumX
}

// ProfileSpectrumX captures the Spectrum-X-specific render-time choices.
type ProfileSpectrumX struct {
	Version        string
	MultiplaneMode string
	NumberOfPlanes int
}

// NodeInfo is one row in the cluster-nodes table. Role is whatever
// `node-role.kubernetes.io/<role>` label survived (typically
// "control-plane" or "worker"); empty string is rendered as "—".
type NodeInfo struct {
	Name         string
	MachineLabel string
	GpuLabel     string
	Role         string
}

// RenderHTML writes the report to w. Returns an error on template
// parse / execute failure; the writer is not flushed (callers
// typically pass an *os.File which the OS will flush on Close).
func RenderHTML(w io.Writer, data ReportData) error {
	tmpl, err := template.New("validation-report").
		Funcs(reportFuncMap()).
		Parse(reportTemplate)
	if err != nil {
		return fmt.Errorf("parse report template: %w", err)
	}
	if data.Cluster.GeneratedAt.IsZero() {
		data.Cluster.GeneratedAt = time.Now()
	}
	return tmpl.Execute(w, data)
}

// reportFuncMap returns the helpers exposed inside the template. Kept
// small on purpose — anything more complex than a one-liner belongs
// in Go code rather than the template.
func reportFuncMap() template.FuncMap {
	return template.FuncMap{
		// stateClass maps a crstate.CRState to the CSS class used
		// to colour the row / badge.
		"stateClass": func(s crstate.CRState) string {
			switch s {
			case crstate.StateSuccess:
				return "state-success"
			case crstate.StateInProgress:
				return "state-inprogress"
			case crstate.StateError:
				return "state-error"
			case crstate.StateNotDeployed:
				return "state-missing"
			}
			return "state-unknown"
		},
		// presetStatusClass maps presetmatch.Status to the same
		// state-color CSS classes manifest rows use, so the visual
		// language is consistent across the report.
		"presetStatusClass": func(s presetmatch.Status) string {
			switch s {
			case presetmatch.StatusMatch:
				return "state-success"
			case presetmatch.StatusDeviation:
				return "state-inprogress"
			case presetmatch.StatusNotFound:
				return "state-missing"
			case presetmatch.StatusSkipped:
				return "state-missing"
			}
			return "state-unknown"
		},
		// pfsHaveMismatch reports whether any row in the given PF
		// list has Mismatched=true. Used in the template to gate
		// the red/orange section-header tint on whether there's
		// actually a conflict to surface — groups whose Actual and
		// Expected line up render with a plain muted header.
		"pfsHaveMismatch": func(pfs []PFInfo) bool {
			for _, pf := range pfs {
				if pf.Mismatched {
					return true
				}
			}
			return false
		},
		// presetStatusLabel renders the human-facing label for the
		// per-group platform-topology row's status badge.
		"presetStatusLabel": func(s presetmatch.Status) string {
			switch s {
			case presetmatch.StatusMatch:
				return "MATCH"
			case presetmatch.StatusDeviation:
				return "MISMATCH"
			case presetmatch.StatusNotFound:
				return "NOT CERTIFIED"
			case presetmatch.StatusSkipped:
				return "SKIPPED"
			}
			return "UNKNOWN"
		},
		// stateLabel renders the human-facing state name.
		"stateLabel": func(s crstate.CRState) string {
			switch s {
			case crstate.StateSuccess:
				return "READY"
			case crstate.StateInProgress:
				return "IN-PROGRESS"
			case crstate.StateError:
				return "ERROR"
			case crstate.StateNotDeployed:
				return "MISSING"
			}
			return "UNKNOWN"
		},
		// formatTime renders timestamps in the report header.
		"formatTime": func(t time.Time) string {
			return t.UTC().Format("2006-01-02 15:04:05 UTC")
		},
		// sortedKeys returns the keys of a string-keyed map in
		// deterministic order so the rendered Details sub-tables
		// don't shuffle between runs.
		"sortedKeys": func(m map[string]string) []string {
			out := make([]string, 0, len(m))
			for k := range m {
				out = append(out, k)
			}
			sort.Strings(out)
			return out
		},
		// matrixByRail groups same-rail PingResults by (rail, kind
		// family) so the template can render one sub-table per
		// (rail, family) bucket. Families are RDMA-ping and
		// RDMA-bandwidth — see PingTestKind.IsRDMAPing /
		// IsRDMABw.
		"matrixByRail": func(results []PingResult) []railSection {
			type key struct {
				rail string
				fam  string
			}
			byKey := map[key][]PingResult{}
			order := []key{}
			for _, r := range results {
				if r.Test.Kind.IsCrossRail() {
					continue
				}
				k := key{r.Test.Rail, htmlFamilyOf(r.Test.Kind)}
				if _, seen := byKey[k]; !seen {
					order = append(order, k)
				}
				byKey[k] = append(byKey[k], r)
			}
			// Sort deterministically: rail name, then a fixed
			// family order so rping → ib_write_bw appear in
			// execution order in the rendered report.
			sort.Slice(order, func(i, j int) bool {
				if order[i].rail != order[j].rail {
					return order[i].rail < order[j].rail
				}
				return htmlFamilyRank(order[i].fam) < htmlFamilyRank(order[j].fam)
			})
			out := make([]railSection, 0, len(order))
			for _, k := range order {
				rs := railSection{Rail: k.rail, Family: k.fam}
				rs.Nodes, rs.Table = buildRailTable(byKey[k])
				out = append(out, rs)
			}
			return out
		},
		// matrixCrossRail filters down to the cross-rail canaries
		// and groups them by family. Sorted by family rank, then
		// src/dst node within each family.
		"matrixCrossRail": func(results []PingResult) []crossRailSection {
			byFam := map[string][]PingResult{}
			fams := []string{}
			for _, r := range results {
				if !r.Test.Kind.IsCrossRail() {
					continue
				}
				fam := htmlFamilyOf(r.Test.Kind)
				if _, seen := byFam[fam]; !seen {
					fams = append(fams, fam)
				}
				byFam[fam] = append(byFam[fam], r)
			}
			sort.Slice(fams, func(i, j int) bool { return htmlFamilyRank(fams[i]) < htmlFamilyRank(fams[j]) })
			out := make([]crossRailSection, 0, len(fams))
			for _, fam := range fams {
				rows := byFam[fam]
				sort.Slice(rows, func(i, j int) bool {
					if rows[i].Test.SrcNode != rows[j].Test.SrcNode {
						return rows[i].Test.SrcNode < rows[j].Test.SrcNode
					}
					return rows[i].Test.DstNode < rows[j].Test.DstNode
				})
				out = append(out, crossRailSection{Family: fam, Results: rows})
			}
			return out
		},
		// familyTitle is the human-friendly section header for a
		// given kind family.
		"familyTitle": func(fam string) string {
			if fam == "ibbw" {
				return "RDMA bandwidth (ib_write_bw)"
			}
			return "RDMA ping (rping)"
		},
		// cellClass returns the CSS class for a matrix cell based
		// on the result outcome (family-agnostic — color depends
		// only on pass/fail/missing).
		"cellClass": func(r *PingResult) string {
			if r == nil {
				return "cell-missing"
			}
			if r.OK {
				return "cell-pass"
			}
			return "cell-fail"
		},
		// cellText renders the cell's body. Family-specific
		// formatting: rping shows ✓/✗, ib_write_bw shows ✓ N Gbps.
		"cellText": func(r *PingResult, fam string) string {
			if r == nil {
				return "·"
			}
			if fam == "ibbw" {
				if r.OK && r.BandwidthGbps > 0 {
					return fmt.Sprintf("✓ %.1f Gbps", r.BandwidthGbps)
				}
				return "✗ ERR"
			}
			if r.OK {
				return "✓"
			}
			return "✗"
		},
		// nodeLabel mirrors the text renderer's axisLabel: prefer
		// SrcNode/DstNode, fall back to the pod name when the
		// node isn't populated (rare race during scheduling).
		"srcLabel": func(t PingTest) string {
			if t.SrcNode != "" {
				return t.SrcNode
			}
			return t.SrcPod
		},
		"dstLabel": func(t PingTest) string {
			if t.DstNode != "" {
				return t.DstNode
			}
			return t.DstPod
		},
	}
}

// railSection feeds the per-(rail, kind family) sub-table in the
// report. Nodes is the deterministic axis ordering; Table is a flat
// node→node→*result map (the template indexes by string keys for
// cells). Family is one of "rping" / "ibbw" — drives the section
// header and the cell-formatter dispatch.
type railSection struct {
	Rail   string
	Family string
	Nodes  []string
	Table  map[string]map[string]*PingResult
}

// crossRailSection groups cross-rail canary results by kind family
// for the cross-rail list at the bottom of the matrix section.
type crossRailSection struct {
	Family  string
	Results []PingResult
}

// htmlFamilyOf maps a PingTestKind to the family identifier used by
// the HTML template's funcs (kept as a separate string from
// text_report.go's kindFamily enum to keep package boundaries clean).
func htmlFamilyOf(k PingTestKind) string {
	if k.IsRDMABw() {
		return "ibbw"
	}
	return "rping"
}

// htmlFamilyRank gives a stable ordering of families in the rendered
// report — rping (QP establishment) before ib_write_bw (bandwidth).
func htmlFamilyRank(fam string) int {
	if fam == "ibbw" {
		return 1
	}
	return 0
}

// buildRailTable indexes a slice of same-rail PingResults by source
// and destination node labels, mirroring the text renderer.
func buildRailTable(results []PingResult) (nodes []string, table map[string]map[string]*PingResult) {
	table = map[string]map[string]*PingResult{}
	set := map[string]struct{}{}
	for i := range results {
		r := &results[i]
		src := axisLabel(r.Test.SrcNode, r.Test.SrcPod)
		dst := axisLabel(r.Test.DstNode, r.Test.DstPod)
		set[src] = struct{}{}
		set[dst] = struct{}{}
		if _, ok := table[src]; !ok {
			table[src] = map[string]*PingResult{}
		}
		table[src][dst] = r
	}
	nodes = make([]string, 0, len(set))
	for n := range set {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	return
}
