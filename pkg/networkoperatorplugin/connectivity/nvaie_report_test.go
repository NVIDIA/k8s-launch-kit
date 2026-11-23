// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package connectivity

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/nvaie"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNVAIEReportRendersCompactComparison(t *testing.T) {
	node := nvaie.NodeResult{
		Facts: nvaie.NodeFacts{
			GPUProduct: "NVIDIA H100 PCIe <gpu>", OS: "Ubuntu 24.04.2 LTS", MatrixOS: "Ubuntu 24.04",
			Platform: nvaie.RKE2, PlatformVersion: "1.32",
		},
		Checks: []nvaie.Check{
			{Name: "deployment", Status: nvaie.Unknown, Detail: "Manual qualification still required"},
			{Name: "matrix-row", Status: nvaie.Pass, Required: true},
		},
	}
	hostEvidence := nvaie.DOCAHostEvidence{
		MembershipKnown: false,
		Fresh:           false,
		Observations: []nvaie.HostPackageObservation{
			{Node: "worker-1", Status: "installed", Version: "3.5.0-1"},
			{Node: "worker-2", Status: "installed", Version: "3.5.0-2"},
		},
	}
	assessment := &nvaie.Assessment{
		Release: "9.0",
		Checks: []nvaie.Check{
			{Name: "catalog", Status: nvaie.Pass, Required: true},
			{Name: "replicasets", Status: nvaie.Unknown, Required: true, Detail: "ReplicaSet read denied"},
		},
		Nodes: []nvaie.NodeResult{node, node},
		Components: []nvaie.ComponentResult{
			{
				Expected:  nvaie.Component{Name: nvaie.NetworkOperator, Version: "26.7.1"},
				Detection: nvaie.Detected, Check: nvaie.Check{Status: nvaie.Unknown},
				Observations: []nvaie.ComponentObservation{{Version: "26.7.1", ImageStatus: nvaie.Unknown}},
				Workloads:    []nvaie.WorkloadObservation{{Version: "26.7.1", ImageStatus: nvaie.Pass}},
			},
			{
				Expected:  nvaie.Component{Name: nvaie.GPUOperator, Version: "26.3.2"},
				Detection: nvaie.Detected, Check: nvaie.Check{Status: nvaie.Fail},
				Observations: []nvaie.ComponentObservation{
					{Image: "example.invalid/gpu-operator:v26.3.2", Version: "26.3.2", StatusVersion: "26.1.0", ImageStatus: nvaie.Fail},
					{Version: "26.3.2", StatusVersion: "26.1.0", ImageStatus: nvaie.Fail},
				},
				Workloads: []nvaie.WorkloadObservation{
					{Version: "26.3.2", ImageStatus: nvaie.Pass},
					{Version: "26.5.1", ImageStatus: nvaie.Pass},
				},
			},
			{
				Expected:  nvaie.Component{Name: "NIM Operator", Version: "3.1.0"},
				Detection: nvaie.Detected, Check: nvaie.Check{Status: nvaie.Unknown},
				Workloads: []nvaie.WorkloadObservation{{Image: "example.invalid/nim:latest", ImageStatus: nvaie.Unknown}},
			},
			{
				Expected:  nvaie.Component{Name: "Run:ai", Version: "3.0"},
				Detection: nvaie.Detected, Check: nvaie.Check{Status: nvaie.Pass},
				Observations: []nvaie.ComponentObservation{
					{Version: "3.0.12", StatusVersion: "3.0.12", ImageStatus: nvaie.Pass},
				},
			},
			{
				Expected:  nvaie.Component{Name: nvaie.GPUDriver, Version: "595.71.05"},
				Detection: nvaie.DetectionUnknown, Check: nvaie.Check{Status: nvaie.Unknown},
			},
			{
				Expected:  nvaie.Component{Name: nvaie.ContainerToolkit, Version: "1.19.1"},
				Detection: nvaie.NotDetected, Check: nvaie.Check{Status: nvaie.Unknown},
			},
			nvaie.AssessDOCAHost("9.0", hostEvidence),
		},
		Advisories: []string{"Manual qualification still required"},
	}
	section := renderNVAIESection(t, assessment)
	assert.Equal(t, 1, strings.Count(section, "<table>"))
	assert.Equal(t, len(assessment.Components)+2, strings.Count(section, "<tr>"))
	assert.Contains(t, section, "<th>Component</th><th>Expected</th><th>Discovered</th><th>Match</th>")
	assert.Contains(t, section, "Platform compatibility</td><td>NVAIE 9.0 matrix</td><td>SUSE Rancher RKE2 1.32 · Ubuntu 24.04 · NVIDIA H100 PCIe &lt;gpu&gt;</td><td><span class=\"badge state-success\">PASS")
	assert.Contains(t, section, "Network Operator</td><td>26.7.1</td><td>26.7.1 (declared), Unknown</td><td><span class=\"badge state-unknown\">UNKNOWN")
	assert.Contains(t, section, "GPU Operator</td><td>26.3.2</td><td>26.1.0, 26.3.2 (declared), 26.5.1 (declared)</td><td><span class=\"badge state-error\">FAIL")
	assert.Contains(t, section, "NIM Operator</td><td>3.1.0</td><td>Unknown</td><td><span class=\"badge state-unknown\">UNKNOWN")
	assert.Contains(t, section, "Run:ai</td><td>3.0</td><td>3.0.12</td><td><span class=\"badge state-success\">PASS")
	assert.Contains(t, section, "Container Toolkit</td><td>1.19.1</td><td>Not detected</td><td><span class=\"badge state-unknown\">UNKNOWN")
	assert.Contains(t, section, "DOCA Driver for Networking<br><small>worker-1, worker-2</small></td><td>3.5.0</td><td>3.5.0 (discovery snapshot)</td><td><span class=\"badge state-unknown\">UNKNOWN")
	for _, hidden := range []string{"<gpu>", "<h3>", "example.invalid", "Manual qualification", "ReplicaSet read denied", "3.5.0-1", "3.5.0-2"} {
		assert.NotContains(t, section, hidden)
	}

	driver := &assessment.Components[len(assessment.Components)-1]
	driver.Observations = []nvaie.ComponentObservation{{
		Node: "worker-1", Version: "3.5.0", StatusVersion: "3.4.0", ImageStatus: nvaie.Fail,
	}}
	driver.Check.Status = nvaie.Fail
	driver.NodeChecks[0].Check.Status = nvaie.Fail
	section = renderNVAIESection(t, assessment)
	assert.Contains(t, section, "Host packages 3.5.0 (discovery snapshot) · Container images 3.4.0, 3.5.0 (declared)</td><td><span class=\"badge state-error\">FAIL")

	hostEvidence.MembershipKnown = true
	hostEvidence.Fresh = true
	hostEvidence.Observations = append(hostEvidence.Observations,
		nvaie.HostPackageObservation{Node: "worker-3", Status: "not-installed"},
		nvaie.HostPackageObservation{Node: "worker-4", Status: "installed", Version: "unrecognized"},
		nvaie.HostPackageObservation{Node: "worker-5", Status: "installed", Version: "3.4.0-1"},
	)
	assessment.Components[len(assessment.Components)-1] = nvaie.AssessDOCAHost("9.0", hostEvidence)
	section = renderNVAIESection(t, assessment)
	assert.Contains(t, section, "DOCA Driver for Networking<br><small>worker-5</small></td><td>3.5.0</td><td>3.4.0</td><td><span class=\"badge state-error\">FAIL")
	assert.Contains(t, section, "worker-1, worker-2</small></td><td>3.5.0</td><td>3.5.0</td><td><span class=\"badge state-success\">PASS")
	assert.Contains(t, section, "worker-3</small></td><td>3.5.0</td><td>Not detected</td><td><span class=\"badge state-unknown\">UNKNOWN")
	assert.Contains(t, section, "worker-4</small></td><td>3.5.0</td><td>Unknown</td><td><span class=\"badge state-unknown\">UNKNOWN")
	assert.NotContains(t, section, "discovery snapshot")
}

