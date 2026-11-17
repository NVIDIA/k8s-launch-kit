// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package hbnruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
)

// FIBRouteDocument is the parsed `show ip route <prefix> json` response.
type FIBRouteDocument struct {
	PrefixKey string
	Routes    []FIBRoute
}

// FIBRoute contains only the FRR fields needed to establish route selection
// and kernel FIB installation. Unknown telemetry is ignored.
type FIBRoute struct {
	Protocol     string
	Prefix       string
	PrefixLen    uint32
	VRFName      string
	VRFID        uint32
	Table        uint32
	Selected     *bool
	DestSelected *bool
	Installed    *bool
	Nexthops     []FIBNexthop
}

// FIBNexthop contains FRR's address (which may be IPv6 link-local), flags, and
// outgoing interface for one path.
type FIBNexthop struct {
	IP            string
	InterfaceName string
	Active        bool
	FIB           bool
}

// FIBExpectation is the explicit route and set of permissible egress ports.
type FIBExpectation struct {
	Prefix           string
	EgressInterfaces []string
}

// FIBMismatch is a stable, compact reason a parsed route did not qualify.
type FIBMismatch struct {
	Reason        string
	InterfaceName string
}

// ParseFIBRoute parses bounded FRR route JSON and rejects duplicate keys via
// checkJSON. The result is independent of any expected prefix or port policy.
func ParseFIBRoute(data []byte) (FIBRouteDocument, error) {
	if len(data) > MaxSummaryBytes {
		return FIBRouteDocument{}, errors.New("route response exceeds 1 MiB limit")
	}
	if err := checkJSON(data); err != nil {
		return FIBRouteDocument{}, fmt.Errorf("invalid route JSON: %w", err)
	}
	var root map[string]json.RawMessage
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&root); err != nil {
		return FIBRouteDocument{}, fmt.Errorf("decode route response: %w", err)
	}
	if len(root) != 1 {
		return FIBRouteDocument{}, errors.New("route response must contain exactly one prefix key")
	}
	var key string
	var routesRaw json.RawMessage
	for k, v := range root {
		key, routesRaw = k, v
	}
	if _, err := canonicalIPv4Prefix(key); err != nil {
		return FIBRouteDocument{}, fmt.Errorf("invalid route prefix key: %w", err)
	}
	if len(routesRaw) == 0 || routesRaw[0] != '[' {
		return FIBRouteDocument{}, errors.New("route prefix value must be an array")
	}
	var rawRoutes []json.RawMessage
	if err := json.Unmarshal(routesRaw, &rawRoutes); err != nil || rawRoutes == nil {
		return FIBRouteDocument{}, errors.New("route prefix value must be an array")
	}
	doc := FIBRouteDocument{PrefixKey: key, Routes: make([]FIBRoute, 0, len(rawRoutes))}
	for i, raw := range rawRoutes {
		route, err := parseFIBRoute(raw)
		if err != nil {
			return FIBRouteDocument{}, fmt.Errorf("route %d: %w", i, err)
		}
		doc.Routes = append(doc.Routes, route)
	}
	return doc, nil
}

type rawFIBRoute struct {
	Protocol     json.RawMessage `json:"protocol"`
	Prefix       json.RawMessage `json:"prefix"`
	PrefixLen    json.RawMessage `json:"prefixLen"`
	VRFName      json.RawMessage `json:"vrfName"`
	VRFID        json.RawMessage `json:"vrfId"`
	Table        json.RawMessage `json:"table"`
	Selected     json.RawMessage `json:"selected"`
	DestSelected json.RawMessage `json:"destSelected"`
	Installed    json.RawMessage `json:"installed"`
	Nexthops     json.RawMessage `json:"nexthops"`
}

