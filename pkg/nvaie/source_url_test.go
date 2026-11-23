// SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package nvaie

import (
	"net/url"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/presets"
	"github.com/stretchr/testify/require"
)

func TestMatrixSourceURL(t *testing.T) {
	release, ok := Lookup("9.0")
	require.True(t, ok)
	ocp := NodeFacts{
		GPUProduct:      "NVIDIA-RTX-PRO-6000-Blackwell-Server-Edition",
		OS:              "Red Hat Enterprise Linux CoreOS 10.2.0",
		MatrixOS:        "Red Hat CoreOS 4.22",
		Platform:        OpenShift,
		PlatformVersion: "4.22.3",
	}
	for _, tt := range []struct {
		name       string
		deployment Deployment
		candidate  bool
		filters    url.Values
	}{
		{
			name:       "observed bare metal",
			deployment: BareMetal,
			filters:    url.Values{"gdep": {"bm-k8s"}, "gplat": {OpenShift}, "gpver": {"4.22"}, "gos": {"Red Hat CoreOS"}, "gosver": {"4.22"}},
		},
		{
			name:      "bare metal candidate for unverified deployment",
			candidate: true,
			filters:   url.Values{"gdep": {"bm-k8s"}, "gplat": {OpenShift}, "gpver": {"4.22"}, "gos": {"Red Hat CoreOS"}, "gosver": {"4.22"}},
		},
		{name: "simulated deployment", deployment: Simulated},
		{name: "virtual deployment", deployment: Deployment("virtualized")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			facts := ocp
			facts.Deployment = tt.deployment
			source, candidate := matrixSourceURL(release, facts)
			require.Equal(t, tt.candidate, candidate)
			parsed, err := url.Parse(source)
			require.NoError(t, err)
			require.Equal(t, "https", parsed.Scheme)
			require.Equal(t, "docs.nvidia.com", parsed.Host)
			require.Equal(t, "/ai-enterprise/support-matrix/latest/index.html", parsed.Path)
			query := url.Values{
				"sm_v": {"1"},
				"view": {"landing"},
				"mode": {"gpu"},
				"gpu":  {"RTX PRO 6000 Blackwell Server Edition"},
				"grel": {"9.0"},
			}
			for name, values := range tt.filters {
				query[name] = values
			}
			require.Equal(t, query, parsed.Query())
		})
	}
}

func TestMatrixSourceURLOmitsUnknownValues(t *testing.T) {
	release, ok := Lookup("9.0")
	require.True(t, ok)
	source, candidate := matrixSourceURL(release, NodeFacts{
		GPUProduct:      "NVIDIA RTX PRO 6000 Blackwell Server Edition",
		OS:              "Ubuntu 24.04.3 LTS",
		Platform:        "unrecognized&injected=true",
		PlatformVersion: "unknown",
	})
	require.True(t, candidate)
	parsed, err := url.Parse(source)
	require.NoError(t, err)
	require.Equal(t, "Ubuntu", parsed.Query().Get("gos"))
	require.Equal(t, "24.04 LTS", parsed.Query().Get("gosver"))
	require.NotContains(t, parsed.Query(), "gplat")
	require.NotContains(t, parsed.Query(), "gpver")
	require.NotContains(t, parsed.Query(), "injected")

	source, candidate = matrixSourceURL(release, NodeFacts{
		GPUProduct:      "NVIDIA RTX PRO 6000 Blackwell Server Edition",
		OS:              "unknown",
		Platform:        RKE2,
		PlatformVersion: "1.34&injected=true",
	})
	require.True(t, candidate)
	parsed, err = url.Parse(source)
	require.NoError(t, err)
	require.Equal(t, RKE2, parsed.Query().Get("gplat"))
	require.NotContains(t, parsed.Query(), "gpver")
	require.NotContains(t, parsed.Query(), "gos")

	source, candidate = matrixSourceURL(release, NodeFacts{
		GPUProduct: "NVIDIA RTX PRO 6000 Blackwell Server Edition",
	})
	require.False(t, candidate)
	parsed, err = url.Parse(source)
	require.NoError(t, err)
	require.NotContains(t, parsed.Query(), "gdep")
}

func TestMatrixSourceURLRequiresUnambiguousGPU(t *testing.T) {
	release, ok := Lookup("9.0")
	require.True(t, ok)
	for _, facts := range []NodeFacts{
		{},
		{GPUProduct: "unsupported"},
		{GPUProduct: "H100"},
		{GPUProduct: "NVIDIA RTX PRO 6000 Blackwell Server Edition", GPUConflict: true},
	} {
		source, candidate := matrixSourceURL(release, facts)
		require.Empty(t, source)
		require.False(t, candidate)
	}
}