func TestNVAIEReportKeepsPartialAndFailedPlatformEvidence(t *testing.T) {
	assessment := &nvaie.Assessment{
		Release: "9.0",
		Checks: []nvaie.Check{{
			Name: "live observation", Status: nvaie.Unknown, Detail: "Could not read <cluster>", Required: true,
		}},
	}
	section := renderNVAIESection(t, assessment)
	assert.Contains(t, section, "Could not read &lt;cluster&gt;</td><td><span class=\"badge state-unknown\">UNKNOWN")
	assert.NotContains(t, section, "<cluster>")

	assessment.Nodes = []nvaie.NodeResult{{
		Facts:  nvaie.NodeFacts{GPUProduct: "NVIDIA H100 PCIe", OS: "Unsupported OS"},
		Checks: []nvaie.Check{{Name: "matrix-row", Status: nvaie.Fail, Detail: "No supported row", Required: true}},
	}}
	section = renderNVAIESection(t, assessment)
	assert.Contains(t, section, "Unsupported OS · NVIDIA H100 PCIe, No supported row</td><td><span class=\"badge state-error\">FAIL")
}

func renderNVAIESection(t *testing.T, assessment *nvaie.Assessment) string {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, RenderHTML(&out, ReportData{NVAIE: assessment, Cluster: ClusterInfo{GeneratedAt: time.Unix(0, 0)}}))
	_, section, found := strings.Cut(out.String(), "<h2>NVIDIA AI Enterprise 9.0 compliance</h2>")
	require.True(t, found)
	section, _, found = strings.Cut(section, "</section>")
	require.True(t, found)
	return section
}

