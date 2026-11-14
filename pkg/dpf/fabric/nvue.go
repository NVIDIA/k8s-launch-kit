// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package fabric parses read-only Cumulus Linux NVUE and FRR JSON output.
// It reports configured facts separately from operational state. It does not
// decide whether a fabric is ready for a particular workload.
package fabric

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// NVUEInputs contains the named JSON outputs used to construct a fabric
// snapshot. Each input is captured independently so missing or failed command
// output can be reported against its source instead of becoming an empty fact.
type NVUEInputs struct {
	AppliedInterfaces     []byte
	AppliedVRFs           []byte
	AppliedRouter         []byte
	AppliedBridge         []byte
	OperationalInterfaces []byte
	OperationalBridge     []byte
	OperationalAddresses  []byte
	OperationalRoutesIPv4 []byte
	OperationalRoutesIPv6 []byte
	LLDP                  []byte
	FRRBGPSummary         []byte
}

// Snapshot is a compact factual view of configured and operational fabric
// state. A nil pointer means the source did not establish that field. Empty
// non-nil slices represent a known-empty collection.
type Snapshot struct {
	Configured  ConfiguredFacts  `json:"configured" yaml:"configured"`
	Operational OperationalFacts `json:"operational" yaml:"operational"`
	Issues      []Issue          `json:"issues,omitempty" yaml:"issues,omitempty"`
}

// ConfiguredFacts are values read from NVUE's applied configuration.
type ConfiguredFacts struct {
	Interfaces []InterfaceConfig `json:"interfaces,omitempty" yaml:"interfaces,omitempty"`
	VRFs       []VRFConfig       `json:"vrfs,omitempty" yaml:"vrfs,omitempty"`
	BGP        *BGPConfig        `json:"bgp,omitempty" yaml:"bgp,omitempty"`
	Bridges    []BridgeConfig    `json:"bridges,omitempty" yaml:"bridges,omitempty"`
}

// OperationalFacts are values read from live interface, bridge, LLDP and FRR
// output. LLDP identifies a possible peer only; it is not authoritative
// inventory or proof that a peer is configured.
type OperationalFacts struct {
	Interfaces []InterfaceState    `json:"interfaces,omitempty" yaml:"interfaces,omitempty"`
	Bridges    []BridgeState       `json:"bridges,omitempty" yaml:"bridges,omitempty"`
	Addresses  *[]InterfaceAddress `json:"addresses,omitempty" yaml:"addresses,omitempty"`
	IPv4Routes *[]Route            `json:"ipv4Routes,omitempty" yaml:"ipv4Routes,omitempty"`
	IPv6Routes *[]Route            `json:"ipv6Routes,omitempty" yaml:"ipv6Routes,omitempty"`
	LLDP       []LLDPPeerHint      `json:"lldp,omitempty" yaml:"lldp,omitempty"`
	BGP        []BGPSession        `json:"bgp,omitempty" yaml:"bgp,omitempty"`
}

// Issue identifies a missing or malformed source field without including raw
// command output or environment-specific values.
type Issue struct {
	Input   string `json:"input" yaml:"input"`
	Field   string `json:"field" yaml:"field"`
	Code    string `json:"code" yaml:"code"`
	Message string `json:"message" yaml:"message"`
}

type InterfaceConfig struct {
	Name         string  `json:"name" yaml:"name"`
	Type         *string `json:"type,omitempty" yaml:"type,omitempty"`
	VRF          *string `json:"vrf,omitempty" yaml:"vrf,omitempty"`
	BridgeDomain *string `json:"bridgeDomain,omitempty" yaml:"bridgeDomain,omitempty"`
	MTU          *int    `json:"mtu,omitempty" yaml:"mtu,omitempty"`
}

type VRFConfig struct {
	Name            string                `json:"name" yaml:"name"`
	BGPEnabled      *bool                 `json:"bgpEnabled,omitempty" yaml:"bgpEnabled,omitempty"`
	ASN             *uint32               `json:"asn,omitempty" yaml:"asn,omitempty"`
	ASNConfigured   *bool                 `json:"asnConfigured,omitempty" yaml:"asnConfigured,omitempty"`
	AddressFamilies []AddressFamilyConfig `json:"addressFamilies,omitempty" yaml:"addressFamilies,omitempty"`
	PeerGroups      []PeerGroupConfig     `json:"peerGroups,omitempty" yaml:"peerGroups,omitempty"`
	Neighbors       *[]BGPNeighborConfig  `json:"neighbors,omitempty" yaml:"neighbors,omitempty"`
}

type BGPConfig struct {
	Enabled       *bool   `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	ASN           *uint32 `json:"asn,omitempty" yaml:"asn,omitempty"`
	ASNConfigured *bool   `json:"asnConfigured,omitempty" yaml:"asnConfigured,omitempty"`
}

type AddressFamilyConfig struct {
	Name         string           `json:"name" yaml:"name"`
	Enabled      *bool            `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Redistribute []Redistribution `json:"redistribute,omitempty" yaml:"redistribute,omitempty"`
}

