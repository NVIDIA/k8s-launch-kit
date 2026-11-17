// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"bufio"
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	maxFRRConfigBytes = 1 << 20
	maxFRRConfigLines = 16384
	maxFRRLineBytes   = 4096
)

// FRRPolicyConfig contains only bounded router and peer facts needed by HBN
// readiness. Raw command text and peer argument values outside typed facts are
// deliberately not retained.
type FRRPolicyConfig struct {
	Defaults           *string        `json:"defaults,omitempty" yaml:"defaults,omitempty"`
	EBGPRequiresPolicy *bool          `json:"ebgpRequiresPolicy,omitempty" yaml:"ebgpRequiresPolicy,omitempty"`
	VRFs               []FRRPolicyVRF `json:"vrfs" yaml:"vrfs"`
	Complete           bool           `json:"complete" yaml:"complete"`
}

type FRRPolicyVRF struct {
	Name               string               `json:"name" yaml:"name"`
	LocalASN           *uint32              `json:"localAsn,omitempty" yaml:"localAsn,omitempty"`
	EBGPRequiresPolicy *bool                `json:"ebgpRequiresPolicy,omitempty" yaml:"ebgpRequiresPolicy,omitempty"`
	IPv4UnicastDefault *bool                `json:"ipv4UnicastDefault,omitempty" yaml:"ipv4UnicastDefault,omitempty"`
	BGPShutdown        *bool                `json:"bgpShutdown,omitempty" yaml:"bgpShutdown,omitempty"`
	Complete           bool                 `json:"complete" yaml:"complete"`
	Neighbors          []FRRPolicyNeighbor  `json:"neighbors" yaml:"neighbors"`
	PeerGroups         []FRRPolicyPeerGroup `json:"peerGroups,omitempty" yaml:"peerGroups,omitempty"`
}

type FRRPolicyNeighbor struct {
	Name                 string   `json:"name" yaml:"name"`
	PeerGroup            *string  `json:"peerGroup,omitempty" yaml:"peerGroup,omitempty"`
	RemoteAS             *string  `json:"remoteAs,omitempty" yaml:"remoteAs,omitempty"`
	Interface            bool     `json:"interface,omitempty" yaml:"interface,omitempty"`
	IPv4UnicastActivated *bool    `json:"ipv4UnicastActivated,omitempty" yaml:"ipv4UnicastActivated,omitempty"`
	Shutdown             *bool    `json:"shutdown,omitempty" yaml:"shutdown,omitempty"`
	InboundFilter        bool     `json:"inboundFilter,omitempty" yaml:"inboundFilter,omitempty"`
	OutboundFilter       bool     `json:"outboundFilter,omitempty" yaml:"outboundFilter,omitempty"`
	UnsupportedPolicy    bool     `json:"unsupportedPolicy,omitempty" yaml:"unsupportedPolicy,omitempty"`
	UnsupportedSettings  []string `json:"unsupportedSettings,omitempty" yaml:"unsupportedSettings,omitempty"`
}

type FRRPolicyPeerGroup struct {
	Name                 string   `json:"name" yaml:"name"`
	RemoteAS             *string  `json:"remoteAs,omitempty" yaml:"remoteAs,omitempty"`
	IPv4UnicastActivated *bool    `json:"ipv4UnicastActivated,omitempty" yaml:"ipv4UnicastActivated,omitempty"`
	Shutdown             *bool    `json:"shutdown,omitempty" yaml:"shutdown,omitempty"`
	InboundFilter        bool     `json:"inboundFilter,omitempty" yaml:"inboundFilter,omitempty"`
	OutboundFilter       bool     `json:"outboundFilter,omitempty" yaml:"outboundFilter,omitempty"`
	UnsupportedPolicy    bool     `json:"unsupportedPolicy,omitempty" yaml:"unsupportedPolicy,omitempty"`
	UnsupportedSettings  []string `json:"unsupportedSettings,omitempty" yaml:"unsupportedSettings,omitempty"`
}

type frrPolicyContext struct {
	vrfName        string
	localASN       *uint32
	ebgp           *bool
	complete       bool
	neighbors      map[string]*FRRPolicyNeighbor
	groups         map[string]*FRRPolicyPeerGroup
	policies       map[string]*FRRPolicyPeerGroup
	peerActivation map[string]*bool
	peerShutdown   map[string]*bool
	peerRemoteAS   map[string]*string
	ipv4Default    *bool
	bgpShutdown    *bool
}