func TestNVAIEPlatformShowsMatchedCatalogName(t *testing.T) {
	row := nvaiePlatformRow(&nvaie.Assessment{Release: "9.0", Checks: []nvaie.Check{{Name: "catalog", Status: nvaie.Pass, Required: true}}, Nodes: []nvaie.NodeResult{{MatrixGPU: "GB300 NVL72", Facts: nvaie.NodeFacts{MachineType: "GB300-NVL", GPUProduct: "NVIDIA-GB300", Platform: nvaie.RKE2, PlatformVersion: "1.36", OS: "Ubuntu 24.04.5 LTS"}, Checks: []nvaie.Check{{Name: "gpu-model", Status: nvaie.Pass, Required: true}, {Name: "matrix-row", Status: nvaie.Pass, Required: true}}}}})
	require.Equal(t, nvaie.Pass, row.Match)
	require.Contains(t, row.Discovered, "GB300 NVL72")
	require.NotContains(t, row.Discovered, "NVIDIA-GB300")
}

func TestNVAIEReportGroupsNodeOutcomes(t *testing.T) {
	ready := true
	assessment := &nvaie.Assessment{
		Release: "9.0",
		Checks:  []nvaie.Check{{Name: "catalog", Status: nvaie.Pass, Required: true}},
	}
	for _, name := range []string{"worker-c", "worker-b", "worker-a"} {
		assessment.Nodes = append(assessment.Nodes, nvaie.NodeResult{
			Facts:     nvaie.NodeFacts{Name: name, Platform: nvaie.RKE2, PlatformVersion: "1.36", MatrixOS: "Ubuntu 24.04 LTS"},
			MatrixGPU: "GB300 NVL72", Checks: []nvaie.Check{{Name: "matrix-row", Status: nvaie.Pass, Required: true}},
		})
	}
	assessment.Nodes[1].Checks[0] = nvaie.Check{Name: "matrix-row", Status: nvaie.Fail, Detail: "No supported row", Required: true}
	assessment.Nodes[1].Facts.MatrixOS = "Unsupported OS"
	toolkit := nvaie.ComponentResult{
		Expected:  nvaie.Component{Name: nvaie.ContainerToolkit, Version: "1.20.1"},
		Detection: nvaie.Detected, Check: nvaie.Check{Status: nvaie.Unknown},
		Workloads: []nvaie.WorkloadObservation{{Version: "1.20.1", ImageStatus: nvaie.Pass}},
	}
	for _, name := range []string{"worker-c", "worker-a"} {
		toolkit.Observations = append(toolkit.Observations, nvaie.ComponentObservation{
			Node: name, Version: "1.20.1", StatusVersion: "1.20.1", ImageStatus: nvaie.Pass, Ready: &ready,
		})
		toolkit.NodeChecks = append(toolkit.NodeChecks, nvaie.ComponentNodeCheck{Node: name, Check: nvaie.Check{Status: nvaie.Pass}})
	}
	toolkit.NodeChecks = append(toolkit.NodeChecks, nvaie.ComponentNodeCheck{Node: "worker-b", Check: nvaie.Check{Status: nvaie.Unknown}})
	assessment.Components = []nvaie.ComponentResult{toolkit, {
		Expected: nvaie.Component{Name: nvaie.NetworkOperator, Version: "26.7.1"}, Detection: nvaie.Detected,
		Check:        nvaie.Check{Status: nvaie.Pass},
		Observations: []nvaie.ComponentObservation{{Node: "worker-b", Version: "26.7.1", StatusVersion: "26.7.1", ImageStatus: nvaie.Pass}},
	}}
	before, err := json.Marshal(assessment)
	require.NoError(t, err)
	rows := nvaieReportRows(assessment)
	require.Len(t, rows, 5)
	assert.Equal(t, []string{"worker-b"}, rows[0].Nodes)
	assert.Equal(t, nvaie.Fail, rows[0].Match)
	assert.Equal(t, []string{"worker-a", "worker-c"}, rows[1].Nodes)
	assert.Equal(t, nvaie.Pass, rows[1].Match)
	assert.Equal(t, nvaieReportRow{Component: nvaie.ContainerToolkit, Expected: "1.20.1", Discovered: "1.20.1", Match: nvaie.Pass, Nodes: []string{"worker-a", "worker-c"}}, rows[2])
	assert.Equal(t, nvaieReportRow{Component: nvaie.ContainerToolkit, Expected: "1.20.1", Discovered: "Not detected", Match: nvaie.Unknown, Nodes: []string{"worker-b"}}, rows[3])
	assert.Equal(t, nvaie.NetworkOperator, rows[4].Component)
	assert.Empty(t, rows[4].Nodes, "operator placement does not define its cluster scope")
	section := renderNVAIESection(t, assessment)
	assert.Equal(t, 1, strings.Count(section, "<table>"))
	assert.Contains(t, section, "Container Toolkit<br><small>worker-a, worker-c</small></td><td>1.20.1</td><td>1.20.1</td><td><span class=\"badge state-success\">PASS")
	assert.Contains(t, section, "Container Toolkit<br><small>worker-b</small></td><td>1.20.1</td><td>Not detected</td><td><span class=\"badge state-unknown\">UNKNOWN")
	after, err := json.Marshal(assessment)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after), "rendering must not mutate the assessment")

	// A different observed version must remain a separate group, even with the same state.
	assessment.Components[0].Observations[0].StatusVersion = "1.19.1"
	assessment.Components[0].NodeChecks[0].Check.Status = nvaie.Unknown
	rows = nvaieReportRows(assessment)
	require.Len(t, rows, 6)
	assert.Equal(t, []string{"worker-a"}, rows[2].Nodes)
	assert.Equal(t, []string{"worker-c"}, rows[3].Nodes)
	assert.Contains(t, rows[3].Discovered, "1.19.1")
	assert.Equal(t, []string{"worker-b"}, rows[4].Nodes)
}

