// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package sshprobe

import (
	"encoding/json"
	"fmt"
	"net"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

var (
	pciLine     = regexp.MustCompile(`(?i)^([0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]) (.+) \[([0-9a-f]{4}):([0-9a-f]{4})\](?: .*)?$`)
	uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

func validSerial(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 256 || strings.ContainsAny(s, "\x00\r\n\t") {
		return false
	}
	switch strings.ToLower(s) {
	case "unknown", "n/a", "none", "default string", "to be filled by o.e.m.":
		return false
	}
	return strings.Trim(s, "0- ") != ""
}

func validUUID(s string) bool {
	return uuidPattern.MatchString(s) && strings.Trim(strings.ToLower(s), "0-") != "" && strings.Trim(strings.ToLower(s), "f-") != ""
}

// fields parses fixed-label utility output. Duplicate labels with different
// values are rejected so a later FRU or injected line cannot replace identity.
func fields(text string) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" {
			continue
		}
		if before, exists := out[k]; exists && before != v {
			return nil, &Error{Code: "SSH_IDENTITY_AMBIGUOUS", Stage: "duplicate-field"}
		}
		out[k] = v
	}
	return out, nil
}

func armIdentity(dmi, fru string) (serial, model, manufacturer string, err error) {
	f, err := fields(fru)
	if err != nil {
		return "", "", "", err
	}
	serial = strings.TrimSpace(dmi)
	model, manufacturer = f["Product Name"], f["Product Manufacturer"]
	if !validSerial(serial) || !validSerial(f["Product Serial"]) || serial != f["Product Serial"] {
		return "", "", "", &Error{Code: "SSH_IDENTITY_CONFLICT", Stage: "arm-dmi-fru"}
	}
	if !strings.EqualFold(manufacturer, "nvidia") || !strings.Contains(strings.ToLower(model), "bluefield") {
		return "", "", "", &Error{Code: "SSH_DPU_IDENTITY_UNSUPPORTED", Stage: "arm-fru"}
	}
	if board := f["Board Serial"]; board != "" && board != serial {
		return "", "", "", &Error{Code: "SSH_IDENTITY_CONFLICT", Stage: "arm-board-fru"}
	}
	return serial, model, manufacturer, nil
}

func bmcAddress(text string) (string, error) {
	f, err := fields(text)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(f["IP Address"])
	mac, macErr := net.ParseMAC(f["MAC Address"])
	if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || macErr != nil || len(mac) != 6 {
		return "", &Error{Code: "SSH_BMC_ADDRESS_UNAVAILABLE", Stage: "arm-ipmi-lan"}
	}
	return "https://" + ip.String(), nil
}

type pciCard struct {
	address, vendor, device, model string
	functions                      []string
}

func pciCards(text string) ([]pciCard, error) {
	bySlot := map[string]pciCard{}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		m := pciLine.FindStringSubmatch(line)
		if m == nil {
			return nil, &Error{Code: "SSH_PCI_INVENTORY_INVALID", Stage: "host-pci"}
		}
		if !strings.EqualFold(m[3], "15b3") || (!strings.Contains(m[2], "[0200]") && !strings.Contains(m[2], "[0280]")) {
			continue
		}
		bdf := strings.ToLower(m[1])
		if seen[bdf] {
			return nil, &Error{Code: "SSH_PCI_INVENTORY_INVALID", Stage: "duplicate-pci-function"}
		}
		seen[bdf] = true
		if len(seen) > 256 {
			return nil, &Error{Code: "SSH_PCI_INVENTORY_LIMIT", Stage: "host-pci"}
		}
		slot := bdf[:len(bdf)-2]
		old, ok := bySlot[slot]
		functions := append(old.functions, bdf)
		if !ok || bdf < old.address {
			old = pciCard{address: bdf, vendor: strings.ToLower(m[3]), device: strings.ToLower(m[4]), model: m[2]}
		}
		old.functions = functions
		bySlot[slot] = old
	}
	if len(bySlot) > 256 {
		return nil, &Error{Code: "SSH_PCI_INVENTORY_LIMIT", Stage: "host-pci"}
	}
	out := make([]pciCard, 0, len(bySlot))
	for _, card := range bySlot {
		sort.Strings(card.functions)
		out = append(out, card)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].address < out[j].address })
	return out, nil
}

