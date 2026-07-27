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

func TestBuildCIDRPools2TierSWPLB(t *testing.T) {
	topologyPath := writeTopology(t, `{
  "nodes": [
    {"name": "compute-a", "role": "host", "type": "default"},
    {"name": "compute-b", "role": "host", "type": "default"},
    {"name": "leaf-p0-r0", "role": "leaf", "type": "cumulus"},
    {"name": "leaf-p0-r1", "role": "leaf", "type": "cumulus"},
    {"name": "leaf-p1-r0", "role": "leaf", "type": "cumulus"},
    {"name": "leaf-p1-r1", "role": "leaf", "type": "cumulus"}
  ],
  "links": [
    [
      {"node": "leaf-p0-r0", "interface": "swp1s0", "attributes": {"role": "leaf", "plane": 0, "pod": 0, "su": 0, "rail_group": [0]}},
      {"node": "compute-a", "interface": "eth_p0_r0", "attributes": {"role": "host", "rail": 0, "pod": 0, "su": 0}}
    ],
    [
      {"node": "leaf-p0-r0", "interface": "swp2s0", "attributes": {"role": "leaf", "plane": 0, "pod": 0, "su": 0, "rail_group": [0]}},
      {"node": "compute-b", "interface": "eth_p0_r0", "attributes": {"role": "host", "rail": 0, "pod": 0, "su": 0}}
    ],
    [
      {"node": "leaf-p0-r1", "interface": "swp1s0", "attributes": {"role": "leaf", "plane": 0, "pod": 0, "su": 0, "rail_group": [1]}},
      {"node": "compute-a", "interface": "eth_p0_r1", "attributes": {"role": "host", "rail": 1, "pod": 0, "su": 0}}
    ],
    [
      {"node": "leaf-p0-r1", "interface": "swp2s0", "attributes": {"role": "leaf", "plane": 0, "pod": 0, "su": 0, "rail_group": [1]}},
      {"node": "compute-b", "interface": "eth_p0_r1", "attributes": {"role": "host", "rail": 1, "pod": 0, "su": 0}}
    ],
    [
      {"node": "leaf-p1-r0", "interface": "swp1s0", "attributes": {"role": "leaf", "plane": 1, "pod": 0, "su": 0, "rail_group": [0]}},
      {"node": "compute-a", "interface": "eth_p1_r0", "attributes": {"role": "host", "rail": 0, "pod": 0, "su": 0}}
    ],
    [
      {"node": "leaf-p1-r0", "interface": "swp2s0", "attributes": {"role": "leaf", "plane": 1, "pod": 0, "su": 0, "rail_group": [0]}},
      {"node": "compute-b", "interface": "eth_p1_r0", "attributes": {"role": "host", "rail": 0, "pod": 0, "su": 0}}
    ],
    [
      {"node": "leaf-p1-r1", "interface": "swp1s0", "attributes": {"role": "leaf", "plane": 1, "pod": 0, "su": 0, "rail_group": [1]}},
      {"node": "compute-a", "interface": "eth_p1_r1", "attributes": {"role": "host", "rail": 1, "pod": 0, "su": 0}}
    ],
    [
      {"node": "leaf-p1-r1", "interface": "swp2s0", "attributes": {"role": "leaf", "plane": 1, "pod": 0, "su": 0, "rail_group": [1]}},
      {"node": "compute-b", "interface": "eth_p1_r1", "attributes": {"role": "host", "rail": 1, "pod": 0, "su": 0}}
    ]
  ]
}`)
	rail0 := 0
	rail1 := 1
	cfg := &config.LaunchKitConfig{
		Profile: &config.Profile{SpectrumX: &config.ProfileSpectrumX{
			Enable:         true,
			TopologyType:   config.SpectrumXTopology2Tier,
			IPVersion:      config.SpectrumXIPVersionIPv4,
			TopologyFile:   topologyPath,
			MultiplaneMode: "swplb",
			NumberOfPlanes: 2,
		}},
	}
	pools, err := BuildCIDRPools(cfg, config.ClusterConfig{
		MergedIdentifier: "gpu-model",
		WorkerNodes:      []string{"compute-a", "compute-b"},
		PFs: []config.PFConfig{
			{Traffic: "east-west", Rail: &rail0},
			{Traffic: "east-west", Rail: &rail0},
			{Traffic: "east-west", Rail: &rail1},
			{Traffic: "east-west", Rail: &rail1},
		},
	})
	require.NoError(t, err)
	require.Len(t, pools, 4)
	require.Equal(t, "rail-0-plane-0-gpu-model", pools[0].Name)
	require.Equal(t, "172.16.0.0/18", pools[0].CIDR)
	require.Equal(t, []string{"172.16.0.0/18", "172.16.0.0/14"}, pools[0].Routes)
	require.Equal(t, []StaticAllocation{
		{Gateway: "172.16.0.1", NodeName: "compute-a", Prefix: "172.16.0.0/31"},
		{Gateway: "172.16.0.3", NodeName: "compute-b", Prefix: "172.16.0.2/31"},
	}, pools[0].StaticAllocations)
	require.Equal(t, "rail-1-plane-1-gpu-model", pools[3].Name)
	require.Equal(t, "172.20.64.0/18", pools[3].CIDR)
}

func TestBuildCIDRPools3Tier(t *testing.T) {
	topologyPath := writeTopology(t, `{
  "nodes": [
    {"name": "compute-a", "role": "host", "type": "default"},
    {"name": "leaf-a", "role": "leaf", "type": "cumulus"}
  ],
  "links": [
    [
      {"node": "leaf-a", "interface": "swp1s0", "attributes": {"role": "leaf", "plane": 0, "pod": 2, "su": 3, "rail_group": [0]}},
      {"node": "compute-a", "interface": "eth_p0_r0", "attributes": {"role": "host", "rail": 0, "pod": 2, "su": 3}}
    ]
  ]
}`)
	rail := 0
	cfg := &config.LaunchKitConfig{
		Profile: &config.Profile{SpectrumX: &config.ProfileSpectrumX{
			Enable:         true,
			TopologyType:   config.SpectrumXTopology3Tier,
			IPVersion:      config.SpectrumXIPVersionIPv4,
			TopologyFile:   topologyPath,
			MultiplaneMode: "hwplb",
			NumberOfPlanes: 4,
		}},
	}
	pools, err := BuildCIDRPools(cfg, config.ClusterConfig{
		WorkerNodes: []string{"compute-a"},
		PFs:         []config.PFConfig{{Traffic: "east-west", Rail: &rail}},
	})
	require.NoError(t, err)
	require.Equal(t, []CIDRPool{{
		Name: "rail-0",
		CIDR: "10.0.0.0/13",
		Routes: []string{
			"10.0.0.0/13",
			"10.0.0.0/10",
		},
		StaticAllocations: []StaticAllocation{{
			Gateway:  "10.0.131.1",
			NodeName: "compute-a",
			Prefix:   "10.0.131.0/31",
		}},
	}}, pools)
}

func TestBuildCIDRPoolsRejectsIPv6UntilRenderable(t *testing.T) {
	cfg := &config.LaunchKitConfig{
		Profile: &config.Profile{SpectrumX: &config.ProfileSpectrumX{
			Enable:       true,
			TopologyType: config.SpectrumXTopology2Tier,
			IPVersion:    config.SpectrumXIPVersionIPv6,
			TopologyFile: "topology.json",
		}},
	}
	_, err := BuildCIDRPools(cfg, config.ClusterConfig{})
	require.ErrorContains(t, err, "currently supports ipVersion=ipv4 only")
}

func writeTopology(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "topology.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}