func parseFRRRunningBGP(raw []byte, out *Snapshot) {
	const input = "frrRunningBGP"
	if len(raw) == 0 {
		addIssue(&out.Issues, input, "$", "missing_output", "FRR running BGP configuration is empty")
		return
	}
	if len(raw) > maxFRRConfigBytes {
		addIssue(&out.Issues, input, "$", "output_limit", "FRR running BGP configuration exceeds the supported size")
		return
	}

	policy := &FRRPolicyConfig{VRFs: []FRRPolicyVRF{}, Complete: true}
	contexts := map[string]*frrPolicyContext{}
	var current *frrPolicyContext
	addressFamily := ""
	lineNumber := 0
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 1024), maxFRRLineBytes)
	for scanner.Scan() {
		lineNumber++
		if lineNumber > maxFRRConfigLines {
			addIssue(&out.Issues, input, "$", "line_limit", "FRR running BGP configuration exceeds the supported line count")
			policy.Complete = false
			break
		}
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "!") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		if indent == 0 {
			current = nil
			addressFamily = ""
			if len(fields) == 3 && fields[0] == "frr" && fields[1] == "defaults" {
				mode := fields[2]
				if mode != "datacenter" && mode != "traditional" {
					addIssue(&out.Issues, input, "$", "unsupported_defaults", "FRR defaults mode is not supported for routing-policy evaluation")
					policy.Complete = false
					continue
				}
				if policy.Defaults != nil && *policy.Defaults != mode {
					addIssue(&out.Issues, input, "$", "conflicting_defaults", "FRR configuration contains conflicting defaults modes")
					policy.Complete = false
					continue
				}
				policy.Defaults = &mode
				if mode == "datacenter" {
					v := false
					policy.EBGPRequiresPolicy = &v
				}
				continue
			}
			if len(fields) >= 3 && fields[0] == "router" && fields[1] == "bgp" {
				vrfName := "default"
				if len(fields) == 5 && fields[3] == "vrf" {
					vrfName = fields[4]
				} else if len(fields) != 3 {
					addIssue(&out.Issues, input, fmt.Sprintf("$[%d]", lineNumber), "unsupported_router_context", "FRR router context could not be resolved")
					policy.Complete = false
					continue
				}
				asn, err := strconv.ParseUint(fields[2], 10, 32)
				if err != nil || asn == 0 {
					addIssue(&out.Issues, input, fmt.Sprintf("$[%d]", lineNumber), "invalid_local_asn", "FRR BGP router ASN is malformed")
					policy.Complete = false
					continue
				}
				if contexts[vrfName] != nil {
					addIssue(&out.Issues, input, "$", "duplicate_router_context", "FRR configuration contains duplicate BGP router contexts")
					policy.Complete = false
					continue
				}
				ipv4Default := true // FRR's documented default is IPv4 unicast enabled; `no bgp default ipv4-unicast` is emitted explicitly.
				bgpShutdown := false
				localASN := uint32(asn)
				ctx := &frrPolicyContext{
					vrfName: vrfName, localASN: &localASN, complete: true,
					neighbors: map[string]*FRRPolicyNeighbor{}, groups: map[string]*FRRPolicyPeerGroup{}, policies: map[string]*FRRPolicyPeerGroup{},
					peerActivation: map[string]*bool{}, peerShutdown: map[string]*bool{}, peerRemoteAS: map[string]*string{}, ipv4Default: &ipv4Default, bgpShutdown: &bgpShutdown,
				}
				contexts[vrfName] = ctx
				current = ctx
				continue
			}
			continue
		}
		if current == nil {
			continue
		}
		if fields[0] == "address-family" {
			addressFamily = normalizeFRRAddressFamily(fields[1:])
			if addressFamily == "unsupported" {
				current.complete = false
				addressFamily = "other"
			}
			continue
		}
		if fields[0] == "exit-address-family" {
			addressFamily = ""
			continue
		}
		if recognized, valid := parseFRRRouterActivation(fields, addressFamily, current); recognized {
			if !valid {
				current.complete = false
			}
			continue
		}
		value, recognized, valid := parseEBGPRequiresPolicy(fields)
		if recognized {
			if !valid {
				current.complete = false
				continue
			}
			if addressFamily != "" {
				current.complete = false
				continue
			}
			current.ebgp = &value
			continue
		}
		if fields[0] == "neighbor" {
			if !parseFRRNeighborPolicy(fields[1:], addressFamily, current, false) {
				// A malformed directive makes this VRF unknown without blocking
				// checks for selected peers in unrelated VRFs.
				current.complete = false
			}
		} else if len(fields) >= 2 && fields[0] == "no" && fields[1] == "neighbor" {
			if !parseFRRNeighborPolicy(fields[2:], addressFamily, current, true) {
				current.complete = false
			}
		}
	}
	if err := scanner.Err(); err != nil {
		addIssue(&out.Issues, input, "$", "line_limit", "FRR running BGP configuration contains an oversized line")
		policy.Complete = false
	}
	if policy.Defaults == nil {
		addIssue(&out.Issues, input, "$.defaults", "missing_field", "FRR defaults mode is absent")
		policy.Complete = false
	}
	vrfNames := make([]string, 0, len(contexts))
	for name := range contexts {
		vrfNames = append(vrfNames, name)
	}
	sort.Strings(vrfNames)
	for _, name := range vrfNames {
		ctx := contexts[name]
		vrf := FRRPolicyVRF{
			Name: ctx.vrfName, LocalASN: ctx.localASN, EBGPRequiresPolicy: ctx.ebgp, IPv4UnicastDefault: ctx.ipv4Default, BGPShutdown: ctx.bgpShutdown,
			Complete: ctx.complete, Neighbors: []FRRPolicyNeighbor{}, PeerGroups: []FRRPolicyPeerGroup{},
		}
		for policyName, policyFacts := range ctx.policies {
			if group := ctx.groups[policyName]; group != nil {
				group.InboundFilter = policyFacts.InboundFilter
				group.OutboundFilter = policyFacts.OutboundFilter
				group.UnsupportedPolicy = policyFacts.UnsupportedPolicy
				group.UnsupportedSettings = append(group.UnsupportedSettings, policyFacts.UnsupportedSettings...)
				continue
			}
			neighbor := frrNeighbor(ctx, policyName)
			neighbor.InboundFilter = policyFacts.InboundFilter
			neighbor.OutboundFilter = policyFacts.OutboundFilter
			neighbor.UnsupportedPolicy = policyFacts.UnsupportedPolicy
			neighbor.UnsupportedSettings = append(neighbor.UnsupportedSettings, policyFacts.UnsupportedSettings...)
		}
		for peerName, active := range ctx.peerActivation {
			if group := ctx.groups[peerName]; group != nil {
				group.IPv4UnicastActivated = active
			} else {
				frrNeighbor(ctx, peerName).IPv4UnicastActivated = active
			}
		}
		for peerName, shutdown := range ctx.peerShutdown {
			if group := ctx.groups[peerName]; group != nil {
				group.Shutdown = shutdown
			} else {
				frrNeighbor(ctx, peerName).Shutdown = shutdown
			}
		}
		for peerName, remoteAS := range ctx.peerRemoteAS {
			if group := ctx.groups[peerName]; group != nil {
				group.RemoteAS = remoteAS
			} else {
				frrNeighbor(ctx, peerName).RemoteAS = remoteAS
			}
		}
		for _, neighbor := range ctx.neighbors {
			vrf.Neighbors = append(vrf.Neighbors, *neighbor)
		}
		for _, group := range ctx.groups {
			vrf.PeerGroups = append(vrf.PeerGroups, *group)
		}
		sort.Slice(vrf.Neighbors, func(i, j int) bool { return vrf.Neighbors[i].Name < vrf.Neighbors[j].Name })
		sort.Slice(vrf.PeerGroups, func(i, j int) bool { return vrf.PeerGroups[i].Name < vrf.PeerGroups[j].Name })
		policy.VRFs = append(policy.VRFs, vrf)
	}
	out.Configured.FRRPolicy = policy
}

