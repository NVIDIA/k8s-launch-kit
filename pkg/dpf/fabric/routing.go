// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

const (
	maxL3Entries    = 8192
	maxL3StringSize = 128
)

func parseOperationalAddresses(raw []byte, out *Snapshot) {
	const input = "operationalAddresses"
	rows, ok := parseL3Array(raw, input, &out.Issues)
	if !ok {
		return
	}
	addresses := make([]InterfaceAddress, 0)
	for i, row := range rows {
		path := fmt.Sprintf("$[%d]", i)
		obj, ok := l3Object(row, input, path, &out.Issues)
		if !ok {
			return
		}
		name, ok := l3String(obj, "ifname", input, path, true, &out.Issues)
		if !ok {
			return
		}
		addrInfo, exists := obj["addr_info"]
		if !exists {
			addIssue(&out.Issues, input, path+".addr_info", "missing_field", "required address list is absent")
			return
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(addrInfo, &entries); err != nil || entries == nil {
			addIssue(&out.Issues, input, path+".addr_info", "invalid_type", "expected an address array")
			return
		}
		if len(entries)+len(addresses) > maxL3Entries {
			addIssue(&out.Issues, input, path+".addr_info", "entry_limit", "address output exceeds the supported entry limit")
			return
		}
		for j, entry := range entries {
			entryPath := fmt.Sprintf("%s.addr_info[%d]", path, j)
			address, ok := l3Object(entry, input, entryPath, &out.Issues)
			if !ok {
				return
			}
			family, ok := l3String(address, "family", input, entryPath, true, &out.Issues)
			if !ok {
				return
			}
			addrFamily := ""
			switch family {
			case "inet":
				addrFamily = "ipv4"
			case "inet6":
				addrFamily = "ipv6"
			default:
				addIssue(&out.Issues, input, entryPath+".family", "unsupported_value", "address family is not supported")
				return
			}
			local, ok := l3String(address, "local", input, entryPath, true, &out.Issues)
			if !ok {
				return
			}
			bits, ok := l3Int(address, "prefixlen", input, entryPath, &out.Issues)
			if !ok {
				return
			}
			ip, err := netip.ParseAddr(local)
			maxBits := 32
			if addrFamily == "ipv6" {
				maxBits = 128
			}
			if err != nil || ip.Zone() != "" || ip.Is4() != (addrFamily == "ipv4") || bits < 0 || bits > maxBits {
				addIssue(&out.Issues, input, entryPath, "invalid_value", "address and prefix length are invalid")
				return
			}
			addresses = append(addresses, InterfaceAddress{
				Interface: name,
				Family:    addrFamily,
				Prefix:    netip.PrefixFrom(ip, bits).String(),
			})
		}
	}
	out.Operational.Addresses = &addresses
}

func parseOperationalRoutes(raw []byte, input, family string, destination **[]Route, out *Snapshot) {
	rows, ok := parseL3Array(raw, input, &out.Issues)
	if !ok {
		return
	}
	routes := make([]Route, 0, len(rows))
	for i, row := range rows {
		path := fmt.Sprintf("$[%d]", i)
		obj, ok := l3Object(row, input, path, &out.Issues)
		if !ok {
			return
		}
		prefix := ""
		dst, exists := obj["dst"]
		if !exists {
			addIssue(&out.Issues, input, path+".dst", "missing_field", "required route destination is absent")
			return
		}
		if bytes.Equal(bytes.TrimSpace(dst), []byte(`"default"`)) {
			if family == "ipv4" {
				prefix = "0.0.0.0/0"
			} else {
				prefix = "::/0"
			}
		} else {
			if err := json.Unmarshal(dst, &prefix); err != nil || len(prefix) > maxL3StringSize {
				addIssue(&out.Issues, input, path+".dst", "invalid_type", "expected a bounded route prefix")
				return
			}
			parsed, err := netip.ParsePrefix(prefix)
			if err != nil {
				if address, addrErr := netip.ParseAddr(prefix); addrErr == nil {
					bits := 128
					if address.Is4() {
						bits = 32
					}
					parsed = netip.PrefixFrom(address, bits)
				} else {
					addIssue(&out.Issues, input, path+".dst", "invalid_value", "route prefix is invalid")
					return
				}
			}
			if parsed.Addr().Is4() != (family == "ipv4") {
				addIssue(&out.Issues, input, path+".dst", "invalid_value", "route prefix does not match the requested address family")
				return
			}
			prefix = parsed.Masked().String()
		}
		table := "main"
		if rawTable, exists := obj["table"]; exists {
			table, ok = l3StringOrNumber(rawTable, input, path+".table", &out.Issues)
			if !ok {
				return
			}
		}
		protocol, ok := optionalL3StringOrNumber(obj, "protocol", input, path, &out.Issues)
		if !ok {
			return
		}
		routeType, ok := optionalL3String(obj, "type", input, path, &out.Issues)
		if !ok {
			return
		}
		routes = append(routes, Route{Family: family, Prefix: prefix, Table: table, Protocol: protocol, Type: routeType})
	}
	*destination = &routes
}

func parseL3Array(raw []byte, input string, issues *[]Issue) ([]json.RawMessage, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		addIssue(issues, input, "$", "missing_output", "command output is empty")
		return nil, false
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		addIssue(issues, input, "$", "malformed_json", "command output is not a JSON array")
		return nil, false
	}
	if rows == nil {
		addIssue(issues, input, "$", "unsupported_shape", "expected a JSON array")
		return nil, false
	}
	if len(rows) > maxL3Entries {
		addIssue(issues, input, "$", "entry_limit", "command output exceeds the supported entry limit")
		return nil, false
	}
	return rows, true
}