func parseFIBRoute(raw json.RawMessage) (FIBRoute, error) {
	var fields rawFIBRoute
	if err := json.Unmarshal(raw, &fields); err != nil {
		return FIBRoute{}, err
	}
	protocol, err := requiredString(fields.Protocol)
	if err != nil {
		return FIBRoute{}, fmt.Errorf("invalid protocol: %w", err)
	}
	prefix, err := requiredString(fields.Prefix)
	if err != nil {
		return FIBRoute{}, fmt.Errorf("invalid prefix: %w", err)
	}
	if _, err = canonicalIPv4Prefix(prefix); err != nil {
		return FIBRoute{}, fmt.Errorf("invalid prefix: %w", err)
	}
	prefixLen, err := requiredNumber(fields.PrefixLen, true)
	if err != nil {
		return FIBRoute{}, fmt.Errorf("invalid prefixLen: %w", err)
	}
	vrfName, err := requiredString(fields.VRFName)
	if err != nil {
		return FIBRoute{}, fmt.Errorf("invalid vrfName: %w", err)
	}
	vrfID, err := requiredNumber(fields.VRFID, true)
	if err != nil {
		return FIBRoute{}, fmt.Errorf("invalid vrfId: %w", err)
	}
	table, err := requiredNumber(fields.Table, true)
	if err != nil {
		return FIBRoute{}, fmt.Errorf("invalid table: %w", err)
	}
	selected, err := optionalBool(fields.Selected)
	if err != nil {
		return FIBRoute{}, fmt.Errorf("invalid selected: %w", err)
	}
	destSelected, err := optionalBool(fields.DestSelected)
	if err != nil {
		return FIBRoute{}, fmt.Errorf("invalid destSelected: %w", err)
	}
	installed, err := optionalBool(fields.Installed)
	if err != nil {
		return FIBRoute{}, fmt.Errorf("invalid installed: %w", err)
	}
	if len(fields.Nexthops) == 0 || bytes.Equal(fields.Nexthops, []byte("null")) || fields.Nexthops[0] != '[' {
		return FIBRoute{}, errors.New("nexthops must be an array")
	}
	var rawNexthops []json.RawMessage
	if err := json.Unmarshal(fields.Nexthops, &rawNexthops); err != nil || rawNexthops == nil {
		return FIBRoute{}, errors.New("nexthops must be an array")
	}
	nexthops := make([]FIBNexthop, 0, len(rawNexthops))
	for i, rawNexthop := range rawNexthops {
		nexthop, err := parseFIBNexthop(rawNexthop)
		if err != nil {
			return FIBRoute{}, fmt.Errorf("nexthop %d: %w", i, err)
		}
		nexthops = append(nexthops, nexthop)
	}
	return FIBRoute{
		Protocol: protocol, Prefix: prefix, PrefixLen: prefixLen,
		VRFName: vrfName, VRFID: vrfID, Table: table,
		Selected: selected, DestSelected: destSelected, Installed: installed,
		Nexthops: nexthops,
	}, nil
}

type rawFIBNexthop struct {
	IP            json.RawMessage `json:"ip"`
	InterfaceName json.RawMessage `json:"interfaceName"`
	Active        json.RawMessage `json:"active"`
	FIB           json.RawMessage `json:"fib"`
}

func parseFIBNexthop(raw json.RawMessage) (FIBNexthop, error) {
	var fields rawFIBNexthop
	if err := json.Unmarshal(raw, &fields); err != nil {
		return FIBNexthop{}, err
	}
	ip, err := optionalIP(fields.IP)
	if err != nil {
		return FIBNexthop{}, fmt.Errorf("invalid ip: %w", err)
	}
	interfaceName, err := optionalString(fields.InterfaceName)
	if err != nil {
		return FIBNexthop{}, fmt.Errorf("invalid interfaceName: %w", err)
	}
	active, err := requiredBool(fields.Active)
	if err != nil {
		return FIBNexthop{}, fmt.Errorf("invalid active: %w", err)
	}
	fib, err := requiredBool(fields.FIB)
	if err != nil {
		return FIBNexthop{}, fmt.Errorf("invalid fib: %w", err)
	}
	return FIBNexthop{IP: ip, InterfaceName: interfaceName, Active: active, FIB: fib}, nil
}

func requiredString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", errors.New("required string is missing or null")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", errors.New("expected nonempty string")
	}
	return value, nil
}

