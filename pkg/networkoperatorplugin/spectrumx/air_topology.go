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
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

type topologyFormat int

const (
	topologyFormatReference topologyFormat = iota
	topologyFormatAIR
)

type airTopologyFile struct {
	Content airTopologyContent `json:"content"`
}

type airTopologyContent struct {
	Nodes map[string]airTopologyNode `json:"nodes"`
	Links []airTopologyLink          `json:"links"`
}

type airTopologyNode struct {
	Model         string                     `json:"model"`
	EmulationType string                     `json:"emulation_type"`
	NetworkPCI    map[string]json.RawMessage `json:"network_pci"`
}

type airTopologyLink struct {
	Endpoints    []airTopologyEndpoint
	Disconnected bool
}

func (l *airTopologyLink) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, item := range raw {
		var sentinel string
		if err := json.Unmarshal(item, &sentinel); err == nil {
			if sentinel != "unconnected" {
				return fmt.Errorf("unsupported AIR link endpoint %q", sentinel)
			}
			l.Disconnected = true
			continue
		}
		var endpoint airTopologyEndpoint
		if err := json.Unmarshal(item, &endpoint); err != nil {
			return err
		}
		l.Endpoints = append(l.Endpoints, endpoint)
	}
	return nil
}

type airTopologyEndpoint struct {
	Node       string  `json:"node"`
	Interface  string  `json:"interface"`
	NetworkPCI *string `json:"network_pci"`
}

func parseTopology(data []byte, topologyType string) (*topologyFile, error) {
	format, err := detectTopologyFormat(data)
	if err != nil {
		return nil, err
	}

	switch format {
	case topologyFormatReference:
		var topology topologyFile
		if err := json.Unmarshal(data, &topology); err != nil {
			return nil, err
		}
		return &topology, nil
	case topologyFormatAIR:
		var topology airTopologyFile
		if err := json.Unmarshal(data, &topology); err != nil {
			return nil, err
		}
		return normalizeAIRTopology(&topology, topologyType)
	default:
		return nil, fmt.Errorf("unsupported topology format")
	}
}

func detectTopologyFormat(data []byte) (topologyFormat, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return 0, err
	}
	if root == nil {
		return 0, fmt.Errorf("topology must be a JSON object")
	}

	reference := jsonValueKind(root["nodes"]) == '[' && jsonValueKind(root["links"]) == '['
	air := false
	if jsonValueKind(root["content"]) == '{' {
		var content map[string]json.RawMessage
		if err := json.Unmarshal(root["content"], &content); err != nil {
			return 0, fmt.Errorf("content: %w", err)
		}
		air = jsonValueKind(content["nodes"]) == '{' && jsonValueKind(content["links"]) == '['
	}

	switch {
	case reference && air:
		return 0, fmt.Errorf("ambiguous topology format: both top-level nodes/links and content.nodes/content.links are present")
	case reference:
		return topologyFormatReference, nil
	case air:
		return topologyFormatAIR, nil
	default:
		return 0, fmt.Errorf("unsupported topology format: expected top-level nodes and links arrays, or an AIR content object with a nodes map and links array")
	}
}

func jsonValueKind(raw json.RawMessage) byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0
	}
	return trimmed[0]
}

func normalizeAIRTopology(air *airTopologyFile, topologyType string) (*topologyFile, error) {
	if air == nil {
		return nil, fmt.Errorf("AIR topology is nil")
	}

	topology := &topologyFile{}
	nodeNames := make([]string, 0, len(air.Content.Nodes))
	for name := range air.Content.Nodes {
		nodeNames = append(nodeNames, name)
	}
	sort.Strings(nodeNames)
	for _, name := range nodeNames {
		node := air.Content.Nodes[name]
		role := ""
		if isAIRHostNode(node) {
			role = "host"
		} else if isAIRLeafNode(name) {
			role = "leaf"
		}
		topology.Nodes = append(topology.Nodes, topologyNode{Name: name, Role: role, Type: node.Model})
	}

	hostPositions := map[string]string{}
	for linkIndex, link := range air.Content.Links {
		if link.Disconnected {
			continue
		}
		if len(link.Endpoints) != 2 {
			return nil, fmt.Errorf("AIR topology link %d must contain exactly two endpoints", linkIndex)
		}

		hostEndpointIndex := -1
		for endpointIndex, endpoint := range link.Endpoints {
			node, ok := air.Content.Nodes[endpoint.Node]
			if !ok {
				return nil, fmt.Errorf("AIR topology link %d references unknown node %q", linkIndex, endpoint.Node)
			}
			if isAIRHostNode(node) {
				if hostEndpointIndex != -1 {
					return nil, fmt.Errorf("AIR topology link %d connects two host nodes", linkIndex)
				}
				hostEndpointIndex = endpointIndex
			}
		}
		if hostEndpointIndex == -1 {
			continue
		}

		host := link.Endpoints[hostEndpointIndex]
		leaf := link.Endpoints[1-hostEndpointIndex]
		if !isAIRLeafNode(leaf.Node) {
			return nil, fmt.Errorf("AIR topology host link %d endpoint %q is not a leaf node; leaf node names must start with leaf-",
				linkIndex, leaf.Node)
		}
		hostNode := air.Content.Nodes[host.Node]
		normalized, err := normalizeAIRHostLink(linkIndex, host, leaf, hostNode, topologyType, hostPositions)
		if err != nil {
			return nil, err
		}
		topology.Links = append(topology.Links, normalized)
	}
	return topology, nil
}

