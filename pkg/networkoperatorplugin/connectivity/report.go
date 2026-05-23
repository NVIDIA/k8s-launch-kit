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

// ReportData is the input to RenderHTML. The validate CLI populates
// it from values it has already computed (Manifests, Matrix) plus a
// few small lookups (cluster API version, node labels).
type ReportData struct {
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
	Manifests      []networkoperatorplugin.ValidationResult
	Matrix         *MatrixResult
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
	EastWestPFs      []PFInfo
	NorthSouthPFs    []PFInfo
}

// PFInfo is one row in a node group's PF table. The fields mirror
// PFConfig but as strings so the template doesn't need helpers for
// "—" fallbacks on nil pointers.
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
	GPUProximity    string
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
	tmpl, err := template.New("verify-report").
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
		// matrixByRail groups same-rail PingResults by rail name so
		// the template can render one sub-table per rail.
		"matrixByRail": func(results []PingResult) []railSection {
			byRail := map[string][]PingResult{}
			rails := []string{}
			for _, r := range results {
				if r.Test.Kind != PingSameRail {
					continue
				}
				if _, seen := byRail[r.Test.Rail]; !seen {
					rails = append(rails, r.Test.Rail)
				}
				byRail[r.Test.Rail] = append(byRail[r.Test.Rail], r)
			}
			sort.Strings(rails)
			out := make([]railSection, 0, len(rails))
			for _, rail := range rails {
				rs := railSection{Rail: rail}
				rs.Nodes, rs.Table = buildRailTable(byRail[rail])
				out = append(out, rs)
			}
			return out
		},
		// matrixCrossRail filters down to the cross-rail canaries
		// and sorts them deterministically.
		"matrixCrossRail": func(results []PingResult) []PingResult {
			cross := make([]PingResult, 0)
			for _, r := range results {
				if r.Test.Kind == PingCrossRail {
					cross = append(cross, r)
				}
			}
			sort.Slice(cross, func(i, j int) bool {
				if cross[i].Test.SrcNode != cross[j].Test.SrcNode {
					return cross[i].Test.SrcNode < cross[j].Test.SrcNode
				}
				return cross[i].Test.DstNode < cross[j].Test.DstNode
			})
			return cross
		},
		// cellClass returns the CSS class for a matrix cell based
		// on the ping outcome.
		"cellClass": func(r *PingResult) string {
			if r == nil {
				return "cell-missing"
			}
			if r.OK {
				return "cell-pass"
			}
			return "cell-fail"
		},
		// cellText renders the cell's body (loss%/rtt or "ERR").
		"cellText": func(r *PingResult) string {
			if r == nil {
				return "·"
			}
			if r.OK {
				if r.RTTAvgMs > 0 {
					return fmt.Sprintf("✓ %d%% %.2fms", r.PacketLoss, r.RTTAvgMs)
				}
				return fmt.Sprintf("✓ %d%%", r.PacketLoss)
			}
			if r.PacketLoss >= 0 {
				return fmt.Sprintf("✗ %d%%", r.PacketLoss)
			}
			return "✗ ERR"
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

// railSection feeds the per-rail sub-table in the report. Nodes is
// the deterministic axis ordering; Table is a flat node→node→*result
// map (the template indexes by string keys for cells).
type railSection struct {
	Rail  string
	Nodes []string
	Table map[string]map[string]*PingResult
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