func l3Object(raw json.RawMessage, input, path string, issues *[]Issue) (jsonObject, bool) {
	var obj jsonObject
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		addIssue(issues, input, path, "invalid_type", "expected an object")
		return nil, false
	}
	return obj, true
}

func l3String(obj jsonObject, key, input, path string, required bool, issues *[]Issue) (string, bool) {
	raw, exists := obj[key]
	if !exists {
		if required {
			addIssue(issues, input, path+"."+key, "missing_field", "required field is absent")
			return "", false
		}
		return "", true
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || len(value) > maxL3StringSize || strings.TrimSpace(value) == "" {
		addIssue(issues, input, path+"."+key, "invalid_type", "expected a non-empty bounded string")
		return "", false
	}
	return value, true
}

func optionalL3String(obj jsonObject, key, input, path string, issues *[]Issue) (string, bool) {
	return l3String(obj, key, input, path, false, issues)
}

func optionalL3StringOrNumber(obj jsonObject, key, input, path string, issues *[]Issue) (string, bool) {
	raw, exists := obj[key]
	if !exists {
		return "", true
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		if len(value) == 0 || len(value) > maxL3StringSize {
			addIssue(issues, input, path+"."+key, "invalid_value", "value is outside the supported bounds")
			return "", false
		}
		return value, true
	}
	// Linux iproute2 emits protocol names on some versions and numeric IDs
	// on others. Preserve numeric IDs rather than making the whole route view
	// unavailable.
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err == nil {
		if _, err := strconv.ParseUint(number.String(), 10, 32); err == nil {
			return number.String(), true
		}
	}
	addIssue(issues, input, path+"."+key, "invalid_type", "expected a bounded protocol name or number")
	return "", false
}

func l3StringOrNumber(raw json.RawMessage, input, path string, issues *[]Issue) (string, bool) {
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		if len(value) == 0 || len(value) > maxL3StringSize {
			addIssue(issues, input, path, "invalid_value", "route table is outside the supported bounds")
			return "", false
		}
		return value, true
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		addIssue(issues, input, path, "invalid_type", "expected a route table name or number")
		return "", false
	}
	_, err := strconv.ParseUint(number.String(), 10, 32)
	if err != nil {
		addIssue(issues, input, path, "invalid_value", "route table number is invalid")
		return "", false
	}
	return number.String(), true
}

func l3Int(obj jsonObject, key, input, path string, issues *[]Issue) (int, bool) {
	raw, exists := obj[key]
	if !exists {
		addIssue(issues, input, path+"."+key, "missing_field", "required field is absent")
		return 0, false
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		addIssue(issues, input, path+"."+key, "invalid_type", "expected an integer")
		return 0, false
	}
	return value, true
}
