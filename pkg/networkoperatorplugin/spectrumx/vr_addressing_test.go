// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package spectrumx

import (
	"encoding/json"
	"fmt"
	"net/netip"
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
	spcx.IPVersion = "unsupported"
	_, _, err = allocateVRHostLeaf(spcx, 0, 0, 0, 0, 1)
	require.ErrorContains(t, err, "unsupported VR IP version")
}

func TestVRIPv6PoolsKeepNICsDistinct(t *testing.T) {
	for _, tier := range []string{config.SpectrumXTopology2Tier, config.SpectrumXTopology3Tier} {
		t.Run(tier, func(t *testing.T) {
			cfg, group := vrAddressConfig(t, vrTopologyForTest(t))
			cfg.Profile.SpectrumX.TopologyType = tier
			cfg.Profile.SpectrumX.IPVersion = config.SpectrumXIPVersionIPv6
			pools, err := BuildCIDRPools(cfg, group)
			require.NoError(t, err)
			prefixes := []string{"fd02::/40", "fd02:0:1000::/40", "fd02:0:100::/40", "fd02:0:1100::/40", "fd02:0:200::/40", "fd02:0:1200::/40", "fd02:0:300::/40", "fd02:0:1300::/40"}
			require.Len(t, pools, len(prefixes))
			for i, pool := range pools {
				require.Equal(t, prefixes[i], pool.CIDR)
				require.Equal(t, fmt.Sprintf("rail-%d-nic-%d", i/2, i%2), pool.Name)
				require.Equal(t, []string{"fd02::/28"}, pool.Routes)
				require.Equal(t, 64, pool.PerNodeNetworkPrefix)
				require.Equal(t, 2, pool.GatewayIndex)
				require.Equal(t, []PerNodeExclusion{{StartIndex: 2, EndIndex: 2}}, pool.PerNodeExclusions)
				require.Len(t, pool.StaticAllocations, 2)
				for n, allocation := range pool.StaticAllocations {
					prefix := netip.MustParsePrefix(prefixes[i]).Addr().As16()
					prefix[7] = byte(n + 1)
					require.Equal(t, netip.PrefixFrom(netip.AddrFrom16(prefix), 64).String(), allocation.Prefix)
					prefix[15] = 2
					require.Equal(t, netip.AddrFrom16(prefix).String(), allocation.Gateway)
					require.Equal(t, fmt.Sprintf("worker-%d", n+1), allocation.NodeName)
				}
			}
		})
	}
}

func TestVRIPv6AddressCoordinatesAndBounds(t *testing.T) {
	spcx := &config.ProfileSpectrumX{MultiplaneMode: "hwplb", NumberOfPlanes: 8, TopologyType: config.SpectrumXTopology2Tier, IPVersion: config.SpectrumXIPVersionIPv6, IPv6SiteID: 15, HostFirstOctet: 172}
	for _, plane := range []int{4, 5, 6, 7} {
		host, leaf, err := allocateVRHostLeaf(spcx, plane, 3, 0, 127, 251)
		require.NoError(t, err)
		require.Equal(t, "fd02:f:1300:7ffb::1", host.String())
		require.Equal(t, "fd02:f:1300:7ffb::2", leaf.String())
	}
	for _, site := range []int{-1, 16} {
		spcx.IPv6SiteID = site
		_, _, err := allocateVRHostLeaf(spcx, 0, 0, 0, 0, 0)
		require.ErrorContains(t, err, "ipv6SiteID")
	}
	spcx.IPv6SiteID = 0
	for _, coordinates := range [][5]int{{8, 0, 0, 0, 0}, {0, 4, 0, 0, 0}, {0, 0, 1, 0, 0}, {0, 0, 0, 128, 0}, {0, 0, 0, 0, 252}, {0, 0, 0, 0, -1}} {
		_, _, err := allocateVRHostLeaf(spcx, coordinates[0], coordinates[1], coordinates[2], coordinates[3], coordinates[4])
		require.Error(t, err)
	}
	host, leaf, err := allocateVRHostLeaf(spcx, 0, 0, 0, 0, 0)
	require.NoError(t, err)
	require.Equal(t, "fd02::1", host.String())
	require.Equal(t, "fd02::2", leaf.String())
}

func TestIPv6SiteIDCannotSilentlyAffectOtherLayouts(t *testing.T) {
	cfg, group := vrAddressConfig(t, vrTopologyForTest(t))
	cfg.Profile.SpectrumX.IPv6SiteID = 1
	_, err := BuildCIDRPools(cfg, group)
	require.ErrorContains(t, err, "only for VR IPv6")
	cfg.Profile.SpectrumX.IPVersion = config.SpectrumXIPVersionIPv6
	group.MachineType, group.GPUType = "machine-a", "NVIDIA-GB300"
	_, err = BuildCIDRPools(cfg, group)
	require.ErrorContains(t, err, "only for VR IPv6")
}