func vpd(text string) (serial, slot string, err error) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[SN] Serial number:") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "[SN] Serial number:"))
			if serial != "" && serial != value {
				return "", "", &Error{Code: "SSH_IDENTITY_AMBIGUOUS", Stage: "host-vpd"}
			}
			serial = value
		}
		if strings.HasPrefix(line, "Physical Slot:") {
			slot = strings.TrimSpace(strings.TrimPrefix(line, "Physical Slot:"))
		}
	}
	if !validSerial(serial) {
		serial = ""
	}
	return serial, slot, nil
}

func interfaces(text string) ([]inventory.Interface, error) {
	var rows []struct {
		Name      string `json:"ifname"`
		Index     int    `json:"ifindex"`
		MTU       int    `json:"mtu"`
		Addresses []struct {
			Local  string `json:"local"`
			Prefix int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "interfaces"}
	}
	if len(rows) > 4096 {
		return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "interfaces"}
	}
	out := make([]inventory.Interface, 0, len(rows))
	for _, r := range rows {
		if r.Name == "" || r.Index < 1 || r.MTU < 1 {
			return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "interfaces"}
		}
		item := inventory.Interface{Name: r.Name, Index: r.Index, MTU: r.MTU, Addresses: []string{}}
		for _, a := range r.Addresses {
			address := a.Local + "/" + strconv.Itoa(a.Prefix)
			if _, _, err := net.ParseCIDR(address); err != nil {
				return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "interfaces"}
			}
			item.Addresses = append(item.Addresses, address)
		}
		out = append(out, item)
	}
	return out, nil
}

func routes(text string, ifaces []inventory.Interface, family int) ([]inventory.Route, error) {
	if family != 4 && family != 6 {
		return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "routes"}
	}
	type nextHop struct {
		Gateway string `json:"gateway"`
		Device  string `json:"dev"`
	}
	var rows []struct {
		Destination string          `json:"dst"`
		Gateway     string          `json:"gateway"`
		Device      string          `json:"dev"`
		Table       json.RawMessage `json:"table"`
		Type        string          `json:"type"`
		NextHops    []nextHop       `json:"nexthops"`
	}
	if err := json.Unmarshal([]byte(text), &rows); err != nil || rows == nil {
		return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "routes"}
	}
	if len(rows) > 8192 {
		return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "routes"}
	}
	indexes := map[string]int{}
	for _, i := range ifaces {
		indexes[i.Name] = i.Index
	}
	out := make([]inventory.Route, 0, len(rows))
	for _, r := range rows {
		dst := r.Destination
		if len(dst) > 128 || len(r.Device) > 128 || len(r.Type) > 64 || len(r.NextHops) > 128 {
			return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "routes"}
		}
		if dst == "default" {
			if family == 6 {
				dst = "::/0"
			} else {
				dst = "0.0.0.0/0"
			}
		}
		if ip := net.ParseIP(dst); ip != nil {
			if ip.To4() != nil {
				dst += "/32"
			} else {
				dst += "/128"
			}
		}
		_, destination, err := net.ParseCIDR(dst)
		if err != nil || family == 4 && destination.IP.To4() == nil || family == 6 && destination.IP.To4() != nil {
			return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "routes"}
		}
		if r.Gateway != "" {
			gateway := net.ParseIP(r.Gateway)
			if gateway == nil || family == 4 && gateway.To4() == nil || family == 6 && gateway.To4() != nil {
				return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "routes"}
			}
		}
		table := 254
		if len(r.Table) > 0 {
			if err := decodeRouteTable(r.Table, &table); err != nil {
				return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "routes"}
			}
		}
		if len(r.NextHops) > 0 {
			if r.Device != "" || r.Gateway != "" {
				return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "routes"}
			}
			for _, hop := range r.NextHops {
				if len(out) >= 8192 || len(hop.Device) > 128 || len(hop.Gateway) > 128 {
					return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "routes"}
				}
				index, err := routeInterfaceIndex(indexes, hop.Device)
				if err != nil || !validRouteGateway(hop.Gateway, family) {
					return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "routes"}
				}
				out = append(out, inventory.Route{Destination: dst, Gateway: hop.Gateway, InterfaceIndex: index, Table: table})
			}
			continue
		}
		index := 0
		if r.Device != "" {
			var err error
			index, err = routeInterfaceIndex(indexes, r.Device)
			if err != nil {
				return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "routes"}
			}
		} else if !nonForwardingRoute(r.Type) {
			return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "routes"}
		}
		if !validRouteGateway(r.Gateway, family) {
			return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "routes"}
		}
		out = append(out, inventory.Route{Destination: dst, Gateway: r.Gateway, InterfaceIndex: index, Table: table})
		if len(out) > 8192 {
			return nil, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "routes"}
		}
	}
	return out, nil
}

