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
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

type CIDRPool struct {
	Name              string
	CIDR              string
	Routes            []string
	StaticAllocations []StaticAllocation
}

type StaticAllocation struct {
	Gateway  string
	NodeName string
	Prefix   string
}

type topologyFile struct {
	Nodes []topologyNode `json:"nodes"`
	Links []topologyLink `json:"links"`
}

type topologyNode struct {
	Name string `json:"name"`
	Role string `json:"role"`
	Type string `json:"type"`
}

type topologyLink []topologyEndpoint

func (l *topologyLink) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	endpoints := make([]topologyEndpoint, 0, len(raw))
	for _, item := range raw {
		var disconnected string
		if err := json.Unmarshal(item, &disconnected); err == nil && disconnected == "unconnected" {
			continue
		}
		var endpoint topologyEndpoint
		if err := json.Unmarshal(item, &endpoint); err != nil {
			return err
		}
		endpoints = append(endpoints, endpoint)
	}
	*l = endpoints
	return nil
}

type topologyEndpoint struct {
	Node      string             `json:"node"`
	Interface string             `json:"interface"`
	Attrs     topologyAttributes `json:"attributes"`
}

type topologyAttributes struct {
	Role     string `json:"role"`
	Plane    int    `json:"plane"`
	Pod      int    `json:"pod"`
	SU       int    `json:"su"`
	Rail     int    `json:"rail"`
	HasPlane bool
	HasPod   bool
	HasSU    bool
	HasRail  bool
}

func (a *topologyAttributes) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if v, ok := raw["role"]; ok {
		if err := json.Unmarshal(v, &a.Role); err != nil {
			return fmt.Errorf("role: %w", err)
		}
	}
	if err := decodeOptionalInt(raw, "plane", &a.Plane, &a.HasPlane); err != nil {
		return err
	}
	if err := decodeOptionalInt(raw, "pod", &a.Pod, &a.HasPod); err != nil {
		return err
	}
	if err := decodeOptionalInt(raw, "su", &a.SU, &a.HasSU); err != nil {
		return err
	}
	if err := decodeOptionalInt(raw, "rail", &a.Rail, &a.HasRail); err != nil {
		return err
	}
	return nil
}

func decodeOptionalInt(raw map[string]json.RawMessage, key string, out *int, present *bool) error {
	v, ok := raw[key]
	if !ok || string(v) == "null" {
		return nil
	}
	if err := json.Unmarshal(v, out); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	*present = true
	return nil
}

type hostLink struct {
	node      string
	plane     int
	pod       int
	su        int
	rail      int
	hostIndex int
	hostIP    net.IP
	leafIP    net.IP
}

type poolKey struct {
	rail  int
	plane int
}