type Redistribution struct {
	Source   string  `json:"source" yaml:"source"`
	Enabled  *bool   `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	RouteMap *string `json:"routeMap,omitempty" yaml:"routeMap,omitempty"`
}

type BGPNeighborConfig struct {
	Name                     string   `json:"name" yaml:"name"`
	PeerGroup                *string  `json:"peerGroup,omitempty" yaml:"peerGroup,omitempty"`
	Type                     *string  `json:"type,omitempty" yaml:"type,omitempty"`
	RemoteAS                 *string  `json:"remoteAs,omitempty" yaml:"remoteAs,omitempty"`
	EffectiveRemoteAS        *string  `json:"effectiveRemoteAs,omitempty" yaml:"effectiveRemoteAs,omitempty"`
	EffectiveAddressFamilies []string `json:"effectiveAddressFamilies,omitempty" yaml:"effectiveAddressFamilies,omitempty"`
}

type PeerGroupConfig struct {
	Name            string   `json:"name" yaml:"name"`
	RemoteAS        *string  `json:"remoteAs,omitempty" yaml:"remoteAs,omitempty"`
	AddressFamilies []string `json:"addressFamilies,omitempty" yaml:"addressFamilies,omitempty"`
}

type BridgeConfig struct {
	Name          string             `json:"name" yaml:"name"`
	Encapsulation *string            `json:"encapsulation,omitempty" yaml:"encapsulation,omitempty"`
	Ports         []BridgePortConfig `json:"ports,omitempty" yaml:"ports,omitempty"`
}

type BridgePortConfig struct {
	Name  string             `json:"name" yaml:"name"`
	VLANs []BridgeVLANConfig `json:"vlans,omitempty" yaml:"vlans,omitempty"`
}

type BridgeVLANConfig struct {
	ID           int     `json:"id" yaml:"id"`
	TagState     *string `json:"tagState,omitempty" yaml:"tagState,omitempty"`
	ForwardState *string `json:"forwardState,omitempty" yaml:"forwardState,omitempty"`
}

type InterfaceState struct {
	Name       string  `json:"name" yaml:"name"`
	Type       *string `json:"type,omitempty" yaml:"type,omitempty"`
	VRF        *string `json:"vrf,omitempty" yaml:"vrf,omitempty"`
	AdminState *string `json:"adminState,omitempty" yaml:"adminState,omitempty"`
	OperState  *string `json:"operState,omitempty" yaml:"operState,omitempty"`
	MTU        *int    `json:"mtu,omitempty" yaml:"mtu,omitempty"`
	Speed      *string `json:"speed,omitempty" yaml:"speed,omitempty"`
}

// InterfaceAddress is an operational IP prefix assigned to an interface.
type InterfaceAddress struct {
	Interface string `json:"interface" yaml:"interface"`
	Family    string `json:"family" yaml:"family"`
	Prefix    string `json:"prefix" yaml:"prefix"`
}

// Route is an operational route from one address-family-specific all-table
// route query. The family-specific slices keep an unavailable source distinct
// from a known-empty routing table.
type Route struct {
	Family   string `json:"family" yaml:"family"`
	Prefix   string `json:"prefix" yaml:"prefix"`
	Table    string `json:"table" yaml:"table"`
	Protocol string `json:"protocol,omitempty" yaml:"protocol,omitempty"`
	Type     string `json:"type,omitempty" yaml:"type,omitempty"`
}

type BridgeState struct {
	Name  string            `json:"name" yaml:"name"`
	Ports []BridgePortState `json:"ports,omitempty" yaml:"ports,omitempty"`
}

type BridgePortState struct {
	Name  string            `json:"name" yaml:"name"`
	State *string           `json:"state,omitempty" yaml:"state,omitempty"`
	VLANs []BridgeVLANState `json:"vlans,omitempty" yaml:"vlans,omitempty"`
}

type BridgeVLANState struct {
	ID           int     `json:"id" yaml:"id"`
	TagState     *string `json:"tagState,omitempty" yaml:"tagState,omitempty"`
	ForwardState *string `json:"forwardState,omitempty" yaml:"forwardState,omitempty"`
}

type LLDPPeerHint struct {
	LocalInterface  string  `json:"localInterface" yaml:"localInterface"`
	ChassisName     *string `json:"chassisName,omitempty" yaml:"chassisName,omitempty"`
	ChassisID       *string `json:"chassisId,omitempty" yaml:"chassisId,omitempty"`
	PortName        *string `json:"portName,omitempty" yaml:"portName,omitempty"`
	PortID          *string `json:"portId,omitempty" yaml:"portId,omitempty"`
	PortDescription *string `json:"portDescription,omitempty" yaml:"portDescription,omitempty"`
}

type BGPPeerState struct {
	Name             string  `json:"name" yaml:"name"`
	RemoteAS         *uint32 `json:"remoteAs,omitempty" yaml:"remoteAs,omitempty"`
	State            *string `json:"state,omitempty" yaml:"state,omitempty"`
	PrefixesReceived *uint64 `json:"prefixesReceived,omitempty" yaml:"prefixesReceived,omitempty"`
	PrefixesAccepted *uint64 `json:"prefixesAccepted,omitempty" yaml:"prefixesAccepted,omitempty"`
	PrefixesSent     *uint64 `json:"prefixesSent,omitempty" yaml:"prefixesSent,omitempty"`
}

type BGPSession struct {
	VRF           string         `json:"vrf" yaml:"vrf"`
	AddressFamily string         `json:"addressFamily" yaml:"addressFamily"`
	RouterID      *string        `json:"routerId,omitempty" yaml:"routerId,omitempty"`
	LocalAS       *uint32        `json:"localAs,omitempty" yaml:"localAs,omitempty"`
	Peers         []BGPPeerState `json:"peers" yaml:"peers"`
}

type jsonObject map[string]json.RawMessage

// ParseNVUE parses the provided applied and operational JSON snapshots.
// Unsupported optional properties are ignored for forward compatibility;
// malformed required containers and fields produce field-specific issues.
func ParseNVUE(in NVUEInputs) Snapshot {
	out := Snapshot{}
	parseConfiguredInterfaces(in.AppliedInterfaces, &out)
	parseConfiguredVRFs(in.AppliedVRFs, &out)
	parseGlobalRouter(in.AppliedRouter, &out)
	parseConfiguredBridges(in.AppliedBridge, &out)
	parseOperationalInterfaces(in.OperationalInterfaces, &out)
	parseOperationalBridges(in.OperationalBridge, &out)
	parseOperationalAddresses(in.OperationalAddresses, &out)
	parseOperationalRoutes(in.OperationalRoutesIPv4, "operationalRoutesIPv4", "ipv4", &out.Operational.IPv4Routes, &out)
	parseOperationalRoutes(in.OperationalRoutesIPv6, "operationalRoutesIPv6", "ipv6", &out.Operational.IPv6Routes, &out)
	parseLLDP(in.LLDP, &out)
	parseFRRSummary(in.FRRBGPSummary, &out)
	sortSnapshot(&out)
	return out
}

func parseObject(raw []byte, input string, issues *[]Issue) (jsonObject, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		addIssue(issues, input, "$", "missing_output", "command output is empty")
		return nil, false
	}
	var obj jsonObject
	if err := json.Unmarshal(raw, &obj); err != nil {
		addIssue(issues, input, "$", "malformed_json", "command output is not a JSON object")
		return nil, false
	}
	if obj == nil {
		addIssue(issues, input, "$", "unsupported_shape", "expected a JSON object")
		return nil, false
	}
	return obj, true
}

func addIssue(issues *[]Issue, input, field, code, message string) {
	*issues = append(*issues, Issue{Input: input, Field: field, Code: code, Message: message})
}

func objectField(obj jsonObject, key, input, path string, issues *[]Issue) (jsonObject, bool) {
	raw, ok := obj[key]
	if !ok {
		addIssue(issues, input, path+"."+key, "missing_field", "required field is absent")
		return nil, false
	}
	var child jsonObject
	if err := json.Unmarshal(raw, &child); err != nil || child == nil {
		addIssue(issues, input, path+"."+key, "invalid_type", "expected an object")
		return nil, false
	}
	return child, true
}

func optionalObjectField(obj jsonObject, key, input, path string, issues *[]Issue) (jsonObject, bool) {
	raw, ok := obj[key]
	if !ok {
		return nil, false
	}
	var child jsonObject
	if err := json.Unmarshal(raw, &child); err != nil || child == nil {
		addIssue(issues, input, path+"."+key, "invalid_type", "expected an object")
		return nil, false
	}
	return child, true
}

func stringField(obj jsonObject, key, input, path string, required bool, issues *[]Issue) *string {
	raw, ok := obj[key]
	if !ok {
		if required {
			addIssue(issues, input, path+"."+key, "missing_field", "required field is absent")
		}
		return nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		addIssue(issues, input, path+"."+key, "invalid_type", "expected a string")
		return nil
	}
	return &value
}

func intField(obj jsonObject, key, input, path string, required bool, issues *[]Issue) *int {
	raw, ok := obj[key]
	if !ok {
		if required {
			addIssue(issues, input, path+"."+key, "missing_field", "required field is absent")
		}
		return nil
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		addIssue(issues, input, path+"."+key, "invalid_type", "expected an integer")
		return nil
	}
	return &value
}

func uintField(obj jsonObject, key, input, path string, bits int, issues *[]Issue) *uint64 {
	raw, ok := obj[key]
	if !ok {
		return nil
	}
	var value uint64
	if err := json.Unmarshal(raw, &value); err != nil || (bits == 32 && value > uint64(^uint32(0))) {
		addIssue(issues, input, path+"."+key, "invalid_type", "expected a non-negative integer in range")
		return nil
	}
	return &value
}

func stringMap(raw json.RawMessage, input, field string, issues *[]Issue) (jsonObject, bool) {
	var value jsonObject
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		addIssue(issues, input, field, "invalid_type", "expected an object keyed by name")
		return nil, false
	}
	return value, true
}

func parseConfiguredInterfaces(raw []byte, out *Snapshot) {
	const input = "appliedInterfaces"
	root, ok := parseObject(raw, input, &out.Issues)
	if !ok {
		return
	}
	for name, entryRaw := range root {
		path := "$[" + strconv.Quote(name) + "]"
		var entry jsonObject
		if err := json.Unmarshal(entryRaw, &entry); err != nil || entry == nil {
			addIssue(&out.Issues, input, path, "invalid_type", "expected an interface object")
			continue
		}
		fact := InterfaceConfig{Name: name}
		fact.Type = stringField(entry, "type", input, path, false, &out.Issues)
		fact.VRF = stringField(entry, "vrf", input, path, false, &out.Issues)
		fact.BridgeDomain = configuredInterfaceBridgeDomain(entry, input, path, &out.Issues)
		if link, exists := optionalObjectField(entry, "link", input, path, &out.Issues); exists {
			fact.MTU = intField(link, "mtu", input, path+".link", false, &out.Issues)
		}
		out.Configured.Interfaces = append(out.Configured.Interfaces, fact)
	}
}

func configuredInterfaceBridgeDomain(entry jsonObject, input, path string, issues *[]Issue) *string {
	bridge, ok := optionalObjectField(entry, "bridge", input, path, issues)
	if !ok {
		return nil
	}
	domain, ok := optionalObjectField(bridge, "domain", input, path+".bridge", issues)
	if !ok {
		return nil
	}
	if len(domain) == 1 {
		for name := range domain {
			return &name
		}
	}
	if len(domain) > 1 {
		addIssue(issues, input, path+".bridge.domain", "ambiguous_membership", "interface lists more than one bridge domain")
	}
	return nil
}

func parseConfiguredVRFs(raw []byte, out *Snapshot) {
	const input = "appliedVRFs"
	root, ok := parseObject(raw, input, &out.Issues)
	if !ok {
		return
	}
	for name, entryRaw := range root {
		path := "$[" + strconv.Quote(name) + "]"
		var entry jsonObject
		if err := json.Unmarshal(entryRaw, &entry); err != nil || entry == nil {
			addIssue(&out.Issues, input, path, "invalid_type", "expected a VRF object")
			continue
		}
		fact := VRFConfig{Name: name}
		router, hasRouter := optionalObjectField(entry, "router", input, path, &out.Issues)
		if hasRouter {
			bgp, hasBGP := optionalObjectField(router, "bgp", input, path+".router", &out.Issues)
			if hasBGP {
				fact.BGPEnabled = enabledState(bgp, input, path+".router.bgp", &out.Issues)
				required := fact.BGPEnabled == nil || *fact.BGPEnabled
				if asn, exists := bgp["autonomous-system"]; exists {
					fact.ASN, fact.ASNConfigured = parseASN(asn, input, path+".router.bgp.autonomous-system", &out.Issues)
				} else if required {
					addIssue(&out.Issues, input, path+".router.bgp.autonomous-system", "missing_field", "required field is absent")
				}
				fact.AddressFamilies = parseAddressFamilies(bgp, input, path+".router.bgp", required, &out.Issues)
				fact.PeerGroups, fact.Neighbors = parseConfiguredNeighbors(bgp, input, path+".router.bgp", required, &out.Issues)
			} else {
				addIssue(&out.Issues, input, path+".router.bgp", "not_configured", "VRF has no BGP configuration block")
			}
		} else {
			addIssue(&out.Issues, input, path+".router", "not_configured", "VRF has no router configuration block")
		}
		out.Configured.VRFs = append(out.Configured.VRFs, fact)
	}
}

func parseASN(raw json.RawMessage, input, field string, issues *[]Issue) (*uint32, *bool) {
	if len(raw) == 0 || string(raw) == "null" {
		addIssue(issues, input, field, "missing_field", "required field is absent")
		return nil, nil
	}
	var n uint64
	if err := json.Unmarshal(raw, &n); err != nil {
		var s string
		if err2 := json.Unmarshal(raw, &s); err2 != nil {
			addIssue(issues, input, field, "invalid_type", "expected an ASN integer or numeric string")
			return nil, nil
		}
		if strings.EqualFold(s, "none") {
			configured := false
			return nil, &configured
		}
		if strings.TrimSpace(s) == "" {
			addIssue(issues, input, field, "invalid_value", "ASN string is empty")
			return nil, nil
		}
		n, err = strconv.ParseUint(s, 10, 32)
		if err != nil {
			addIssue(issues, input, field, "invalid_value", "ASN is not a valid 32-bit unsigned integer")
			return nil, nil
		}
	}
	if n > uint64(^uint32(0)) {
		addIssue(issues, input, field, "invalid_value", "ASN exceeds the 32-bit unsigned range")
		return nil, nil
	}
	v := uint32(n)
	configured := true
	return &v, &configured
}

func parseAddressFamilies(bgp jsonObject, input, path string, required bool, issues *[]Issue) []AddressFamilyConfig {
	afRaw, ok := bgp["address-family"]
	if !ok {
		if required {
			addIssue(issues, input, path+".address-family", "missing_field", "address-family configuration is absent")
		}
		return nil
	}
	afMap, ok := stringMap(afRaw, input, path+".address-family", issues)
	if !ok {
		return nil
	}
	out := make([]AddressFamilyConfig, 0, len(afMap))
	for name, raw := range afMap {
		afPath := path + ".address-family." + name
		var entry jsonObject
		if err := json.Unmarshal(raw, &entry); err != nil || entry == nil {
			addIssue(issues, input, afPath, "invalid_type", "expected an address-family object")
			continue
		}
		fact := AddressFamilyConfig{Name: name}
		// NVUE reports state as "enabled"/"disabled", rather than a bool.
		fact.Enabled = enabledState(entry, input, afPath, issues)
		fact.Redistribute = parseRedistribution(entry, input, afPath, issues)
		out = append(out, fact)
	}
	return out
}

func enabledState(obj jsonObject, input, path string, issues *[]Issue) *bool {
	raw, ok := obj["state"]
	if !ok {
		addIssue(issues, input, path+".state", "missing_field", "required field is absent")
		return nil
	}
	var state string
	if err := json.Unmarshal(raw, &state); err != nil {
		addIssue(issues, input, path+".state", "invalid_type", "expected enabled/disabled string")
		return nil
	}
	switch strings.ToLower(state) {
	case "enabled":
		v := true
		return &v
	case "disabled":
		v := false
		return &v
	default:
		addIssue(issues, input, path+".state", "unsupported_value", "expected enabled or disabled")
		return nil
	}
}

func parseRedistribution(af jsonObject, input, path string, issues *[]Issue) []Redistribution {
	redist, ok := optionalObjectField(af, "redistribute", input, path, issues)
	if !ok {
		return nil
	}
	out := make([]Redistribution, 0, len(redist))
	for source, raw := range redist {
		itemPath := path + ".redistribute." + source
		var entry jsonObject
		if err := json.Unmarshal(raw, &entry); err != nil || entry == nil {
			addIssue(issues, input, itemPath, "invalid_type", "expected redistribution object")
			continue
		}
		item := Redistribution{Source: source}
		item.Enabled = enabledState(entry, input, itemPath, issues)
		item.RouteMap = stringField(entry, "route-map", input, itemPath, false, issues)
		out = append(out, item)
	}
	return out
}

func parseConfiguredNeighbors(bgp jsonObject, input, path string, required bool, issues *[]Issue) ([]PeerGroupConfig, *[]BGPNeighborConfig) {
	groups, _ := optionalObjectField(bgp, "peer-group", input, path, issues)
	groupAFs := map[string][]string{}
	groupAS := map[string]*string{}
	configuredGroups := make([]PeerGroupConfig, 0, len(groups))
	for name, rawGroup := range groups {
		var group jsonObject
		if err := json.Unmarshal(rawGroup, &group); err != nil || group == nil {
			addIssue(issues, input, path+".peer-group."+name, "invalid_type", "expected peer-group object")
			continue
		}
		remoteAS := stringOrNumberField(group, "remote-as", input, path+".peer-group."+name, issues)
		groupAS[name] = remoteAS
		configuredGroup := PeerGroupConfig{Name: name, RemoteAS: remoteAS}
		if af, ok := optionalObjectField(group, "address-family", input, path+".peer-group."+name, issues); ok {
			for afName, afRaw := range af {
				var cfg jsonObject
				if err := json.Unmarshal(afRaw, &cfg); err != nil || cfg == nil {
					addIssue(issues, input, path+".peer-group."+name+".address-family."+afName, "invalid_type", "expected address-family object")
					continue
				}
				state := enabledState(cfg, input, path+".peer-group."+name+".address-family."+afName, issues)
				if state != nil && *state {
					groupAFs[name] = append(groupAFs[name], afName)
					configuredGroup.AddressFamilies = append(configuredGroup.AddressFamilies, afName)
				}
			}
		}
		configuredGroups = append(configuredGroups, configuredGroup)
	}

	raw, ok := bgp["neighbor"]
	if !ok {
		if required {
			addIssue(issues, input, path+".neighbor", "missing_field", "neighbor map is absent")
		}
		return configuredGroups, nil
	}
	neighbors, ok := stringMap(raw, input, path+".neighbor", issues)
	if !ok {
		return configuredGroups, nil
	}
	result := make([]BGPNeighborConfig, 0, len(neighbors))
	for name, rawNeighbor := range neighbors {
		neighborPath := path + ".neighbor." + name
		var entry jsonObject
		if err := json.Unmarshal(rawNeighbor, &entry); err != nil || entry == nil {
			addIssue(issues, input, neighborPath, "invalid_type", "expected neighbor object")
			continue
		}
		fact := BGPNeighborConfig{Name: name}
		fact.PeerGroup = stringField(entry, "peer-group", input, neighborPath, false, issues)
		fact.Type = stringField(entry, "type", input, neighborPath, false, issues)
		fact.RemoteAS = stringOrNumberField(entry, "remote-as", input, neighborPath, issues)
		if fact.PeerGroup != nil {
			if remoteAS, exists := groupAS[*fact.PeerGroup]; exists {
				fact.EffectiveRemoteAS = fact.RemoteAS
				if fact.EffectiveRemoteAS == nil {
					fact.EffectiveRemoteAS = remoteAS
				}
				fact.EffectiveAddressFamilies = append(fact.EffectiveAddressFamilies, groupAFs[*fact.PeerGroup]...)
			} else {
				addIssue(issues, input, neighborPath+".peer-group", "unresolved_reference", "neighbor references an absent peer group")
			}
		} else {
			fact.EffectiveRemoteAS = fact.RemoteAS
		}
		result = append(result, fact)
	}
	return configuredGroups, &result
}

func stringOrNumberField(obj jsonObject, key, input, path string, issues *[]Issue) *string {
	raw, ok := obj[key]
	if !ok {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return &text
	}
	var number uint64
	if err := json.Unmarshal(raw, &number); err != nil {
		addIssue(issues, input, path+"."+key, "invalid_type", "expected string or non-negative integer")
		return nil
	}
	text = strconv.FormatUint(number, 10)
	return &text
}

func parseGlobalRouter(raw []byte, out *Snapshot) {
	const input = "appliedRouter"
	root, ok := parseObject(raw, input, &out.Issues)
	if !ok {
		return
	}
	bgp, ok := objectField(root, "bgp", input, "$", &out.Issues)
	if !ok {
		return
	}
	fact := &BGPConfig{}
	fact.Enabled = enabledState(bgp, input, "$.bgp", &out.Issues)
	if rawASN, exists := bgp["autonomous-system"]; exists {
		fact.ASN, fact.ASNConfigured = parseASN(rawASN, input, "$.bgp.autonomous-system", &out.Issues)
	} else {
		addIssue(&out.Issues, input, "$.bgp.autonomous-system", "missing_field", "required field is absent")
	}
	out.Configured.BGP = fact
}

func parseConfiguredBridges(raw []byte, out *Snapshot) {
	const input = "appliedBridge"
	root, ok := parseObject(raw, input, &out.Issues)
	if !ok {
		return
	}
	domains, ok := objectField(root, "domain", input, "$", &out.Issues)
	if !ok {
		return
	}
	for name, rawDomain := range domains {
		path := "$.domain." + name
		var domain jsonObject
		if err := json.Unmarshal(rawDomain, &domain); err != nil || domain == nil {
			addIssue(&out.Issues, input, path, "invalid_type", "expected bridge domain object")
			continue
		}
		fact := BridgeConfig{Name: name}
		fact.Encapsulation = stringField(domain, "encap", input, path, false, &out.Issues)
		if ports, exists := optionalObjectField(domain, "port", input, path, &out.Issues); exists {
			for portName, rawPort := range ports {
				port := parseBridgePortConfig(portName, rawPort, input, path+".port."+portName, &out.Issues)
				fact.Ports = append(fact.Ports, port)
			}
		}
		out.Configured.Bridges = append(out.Configured.Bridges, fact)
	}
}

func parseBridgePortConfig(name string, raw json.RawMessage, input, path string, issues *[]Issue) BridgePortConfig {
	var obj jsonObject
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		addIssue(issues, input, path, "invalid_type", "expected bridge port object")
		return BridgePortConfig{Name: name}
	}
	return BridgePortConfig{Name: name, VLANs: parseVLANMap(obj, "vlan", input, path, issues)}
}

func parseVLANMap(obj jsonObject, key, input, path string, issues *[]Issue) []BridgeVLANConfig {
	vlans, ok := optionalObjectField(obj, key, input, path, issues)
	if !ok {
		return nil
	}
	out := make([]BridgeVLANConfig, 0, len(vlans))
	for idText, raw := range vlans {
		id, err := strconv.Atoi(idText)
		vlanPath := path + "." + key + "." + idText
		if err != nil || id < 1 || id > 4094 {
			addIssue(issues, input, vlanPath, "invalid_value", "VLAN id must be an integer from 1 through 4094")
			continue
		}
		var entry jsonObject
		if err := json.Unmarshal(raw, &entry); err != nil || entry == nil {
			addIssue(issues, input, vlanPath, "invalid_type", "expected VLAN object")
			continue
		}
		out = append(out, BridgeVLANConfig{ID: id, TagState: stringField(entry, "tag-state", input, vlanPath, false, issues), ForwardState: stringField(entry, "fwd-state", input, vlanPath, false, issues)})
	}
	return out
}

func parseOperationalInterfaces(raw []byte, out *Snapshot) {
	const input = "operationalInterfaces"
	root, ok := parseObject(raw, input, &out.Issues)
	if !ok {
		return
	}
	for name, rawEntry := range root {
		path := "$[" + strconv.Quote(name) + "]"
		var entry jsonObject
		if err := json.Unmarshal(rawEntry, &entry); err != nil || entry == nil {
			addIssue(&out.Issues, input, path, "invalid_type", "expected an interface object")
			continue
		}
		fact := InterfaceState{Name: name}
		fact.Type = stringField(entry, "type", input, path, false, &out.Issues)
		fact.VRF = stringField(entry, "vrf", input, path, false, &out.Issues)
		if link, exists := optionalObjectField(entry, "link", input, path, &out.Issues); exists {
			physical := fact.Type != nil && *fact.Type == "swp"
			fact.AdminState = stringField(link, "admin-status", input, path+".link", physical, &out.Issues)
			fact.OperState = stringField(link, "oper-status", input, path+".link", physical, &out.Issues)
			fact.MTU = intField(link, "mtu", input, path+".link", physical, &out.Issues)
			fact.Speed = stringField(link, "speed", input, path+".link", false, &out.Issues)
		} else if fact.Type != nil && *fact.Type == "swp" {
			addIssue(&out.Issues, input, path+".link", "missing_field", "switch port has no operational link state")
		}
		out.Operational.Interfaces = append(out.Operational.Interfaces, fact)
	}
}

func parseOperationalBridges(raw []byte, out *Snapshot) {
	const input = "operationalBridge"
	root, ok := parseObject(raw, input, &out.Issues)
	if !ok {
		return
	}
	domains, ok := objectField(root, "domain", input, "$", &out.Issues)
	if !ok {
		return
	}
	for name, rawDomain := range domains {
		path := "$.domain." + name
		var domain jsonObject
		if err := json.Unmarshal(rawDomain, &domain); err != nil || domain == nil {
			addIssue(&out.Issues, input, path, "invalid_type", "expected bridge domain object")
			continue
		}
		fact := BridgeState{Name: name}
		if ports, exists := optionalObjectField(domain, "port", input, path, &out.Issues); exists {
			for portName, rawPort := range ports {
				var entry jsonObject
				portPath := path + ".port." + portName
				if err := json.Unmarshal(rawPort, &entry); err != nil || entry == nil {
					addIssue(&out.Issues, input, portPath, "invalid_type", "expected bridge port object")
					continue
				}
				state := BridgePortState{Name: portName, State: stringField(entry, "state", input, portPath, false, &out.Issues)}
				for _, vlan := range parseVLANMap(entry, "vlan", input, portPath, &out.Issues) {
					state.VLANs = append(state.VLANs, BridgeVLANState(vlan))
				}
				fact.Ports = append(fact.Ports, state)
			}
		}
		out.Operational.Bridges = append(out.Operational.Bridges, fact)
	}
}

func parseLLDP(raw []byte, out *Snapshot) {
	const input = "lldp"
	_, ok := parseObject(raw, input, &out.Issues)
	if !ok {
		return
	}
	var lldp struct {
		LLDP []struct {
			Interfaces []struct {
				Name    string `json:"name" yaml:"name"`
				Chassis []struct {
					ID []struct {
						Value string `json:"value" yaml:"value"`
					} `json:"id" yaml:"id"`
					Name []struct {
						Value string `json:"value" yaml:"value"`
					} `json:"name" yaml:"name"`
				} `json:"chassis" yaml:"chassis"`
				Port []struct {
					ID []struct {
						Value string `json:"value" yaml:"value"`
					} `json:"id" yaml:"id"`
					Name []struct {
						Value string `json:"value" yaml:"value"`
					} `json:"name" yaml:"name"`
					Description []struct {
						Value string `json:"value" yaml:"value"`
					} `json:"descr" yaml:"descr"`
				} `json:"port" yaml:"port"`
			} `json:"interface" yaml:"interface"`
		} `json:"lldp" yaml:"lldp"`
	}
	if err := json.Unmarshal(raw, &lldp); err != nil {
		addIssue(&out.Issues, input, "$.lldp", "invalid_type", "expected lldp interface list")
		return
	}
	if lldp.LLDP == nil {
		addIssue(&out.Issues, input, "$.lldp", "missing_field", "LLDP interface list is absent")
		return
	}
	for _, group := range lldp.LLDP {
		if group.Interfaces == nil {
			addIssue(&out.Issues, input, "$.lldp.interface", "missing_field", "LLDP interface collection is absent or null")
			return
		}
		for _, iface := range group.Interfaces {
			fact := LLDPPeerHint{LocalInterface: iface.Name}
			if len(iface.Chassis) > 0 {
				if len(iface.Chassis[0].Name) > 0 {
					value := iface.Chassis[0].Name[0].Value
					fact.ChassisName = &value
				}
				if len(iface.Chassis[0].ID) > 0 {
					value := iface.Chassis[0].ID[0].Value
					fact.ChassisID = &value
				}
			}
			if len(iface.Port) == 1 {
				if len(iface.Port[0].ID) > 1 || len(iface.Port[0].Name) > 1 || len(iface.Port[0].Description) > 1 {
					addIssue(&out.Issues, input, "$.lldp.interface.port", "ambiguous_peer", "LLDP port reports multiple identity values")
					continue
				}
				if len(iface.Port[0].Description) == 1 {
					value := iface.Port[0].Description[0].Value
					fact.PortDescription = &value
				}
				if len(iface.Port[0].Name) > 0 {
					value := iface.Port[0].Name[0].Value
					fact.PortName = &value
				}
				if len(iface.Port[0].ID) > 0 {
					value := iface.Port[0].ID[0].Value
					fact.PortID = &value
				}
			}
			if len(iface.Port) > 1 {
				addIssue(&out.Issues, input, "$.lldp.interface.port", "ambiguous_peer", "LLDP interface reports multiple port identities")
				continue
			}
			if fact.LocalInterface == "" {
				addIssue(&out.Issues, input, "$.lldp.interface.name", "missing_field", "LLDP local interface name is absent")
				continue
			}
			out.Operational.LLDP = append(out.Operational.LLDP, fact)
		}
	}
}

func parseFRRSummary(raw []byte, out *Snapshot) {
	const input = "frrBGPSummary"
	root, ok := parseObject(raw, input, &out.Issues)
	if !ok {
		return
	}
	for vrf, rawVRF := range root {
		vrfPath := "$[" + strconv.Quote(vrf) + "]"
		var entry jsonObject
		if err := json.Unmarshal(rawVRF, &entry); err != nil || entry == nil {
			addIssue(&out.Issues, input, vrfPath, "invalid_type", "expected VRF summary object")
			continue
		}
		if len(entry) == 0 {
			// Empty VRF summary means no peers were returned for this VRF.
			out.Operational.BGP = append(out.Operational.BGP, BGPSession{VRF: vrf, Peers: []BGPPeerState{}})
			continue
		}
		families := make([]string, 0)
		for key, value := range entry {
			if key == "ipv4Unicast" || key == "ipv6Unicast" || key == "l2vpnEvpn" || key == "ipv4-unicast" || key == "ipv6-unicast" || key == "l2vpn-evpn" {
				families = append(families, key)
				_ = value
			}
		}
		if len(families) == 0 {
			addIssue(&out.Issues, input, vrfPath, "unsupported_shape", "no recognized BGP address-family summary")
			continue
		}
		sort.Strings(families)
		for _, family := range families {
			familyRaw := entry[family]
			var af jsonObject
			if err := json.Unmarshal(familyRaw, &af); err != nil || af == nil {
				addIssue(&out.Issues, input, vrfPath+"."+family, "invalid_type", "expected address-family summary object")
				continue
			}
			session := BGPSession{VRF: vrf, AddressFamily: family}
			session.RouterID = stringField(af, "routerId", input, vrfPath+"."+family, false, &out.Issues)
			session.LocalAS = uintASNField(af, "as", input, vrfPath+"."+family, &out.Issues)
			if peers, exists := af["peers"]; exists {
				peerMap, good := stringMap(peers, input, vrfPath+"."+family+".peers", &out.Issues)
				if good {
					session.Peers = make([]BGPPeerState, 0, len(peerMap))
					for peerName, peerRaw := range peerMap {
						var peer jsonObject
						peerPath := vrfPath + "." + family + ".peers." + peerName
						if err := json.Unmarshal(peerRaw, &peer); err != nil || peer == nil {
							addIssue(&out.Issues, input, peerPath, "invalid_type", "expected BGP peer object")
							continue
						}
						fact := BGPPeerState{Name: peerName}
						if _, exists := peer["remoteAs"]; !exists {
							addIssue(&out.Issues, input, peerPath+".remoteAs", "missing_field", "peer remote ASN is absent")
						} else {
							fact.RemoteAS = uintASNField(peer, "remoteAs", input, peerPath, &out.Issues)
						}
						fact.State = stringField(peer, "state", input, peerPath, true, &out.Issues)
						fact.PrefixesReceived = uintField(peer, "prefixReceived", input, peerPath, 64, &out.Issues)
						fact.PrefixesAccepted = uintField(peer, "prefixAccepted", input, peerPath, 64, &out.Issues)
						fact.PrefixesSent = uintField(peer, "prefixSent", input, peerPath, 64, &out.Issues)
						session.Peers = append(session.Peers, fact)
					}
				}
			} else {
				addIssue(&out.Issues, input, vrfPath+"."+family+".peers", "missing_field", "peer map is absent")
			}
			out.Operational.BGP = append(out.Operational.BGP, session)
		}
	}
}

func uintASNField(obj jsonObject, key, input, path string, issues *[]Issue) *uint32 {
	value := uintField(obj, key, input, path, 32, issues)
	if value == nil {
		return nil
	}
	n := uint32(*value)
	return &n
}

func sortSnapshot(out *Snapshot) {
	sort.Slice(out.Configured.Interfaces, func(i, j int) bool { return out.Configured.Interfaces[i].Name < out.Configured.Interfaces[j].Name })
	sort.Slice(out.Configured.VRFs, func(i, j int) bool { return out.Configured.VRFs[i].Name < out.Configured.VRFs[j].Name })
	sort.Slice(out.Configured.Bridges, func(i, j int) bool { return out.Configured.Bridges[i].Name < out.Configured.Bridges[j].Name })
	sort.Slice(out.Operational.Interfaces, func(i, j int) bool { return out.Operational.Interfaces[i].Name < out.Operational.Interfaces[j].Name })
	sort.Slice(out.Operational.Bridges, func(i, j int) bool { return out.Operational.Bridges[i].Name < out.Operational.Bridges[j].Name })
	if out.Operational.Addresses != nil {
		sort.Slice(*out.Operational.Addresses, func(i, j int) bool {
			a, b := (*out.Operational.Addresses)[i], (*out.Operational.Addresses)[j]
			if a.Interface != b.Interface {
				return a.Interface < b.Interface
			}
			if a.Family != b.Family {
				return a.Family < b.Family
			}
			return a.Prefix < b.Prefix
		})
	}
	sortRoutes := func(observed *[]Route) {
		if observed == nil {
			return
		}
		routes := *observed
		sort.Slice(routes, func(i, j int) bool {
			a, b := routes[i], routes[j]
			if a.Table != b.Table {
				return a.Table < b.Table
			}
			if a.Prefix != b.Prefix {
				return a.Prefix < b.Prefix
			}
			if a.Type != b.Type {
				return a.Type < b.Type
			}
			return a.Protocol < b.Protocol
		})
	}
	sortRoutes(out.Operational.IPv4Routes)
	sortRoutes(out.Operational.IPv6Routes)
	sort.Slice(out.Operational.LLDP, func(i, j int) bool {
		return out.Operational.LLDP[i].LocalInterface < out.Operational.LLDP[j].LocalInterface
	})
	sort.Slice(out.Operational.BGP, func(i, j int) bool {
		if out.Operational.BGP[i].VRF != out.Operational.BGP[j].VRF {
			return out.Operational.BGP[i].VRF < out.Operational.BGP[j].VRF
		}
		return out.Operational.BGP[i].AddressFamily < out.Operational.BGP[j].AddressFamily
	})
	for i := range out.Configured.VRFs {
		vrf := &out.Configured.VRFs[i]
		sort.Slice(vrf.AddressFamilies, func(i, j int) bool { return vrf.AddressFamilies[i].Name < vrf.AddressFamilies[j].Name })
		sort.Slice(vrf.PeerGroups, func(i, j int) bool { return vrf.PeerGroups[i].Name < vrf.PeerGroups[j].Name })
		for j := range vrf.PeerGroups {
			sort.Strings(vrf.PeerGroups[j].AddressFamilies)
		}
		for j := range vrf.AddressFamilies {
			sort.Slice(vrf.AddressFamilies[j].Redistribute, func(a, b int) bool {
				return vrf.AddressFamilies[j].Redistribute[a].Source < vrf.AddressFamilies[j].Redistribute[b].Source
			})
		}
		if vrf.Neighbors != nil {
			sort.Slice(*vrf.Neighbors, func(i, j int) bool { return (*vrf.Neighbors)[i].Name < (*vrf.Neighbors)[j].Name })
		}
	}
	for i := range out.Configured.Bridges {
		sort.Slice(out.Configured.Bridges[i].Ports, func(a, b int) bool {
			return out.Configured.Bridges[i].Ports[a].Name < out.Configured.Bridges[i].Ports[b].Name
		})
		for j := range out.Configured.Bridges[i].Ports {
			sort.Slice(out.Configured.Bridges[i].Ports[j].VLANs, func(a, b int) bool {
				return out.Configured.Bridges[i].Ports[j].VLANs[a].ID < out.Configured.Bridges[i].Ports[j].VLANs[b].ID
			})
		}
	}
	for i := range out.Operational.Bridges {
		sort.Slice(out.Operational.Bridges[i].Ports, func(a, b int) bool {
			return out.Operational.Bridges[i].Ports[a].Name < out.Operational.Bridges[i].Ports[b].Name
		})
		for j := range out.Operational.Bridges[i].Ports {
			sort.Slice(out.Operational.Bridges[i].Ports[j].VLANs, func(a, b int) bool {
				return out.Operational.Bridges[i].Ports[j].VLANs[a].ID < out.Operational.Bridges[i].Ports[j].VLANs[b].ID
			})
		}
	}
	for i := range out.Operational.BGP {
		sort.Slice(out.Operational.BGP[i].Peers, func(a, b int) bool {
			return out.Operational.BGP[i].Peers[a].Name < out.Operational.BGP[i].Peers[b].Name
		})
	}
	sort.Slice(out.Issues, func(i, j int) bool {
		a, b := out.Issues[i], out.Issues[j]
		if a.Input != b.Input {
			return a.Input < b.Input
		}
		if a.Field != b.Field {
			return a.Field < b.Field
		}
		return a.Code < b.Code
	})
	// Duplicate issue keys indicate the same malformed shape was reached by
	// multiple parsing branches. Keep the first deterministic diagnosis.
	if len(out.Issues) > 1 {
		unique := out.Issues[:1]
		for _, issue := range out.Issues[1:] {
			last := unique[len(unique)-1]
			if issue.Input != last.Input || issue.Field != last.Field || issue.Code != last.Code {
				unique = append(unique, issue)
			}
		}
		out.Issues = unique
	}
}