func normalizeFRRAddressFamily(fields []string) string {
	if len(fields) == 2 && fields[0] == "ipv4" && fields[1] == "unicast" {
		return "ipv4-unicast"
	}
	if len(fields) == 2 && fields[0] == "ipv6" && fields[1] == "unicast" {
		return "ipv6-unicast"
	}
	if len(fields) == 2 && fields[0] == "l2vpn" && fields[1] == "evpn" {
		return "l2vpn-evpn"
	}
	return "unsupported"
}

func parseEBGPRequiresPolicy(fields []string) (value bool, recognized, valid bool) {
	if len(fields) == 2 && fields[0] == "bgp" && fields[1] == "ebgp-requires-policy" {
		return true, true, true
	}
	if len(fields) >= 3 && fields[0] == "no" && fields[1] == "bgp" && fields[2] == "ebgp-requires-policy" {
		return false, true, len(fields) == 3
	}
	if len(fields) >= 2 && fields[0] == "bgp" && fields[1] == "ebgp-requires-policy" {
		if len(fields) != 3 {
			return false, true, false
		}
		value, err := strconv.ParseBool(fields[2])
		if err == nil {
			return value, true, true
		}
		return false, true, false
	}
	return false, false, false
}

func parseFRRRouterActivation(fields []string, af string, ctx *frrPolicyContext) (recognized, valid bool) {
	negated := len(fields) > 0 && fields[0] == "no"
	command := fields
	if negated {
		command = fields[1:]
	}
	if len(command) >= 2 && command[0] == "bgp" && command[1] == "default" {
		if len(command) < 3 || command[2] != "ipv4-unicast" {
			return false, false
		}
		if af != "" || len(command) != 3 {
			return true, false
		}
		value := !negated
		ctx.ipv4Default = &value
		return true, true
	}
	if len(command) >= 2 && command[0] == "bgp" && command[1] == "shutdown" {
		if af != "" || len(command) != 2 {
			return true, false
		}
		value := !negated
		ctx.bgpShutdown = &value
		return true, true
	}
	return false, false
}

