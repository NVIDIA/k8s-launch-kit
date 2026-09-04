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

package spectrumx

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

func TestDetectTopologyFormat(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		wantFormat topologyFormat
		wantError  string
	}{
		{
			name:       "reference generator",
			content:    `{"nodes": [], "links": []}`,
			wantFormat: topologyFormatReference,
		},
		{
			name:       "NVIDIA AIR ignores outer format value",
			content:    `{"format": "not-used", "content": {"nodes": {}, "links": []}}`,
			wantFormat: topologyFormatAIR,
		},
		{
			name:      "ambiguous",
			content:   `{"nodes": [], "links": [], "content": {"nodes": {}, "links": []}}`,
			wantError: "ambiguous topology format",
		},
		{
			name:      "unsupported shape",
			content:   `{"format": "JSON", "nodes": {}, "links": []}`,
			wantError: "unsupported topology format",
		},
		{
			name:      "invalid JSON",
			content:   `{"nodes":`,
			wantError: "unexpected end of JSON input",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			format, err := detectTopologyFormat([]byte(tt.content))
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantFormat, format)
		})
	}
}

func TestParseAIRTopologyNormalizesOneBasedNamingContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "air-simple-quadplane.json"))
	require.NoError(t, err)

	topology, err := parseTopology(data, config.SpectrumXTopology2Tier)
	require.NoError(t, err)
	require.Len(t, topology.Links, 8)

	host := topology.Links[0][0]
	leaf := topology.Links[0][1]
	require.Equal(t, "host", host.Attrs.Role)
	require.Equal(t, 0, host.Attrs.Rail)
	require.Equal(t, 0, host.Attrs.SU)
	require.Equal(t, "leaf", leaf.Attrs.Role)
	require.Equal(t, 0, leaf.Attrs.Plane)
	require.Equal(t, 0, leaf.Attrs.Rail)
	require.Equal(t, 0, leaf.Attrs.SU)
	require.Equal(t, 3, topology.Links[3][1].Attrs.Plane)
}

func TestParseAIRTopologyNormalizes3TierNamingContract(t *testing.T) {
	topology, err := parseTopology([]byte(`{
  "content": {
    "nodes": {
      "worker-pod02-su03-h04": {
        "model": "host",
        "network_pci": {"rail2": {}}
      },
      "leaf-p1-pod02-su03-r2": {"model": "SN5600", "network_pci": {}},
      "spine-pod02-r2-s01": {"model": "SN5600", "network_pci": {}}
    },
    "links": [
      [
        {"node": "leaf-p1-pod02-su03-r2", "interface": "swp2"},
        {"node": "spine-pod02-r2-s01", "interface": "swp1"}
      ],
      [
        {"node": "worker-pod02-su03-h04", "interface": "rail2p1", "network_pci": "rail2"},
        {"node": "leaf-p1-pod02-su03-r2", "interface": "swp1s0"}
      ]
    ]
  }
}`), config.SpectrumXTopology3Tier)
	require.NoError(t, err)
	require.Len(t, topology.Links, 1)

	host := topology.Links[0][0]
	leaf := topology.Links[0][1]
	require.Equal(t, 1, host.Attrs.Pod)
	require.True(t, host.Attrs.HasPod)
	require.Equal(t, 2, host.Attrs.SU)
	require.Equal(t, 1, host.Attrs.Rail)
	require.Equal(t, 1, leaf.Attrs.Pod)
	require.Equal(t, 0, leaf.Attrs.Plane)
}

func TestParseAIRTopologyRejectsContractViolations(t *testing.T) {
	tests := []struct {
		name         string
		hostName     string
		leafName     string
		hostIface    string
		networkPCI   string
		topologyType string
		wantError    string
	}{
		{
			name:         "host interface format",
			hostName:     "worker-su01-h01",
			leafName:     "leaf-p1-su01-r1",
			hostIface:    "eth0",
			networkPCI:   "rail1",
			topologyType: config.SpectrumXTopology2Tier,
			wantError:    "interface must match rail<R>p<P>",
		},
		{
			name:         "interface and network PCI rail disagree",
			hostName:     "worker-su01-h01",
			leafName:     "leaf-p1-su01-r2",
			hostIface:    "rail2p1",
			networkPCI:   "rail1",
			topologyType: config.SpectrumXTopology2Tier,
			wantError:    "has rail 2 but network_pci \"rail1\" identifies rail 1",
		},
		{
			name:         "host and leaf SU disagree",
			hostName:     "worker-su01-h01",
			leafName:     "leaf-p1-su02-r1",
			hostIface:    "rail1p1",
			networkPCI:   "rail1",
			topologyType: config.SpectrumXTopology2Tier,
			wantError:    "has host SU 1",
		},
		{
			name:         "host connected to non-leaf",
			hostName:     "worker-su01-h01",
			leafName:     "spine-p1-su01-r1",
			hostIface:    "rail1p1",
			networkPCI:   "rail1",
			topologyType: config.SpectrumXTopology2Tier,
			wantError:    "is not a leaf node",
		},
		{
			name:         "3-tier pod missing",
			hostName:     "worker-su01-h01",
			leafName:     "leaf-p1-su01-r1",
			hostIface:    "rail1p1",
			networkPCI:   "rail1",
			topologyType: config.SpectrumXTopology3Tier,
			wantError:    "requires pod<N> name tokens",
		},
		{
			name:         "zero ordinal",
			hostName:     "worker-su00-h01",
			leafName:     "leaf-p1-su01-r1",
			hostIface:    "rail1p1",
			networkPCI:   "rail1",
			topologyType: config.SpectrumXTopology2Tier,
			wantError:    "ordinal must be at least 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := `{
  "content": {
    "nodes": {
      "` + tt.hostName + `": {"model": "host", "network_pci": {"` + tt.networkPCI + `": {}}},
      "` + tt.leafName + `": {"model": "SN5600", "network_pci": {}}
    },
    "links": [[
      {"node": "` + tt.hostName + `", "interface": "` + tt.hostIface + `", "network_pci": "` + tt.networkPCI + `"},
      {"node": "` + tt.leafName + `", "interface": "swp1s0"}
    ]]
  }
}`
			_, err := parseTopology([]byte(content), tt.topologyType)
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}
