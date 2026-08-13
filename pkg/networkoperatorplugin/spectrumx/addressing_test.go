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
	"fmt"
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
	require.Equal(t, 1, pools[0].GatewayIndex)
	require.Equal(t, 31, pools[0].PerNodeNetworkPrefix)
	require.Equal(t, []PerNodeExclusion{{StartIndex: 1, EndIndex: 1}}, pools[0].PerNodeExclusions)
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
		Name:                 "rail-0",
		CIDR:                 "10.0.0.0/13",
		GatewayIndex:         1,
		PerNodeNetworkPrefix: 31,
		PerNodeExclusions:    []PerNodeExclusion{{StartIndex: 1, EndIndex: 1}},
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

func TestBuildCIDRPoolsFromAIR2Tier(t *testing.T) {
	topologyPath := filepath.Join("testdata", "air-simple-quadplane.json")
	rail := 0
	tests := []struct {
		name          string
		mode          string
		wantPools     int
		wantFirstName string
		wantLastName  string
		wantLastCIDR  string
	}{
		{
			name:          "software plane load balancing",
			mode:          "swplb",
			wantPools:     4,
			wantFirstName: "rail-0-plane-0",
			wantLastName:  "rail-0-plane-3",
			wantLastCIDR:  "172.28.0.0/18",
		},
		{
			name:          "hardware plane load balancing",
			mode:          "hwplb",
			wantPools:     1,
			wantFirstName: "rail-0",
			wantLastName:  "rail-0",
			wantLastCIDR:  "172.16.0.0/18",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.LaunchKitConfig{
				Profile: &config.Profile{SpectrumX: &config.ProfileSpectrumX{
					Enable:         true,
					TopologyType:   config.SpectrumXTopology2Tier,
					IPVersion:      config.SpectrumXIPVersionIPv4,
					TopologyFile:   topologyPath,
					MultiplaneMode: tt.mode,
					NumberOfPlanes: 4,
				}},
			}
			pools, err := BuildCIDRPools(cfg, config.ClusterConfig{
				WorkerNodes: []string{"worker-su01-rack01-h01", "worker-su01-rack01-h02"},
				PFs:         []config.PFConfig{{Traffic: "east-west", Rail: &rail}},
			})
			require.NoError(t, err)
			require.Len(t, pools, tt.wantPools)
			require.Equal(t, tt.wantFirstName, pools[0].Name)
			require.Equal(t, []StaticAllocation{
				{Gateway: "172.16.0.1", NodeName: "worker-su01-rack01-h01", Prefix: "172.16.0.0/31"},
				{Gateway: "172.16.0.3", NodeName: "worker-su01-rack01-h02", Prefix: "172.16.0.2/31"},
			}, pools[0].StaticAllocations)
			require.Equal(t, tt.wantLastName, pools[len(pools)-1].Name)
			require.Equal(t, tt.wantLastCIDR, pools[len(pools)-1].CIDR)
		})
	}
}

func TestBuildCIDRPoolsFromAIR3Tier(t *testing.T) {
	topologyPath := writeTopology(t, `{
  "content": {
    "nodes": {
      "worker-pod02-su03-h04": {"model": "host", "network_pci": {"rail2": {}}},
      "leaf-p1-pod02-su03-r2": {"model": "SN5600", "network_pci": {}}
    },
    "links": [[
      {"node": "worker-pod02-su03-h04", "interface": "rail2p1", "network_pci": "rail2"},
      {"node": "leaf-p1-pod02-su03-r2", "interface": "swp1s0"}
    ]]
  }
}`)
	rail := 1
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
		WorkerNodes: []string{"worker-pod02-su03-h04"},
		PFs:         []config.PFConfig{{Traffic: "east-west", Rail: &rail}},
	})
	require.NoError(t, err)
	require.Equal(t, []CIDRPool{{
		Name:                 "rail-1",
		CIDR:                 "10.8.0.0/13",
		GatewayIndex:         1,
		PerNodeNetworkPrefix: 31,
		PerNodeExclusions:    []PerNodeExclusion{{StartIndex: 1, EndIndex: 1}},
		Routes:               []string{"10.8.0.0/13", "10.0.0.0/10"},
		StaticAllocations: []StaticAllocation{{
			Gateway:  "10.8.66.7",
			NodeName: "worker-pod02-su03-h04",
			Prefix:   "10.8.66.6/31",
		}},
	}}, pools)
}