func parseFRRNeighborPolicy(fields []string, af string, ctx *frrPolicyContext, negated bool) bool {
	if len(fields) < 2 || !validFRRName(fields[0]) {
		return false
	}
	name, rest := fields[0], fields[1:]
	unsupported := func() bool {
		markFRRUnsupportedSetting(name, unsupportedFRRSettingCategory(rest), ctx)
		return true
	}
	if rest[0] == "activate" {
		if len(rest) != 1 {
			return unsupported()
		}
		// IPv4 unicast is FRR's default address family. An activation or
		// negation outside an explicit AF therefore refers to this default.
		// IPv6 and other AF directives do not affect the selected IPv4 check.
		if af == "" || af == "ipv4-unicast" {
			value := !negated
			ctx.peerActivation[name] = &value
		}
		return true
	}
	if rest[0] == "shutdown" {
		if negated && len(rest) != 1 {
			return unsupported()
		}
		value := !negated
		ctx.peerShutdown[name] = &value
		return true
	}
	if rest[0] == "remote-as" {
		if negated || af != "" || len(rest) != 2 || !validFRRRemoteAS(rest[1]) {
			return unsupported()
		}
		remoteAS := rest[1]
		ctx.peerRemoteAS[name] = &remoteAS
		return true
	}
	if rest[0] == "description" {
		if !negated && len(rest) >= 2 {
			return true // Descriptive text does not affect peer establishment or route policy.
		}
		return unsupported()
	}
	if rest[0] == "advertisement-interval" {
		if !negated && af == "" && len(rest) == 2 && validFRRUint(rest[1], 600) {
			return true
		}
		return unsupported()
	}
	if rest[0] == "timers" {
		if !negated && af == "" && validFRRTimers(rest[1:]) {
			return true
		}
		return unsupported()
	}
	if negated {
		return unsupported()
	}
	if rest[0] == "peer-group" {
		if len(rest) == 1 {
			if ctx.groups[name] == nil {
				ctx.groups[name] = &FRRPolicyPeerGroup{Name: name}
			}
			return true
		}
		if len(rest) == 2 {
			if !validFRRName(rest[1]) {
				return unsupported()
			}
			neighbor := frrNeighbor(ctx, name)
			if neighbor.PeerGroup != nil && *neighbor.PeerGroup != rest[1] {
				return unsupported()
			}
			group := rest[1]
			neighbor.PeerGroup = &group
			return true
		}
		return unsupported()
	}
	if rest[0] == "interface" {
		// `neighbor <ifname> interface peer-group <group>` is an unnumbered
		// neighbor declaration, not a route filter.
		if len(rest) == 1 {
			frrNeighbor(ctx, name).Interface = true
			return true
		}
		if len(rest) == 3 && rest[1] == "peer-group" {
			if !validFRRName(rest[2]) {
				return unsupported()
			}
			neighbor := frrNeighbor(ctx, name)
			group := rest[2]
			if neighbor.PeerGroup != nil && *neighbor.PeerGroup != group {
				return unsupported()
			}
			neighbor.PeerGroup = &group
			neighbor.Interface = true
			return true
		}
		if len(rest) == 3 && rest[1] == "remote-as" && validFRRRemoteAS(rest[2]) && af == "" {
			neighbor := frrNeighbor(ctx, name)
			neighbor.Interface = true
			remoteAS := rest[2]
			neighbor.RemoteAS = &remoteAS
			return true
		}
		return unsupported()
	}
	if rest[0] == "route-map" || rest[0] == "prefix-list" || rest[0] == "distribute-list" || rest[0] == "filter-list" {
		if len(rest) != 3 || (rest[2] != "in" && rest[2] != "out") {
			return unsupported()
		}
		markFRRFilter(name, rest[2], af, ctx)
		return true
	}
	if rest[0] == "unsuppress-map" || (rest[0] == "default-originate" && len(rest) >= 2 && rest[1] == "route-map") {
		markFRRUnsupportedPolicy(name, af, ctx)
		markFRRUnsupportedSetting(name, "routing-policy", ctx)
		return true
	}
	// The parser only proves a deliberately small set of baseline settings.
	// Authentication, local-AS, transport, and other peer options can change
	// whether the generated unnumbered eBGP session can establish. Keep those
	// as a scoped boolean; never retain their arguments (which may be secrets).
	return unsupported()
}