func optionalString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	if bytes.Equal(raw, []byte("null")) {
		return "", errors.New("null is not accepted")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("expected string")
	}
	return value, nil
}

func optionalIP(raw json.RawMessage) (string, error) {
	value, err := optionalString(raw)
	if err != nil || value == "" {
		return value, err
	}
	if _, err := netip.ParseAddr(value); err != nil {
		return "", errors.New("expected IPv4 or IPv6 address")
	}
	return value, nil
}

func optionalBool(raw json.RawMessage) (*bool, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	value, err := requiredBool(raw)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func requiredBool(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false, errors.New("required boolean is missing or null")
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, errors.New("expected boolean")
	}
	return value, nil
}

func canonicalIPv4Prefix(value string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() || prefix.String() != value {
		return netip.Prefix{}, errors.New("expected canonical IPv4 prefix")
	}
	return prefix, nil
}

// ValidateFIBRoute checks exact prefix identity, default-VRF route selection,
// Zebra installation, and active FIB nexthops on explicitly allowed ports.
func ValidateFIBRoute(doc FIBRouteDocument, expected FIBExpectation) []FIBMismatch {
	var mismatches []FIBMismatch
	add := func(reason, iface string) {
		mismatches = append(mismatches, FIBMismatch{Reason: reason, InterfaceName: iface})
	}
	prefix, err := canonicalIPv4Prefix(expected.Prefix)
	if err != nil || prefix.Bits() == 0 || len(expected.EgressInterfaces) == 0 {
		add("INVALID_EXPECTED_ROUTE", "")
		return mismatches
	}
	allowed := make(map[string]bool, len(expected.EgressInterfaces))
	for _, name := range expected.EgressInterfaces {
		if name == "" || allowed[name] {
			add("INVALID_EXPECTED_ROUTE", "")
			return mismatches
		}
		allowed[name] = true
	}
	if doc.PrefixKey != expected.Prefix {
		add("PREFIX_KEY_MISMATCH", "")
	}
	selected := make([]FIBRoute, 0, 1)
	for _, route := range doc.Routes {
		if route.Selected != nil && *route.Selected {
			selected = append(selected, route)
		}
	}
	if len(selected) == 0 {
		add("ROUTE_NOT_SELECTED", "")
		return mismatches
	}
	if len(selected) != 1 {
		add("AMBIGUOUS_SELECTED_ROUTES", "")
		return mismatches
	}
	route := selected[0]
	routePrefix, _ := canonicalIPv4Prefix(route.Prefix) // ParseFIBRoute validated this.
	if route.Prefix != expected.Prefix || routePrefix != prefix || route.PrefixLen != uint32(prefix.Bits()) {
		add("ROUTE_PREFIX_MISMATCH", "")
	}
	if route.VRFName != "default" || route.VRFID != 0 || route.Table != 254 {
		add("ROUTE_TABLE_MISMATCH", "")
	}
	if route.Protocol != "bgp" {
		add("ROUTE_PROTOCOL_MISMATCH", "")
	}
	if route.Selected == nil || !*route.Selected || route.DestSelected == nil || !*route.DestSelected {
		add("ROUTE_NOT_DEST_SELECTED", "")
	}
	if route.Installed == nil || !*route.Installed {
		add("ROUTE_NOT_INSTALLED", "")
	}
	eligible := 0
	for _, nexthop := range route.Nexthops {
		if nexthop.FIB && !nexthop.Active {
			add("NEXTHOP_FLAGS_CONTRADICTORY", nexthop.InterfaceName)
			continue
		}
		if !nexthop.FIB || !nexthop.Active {
			continue
		}
		if route.Installed == nil || !*route.Installed {
			add("NEXTHOP_FLAGS_CONTRADICTORY", nexthop.InterfaceName)
		}
		if !allowed[nexthop.InterfaceName] {
			add("NEXTHOP_INTERFACE_MISMATCH", nexthop.InterfaceName)
			continue
		}
		eligible++
	}
	if eligible == 0 {
		add("FIB_NEXTHOP_MISSING", "")
	}
	return mismatches
}
