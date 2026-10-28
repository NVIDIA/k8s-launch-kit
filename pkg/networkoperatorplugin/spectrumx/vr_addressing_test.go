// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package spectrumx

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
)

func vrTopologyForTest(t *testing.T) map[string]any {
	t.Helper()
	var links []any
	for node := 1; node <= 2; node++ {
		for rail := 0; rail < 4; rail++ {
			for plane := 0; plane < 8; plane++ {
				links = append(links, []any{
					map[string]any{"node": fmt.Sprintf("worker-%d", node), "interface": fmt.Sprintf("eth_r%d_p%d", rail, plane), "attributes": map[string]any{"role": "host", "rail": rail, "pod": 0, "su": 0, "host_index": node}},
					map[string]any{"node": "leaf", "interface": fmt.Sprintf("swp%d-%d-%d", node, rail, plane), "attributes": map[string]any{"role": "leaf", "plane": plane}},
				})
			}
		}
	}
	return map[string]any{"nodes": []any{}, "links": links}
}

func vrAddressConfig(t *testing.T, topology map[string]any) (*config.LaunchKitConfig, config.ClusterConfig) {
	t.Helper()
	raw, err := json.Marshal(topology)
	require.NoError(t, err)
	cfg := &config.LaunchKitConfig{Profile: &config.Profile{SpectrumX: &config.ProfileSpectrumX{
		Enable: true, SPCXVersion: "RA3.1", MultiplaneMode: "hwplb", NumberOfPlanes: 8,
		TopologyType: config.SpectrumXTopology2Tier, IPVersion: config.SpectrumXIPVersionIPv4,
		TopologyFile: writeTopology(t, string(raw)),
	}}}
	group := config.ClusterConfig{MachineType: "VR-NVL72", WorkerNodes: []string{"worker-1", "worker-2"}}
	return cfg, group
}

func TestVRPoolsAddressBothNICsPerRail(t *testing.T) {
	for _, tier := range []string{config.SpectrumXTopology2Tier, config.SpectrumXTopology3Tier} {
		t.Run(tier, func(t *testing.T) {
			cfg, group := vrAddressConfig(t, vrTopologyForTest(t))
			cfg.Profile.SpectrumX.TopologyType = tier
			pools, err := BuildCIDRPools(cfg, group)
			require.NoError(t, err)
			require.Len(t, pools, 8)
			for i, pool := range pools {
				rail, nic := i/2, i%2
				second := nic*128 + rail*32
				require.Equal(t, fmt.Sprintf("rail-%d-nic-%d", rail, nic), pool.Name)
				require.Equal(t, fmt.Sprintf("240.%d.0.0/11", second), pool.CIDR)
				require.Equal(t, []string{pool.CIDR, "240.0.0.0/8"}, pool.Routes)
				require.Equal(t, 31, pool.PerNodeNetworkPrefix)
				require.Equal(t, []PerNodeExclusion{{StartIndex: 1, EndIndex: 1}}, pool.PerNodeExclusions)
				require.Equal(t, []StaticAllocation{
					{NodeName: "worker-1", Prefix: fmt.Sprintf("240.%d.0.2/31", second), Gateway: fmt.Sprintf("240.%d.0.3", second)},
					{NodeName: "worker-2", Prefix: fmt.Sprintf("240.%d.0.4/31", second), Gateway: fmt.Sprintf("240.%d.0.5", second)},
				}, pool.StaticAllocations)
			}
		})
	}
}

func TestVRRejectsIncompleteOrAmbiguousTopology(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		want   string
	}{
		{"missing second NIC", func(top map[string]any) {
			links := top["links"].([]any)
			top["links"] = append(links[:4], links[8:]...)
		}, "requires all eight planes"},
		{"missing explicit host index", func(top map[string]any) {
			delete(top["links"].([]any)[0].([]any)[0].(map[string]any)["attributes"].(map[string]any), "host_index")
		}, "requires explicit attributes.host_index"},
		{"duplicate switch port", func(top map[string]any) {
			links := top["links"].([]any)
			links[1].([]any)[1].(map[string]any)["interface"] = links[0].([]any)[1].(map[string]any)["interface"]
		}, "repeats switch port"},
		{"inconsistent host index", func(top map[string]any) {
			top["links"].([]any)[1].([]any)[0].(map[string]any)["attributes"].(map[string]any)["host_index"] = 7
		}, "inconsistent host_index"},
		{"duplicate host coordinates", func(top map[string]any) {
			for _, raw := range top["links"].([]any) {
				raw.([]any)[0].(map[string]any)["attributes"].(map[string]any)["host_index"] = 1
			}
		}, "share pod, SU and host index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			top := vrTopologyForTest(t)
			tc.change(top)
			cfg, group := vrAddressConfig(t, top)
			_, err := BuildCIDRPools(cfg, group)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestVRAddressBounds(t *testing.T) {
	spcx := &config.ProfileSpectrumX{MultiplaneMode: "hwplb", NumberOfPlanes: 8, TopologyType: config.SpectrumXTopology3Tier, IPVersion: "ipv4", HostFirstOctet: 255}
	host, leaf, err := allocateVRHostLeaf(spcx, 7, 3, 0, 127, 251)
	require.NoError(t, err)
	require.Equal(t, "255.224.255.246", host.String())
	require.Equal(t, "255.224.255.247", leaf.String())
	for _, fields := range [][5]int{{8, 0, 0, 0, 0}, {0, 4, 0, 0, 0}, {0, 0, 1, 0, 0}, {0, 0, 0, 128, 0}, {0, 0, 0, 0, 252}, {-1, 0, 0, 0, 0}} {
		_, _, err := allocateVRHostLeaf(spcx, fields[0], fields[1], fields[2], fields[3], fields[4])
		require.Error(t, err)
	}
	spcx.HostFirstOctet = 172
	_, _, err = allocateVRHostLeaf(spcx, 0, 0, 0, 0, 1)
	require.ErrorContains(t, err, "hostFirstOctet")
	spcx.HostFirstOctet = 240
	spcx.IPVersion = "ipv6"
	_, _, err = allocateVRHostLeaf(spcx, 0, 0, 0, 0, 1)
	require.ErrorContains(t, err, "IPv4 only")
}