func TestNVAIEReportKeepsScopeEvidenceAndEscapesMembership(t *testing.T) {
	assessment := &nvaie.Assessment{Release: "9.0", Components: []nvaie.ComponentResult{{
		Expected: nvaie.Component{Name: nvaie.ContainerToolkit, Version: "1.20.1"}, Detection: nvaie.Detected,
		Check:        nvaie.Check{Status: nvaie.Fail},
		NodeChecks:   []nvaie.ComponentNodeCheck{{Node: "worker-<a>", Check: nvaie.Check{Status: nvaie.Pass}}},
		Observations: []nvaie.ComponentObservation{{Node: "worker-<a>", Version: "1.20.1", StatusVersion: "1.20.1", ImageStatus: nvaie.Pass}},
		Workloads:    []nvaie.WorkloadObservation{{Version: "1.19.1", ImageStatus: nvaie.Pass}},
		ScopeCheck:   &nvaie.Check{Status: nvaie.Fail, Detail: "Unplaced conflicting template <version>"},
	}}}
	rows := nvaieReportRows(assessment)
	require.Len(t, rows, 3)
	assert.Equal(t, "1.20.1", rows[1].Discovered)
	assert.Equal(t, nvaie.Pass, rows[1].Match)
	assert.Equal(t, "Cluster / scope", rows[2].Scope)
	assert.Equal(t, nvaie.Fail, rows[2].Match)
	assert.Contains(t, rows[2].Discovered, "1.19.1 (declared)")
	section := renderNVAIESection(t, assessment)
	assert.Contains(t, section, "worker-&lt;a&gt;")
	assert.Contains(t, section, "&lt;version&gt;")
	assert.NotContains(t, section, "worker-<a>")
}