func isAIRHostNode(node airTopologyNode) bool {
	return strings.EqualFold(node.Model, "host") ||
		strings.EqualFold(node.EmulationType, "HOST") || len(node.NetworkPCI) > 0
}

func isAIRLeafNode(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "leaf-")
}

func normalizeAIRHostLink(
	linkIndex int,
	host, leaf airTopologyEndpoint,
	hostNode airTopologyNode,
	topologyType string,
	hostPositions map[string]string,
) (topologyLink, error) {
	if host.NetworkPCI == nil || *host.NetworkPCI == "" {
		return nil, fmt.Errorf("AIR topology host link %d endpoint %s/%s is missing network_pci",
			linkIndex, host.Node, host.Interface)
	}
	if _, ok := hostNode.NetworkPCI[*host.NetworkPCI]; !ok {
		return nil, fmt.Errorf("AIR topology host link %d endpoint %s/%s references network_pci %q not defined by node %q",
			linkIndex, host.Node, host.Interface, *host.NetworkPCI, host.Node)
	}

	interfaceRail, interfacePlane, err := parseAIRHostInterface(host.Interface)
	if err != nil {
		return nil, fmt.Errorf("AIR topology host link %d endpoint %s/%s: %w", linkIndex, host.Node, host.Interface, err)
	}
	networkPCIRail, err := parseAIRNetworkPCI(*host.NetworkPCI)
	if err != nil {
		return nil, fmt.Errorf("AIR topology host link %d endpoint %s/%s: %w", linkIndex, host.Node, host.Interface, err)
	}
	if interfaceRail != networkPCIRail {
		return nil, fmt.Errorf("AIR topology host link %d endpoint %s/%s has rail %d but network_pci %q identifies rail %d",
			linkIndex, host.Node, host.Interface, interfaceRail+1, *host.NetworkPCI, networkPCIRail+1)
	}

	hostSU, err := requiredAIRNameOrdinal(host.Node, "su")
	if err != nil {
		return nil, fmt.Errorf("AIR topology host link %d: %w", linkIndex, err)
	}
	hostIndex, err := requiredAIRNameOrdinal(host.Node, "h")
	if err != nil {
		return nil, fmt.Errorf("AIR topology host link %d: %w", linkIndex, err)
	}
	leafPlane, err := requiredAIRNameOrdinal(leaf.Node, "p")
	if err != nil {
		return nil, fmt.Errorf("AIR topology host link %d: %w", linkIndex, err)
	}
	leafSU, err := requiredAIRNameOrdinal(leaf.Node, "su")
	if err != nil {
		return nil, fmt.Errorf("AIR topology host link %d: %w", linkIndex, err)
	}
	leafRail, err := requiredAIRNameOrdinal(leaf.Node, "r")
	if err != nil {
		return nil, fmt.Errorf("AIR topology host link %d: %w", linkIndex, err)
	}
	if hostSU != leafSU {
		return nil, fmt.Errorf("AIR topology host link %d has host SU %d but leaf %q identifies SU %d",
			linkIndex, hostSU+1, leaf.Node, leafSU+1)
	}
	if interfacePlane != leafPlane {
		return nil, fmt.Errorf("AIR topology host link %d endpoint %s/%s has plane %d but leaf %q identifies plane %d",
			linkIndex, host.Node, host.Interface, interfacePlane+1, leaf.Node, leafPlane+1)
	}
	if interfaceRail != leafRail {
		return nil, fmt.Errorf("AIR topology host link %d endpoint %s/%s has rail %d but leaf %q identifies rail %d",
			linkIndex, host.Node, host.Interface, interfaceRail+1, leaf.Node, leafRail+1)
	}

	hostPod, hostHasPod, err := optionalAIRNameOrdinal(host.Node, "pod")
	if err != nil {
		return nil, fmt.Errorf("AIR topology host link %d: %w", linkIndex, err)
	}
	leafPod, leafHasPod, err := optionalAIRNameOrdinal(leaf.Node, "pod")
	if err != nil {
		return nil, fmt.Errorf("AIR topology host link %d: %w", linkIndex, err)
	}
	if topologyType == config.SpectrumXTopology3Tier && (!hostHasPod || !leafHasPod) {
		return nil, fmt.Errorf("AIR topology host link %d requires pod<N> name tokens on both host %q and leaf %q for 3-tier allocation",
			linkIndex, host.Node, leaf.Node)
	}
	if hostHasPod != leafHasPod || (hostHasPod && hostPod != leafPod) {
		return nil, fmt.Errorf("AIR topology host link %d has inconsistent pod<N> name tokens on host %q and leaf %q",
			linkIndex, host.Node, leaf.Node)
	}

	position := fmt.Sprintf("%d/%d/%d", hostPod, hostSU, hostIndex)
	if existing, ok := hostPositions[position]; ok && existing != host.Node {
		return nil, fmt.Errorf("AIR topology host nodes %q and %q identify the same pod/SU/host position", existing, host.Node)
	}
	hostPositions[position] = host.Node

	hostAttrs := topologyAttributes{
		Role:    "host",
		Pod:     hostPod,
		SU:      hostSU,
		Rail:    interfaceRail,
		HasPod:  hostHasPod,
		HasSU:   true,
		HasRail: true,
	}
	leafAttrs := topologyAttributes{
		Role:     "leaf",
		Plane:    leafPlane,
		Pod:      leafPod,
		SU:       leafSU,
		Rail:     leafRail,
		HasPlane: true,
		HasPod:   leafHasPod,
		HasSU:    true,
		HasRail:  true,
	}
	return topologyLink{
		{Node: host.Node, Interface: host.Interface, Attrs: hostAttrs},
		{Node: leaf.Node, Interface: leaf.Interface, Attrs: leafAttrs},
	}, nil
}