func validFRRRemoteAS(value string) bool {
	if value == "external" || value == "internal" {
		return true
	}
	return validFRRUint(value, ^uint32(0)) && value != "0"
}

func validFRRUint(value string, maximum uint32) bool {
	parsed, err := strconv.ParseUint(value, 10, 32)
	return err == nil && parsed <= uint64(maximum)
}

func validFRRTimers(fields []string) bool {
	if len(fields) != 2 {
		return false
	}
	if fields[0] == "connect" {
		interval, err := strconv.ParseUint(fields[1], 10, 16)
		return err == nil && interval > 0
	}
	return validFRRUint(fields[0], 65535) && validFRRUint(fields[1], 65535)
}

func validFRRName(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

func frrNeighbor(ctx *frrPolicyContext, name string) *FRRPolicyNeighbor {
	if ctx.neighbors[name] == nil {
		ctx.neighbors[name] = &FRRPolicyNeighbor{Name: name}
	}
	return ctx.neighbors[name]
}

func markFRRFilter(name, direction, af string, ctx *frrPolicyContext) {
	if af != "" && af != "ipv4-unicast" {
		return
	}
	policy := ctx.policies[name]
	if policy == nil {
		policy = &FRRPolicyPeerGroup{Name: name}
		ctx.policies[name] = policy
	}
	setFRRFilter(&policy.InboundFilter, &policy.OutboundFilter, direction)
}

func markFRRUnsupportedPolicy(name, af string, ctx *frrPolicyContext) {
	if af != "" && af != "ipv4-unicast" {
		return
	}
	policy := ctx.policies[name]
	if policy == nil {
		policy = &FRRPolicyPeerGroup{Name: name}
		ctx.policies[name] = policy
	}
	policy.UnsupportedPolicy = true
}

func markFRRUnsupportedSetting(name, category string, ctx *frrPolicyContext) {
	policy := ctx.policies[name]
	if policy == nil {
		policy = &FRRPolicyPeerGroup{Name: name}
		ctx.policies[name] = policy
	}
	policy.UnsupportedPolicy = true
	for _, existing := range policy.UnsupportedSettings {
		if existing == category {
			return
		}
	}
	if len(policy.UnsupportedSettings) < 5 {
		policy.UnsupportedSettings = append(policy.UnsupportedSettings, category)
		sort.Strings(policy.UnsupportedSettings)
	}
}

func unsupportedFRRSettingCategory(fields []string) string {
	if len(fields) == 0 {
		return "other"
	}
	switch fields[0] {
	case "password":
		return "authentication"
	case "local-as":
		return "local-as"
	case "remote-as":
		return "asn"
	case "interface":
		if len(fields) > 1 && fields[1] == "remote-as" {
			return "asn"
		}
		return "other"
	case "update-source", "port", "ebgp-multihop", "ttl-security", "connect", "capability", "bfd", "transport":
		return "transport"
	case "route-map", "prefix-list", "distribute-list", "filter-list", "unsuppress-map", "default-originate":
		return "routing-policy"
	default:
		return "other"
	}
}

func setFRRFilter(inbound, outbound *bool, direction string) {
	if direction == "in" {
		*inbound = true
	} else {
		*outbound = true
	}
}