func TestNVAIEReportShowsOnlyAttributedTemplateVersions(t *testing.T) {
	component := nvaie.ComponentResult{
		Expected: nvaie.Component{Name: nvaie.ContainerToolkit, Version: "1.20.1"}, Detection: nvaie.Detected,
		Check:     nvaie.Check{Status: nvaie.Fail},
		Workloads: []nvaie.WorkloadObservation{{Version: "1.18.0", ImageStatus: nvaie.Pass}, {Version: "1.19.1", ImageStatus: nvaie.Pass}},
	}
	for _, name := range []string{"worker-a", "worker-b"} {
		component.Observations = append(component.Observations, nvaie.ComponentObservation{Node: name, Version: "1.20.1", StatusVersion: "1.20.1", ImageStatus: nvaie.Pass})
	}
	component.NodeChecks = []nvaie.ComponentNodeCheck{
		{Node: "worker-a", Check: nvaie.Check{Status: nvaie.Fail}, Workloads: component.Workloads[:1]},
		{Node: "worker-b", Check: nvaie.Check{Status: nvaie.Fail}, Workloads: component.Workloads[1:]},
	}
	rows := nvaieReportRows(&nvaie.Assessment{Release: "9.0", Components: []nvaie.ComponentResult{component}})
	require.Len(t, rows, 3)
	assert.Equal(t, []string{"worker-a"}, rows[1].Nodes)
	assert.Equal(t, "1.18.0 (declared), 1.20.1", rows[1].Discovered)
	assert.Equal(t, []string{"worker-b"}, rows[2].Nodes)
	assert.Equal(t, "1.19.1 (declared), 1.20.1", rows[2].Discovered)
}

func TestNVAIEPlatformGroupsRetainCommonPrerequisites(t *testing.T) {
	assessment := &nvaie.Assessment{
		Release: "9.0", Checks: []nvaie.Check{{Name: "api-server-version", Status: nvaie.Unknown, Required: true, Detail: "API server evidence unavailable"}},
		Nodes: []nvaie.NodeResult{
			{Facts: nvaie.NodeFacts{Name: "worker-b"}, Checks: []nvaie.Check{{Name: "matrix-row", Required: true, Status: nvaie.Pass}}},
			{Facts: nvaie.NodeFacts{Name: "worker-a"}, Checks: []nvaie.Check{{Name: "matrix-row", Required: true, Status: nvaie.Pass}}},
			{Facts: nvaie.NodeFacts{Name: "worker-c"}, Checks: []nvaie.Check{{Name: "matrix-row", Required: true, Status: nvaie.Fail, Detail: "Unsupported platform"}}},
		},
	}
	rows := nvaieReportRows(assessment)
	require.Len(t, rows, 2)
	assert.Equal(t, nvaie.Fail, rows[0].Match)
	assert.Equal(t, []string{"worker-c"}, rows[0].Nodes)
	assert.Contains(t, rows[0].Discovered, "Unsupported platform")
	assert.Equal(t, nvaie.Unknown, rows[1].Match)
	assert.Equal(t, []string{"worker-a", "worker-b"}, rows[1].Nodes)
	assert.Contains(t, rows[1].Discovered, "API server evidence unavailable")
}
