// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package spectrumx

import (
	"fmt"
	"net/netip"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

// VR uses one /31 per NIC, not one per logical rail. The IPv4 fields are:
// site octet | NIC(1), rail(2), pod(5) | SU(7), host(8), peer(1).
func allocateVRHostLeaf(spcx *config.ProfileSpectrumX, plane, rail, pod, su, host int) (netip.Addr, netip.Addr, error) {
	if spcx.MultiplaneMode != "hwplb" || spcx.NumberOfPlanes != 8 {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("VR addressing requires eight-plane HWPLB")
	}
	if spcx.IPVersion != "" && spcx.IPVersion != config.SpectrumXIPVersionIPv4 {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("VR addressing currently supports IPv4 only")
	}
	for _, field := range []struct {
		name           string
		value, maximum int
	}{
		{"plane", plane, 7}, {"rail", rail, 3}, {"pod", pod, 0}, {"su", su, 127}, {"host index", host, 251},
	} {
		if field.value < 0 || field.value > field.maximum {
			return netip.Addr{}, netip.Addr{}, fmt.Errorf("VR %s %d must be between 0 and %d", field.name, field.value, field.maximum)
		}
	}

	if spcx.TopologyType != config.SpectrumXTopology2Tier && spcx.TopologyType != config.SpectrumXTopology3Tier {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("unsupported VR topologyType %q", spcx.TopologyType)
	}
	first := spcx.HostFirstOctet
	if first == 0 {
		first = 240
	}
	if first < 240 || first > 255 {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("VR hostFirstOctet %d must be 240..255 (240 plus site ID)", first)
	}
	nic := plane / 4
	octets := [4]byte{byte(first), byte(nic<<7 | rail<<5 | pod), byte(su<<1 | host>>7), byte((host & 127) << 1)}
	hostIP := netip.AddrFrom4(octets)
	octets[3] |= 1
	return hostIP, netip.AddrFrom4(octets), nil
}

// Every selected worker needs all eight planes of each logical rail. Otherwise
// a stale topology could silently omit one NIC's pool or assign mixed host IDs.
func validateVRTopology(links []hostLink, allowedNodes map[string]struct{}) error {
	type coordinate struct{ pod, su, host int }
	coordinates := map[string]coordinate{}
	owners := map[coordinate]string{}
	planes := map[string]map[int]map[int]bool{}
	for _, link := range links {
		if len(allowedNodes) > 0 {
			if _, ok := allowedNodes[link.node]; !ok {
				continue
			}
		}
		c := coordinate{link.pod, link.su, link.hostIndex}
		if previous, ok := coordinates[link.node]; ok && previous != c {
			return fmt.Errorf("VR topology host %s has inconsistent pod, SU or host index across its NICs", link.node)
		}
		if owner, ok := owners[c]; ok && owner != link.node {
			return fmt.Errorf("VR topology hosts %s and %s share pod, SU and host index", owner, link.node)
		}
		owners[c] = link.node
		coordinates[link.node] = c
		if planes[link.node] == nil {
			planes[link.node] = map[int]map[int]bool{}
		}
		if planes[link.node][link.rail] == nil {
			planes[link.node][link.rail] = map[int]bool{}
		}
		if planes[link.node][link.rail][link.plane] {
			return fmt.Errorf("VR topology duplicates host %s rail %d plane %d", link.node, link.rail, link.plane)
		}
		planes[link.node][link.rail][link.plane] = true
	}
	for node, rails := range planes {
		for rail := 0; rail < 4; rail++ {
			if len(rails[rail]) != 8 {
				return fmt.Errorf("VR topology host %s rail %d requires all eight planes for its two NIC endpoints, found %d", node, rail, len(rails[rail]))
			}
		}
	}
	return nil
}
