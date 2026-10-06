// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

func TestSpectrumXPlatformSubstrings(t *testing.T) {
	for product, want := range map[string]string{
		"NVIDIA-H100-NVL": "h100", "NVIDIA-H200-NVL": "h200", "NVIDIA-B200": "b200",
		"NVIDIA-GB200": "gb200", "NVIDIA-B300": "b300", "NVIDIA-GB300": "gb300",
		"NVIDIA-RTX-PRO-6000-Blackwell-Server-Edition": "rtx", "NVIDIA-RTX-PRO-4500-Blackwell": "rtx",
		" no-preset-h200-sxm5-141GB ": "h200", "H2000": "h200", "NVIDIA-VR": "vr",
		"": "", "unknown": "", "H100-H200": "", "B200-GB300": "gb300",
	} {
		t.Run(product, func(t *testing.T) { require.Equal(t, want, SpectrumXPlatformForGPUProduct(product)) })
	}
}

func TestRAReleasePolicy(t *testing.T) {
	for _, tc := range []struct {
		ra, release string
		valid       bool
	}{
		{"RA2.3", "26.7", true}, {"RA2.3", "26.10", false}, {"RA2.4", "26.7", false},
		{"RA2.4", "26.10", true}, {"RA2.4", "26.11", true}, {"RA2.4", "27.1", true},
		{"RA2.4", "invalid", false}, {"unknown", "26.10", false},
	} {
		t.Run(tc.ra+tc.release, func(t *testing.T) {
			err := ValidateSPCXRelease(tc.ra, tc.release)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	require.Equal(t, "26.10", DefaultSPCXReleaseFor("RA2.4"))
}

func TestDospcxConfigMapRoundTripAndNamespace(t *testing.T) {
	raw, err := os.ReadFile("testdata/dospcx-configmap.yaml")
	require.NoError(t, err)
	spcx := &ProfileSpectrumX{SPCXVersion: "RA2.4", Profile: string(raw)}
	for i := 0; i < 3; i++ {
		require.NoError(t, NormalizeSpectrumXProfileConfig(spcx))
		cfg := &LaunchKitConfig{Profile: &Profile{SpectrumX: spcx}}
		cloned, err := CloneConfig(cfg)
		require.NoError(t, err)
		encoded, err := yaml.Marshal(cloned)
		require.NoError(t, err)
		decoded := &LaunchKitConfig{}
		require.NoError(t, yaml.Unmarshal(encoded, decoded))
		spcx = decoded.Profile.SpectrumX
	}
	require.Equal(t, string(raw), spcx.Profile)
	require.Equal(t, "test-dospcx-data", spcx.ConfigMapName)
	rendered, err := RenderDospcxConfigMap(spcx, "operator-namespace")
	require.NoError(t, err)
	cm, _, err := parseSpectrumXProfileConfigMap(rendered)
	require.NoError(t, err)
	source, _, err := parseSpectrumXProfileConfigMap(string(raw))
	require.NoError(t, err)
	require.Equal(t, "operator-namespace", cm.Metadata.Namespace)
	require.Equal(t, source.BinaryData, cm.BinaryData)
	require.Equal(t, source.Metadata.Annotations, cm.Metadata.Annotations)
	require.Contains(t, cm.Metadata.Labels, SpectrumXProfileLabel)
	// Validation is delayed until after a caller can override the RA.
	spcx.SPCXVersion = "RA2.3"
	require.NoError(t, NormalizeSpectrumXProfileConfig(spcx))
	require.Error(t, ValidateSpectrumXProfileFormat(spcx))
	spcx.SPCXVersion = "RA2.4"
	require.NoError(t, ValidateSpectrumXProfileFormat(spcx))
}

func TestDospcxMalformedInputs(t *testing.T) {
	raw, err := os.ReadFile("testdata/dospcx-configmap.yaml")
	require.NoError(t, err)
	cm, _, err := parseSpectrumXProfileConfigMap(string(raw))
	require.NoError(t, err)
	for name, input := range map[string]string{
		"legacy":       "useSoftwareCCAlgorithm: true\n",
		"wrong format": strings.Replace(string(raw), DospcxDataFormat, "unsupported", 1),
		"missing name": strings.Replace(string(raw), "name: test-dospcx-data", "name: ''", 1),
		"bad archive":  strings.Replace(string(raw), cm.BinaryData[DospcxArchiveKey], "invalid", 1),
		"wrong api":    strings.Replace(string(raw), "apiVersion: v1", "apiVersion: v2", 1),
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, ValidateSpectrumXProfileFormat(&ProfileSpectrumX{SPCXVersion: "RA2.4", ConfigMapName: "test-dospcx-data", Profile: input}))
		})
	}
}

func TestDerivedPlatformPreservesRailIntentAndCloneIsolation(t *testing.T) {
	cfg := &LaunchKitConfig{ClusterConfig: []ClusterConfig{{GPUType: "NVIDIA-GB300", SpectrumX: &SpectrumXGroupConfig{PlatformType: "stale", SwPlaneByRail: map[int]int{0: 0, 1: 1}}}}}
	cloned, err := CloneConfig(cfg)
	require.NoError(t, err)
	PopulateSpectrumXPlatforms(cloned.ClusterConfig)
	require.Equal(t, "gb300", cloned.ClusterConfig[0].SpectrumX.PlatformType)
	require.Equal(t, map[int]int{0: 0, 1: 1}, cloned.ClusterConfig[0].SpectrumX.SwPlaneByRail)
	cloned.ClusterConfig[0].SpectrumX.SwPlaneByRail[1] = 2
	require.Equal(t, "stale", cfg.ClusterConfig[0].SpectrumX.PlatformType)
	require.Equal(t, 1, cfg.ClusterConfig[0].SpectrumX.SwPlaneByRail[1])
}