func TestBuildCIDRPoolsIPv6TwoTierSWPLB(t *testing.T) {
	topologyPath := writeTopology(t, `{
  "nodes": [
    {"name": "compute-a", "role": "host", "type": "default"},
    {"name": "compute-b", "role": "host", "type": "default"},
    {"name": "leaf-a", "role": "leaf", "type": "cumulus"}
  ],
  "links": [
    [
      {"node": "leaf-a", "interface": "swp1s0", "attributes": {"role": "leaf", "plane": 1}},
      {"node": "compute-a", "interface": "eth_p1_r1", "attributes": {"role": "host", "rail": 1, "pod": 0, "su": 3}}
    ],
    [
      {"node": "leaf-a", "interface": "swp2s0", "attributes": {"role": "leaf", "plane": 1}},
      {"node": "compute-b", "interface": "eth_p1_r1", "attributes": {"role": "host", "rail": 1, "pod": 0, "su": 3}}
    ]
  ]
}`)
	cfg := &config.LaunchKitConfig{
		Profile: &config.Profile{SpectrumX: &config.ProfileSpectrumX{
			Enable:         true,
			TopologyType:   config.SpectrumXTopology2Tier,
			IPVersion:      config.SpectrumXIPVersionIPv6,
			TopologyFile:   topologyPath,
			MultiplaneMode: "swplb",
			NumberOfPlanes: 2,
		}},
	}
	pools, err := BuildCIDRPools(cfg, config.ClusterConfig{WorkerNodes: []string{"compute-a", "compute-b"}})
	require.NoError(t, err)
	require.Equal(t, []CIDRPool{{
		Name:                 "rail-1-plane-1",
		CIDR:                 "fd02:1:100::/40",
		GatewayIndex:         2,
		PerNodeNetworkPrefix: 64,
		PerNodeExclusions:    []PerNodeExclusion{{StartIndex: 2, EndIndex: 2}},
		Routes:               []string{"fd02::/24"},
		StaticAllocations: []StaticAllocation{
			{Gateway: "fd02:1:100:300::2", NodeName: "compute-a", Prefix: "fd02:1:100:300::/64"},
			{Gateway: "fd02:1:100:301::2", NodeName: "compute-b", Prefix: "fd02:1:100:301::/64"},
		},
	}}, pools)
}

func TestBuildCIDRPoolsIPv6ThreeTierHWPLB(t *testing.T) {
	topologyPath := writeTopology(t, `{
  "nodes": [
    {"name": "compute-a", "role": "host", "type": "default"},
    {"name": "leaf-a", "role": "leaf", "type": "cumulus"}
  ],
  "links": [[
    {"node": "leaf-a", "interface": "swp1s0", "attributes": {"role": "leaf", "plane": 3}},
    {"node": "compute-a", "interface": "eth_p3_r2", "attributes": {"role": "host", "rail": 2, "pod": 4, "su": 5}}
  ]]
}`)
	cfg := &config.LaunchKitConfig{
		Profile: &config.Profile{SpectrumX: &config.ProfileSpectrumX{
			Enable:         true,
			TopologyType:   config.SpectrumXTopology3Tier,
			IPVersion:      config.SpectrumXIPVersionIPv6,
			TopologyFile:   topologyPath,
			MultiplaneMode: "hwplb",
			NumberOfPlanes: 4,
		}},
	}
	pools, err := BuildCIDRPools(cfg, config.ClusterConfig{WorkerNodes: []string{"compute-a"}})
	require.NoError(t, err)
	require.Equal(t, []CIDRPool{{
		Name:                 "rail-2",
		CIDR:                 "fd02:0:200::/40",
		GatewayIndex:         2,
		PerNodeNetworkPrefix: 64,
		PerNodeExclusions:    []PerNodeExclusion{{StartIndex: 2, EndIndex: 2}},
		Routes:               []string{"fd02::/24"},
		StaticAllocations: []StaticAllocation{{
			Gateway:  "fd02:0:204:500::2",
			NodeName: "compute-a",
			Prefix:   "fd02:0:204:500::/64",
		}},
	}}, pools)
}

func TestBuildCIDRPoolsIPv6SinglePlaneRoute(t *testing.T) {
	topologyPath := writeSingleLinkTopology(t, "compute-a")
	cfg := spectrumXTestConfig(topologyPath, "none")
	cfg.Profile.SpectrumX.IPVersion = config.SpectrumXIPVersionIPv6
	cfg.Profile.SpectrumX.NumberOfPlanes = 1

	pools, err := BuildCIDRPools(cfg, config.ClusterConfig{WorkerNodes: []string{"compute-a"}})
	require.NoError(t, err)
	require.Len(t, pools, 1)
	require.Equal(t, "fd02::/40", pools[0].CIDR)
	require.Equal(t, []string{"fd02::/32"}, pools[0].Routes)
}