// BuildCIDRPools builds nv-ipam CIDRPool data for the Spectrum-X profiles from
// a topology.json that follows the reference generator schema.
func BuildCIDRPools(cfg *config.LaunchKitConfig, group config.ClusterConfig) ([]CIDRPool, error) {
	if cfg == nil || cfg.Profile == nil || cfg.Profile.SpectrumX == nil || !cfg.Profile.SpectrumX.Enable {
		return nil, nil
	}
	spcx := cfg.Profile.SpectrumX
	if spcx.IPVersion == config.SpectrumXIPVersionIPv6 {
		return nil, fmt.Errorf("Spectrum-X CIDRPool generation currently supports ipVersion=%s only; got %s",
			config.SpectrumXIPVersionIPv4, spcx.IPVersion)
	}
	if spcx.TopologyFile == "" {
		return nil, fmt.Errorf("profile.spectrumX.topologyFile or --topology-file is required for Spectrum-X CIDRPool generation")
	}
	topologyPath := spcx.ResolvedTopologyFile
	if topologyPath == "" {
		topologyPath = spcx.TopologyFile
	}
	topology, err := loadTopology(topologyPath)
	if err != nil {
		return nil, err
	}
	links, err := hostLinks(topology, spcx)
	if err != nil {
		return nil, err
	}

	allowedNodes := nodeSet(group.WorkerNodes)
	allocations := allocationsByPool(links, allowedNodes, spcx)
	poolKeys := sortedPoolKeys(allocations)
	if len(poolKeys) == 0 {
		return nil, fmt.Errorf("no Spectrum-X topology allocations matched clusterConfig group %q", group.Identifier)
	}
	pools := make([]CIDRPool, 0, len(poolKeys))
	for _, key := range poolKeys {
		staticAllocations := allocations[key]
		if len(staticAllocations) == 0 {
			return nil, fmt.Errorf("CIDR pool %s has no host static allocations in topology file %s",
				poolName(key, group.MergedIdentifier, spcx), topologyPath)
		}
		if len(allowedNodes) > 0 {
			if missing := missingNodes(allowedNodes, staticAllocations); len(missing) > 0 {
				return nil, fmt.Errorf("CIDR pool %s is missing topology allocations for worker nodes: %s",
					poolName(key, group.MergedIdentifier, spcx), strings.Join(missing, ", "))
			}
		}
		cidr := poolCIDR(staticAllocations[0].Prefix, spcx)
		planeCIDR := planeCIDR(staticAllocations[0].Prefix, spcx)
		routes := []string{cidr}
		if planeCIDR != "" && planeCIDR != cidr {
			routes = append(routes, planeCIDR)
		}
		pools = append(pools, CIDRPool{
			Name:              poolName(key, group.MergedIdentifier, spcx),
			CIDR:              cidr,
			Routes:            routes,
			StaticAllocations: staticAllocations,
		})
	}
	return pools, nil
}

func loadTopology(path string) (*topologyFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read Spectrum-X topology file %s: %w", path, err)
	}
	var topology topologyFile
	if err := json.Unmarshal(data, &topology); err != nil {
		return nil, fmt.Errorf("failed to parse Spectrum-X topology file %s: %w", path, err)
	}
	if len(topology.Links) == 0 {
		return nil, fmt.Errorf("Spectrum-X topology file %s has no links", path)
	}
	return &topology, nil
}

func hostLinks(topology *topologyFile, spcx *config.ProfileSpectrumX) ([]hostLink, error) {
	var links []hostLink
	hostIndexes := map[string]int{}
	for linkIdx, link := range topology.Links {
		if len(link) == 0 {
			continue
		}
		if len(link) != 2 {
			return nil, fmt.Errorf("topology link %d must contain exactly two endpoints", linkIdx)
		}
		hostIdx := -1
		leafIdx := -1
		for i := range link {
			switch link[i].Attrs.Role {
			case "host":
				hostIdx = i
			case "leaf":
				leafIdx = i
			}
		}
		if hostIdx == -1 {
			continue
		}
		if leafIdx == -1 {
			return nil, fmt.Errorf("topology host link %d has no leaf endpoint", linkIdx)
		}
		host := link[hostIdx]
		leaf := link[leafIdx]
		if !host.Attrs.HasRail {
			return nil, fmt.Errorf("topology host link %d endpoint %s/%s is missing attributes.rail",
				linkIdx, host.Node, host.Interface)
		}
		if !leaf.Attrs.HasPlane {
			return nil, fmt.Errorf("topology host link %d endpoint %s/%s is missing leaf attributes.plane",
				linkIdx, leaf.Node, leaf.Interface)
		}
		if spcx.TopologyType == config.SpectrumXTopology3Tier && !host.Attrs.HasPod {
			return nil, fmt.Errorf("topology host link %d endpoint %s/%s is missing attributes.pod for 3-tier allocation",
				linkIdx, host.Node, host.Interface)
		}
		key := hostIndexKey(host.Attrs)
		index, ok := hostIndexes[key+"|"+host.Node]
		if !ok {
			index = countHostIndex(hostIndexes, key)
			hostIndexes[key+"|"+host.Node] = index
		}
		addressPlane := leaf.Attrs.Plane
		if spcx.MultiplaneMode == "hwplb" {
			addressPlane = 0
		}
		hostIP, leafIP, err := allocateIPv4HostLeaf(spcx, addressPlane, host.Attrs.Rail, host.Attrs.Pod, host.Attrs.SU, index)
		if err != nil {
			return nil, fmt.Errorf("topology host link %d endpoint %s/%s: %w", linkIdx, host.Node, host.Interface, err)
		}
		links = append(links, hostLink{
			node:      host.Node,
			plane:     leaf.Attrs.Plane,
			pod:       host.Attrs.Pod,
			su:        host.Attrs.SU,
			rail:      host.Attrs.Rail,
			hostIndex: index,
			hostIP:    hostIP,
			leafIP:    leafIP,
		})
	}
	if len(links) == 0 {
		return nil, fmt.Errorf("Spectrum-X topology file has no host-to-leaf links")
	}
	return links, nil
}