// Keep every bundled machine/product pair in the audit. New presets require
// an explicit expected mapping or a deliberately unresolved result.
func TestAllPresetGPUProductsMatchNVAIECatalog(t *testing.T) {
	catalog, err := presets.EmbeddedCatalog()
	require.NoError(t, err)
	summaries, skipped, err := catalog.ListPresetSummaries()
	require.NoError(t, err)
	require.Empty(t, skipped)
	expected := map[string]string{
		"DGX-GB300-Compute-Tray-NVIDIA-GB300":            "DGX GB300 NVL72",
		"GB200-NVL-NVIDIA-GB200":                         "", // NVL4 and NVL72 are distinct published entries.
		"GB300-DGX-Station-NVIDIA-GB300":                 "", // Desktop Superchip is not NVL72.
		"GB300-NVL-NVIDIA-GB300":                         "GB300 NVL72",
		"PowerEdge-R760xa-H100-NVL":                      "H100 NVL",
		"PowerEdge-XE7745-RTX-PRO-4500":                  "RTX PRO 4500 Blackwell Server Edition",
		"PowerEdge-XE9680-H200":                          "H200 SXM",
		"ThinkSystem-SR650-V4-RTX-PRO-6000":              "RTX PRO 6000 Blackwell Server Edition",
		"ThinkSystem-SR675-V3-H200-NVL":                  "H200 NVL",
		"ThinkSystem-SR675-V3-RTX-PRO-6000":              "RTX PRO 6000 Blackwell Server Edition",
		"ThinkSystem-SR675-V3-System-Board-RTX-PRO-6000": "RTX PRO 6000 Blackwell Server Edition",
		"ThinkSystem-SR680a-V3-H200":                     "H200 SXM",
		"UCSC-885A-M8-H22-H200":                          "H200 SXM",
	}
	require.Len(t, summaries, len(expected))
	release, ok := Lookup("9.0")
	require.True(t, ok)
	for _, preset := range summaries {
		t.Run(preset.DirName, func(t *testing.T) {
			name, exists := expected[preset.DirName]
			require.True(t, exists, "audit the new preset")
			facts := NodeFacts{GPUProduct: preset.GPUType, MachineType: preset.MachineType, OS: "Ubuntu 24.04.5 LTS", Platform: RKE2, PlatformVersion: "1.36", ServerPlatformVersion: "1.36", Runtime: "Containerd"}
			result := EvaluateNode(release, facts)
			require.Equal(t, name, result.MatrixGPU)
			require.Equal(t, facts.GPUProduct, result.Facts.GPUProduct, "preserve raw product")
			require.Equal(t, facts.MachineType, result.Facts.MachineType, "preserve raw machine")
			if name == "" {
				require.Equal(t, Unknown, findCheck(result.Checks, "gpu-model").Status)
				require.Empty(t, result.SourceURL)
				t.Logf("%s | %s | unresolved", preset.MachineType, preset.GPUType)
				return
			}
			require.Equal(t, Pass, findCheck(result.Checks, "gpu-model").Status)
			require.Equal(t, Pass, findCheck(result.Checks, "matrix-row").Status)
			query, err := url.Parse(result.SourceURL)
			require.NoError(t, err)
			require.Equal(t, name, query.Query().Get("gpu"))
			require.Equal(t, "24.04 LTS", query.Query().Get("gosver"))
			t.Logf("%s | %s | %s", preset.MachineType, preset.GPUType, name)
		})
	}
}

func TestMachineAliasesDoNotGuessOrOverrideGPUConflicts(t *testing.T) {
	release, ok := Lookup("9.0")
	require.True(t, ok)
	for _, facts := range []NodeFacts{
		{GPUProduct: "NVIDIA-GB300"},
		{GPUProduct: "NVIDIA-GB300", MachineType: "GB300-DGX-Station"},
		{GPUProduct: "NVIDIA-GB300", MachineType: "GB300-NVL-prototype"},
		{GPUProduct: "NVIDIA-GB200", MachineType: "GB200-NVL"},
		{GPUProduct: "NVIDIA-H200", MachineType: "unknown"},
		{GPUProduct: "NVIDIA-RTX-PRO-4500-Blackwell", MachineType: "workstation"},
		{GPUProduct: "NVIDIA-GB300", MachineType: "GB300-NVL", GPUConflict: true},
		{GPUProduct: "", MachineType: "GB300-NVL"},
	} {
		result := EvaluateNode(release, facts)
		require.Empty(t, result.MatrixGPU, "%+v", facts)
		require.Empty(t, result.SourceURL, "%+v", facts)
		require.Equal(t, Unknown, findCheck(result.Checks, "gpu-model").Status)
	}
}