func TestBuildCIDRPoolsFromAIRIPv6(t *testing.T) {
	tests := []struct {
		name         string
		mode         string
		wantPools    int
		wantLastCIDR string
	}{
		{name: "software plane load balancing", mode: "swplb", wantPools: 4, wantLastCIDR: "fd02:3::/40"},
		{name: "hardware plane load balancing", mode: "hwplb", wantPools: 1, wantLastCIDR: "fd02::/40"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.LaunchKitConfig{
				Profile: &config.Profile{SpectrumX: &config.ProfileSpectrumX{
					Enable:         true,
					TopologyType:   config.SpectrumXTopology2Tier,
					IPVersion:      config.SpectrumXIPVersionIPv6,
					TopologyFile:   filepath.Join("testdata", "air-simple-quadplane.json"),
					MultiplaneMode: tt.mode,
					NumberOfPlanes: 4,
				}},
			}
			pools, err := BuildCIDRPools(cfg, config.ClusterConfig{
				WorkerNodes: []string{"worker-su01-rack01-h01", "worker-su01-rack01-h02"},
			})
			require.NoError(t, err)
			require.Len(t, pools, tt.wantPools)
			require.Equal(t, "fd02::/40", pools[0].CIDR)
			require.Equal(t, []string{"fd02::/24"}, pools[0].Routes)
			require.Equal(t, []StaticAllocation{
				{Gateway: "fd02::2", NodeName: "worker-su01-rack01-h01", Prefix: "fd02::/64"},
				{Gateway: "fd02:0:0:1::2", NodeName: "worker-su01-rack01-h02", Prefix: "fd02:0:0:1::/64"},
			}, pools[0].StaticAllocations)
			require.Equal(t, tt.wantLastCIDR, pools[len(pools)-1].CIDR)
		})
	}
}

func TestAllocateIPv6HostLeafRejectsInvalidFields(t *testing.T) {
	tests := []struct {
		name         string
		topologyType string
		plane        int
		rail         int
		pod          int
		su           int
		hostIndex    int
		wantError    string
	}{
		{name: "negative plane", topologyType: config.SpectrumXTopology3Tier, plane: -1, wantError: "plane -1 exceeds"},
		{name: "rail overflow", topologyType: config.SpectrumXTopology3Tier, rail: 256, wantError: "rail 256 exceeds"},
		{name: "pod overflow", topologyType: config.SpectrumXTopology3Tier, pod: 256, wantError: "pod 256 exceeds"},
		{name: "su overflow", topologyType: config.SpectrumXTopology3Tier, su: 256, wantError: "su 256 exceeds"},
		{name: "host overflow", topologyType: config.SpectrumXTopology3Tier, hostIndex: 256, wantError: "host index 256 exceeds"},
		{name: "two-tier pod", topologyType: config.SpectrumXTopology2Tier, pod: 1, wantError: "pod 1 must be zero"},
		{name: "unsupported topology", topologyType: "fabric", wantError: `unsupported Spectrum-X topologyType "fabric"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spcx := &config.ProfileSpectrumX{TopologyType: tt.topologyType, IPVersion: config.SpectrumXIPVersionIPv6}
			_, _, err := allocateIPv6HostLeaf(spcx, tt.plane, tt.rail, tt.pod, tt.su, tt.hostIndex)
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestBuildCIDRPoolsReportsWrongTopologyFile(t *testing.T) {
	topologyPath := writeSingleLinkTopology(t, "stale-worker")
	cfg := spectrumXTestConfig(topologyPath, "hwplb")

	_, err := BuildCIDRPools(cfg, config.ClusterConfig{
		WorkerNodes: []string{"compute-b", "compute-a"},
	})

	require.ErrorContains(t, err, "no Spectrum-X topology allocations matched unnamed clusterConfig group")
	require.ErrorContains(t, err, "selected workers=[compute-a, compute-b]")
	require.ErrorContains(t, err, "topology host nodes=[stale-worker]")
	require.ErrorContains(t, err, "exact matches=[]")
	require.ErrorContains(t, err, "missing workers=[compute-a, compute-b]")
	require.ErrorContains(t, err, "topology-only hosts=[stale-worker]")
	require.ErrorContains(t, err, "topology may describe a different cluster")
	require.ErrorContains(t, err, "must exactly match clusterConfig.workerNodes")
}

func TestBuildCIDRPoolsReportsLikelyNodeNameMismatch(t *testing.T) {
	tests := []struct {
		name         string
		topologyNode string
		workerNode   string
		wantHint     string
	}{
		{
			name:         "case mismatch",
			topologyNode: "compute-a",
			workerNode:   "Compute-A",
			wantHint:     `possible case mismatch between selected worker "Compute-A" and topology host "compute-a"`,
		},
		{
			name:         "short name and FQDN mismatch",
			topologyNode: "compute-a.example.test",
			workerNode:   "compute-a",
			wantHint:     `possible short-name/FQDN mismatch between selected worker "compute-a" and topology host "compute-a.example.test"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			topologyPath := writeSingleLinkTopology(t, tt.topologyNode)
			cfg := spectrumXTestConfig(topologyPath, "hwplb")

			_, err := BuildCIDRPools(cfg, config.ClusterConfig{WorkerNodes: []string{tt.workerNode}})

			require.ErrorContains(t, err, tt.wantHint)
			require.ErrorContains(t, err, "must exactly match clusterConfig.workerNodes")
		})
	}
}

