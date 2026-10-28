// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

// spectrumXEndpoint describes one workload attachment and XPlane group. On VR,
// each logical rail has two independently addressed four-PF NIC endpoints.
type spectrumXEndpoint struct {
	Name             string
	PoolName         string
	ClaimName        string
	ClaimSuffixLimit int
	PFNames          []string
	PF               config.PFConfig
	HardwarePLB      bool
	SwPlane          int
	VFCount          int
	VFRequestName    string
}

func spectrumXEndpoints(profile *config.Profile, settings *config.SpectrumXConfig, group *config.ClusterConfig) []spectrumXEndpoint {
	spcx := profile.SpectrumX
	planes := spcx.NumberOfPlanes
	_, prefix := config.SpectrumXInterfaceNamePrefixes(settings, spcx.MultiplaneMode)
	var endpoints []spectrumXEndpoint
	vr := config.SpectrumXPlatformForGroup(*group) == "vr"
	masters := railPciGroups(group.PFs, planes)
	for rail := 0; rail < railCount(group.PFs, planes); rail++ {
		count := 1
		if spcx.MultiplaneMode == "swplb" {
			count = planes
		} else if vr {
			count = 2
		}
		for index := 0; index < count; index++ {
			endpoint := spectrumXEndpoint{
				Name: fmt.Sprintf("rail%d", rail), PoolName: fmt.Sprintf("rail-%d", rail),
				ClaimName: fmt.Sprintf("rail-%d", rail), ClaimSuffixLimit: 38,
				HardwarePLB: spcx.MultiplaneMode == "hwplb", SwPlane: spectrumXSwPlane(group, rail), VFCount: planes, VFRequestName: "vf",
			}
			start, end := 0, planes
			switch {
			case spcx.MultiplaneMode == "swplb":
				endpoint.Name += fmt.Sprintf("p%d", index)
				endpoint.PoolName += fmt.Sprintf("-plane-%d", index)
				endpoint.ClaimName += fmt.Sprintf("-plane-%d", index)
				endpoint.ClaimSuffixLimit, endpoint.VFCount = 32, 1
				endpoint.PF = spectrumXPFForRailPlane(group.PFs, rail, index, planes)
				start, end = index, index+1
			case vr:
				endpoint.Name += fmt.Sprintf("nic%d", index)
				endpoint.PoolName += fmt.Sprintf("-nic-%d", index)
				endpoint.VFRequestName = fmt.Sprintf("vf%d", index)
				endpoint.ClaimSuffixLimit, endpoint.VFCount = 32, 1
				endpoint.SwPlane = 2*endpoint.SwPlane + index
				start, end = 4*index, 4*(index+1)
				for _, pf := range group.PFs {
					if pf.PciAddress == masters[rail][index] {
						endpoint.PF = pf
						break
					}
				}
			default:
				endpoint.PF = spectrumXPFForRailPlane(group.PFs, rail, 0, planes)
			}
			for plane := start; plane < end; plane++ {
				nicID := rail
				if vr {
					nicID = index
				}
				name := applyPrefix(prefix, nicID, plane, rail)
				endpoint.PFNames = append(endpoint.PFNames, name)
			}
			endpoints = append(endpoints, endpoint)
		}
	}
	return endpoints
}

// Each VR logical rail shares one GPU claim between its two NIC endpoints.
// Other platforms retain one claim per endpoint.
type spectrumXClaim struct {
	Name        string
	SuffixLimit int
	GPUPCI      string
	Endpoints   []spectrumXEndpoint
}

func spectrumXClaims(endpoints []spectrumXEndpoint) []spectrumXClaim {
	var claims []spectrumXClaim
	indexes := map[string]int{}
	for _, endpoint := range endpoints {
		index, found := indexes[endpoint.ClaimName]
		if !found {
			index = len(claims)
			indexes[endpoint.ClaimName] = index
			claims = append(claims, spectrumXClaim{Name: endpoint.ClaimName, SuffixLimit: endpoint.ClaimSuffixLimit, GPUPCI: endpoint.PF.ConnectedGPUPCIAddress})
		}
		claims[index].Endpoints = append(claims[index].Endpoints, endpoint)
	}
	return claims
}

func spectrumXRailPrefix(prefix string, rail int) string {
	prefix = strings.ReplaceAll(prefix, "%rail_id%", fmt.Sprint(rail))
	return strings.ReplaceAll(prefix, "%rail%", fmt.Sprint(rail))
}

func spectrumXVRRdmaPrefix(settings *config.SpectrumXConfig, rail int) (string, error) {
	rdma, _ := config.SpectrumXInterfaceNamePrefixes(settings, "hwplb")
	if rdma == "roce_r%rail_id%" {
		rdma += "_n%nic_id%"
	}
	if !strings.Contains(rdma, "%nic_id%") || strings.Contains(rdma, "%plane_id%") || strings.Contains(rdma, "%plane%") {
		return "", fmt.Errorf("VR HWPLB RDMA naming requires a %%nic_id%% placeholder and no plane placeholder; got %q", rdma)
	}
	return spectrumXRailPrefix(rdma, rail), nil
}