func hostIndexKey(attrs topologyAttributes) string {
	plane := "nil"
	if attrs.HasPlane {
		plane = fmt.Sprintf("%d", attrs.Plane)
	}
	pod := "nil"
	if attrs.HasPod {
		pod = fmt.Sprintf("%d", attrs.Pod)
	}
	su := "nil"
	if attrs.HasSU {
		su = fmt.Sprintf("%d", attrs.SU)
	}
	return fmt.Sprintf("%s/%s/%s", plane, pod, su)
}

func countHostIndex(indexes map[string]int, key string) int {
	prefix := key + "|"
	count := 0
	for k := range indexes {
		if strings.HasPrefix(k, prefix) {
			count++
		}
	}
	return count
}

func allocateIPv4HostLeaf(spcx *config.ProfileSpectrumX, plane, rail, pod, su, hostIndex int) (net.IP, net.IP, error) {
	if rail < 0 || rail > 7 {
		return nil, nil, fmt.Errorf("rail %d exceeds the supported 3-bit rail field", rail)
	}
	if hostIndex < 0 || hostIndex > 127 {
		return nil, nil, fmt.Errorf("host index %d exceeds the supported 7-bit host field", hostIndex)
	}
	firstOctet := spcx.HostFirstOctet
	if firstOctet == 0 {
		firstOctet = config.SpectrumXDefaultHostFirstOctet(spcx.TopologyType)
	}
	if firstOctet < 1 || firstOctet > 255 {
		return nil, nil, fmt.Errorf("hostFirstOctet %d must fit in one IPv4 octet", firstOctet)
	}
	second, third, err := ipv4HostLeafMiddleOctets(spcx, plane, rail, pod, su)
	if err != nil {
		return nil, nil, err
	}
	host := net.IPv4(byte(firstOctet), byte(second), byte(third), byte(hostIndex<<1))
	leaf := net.IPv4(byte(firstOctet), byte(second), byte(third), byte(hostIndex<<1|1))
	return host, leaf, nil
}

func ipv4HostLeafMiddleOctets(spcx *config.ProfileSpectrumX, plane, rail, pod, su int) (int, int, error) {
	planeAware := planeAwareAddressing(spcx)
	switch spcx.TopologyType {
	case config.SpectrumXTopology2Tier:
		if su < 0 || su > 63 {
			return 0, 0, fmt.Errorf("su %d exceeds the supported 6-bit SU field", su)
		}
		if planeAware {
			if plane < 0 || plane > 3 {
				return 0, 0, fmt.Errorf("plane %d exceeds the supported 2-bit plane field", plane)
			}
			second := 16 | (plane << 2) | ((rail >> 2) & 1)
			third := ((rail & 3) << 6) | su
			return second, third, nil
		}
		return 16 | (rail << 1), su, nil
	case config.SpectrumXTopology3Tier:
		if pod < 0 || pod > 31 {
			return 0, 0, fmt.Errorf("pod %d exceeds the supported 5-bit POD field", pod)
		}
		if planeAware {
			if su < 0 || su > 63 {
				return 0, 0, fmt.Errorf("su %d exceeds the supported 6-bit SU field", su)
			}
			if plane < 0 || plane > 3 {
				return 0, 0, fmt.Errorf("plane %d exceeds the supported 2-bit plane field", plane)
			}
			second := (plane << 6) | (rail << 3) | ((pod >> 2) & 7)
			third := ((pod & 3) << 6) | su
			return second, third, nil
		}
		if su < 0 || su > 255 {
			return 0, 0, fmt.Errorf("su %d exceeds the supported 8-bit SU field", su)
		}
		return (rail << 5) | pod, su, nil
	default:
		return 0, 0, fmt.Errorf("unsupported Spectrum-X topologyType %q", spcx.TopologyType)
	}
}