func TestBuildCIDRPoolsReportsMissingRailPlaneCoverage(t *testing.T) {
	topologyPath := writeTopology(t, `{
  "nodes": [
    {"name": "compute-a", "role": "host", "type": "default"},
    {"name": "compute-b", "role": "host", "type": "default"},
    {"name": "leaf-a", "role": "leaf", "type": "cumulus"}
  ],
  "links": [
    [
      {"node": "leaf-a", "interface": "swp1s0", "attributes": {"role": "leaf", "plane": 0, "pod": 0, "su": 0}},
      {"node": "compute-a", "interface": "eth_p0_r0", "attributes": {"role": "host", "rail": 0, "pod": 0, "su": 0}}
    ],
    [
      {"node": "leaf-a", "interface": "swp2s0", "attributes": {"role": "leaf", "plane": 0, "pod": 0, "su": 0}},
      {"node": "compute-b", "interface": "eth_p0_r0", "attributes": {"role": "host", "rail": 0, "pod": 0, "su": 0}}
    ],
    [
      {"node": "leaf-a", "interface": "swp1s1", "attributes": {"role": "leaf", "plane": 1, "pod": 0, "su": 0}},
      {"node": "compute-a", "interface": "eth_p1_r0", "attributes": {"role": "host", "rail": 0, "pod": 0, "su": 0}}
    ]
  ]
}`)
	cfg := spectrumXTestConfig(topologyPath, "swplb")

	_, err := BuildCIDRPools(cfg, config.ClusterConfig{
		Identifier:  "gpu-workers",
		WorkerNodes: []string{"compute-a", "compute-b"},
	})

	require.ErrorContains(t, err, `CIDRPool rail-0-plane-1 for clusterConfig group "gpu-workers"`)
	require.ErrorContains(t, err, "missing topology allocations for workers [compute-b]")
	require.ErrorContains(t, err, "available topology coverage for missing workers [compute-b=[rail-0-plane-0]]")
	require.ErrorContains(t, err, "check host attributes.rail and leaf attributes.plane")
}

func TestBuildCIDRPoolsAIRDiagnosticsUseNormalizedHostNames(t *testing.T) {
	cfg := spectrumXTestConfig(filepath.Join("testdata", "air-simple-quadplane.json"), "swplb")

	_, err := BuildCIDRPools(cfg, config.ClusterConfig{WorkerNodes: []string{"unrelated-worker"}})

	require.ErrorContains(t, err, "topology host nodes=[worker-su01-rack01-h01, worker-su01-rack01-h02]")
	require.ErrorContains(t, err, "topology-only hosts=[worker-su01-rack01-h01, worker-su01-rack01-h02]")
}

func TestFormatLimitedList(t *testing.T) {
	values := []string{"node-09", "node-03", "node-01", "node-07", "node-05", "node-10", "node-08", "node-02", "node-06", "node-04"}

	require.Equal(t,
		"[node-01, node-02, node-03, node-04, node-05, node-06, node-07, node-08] (+2 more)",
		formatLimitedList(values))
}

func spectrumXTestConfig(topologyPath, multiplaneMode string) *config.LaunchKitConfig {
	return &config.LaunchKitConfig{
		Profile: &config.Profile{SpectrumX: &config.ProfileSpectrumX{
			Enable:         true,
			TopologyType:   config.SpectrumXTopology2Tier,
			IPVersion:      config.SpectrumXIPVersionIPv4,
			TopologyFile:   topologyPath,
			MultiplaneMode: multiplaneMode,
			NumberOfPlanes: 2,
		}},
	}
}

func writeSingleLinkTopology(t *testing.T, hostNode string) string {
	t.Helper()
	return writeTopology(t, fmt.Sprintf(`{
  "nodes": [
    {"name": %q, "role": "host", "type": "default"},
    {"name": "leaf-a", "role": "leaf", "type": "cumulus"}
  ],
  "links": [[
    {"node": "leaf-a", "interface": "swp1s0", "attributes": {"role": "leaf", "plane": 0, "pod": 0, "su": 0}},
    {"node": %q, "interface": "eth_p0_r0", "attributes": {"role": "host", "rail": 0, "pod": 0, "su": 0}}
  ]]
}`, hostNode, hostNode))
}

func writeTopology(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "topology.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}