func parseAIRHostInterface(name string) (int, int, error) {
	railText, planeText, ok := splitAIRCompoundOrdinal(name, "rail", "p")
	if !ok {
		return 0, 0, fmt.Errorf("interface must match rail<R>p<P> with one-based ordinals")
	}
	rail, err := parseOneBasedOrdinal(railText)
	if err != nil {
		return 0, 0, fmt.Errorf("interface rail: %w", err)
	}
	plane, err := parseOneBasedOrdinal(planeText)
	if err != nil {
		return 0, 0, fmt.Errorf("interface plane: %w", err)
	}
	return rail, plane, nil
}

func parseAIRNetworkPCI(name string) (int, error) {
	if !strings.HasPrefix(name, "rail") || len(name) == len("rail") {
		return 0, fmt.Errorf("network_pci must match rail<R> with a one-based ordinal")
	}
	rail, err := parseOneBasedOrdinal(strings.TrimPrefix(name, "rail"))
	if err != nil {
		return 0, fmt.Errorf("network_pci %q: %w", name, err)
	}
	return rail, nil
}

func splitAIRCompoundOrdinal(value, firstPrefix, secondPrefix string) (string, string, bool) {
	if !strings.HasPrefix(value, firstPrefix) {
		return "", "", false
	}
	remainder := strings.TrimPrefix(value, firstPrefix)
	separator := strings.Index(remainder, secondPrefix)
	if separator <= 0 || separator+len(secondPrefix) >= len(remainder) {
		return "", "", false
	}
	first := remainder[:separator]
	second := remainder[separator+len(secondPrefix):]
	if !allDigits(first) || !allDigits(second) {
		return "", "", false
	}
	return first, second, true
}

func requiredAIRNameOrdinal(name, prefix string) (int, error) {
	value, found, err := optionalAIRNameOrdinal(name, prefix)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("AIR node name %q is missing required %s<N> token", name, prefix)
	}
	return value, nil
}

func optionalAIRNameOrdinal(name, prefix string) (int, bool, error) {
	var value int
	found := false
	for _, token := range strings.Split(name, "-") {
		if !strings.HasPrefix(token, prefix) {
			continue
		}
		digits := strings.TrimPrefix(token, prefix)
		if digits == "" || !allDigits(digits) {
			continue
		}
		if found {
			return 0, false, fmt.Errorf("AIR node name %q has more than one %s<N> token", name, prefix)
		}
		parsed, err := parseOneBasedOrdinal(digits)
		if err != nil {
			return 0, false, fmt.Errorf("AIR node name %q token %q: %w", name, token, err)
		}
		value = parsed
		found = true
	}
	return value, found, nil
}

func parseOneBasedOrdinal(value string) (int, error) {
	if !allDigits(value) {
		return 0, fmt.Errorf("%q is not a decimal ordinal", value)
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	if parsed < 1 {
		return 0, fmt.Errorf("ordinal must be at least 1")
	}
	return parsed - 1, nil
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !unicode.IsDigit(r) || r > unicode.MaxASCII {
			return false
		}
	}
	return true
}