func planeAwareAddressing(spcx *config.ProfileSpectrumX) bool {
	return spcx.MultiplaneMode == "swplb" || spcx.MultiplaneMode == "hwplb"
}

func allocationsByPool(links []hostLink, allowedNodes map[string]struct{}, spcx *config.ProfileSpectrumX) map[poolKey][]StaticAllocation {
	result := map[poolKey][]StaticAllocation{}
	seen := map[poolKey]map[string]struct{}{}
	for _, link := range links {
		if len(allowedNodes) > 0 {
			if _, ok := allowedNodes[link.node]; !ok {
				continue
			}
		}
		key := poolKey{rail: link.rail}
		if spcx.MultiplaneMode == "swplb" {
			key.plane = link.plane
		}
		if seen[key] == nil {
			seen[key] = map[string]struct{}{}
		}
		if _, ok := seen[key][link.node]; ok {
			continue
		}
		seen[key][link.node] = struct{}{}
		result[key] = append(result[key], StaticAllocation{
			Gateway:  link.leafIP.String(),
			NodeName: link.node,
			Prefix:   link.hostIP.String() + "/31",
		})
	}
	return result
}

func sortedPoolKeys(allocations map[poolKey][]StaticAllocation) []poolKey {
	keys := make([]poolKey, 0, len(allocations))
	for key := range allocations {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].rail != keys[j].rail {
			return keys[i].rail < keys[j].rail
		}
		return keys[i].plane < keys[j].plane
	})
	return keys
}

func poolName(key poolKey, suffix string, spcx *config.ProfileSpectrumX) string {
	name := fmt.Sprintf("rail-%d", key.rail)
	if spcx.MultiplaneMode == "swplb" {
		name = fmt.Sprintf("%s-plane-%d", name, key.plane)
	}
	if suffix != "" {
		name += "-" + suffix
	}
	return name
}

func poolCIDR(prefix string, spcx *config.ProfileSpectrumX) string {
	return supernet(prefix, railPrefixLength(spcx))
}

func planeCIDR(prefix string, spcx *config.ProfileSpectrumX) string {
	return supernet(prefix, planePrefixLength(spcx))
}

func railPrefixLength(spcx *config.ProfileSpectrumX) int {
	if spcx.TopologyType == config.SpectrumXTopology3Tier {
		if planeAwareAddressing(spcx) {
			return 13
		}
		return 11
	}
	if planeAwareAddressing(spcx) {
		return 18
	}
	return 15
}

func planePrefixLength(spcx *config.ProfileSpectrumX) int {
	if spcx.TopologyType == config.SpectrumXTopology3Tier {
		if planeAwareAddressing(spcx) {
			return 10
		}
		return 8
	}
	if planeAwareAddressing(spcx) {
		return 14
	}
	return 12
}

func supernet(prefix string, prefixLength int) string {
	ip, _, err := net.ParseCIDR(prefix)
	if err != nil {
		return ""
	}
	mask := net.CIDRMask(prefixLength, 32)
	network := ip.Mask(mask)
	return (&net.IPNet{IP: network, Mask: mask}).String()
}

func nodeSet(nodes []string) map[string]struct{} {
	if len(nodes) == 0 {
		return nil
	}
	result := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		result[node] = struct{}{}
	}
	return result
}

func missingNodes(nodes map[string]struct{}, allocations []StaticAllocation) []string {
	seen := map[string]struct{}{}
	for _, allocation := range allocations {
		seen[allocation.NodeName] = struct{}{}
	}
	missing := make([]string, 0)
	for node := range nodes {
		if _, ok := seen[node]; !ok {
			missing = append(missing, node)
		}
	}
	sort.Strings(missing)
	return missing
}
