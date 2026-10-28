// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestTraditionalSpectrumXManifestContracts(t *testing.T) {
	for _, platform := range []string{"h100", "h200", "b200", "gb200", "b300", "gb300", "rtx"} {
		for _, ra := range []string{"RA2.4", "RA3.1"} {
			for _, mode := range []struct {
				name   string
				planes int
			}{{"none", 1}, {"swplb", 2}, {"hwplb", 4}} {
				for _, dra := range []bool{false, true} {
					for _, ip := range []string{config.SpectrumXIPVersionIPv4, config.SpectrumXIPVersionIPv6} {
						key := fmt.Sprintf("%s/%s/%s/dra=%t/%s", platform, ra, mode.name, dra, ip)
						t.Run(key, func(t *testing.T) {
							assertTraditionalSpectrumXManifests(t, platform, ra, mode.name, mode.planes, dra, ip)
						})
					}
				}
			}
		}
	}
}

func assertTraditionalSpectrumXManifests(t *testing.T, platform, ra, mode string, planes int, dra bool, ip string) {
	t.Helper()
	cfg, p := dospcxConfigAndProfile(t, ra, mode, planes)
	cfg.Profile.SpectrumX.UseDRA = dra
	cfg.Profile.SpectrumX.IPVersion = ip
	for i := range cfg.ClusterConfig {
		cfg.ClusterConfig[i].GPUType = "NVIDIA-" + strings.ToUpper(platform)
		if platform == "rtx" {
			for j := range cfg.ClusterConfig[i].PFs {
				cfg.ClusterConfig[i].PFs[j].DeviceID = "1023"
			}
		}
	}
	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(p, cfg)
	if platform == "rtx" && mode != "none" {
		require.Error(t, err)
		require.ErrorContains(t, err, "requires ConnectX-8")
		return
	}
	require.NoError(t, err)
	claims, pools, railPools := 0, 0, 0
	for name, content := range rendered {
		b, e := bundle.FromFiles([]bundle.File{{Name: name, Content: content}})
		require.NoError(t, e)
		for _, doc := range b.Documents() {
			o := doc.ObjectCopy()
			switch o.GetKind() {
			case "NicConfigurationTemplate":
				require.Equal(t, platform, vrManifestField(t, o.Object, "spec", "template", "spectrumXOptimized", "platformType"))
			case "SpectrumXRailPoolConfig":
				railPools++
				rails := vrManifestField(t, o.Object, "spec", "railTopology").([]any)
				expectedCount := 8
				if mode == "swplb" {
					expectedCount *= planes
				}
				require.Len(t, rails, expectedCount)
				for i, item := range rails {
					rail := item.(map[string]any)
					name := fmt.Sprintf("rail%d", i)
					if mode == "swplb" {
						name = fmt.Sprintf("rail%dp%d", i/planes, i%planes)
					}
					require.Equal(t, name, rail["name"])
					pfCount := planes
					if mode == "swplb" {
						pfCount = 1
					}
					require.Len(t, vrManifestField(t, rail, "nicSelector", "pfNames"), pfCount)
				}
			case "CIDRPool":
				pools++
				require.NotContains(t, o.GetName(), "-nic-")
				prefix, err := netip.ParsePrefix(vrManifestField(t, o.Object, "spec", "cidr").(string))
				require.NoError(t, err)
				require.Equal(t, ip == config.SpectrumXIPVersionIPv6, prefix.Addr().Is6())
			case "ResourceClaimTemplate":
				claims++
				requests := vrManifestField(t, o.Object, "spec", "spec", "devices", "requests").([]any)
				require.Len(t, requests, 2)
				require.Equal(t, "gpu", requests[0].(map[string]any)["name"])
				vf := requests[1].(map[string]any)
				require.Equal(t, "vf", vf["name"])
				count := planes
				if mode == "swplb" {
					count = 1
				}
				require.EqualValues(t, count, vrManifestField(t, vf, "exactly", "count"))
			}
		}
	}
	require.Equal(t, 1, railPools)
	expectedPools := 8
	if mode == "swplb" {
		expectedPools *= planes
	}
	require.Equal(t, expectedPools, pools)
	if dra {
		require.Equal(t, expectedPools, claims)
	} else {
		require.Zero(t, claims)
	}
}