func decodeRouteTable(raw json.RawMessage, destination *int) error {
	if len(raw) == 0 || destination == nil {
		return fmt.Errorf("route table is missing")
	}
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		switch name {
		case "default":
			*destination = 253
			return nil
		case "main":
			*destination = 254
			return nil
		case "local":
			*destination = 255
			return nil
		default:
			value, err := strconv.Atoi(name)
			if err != nil || value < 0 || strconv.Itoa(value) != name {
				return fmt.Errorf("unsupported named route table")
			}
			*destination = value
			return nil
		}
	}
	if err := json.Unmarshal(raw, destination); err == nil && *destination >= 0 {
		return nil
	}
	var numericString string
	if err := json.Unmarshal(raw, &numericString); err != nil {
		return fmt.Errorf("route table must be a known name or number")
	}
	value, err := strconv.Atoi(numericString)
	if err != nil || value < 0 || strconv.Itoa(value) != numericString {
		return fmt.Errorf("route table number is invalid")
	}
	*destination = value
	return nil
}

func routeInterfaceIndex(indexes map[string]int, device string) (int, error) {
	if device == "" || len(device) > 128 {
		return 0, fmt.Errorf("route device is missing")
	}
	index := indexes[device]
	if index < 1 {
		return 0, fmt.Errorf("route device was not observed")
	}
	return index, nil
}

func validRouteGateway(value string, family int) bool {
	if value == "" {
		return true
	}
	if len(value) > 128 {
		return false
	}
	ip := net.ParseIP(value)
	return ip != nil && (family == 4 && ip.To4() != nil || family == 6 && ip.To4() == nil)
}

func nonForwardingRoute(routeType string) bool {
	switch routeType {
	case "blackhole", "unreachable", "prohibit", "throw":
		return true
	default:
		return false
	}
}

func iommuGroup(text string) (*int, bool, error) {
	value := strings.TrimSpace(text)
	if value == "absent" {
		return nil, true, nil
	}
	const prefix = "/sys/kernel/iommu_groups/"
	if !strings.HasPrefix(value, prefix) || path.Clean(value) != value {
		return nil, false, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "iommu-group"}
	}
	name := strings.TrimPrefix(value, prefix)
	if strings.Contains(name, "/") {
		return nil, false, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "iommu-group"}
	}
	group, err := strconv.Atoi(name)
	if err != nil || group < 0 || strconv.Itoa(group) != name {
		return nil, false, &Error{Code: "SSH_RUNTIME_INVALID", Stage: "iommu-group"}
	}
	return &group, false, nil
}
