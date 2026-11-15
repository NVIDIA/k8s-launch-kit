// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package hostnetwork plans a minimal persistent Netplan fragment for an
// already-selected host interface. It does not inspect or change a host.
package hostnetwork

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/netip"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// Desired contains already-resolved workload and selected-interface facts.
// HostAddressPrefix is the reserved host address plus its leased prefix. MTU
// is supplied by the caller from the resolved workload profile.
type Desired struct {
	InterfaceName     string
	PermanentMAC      string
	MTU               int
	HostAddressPrefix string
	Gateway           string
	WorkloadCIDR      string
	MergedNetplanYAML []byte // Full merged Netplan document; never rewritten by the planner.
}

// Plan is a pure fragment proposal; it does not report runtime application.
type Plan struct {
	LogicalID              string
	DesiredFragment        []byte
	MTUAlreadyDeclared     bool
	MTUMatchesDesired      bool
	AddressAlreadyDeclared bool
	RouteAlreadyDeclared   bool
	WaitOnlineOptional     bool // Existing selected optional value; false is Netplan default.
	ReasonCode             string
	Message                string
}

type netplanDocument struct {
	Network netplanNetwork `yaml:"network"`
	Raw     map[string]any `yaml:"-"`
}

type netplanNetwork struct {
	Version   int                        `yaml:"version"`
	Renderer  string                     `yaml:"renderer"`
	Ethernets map[string]netplanEthernet `yaml:"ethernets"`
	Bonds     map[string]netplanDevice   `yaml:"bonds"`
	Bridges   map[string]netplanDevice   `yaml:"bridges"`
	Vlans     map[string]netplanDevice   `yaml:"vlans"`
	VRFs      map[string]netplanDevice   `yaml:"vrfs"`
}

type netplanEthernet struct {
	Match         *netplanMatch  `yaml:"match"`
	SetName       string         `yaml:"set-name"`
	Renderer      string         `yaml:"renderer"`
	DHCP4         *bool          `yaml:"dhcp4"`
	Optional      *bool          `yaml:"optional"`
	DHCP6         *bool          `yaml:"dhcp6"`
	Gateway4      any            `yaml:"gateway4"`
	Gateway6      any            `yaml:"gateway6"`
	OpenVSwitch   any            `yaml:"openvswitch"`
	Addresses     []any          `yaml:"addresses"`
	Routes        []netplanRoute `yaml:"routes"`
	RoutingPolicy []any          `yaml:"routing-policy"`
}

type netplanMatch struct {
	MACAddress any `yaml:"macaddress"`
	Name       any `yaml:"name"`
	Driver     any `yaml:"driver"`
}

type netplanDevice struct {
	Interfaces []string `yaml:"interfaces"`
	Link       string   `yaml:"link"`
}

type netplanRoute struct {
	To    string `yaml:"to"`
	Via   string `yaml:"via"`
	From  string `yaml:"from"`
	Table any    `yaml:"table"`
	Type  string `yaml:"type"`
	Scope string `yaml:"scope"`
}

// PlanNetplan returns only missing or differing MTU, address, and route fields
// for the unique existing Ethernet definition bound to the requested permanent
// MAC and interface name.
func PlanNetplan(desired Desired) (Plan, error) {
	name, mac, host, gateway, workload, err := validateDesired(desired)
	if err != nil {
		return Plan{}, err
	}
	selected, err := inspectNetplanInterface(name, mac, desired.MergedNetplanYAML)
	if err != nil {
		return Plan{}, err
	}
	logicalID := selected.logicalID
	mtuDeclared := selected.mtu != 0
	mtuMatches := mtuDeclared && selected.mtu == desired.MTU
	addressPresent, err := inspectAddresses(selected.addresses, host)
	if err != nil {
		return Plan{}, err
	}
	routePresent, err := inspectRoutes(selected.routes, workload, gateway)
	if err != nil {
		return Plan{}, err
	}
	fragment, err := makeFragment(logicalID, mtuMatches, addressPresent, routePresent, desired.MTU, host, workload, gateway)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{LogicalID: logicalID, DesiredFragment: fragment, MTUAlreadyDeclared: mtuDeclared, MTUMatchesDesired: mtuMatches, AddressAlreadyDeclared: addressPresent, RouteAlreadyDeclared: routePresent, WaitOnlineOptional: selected.optional}
	if mtuMatches && addressPresent && routePresent {
		plan.ReasonCode = "NETPLAN_DESIRED_STATE_DECLARED"
		plan.Message = "The selected interface declares the exact desired MTU, address and route; this does not establish runtime application or ownership."
	} else {
		plan.ReasonCode = "NETPLAN_FRAGMENT_REQUIRED"
		plan.Message = "The fragment contains only missing or differing MTU, address and route entries for the uniquely matched selected interface."
	}
	return plan, nil
}

// NetplanInterfaceID returns the unique selected interface's Netplan ID after
// validating its backend definition without a future IPAM lease. It establishes an exact
// permanent-MAC/name match, supported renderer and fields, and absence of
// unsupported DHCP, default gateways, virtual-device membership or policy rules.
// It does not establish that declared addresses or routes match a future lease;
// PlanNetplan performs those checks after an actual allocation is available.
func NetplanInterfaceID(interfaceName, permanentMAC string, mergedYAML []byte) (string, error) {
	selected, err := InspectNetplanInterface(interfaceName, permanentMAC, mergedYAML)
	return selected.LogicalID, err
}

// NetplanInterface contains lease-independent properties of the unique supported
// Ethernet definition. WaitOnlineOptional preserves Netplan's default false.
type NetplanInterface struct {
	LogicalID          string
	WaitOnlineOptional bool
}

// InspectNetplanInterface validates the same source contract as NetplanInterfaceID
// and returns properties needed by read-only backend qualification in one parse.
func InspectNetplanInterface(interfaceName, permanentMAC string, mergedYAML []byte) (NetplanInterface, error) {
	mac, err := validateInterfaceInput(interfaceName, permanentMAC, mergedYAML)
	if err != nil {
		return NetplanInterface{}, err
	}
	selected, err := inspectNetplanInterface(interfaceName, mac, mergedYAML)
	if err != nil {
		return NetplanInterface{}, err
	}
	return NetplanInterface{LogicalID: selected.logicalID, WaitOnlineOptional: selected.optional}, nil
}

type inspectedNetplanInterface struct {
	logicalID string
	addresses []netip.Prefix
	routes    []inspectedNetplanRoute
	optional  bool
	mtu       int // Zero means the existing definition does not declare an MTU.
}

type inspectedNetplanRoute struct {
	definition  netplanRoute
	destination netip.Prefix
}

func inspectNetplanInterface(name string, mac net.HardwareAddr, data []byte) (inspectedNetplanInterface, error) {
	config, err := decodeNetplan(data)
	if err != nil {
		return inspectedNetplanInterface{}, err
	}
	network := config.Network
	if network.Version != 2 {
		return inspectedNetplanInterface{}, fmt.Errorf("merged Netplan version must be 2")
	}
	if network.Renderer != "" && network.Renderer != "networkd" {
		return inspectedNetplanInterface{}, fmt.Errorf("selected host uses unsupported Netplan renderer")
	}
	if len(network.Ethernets) == 0 || len(network.Ethernets) > 512 {
		return inspectedNetplanInterface{}, fmt.Errorf("merged Netplan Ethernet definition count is empty or exceeds the limit")
	}
	logicalID, selected, err := selectEthernet(network.Ethernets, name, mac)
	if err != nil {
		return inspectedNetplanInterface{}, err
	}
	if err := validateSelectedNetplanFields(config.Raw, logicalID); err != nil {
		return inspectedNetplanInterface{}, err
	}
	for _, group := range []map[string]netplanDevice{network.Bonds, network.Bridges, network.Vlans, network.VRFs} {
		for _, device := range group {
			members := append(append([]string(nil), device.Interfaces...), device.Link)
			for _, iface := range members {
				if iface == name || iface == logicalID {
					return inspectedNetplanInterface{}, fmt.Errorf("selected Ethernet is attached to an unsupported virtual or routed device")
				}
			}
		}
	}
	if selected.Renderer != "" && selected.Renderer != "networkd" {
		return inspectedNetplanInterface{}, fmt.Errorf("selected Ethernet overrides the renderer with an unsupported value")
	}
	if selected.DHCP4 != nil && *selected.DHCP4 {
		return inspectedNetplanInterface{}, fmt.Errorf("selected Ethernet uses DHCPv4; static HBN host addressing is unsupported")
	}
	if selected.DHCP6 != nil && *selected.DHCP6 {
		return inspectedNetplanInterface{}, fmt.Errorf("selected Ethernet uses DHCPv6; static HBN host addressing is unsupported")
	}
	if selected.Gateway4 != nil || selected.Gateway6 != nil {
		return inspectedNetplanInterface{}, fmt.Errorf("selected Ethernet has an unsupported legacy gateway route")
	}
	if selected.OpenVSwitch != nil {
		return inspectedNetplanInterface{}, fmt.Errorf("selected Ethernet uses unsupported Open vSwitch processing")
	}
	if len(selected.RoutingPolicy) != 0 {
		return inspectedNetplanInterface{}, fmt.Errorf("selected Ethernet has unsupported routing-policy rules")
	}
	selectedRaw := config.Raw["network"].(map[string]any)["ethernets"].(map[string]any)[logicalID].(map[string]any)
	declaredMTU := 0
	if rawMTU, exists := selectedRaw["mtu"]; exists {
		mtu, ok := netplanMTU(rawMTU)
		if !ok {
			return inspectedNetplanInterface{}, fmt.Errorf("selected Ethernet has an invalid or unsupported MTU")
		}
		declaredMTU = mtu
	}
	addresses, err := parseNetplanAddresses(selected.Addresses)
	if err != nil {
		return inspectedNetplanInterface{}, err
	}
	routes, err := parseNetplanRoutes(selected.Routes)
	if err != nil {
		return inspectedNetplanInterface{}, err
	}
	return inspectedNetplanInterface{logicalID: logicalID, addresses: addresses, routes: routes, mtu: declaredMTU, optional: selected.Optional != nil && *selected.Optional}, nil
}

func validateDesired(d Desired) (string, net.HardwareAddr, netip.Prefix, netip.Addr, netip.Prefix, error) {
	invalid := func(reason string) (string, net.HardwareAddr, netip.Prefix, netip.Addr, netip.Prefix, error) {
		return "", nil, netip.Prefix{}, netip.Addr{}, netip.Prefix{}, fmt.Errorf("%s", reason)
	}
	if d.MTU < 1 || d.MTU > 65535 {
		return invalid("desired MTU must be between 1 and 65535")
	}
	name := d.InterfaceName
	mac, err := validateInterfaceInput(name, d.PermanentMAC, d.MergedNetplanYAML)
	if err != nil {
		return invalid(err.Error())
	}
	host, err := netip.ParsePrefix(d.HostAddressPrefix)
	if err != nil || !host.Addr().Is4() || host.Bits() > 30 || !usableHost(host) {
		return invalid("leased host address prefix must contain a usable IPv4 host address")
	}
	gateway, err := netip.ParseAddr(d.Gateway)
	if err != nil || !gateway.Is4() || !host.Contains(gateway) || !usableAddress(host, gateway) || gateway == host.Addr() {
		return invalid("leased gateway must be a distinct usable address in the host prefix")
	}
	workload, err := netip.ParsePrefix(d.WorkloadCIDR)
	if err != nil || !workload.Addr().Is4() || workload.Bits() == 0 || workload != workload.Masked() {
		return invalid("workload CIDR must be a canonical IPv4 prefix")
	}
	if !workload.Contains(host.Masked().Addr()) || !workload.Contains(lastIPv4(host)) || !workload.Contains(gateway) {
		return invalid("leased host prefix and gateway must be contained by the workload CIDR")
	}
	return name, mac, host, gateway, workload, nil
}

func validateInterfaceInput(name, permanentMAC string, mergedYAML []byte) (net.HardwareAddr, error) {
	if name == "" || len(name) > 15 || strings.ContainsAny(name, "/:\x00\t\r\n ") {
		return nil, fmt.Errorf("selected interface name is invalid")
	}
	mac, err := net.ParseMAC(permanentMAC)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || bytes.Equal(mac, make([]byte, 6)) {
		return nil, fmt.Errorf("selected permanent MAC must be a nonzero unicast EUI-48 address")
	}
	if len(mergedYAML) == 0 || len(mergedYAML) > 1<<20 {
		return nil, fmt.Errorf("merged Netplan YAML is empty or exceeds the size limit")
	}
	return mac, nil
}

func netplanMTU(raw any) (int, bool) {
	var mtu int64
	switch value := raw.(type) {
	case int:
		mtu = int64(value)
	case int64:
		mtu = value
	case uint64:
		if value > 65535 {
			return 0, false
		}
		mtu = int64(value)
	default:
		return 0, false
	}
	if mtu < 1 || mtu > 65535 {
		return 0, false
	}
	return int(mtu), true
}

func usableHost(prefix netip.Prefix) bool {
	return usableAddress(prefix, prefix.Addr())
}

func usableAddress(prefix netip.Prefix, address netip.Addr) bool {
	return address != prefix.Masked().Addr() && address != lastIPv4(prefix)
}

func decodeNetplan(data []byte) (netplanDocument, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var config netplanDocument
	if err := decoder.Decode(&config); err != nil || config.Network.Ethernets == nil {
		return netplanDocument{}, fmt.Errorf("merged Netplan YAML is invalid or has no Ethernet definitions")
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return netplanDocument{}, fmt.Errorf("merged Netplan YAML must contain one document")
	} else if err != io.EOF {
		return netplanDocument{}, fmt.Errorf("merged Netplan YAML has invalid trailing data")
	}
	if err := yaml.Unmarshal(data, &config.Raw); err != nil {
		return netplanDocument{}, fmt.Errorf("merged Netplan YAML is invalid or has no Ethernet definitions")
	}
	return config, nil
}

func validateSelectedNetplanFields(document map[string]any, logicalID string) error {
	network, ok := document["network"].(map[string]any)
	if !ok {
		return fmt.Errorf("merged Netplan YAML has no network mapping")
	}
	ethernets, ok := network["ethernets"].(map[string]any)
	if !ok {
		return fmt.Errorf("merged Netplan YAML has no Ethernet mapping")
	}
	selected, ok := ethernets[logicalID].(map[string]any)
	if !ok {
		return fmt.Errorf("selected Ethernet definition is malformed")
	}
	allowed := map[string]bool{
		"match": true, "set-name": true, "renderer": true, "dhcp4": true, "dhcp6": true,
		"addresses": true, "routes": true, "routing-policy": true, "gateway4": true,
		"gateway6": true, "openvswitch": true, "mtu": true, "optional": true,
		"optional-addresses": true, "critical": true,
	}
	for key := range selected {
		if !allowed[key] {
			return fmt.Errorf("selected Ethernet has unsupported Netplan field %q", key)
		}
	}
	if match, exists := selected["match"]; exists {
		fields, ok := match.(map[string]any)
		if !ok {
			return fmt.Errorf("selected Ethernet match mapping is malformed")
		}
		for key := range fields {
			if key != "macaddress" {
				return fmt.Errorf("selected Ethernet has unsupported match selector %q", key)
			}
		}
		if rawMAC, exists := fields["macaddress"]; exists {
			if _, ok := rawMAC.(string); !ok {
				return fmt.Errorf("selected Ethernet requires one scalar permanent-MAC selector")
			}
		}
	}
	if routes, exists := selected["routes"]; exists {
		entries, ok := routes.([]any)
		if !ok {
			return fmt.Errorf("selected Ethernet route list is malformed")
		}
		for _, entry := range entries {
			fields, ok := entry.(map[string]any)
			if !ok {
				return fmt.Errorf("selected Ethernet route entry is malformed")
			}
			if _, ok := fields["to"].(string); !ok {
				return fmt.Errorf("selected Ethernet route destination is missing or malformed")
			}
			for key := range fields {
				switch key {
				case "to", "via", "from", "type", "scope":
					if _, ok := fields[key].(string); !ok {
						return fmt.Errorf("selected Ethernet route field %q is malformed", key)
					}
				case "table":
					switch fields[key].(type) {
					case int, int64, uint64, string:
					default:
						return fmt.Errorf("selected Ethernet route table is malformed")
					}
				default:
					return fmt.Errorf("selected Ethernet has unsupported route field %q", key)
				}
			}
		}
	}
	return nil
}

func selectEthernet(entries map[string]netplanEthernet, name string, mac net.HardwareAddr) (string, netplanEthernet, error) {
	targetMAC := strings.ToLower(mac.String())
	selectedID := ""
	var selected netplanEthernet
	for id, entry := range entries {
		if entry.Match == nil {
			if id == name || entry.SetName == name {
				return "", netplanEthernet{}, fmt.Errorf("selected interface name is claimed without a permanent-MAC match")
			}
			continue
		}
		if entry.Match.Driver != nil && driverSelectorMayMatch(entry.Match, name, targetMAC) {
			return "", netplanEthernet{}, fmt.Errorf("netplan driver selector may match the selected Ethernet without an observed driver identity")
		}
		nameConflict := false
		if entry.Match.Name != nil {
			names, ok := stringSelectors(entry.Match.Name)
			if !ok {
				return "", netplanEthernet{}, fmt.Errorf("netplan name selector is malformed")
			}
			for _, selector := range names {
				matched, err := path.Match(selector, name)
				if err != nil {
					return "", netplanEthernet{}, fmt.Errorf("netplan name selector is malformed")
				}
				nameConflict = nameConflict || matched
			}
		}
		selectors, ok := macSelectors(entry.Match.MACAddress)
		if !ok {
			if id == name || entry.SetName == name || nameConflict {
				return "", netplanEthernet{}, fmt.Errorf("selected interface name is claimed without an exact permanent-MAC match")
			}
			continue
		}
		matches := false
		for _, selector := range selectors {
			if strings.ContainsAny(selector, "*?[]") {
				matched, err := path.Match(strings.ToLower(selector), targetMAC)
				if err != nil {
					return "", netplanEthernet{}, fmt.Errorf("netplan MAC selector is malformed")
				}
				if matched {
					return "", netplanEthernet{}, fmt.Errorf("selected interface is covered by a glob MAC selector")
				}
				continue
			}
			parsed, err := net.ParseMAC(selector)
			if err != nil || len(parsed) != 6 {
				return "", netplanEthernet{}, fmt.Errorf("netplan MAC selector is malformed")
			}
			matches = matches || strings.EqualFold(parsed.String(), targetMAC)
		}
		if !matches {
			if entry.SetName == name || nameConflict {
				return "", netplanEthernet{}, fmt.Errorf("selected interface name is assigned from a different permanent MAC")
			}
			continue
		}
		if entry.Match.Name != nil || entry.SetName != name || selectedID != "" {
			return "", netplanEthernet{}, fmt.Errorf("selected MAC and interface name are not uniquely bound")
		}
		selectedID, selected = id, entry
	}
	if selectedID == "" {
		return "", netplanEthernet{}, fmt.Errorf("no Ethernet definition exactly binds the selected MAC and set-name")
	}
	return selectedID, selected, nil
}

func macSelectors(raw any) ([]string, bool) {
	return stringSelectors(raw)
}

// driverSelectorMayMatch reports whether a driver-qualified definition could
// select the target. Without an observed driver identity, only an explicit
// nonmatching name or MAC selector proves that it cannot.
func driverSelectorMayMatch(match *netplanMatch, name, targetMAC string) bool {
	if match.Name != nil {
		names, ok := stringSelectors(match.Name)
		if !ok {
			return true
		}
		nameCouldMatch := false
		for _, selector := range names {
			matched, err := path.Match(selector, name)
			if err != nil {
				return true
			}
			nameCouldMatch = nameCouldMatch || matched
		}
		if !nameCouldMatch {
			return false
		}
	}
	if match.MACAddress != nil {
		macs, ok := stringSelectors(match.MACAddress)
		if !ok {
			return true
		}
		macCouldMatch := false
		for _, selector := range macs {
			if strings.ContainsAny(selector, "*?[]") {
				matched, err := path.Match(strings.ToLower(selector), targetMAC)
				if err != nil {
					return true
				}
				macCouldMatch = macCouldMatch || matched
				continue
			}
			parsed, err := net.ParseMAC(selector)
			if err != nil || len(parsed) != 6 {
				return true
			}
			macCouldMatch = macCouldMatch || strings.EqualFold(parsed.String(), targetMAC)
		}
		if !macCouldMatch {
			return false
		}
	}
	return true
}

func stringSelectors(raw any) ([]string, bool) {
	switch value := raw.(type) {
	case string:
		return []string{value}, true
	case []any:
		selectors := make([]string, 0, len(value))
		for _, item := range value {
			selector, ok := item.(string)
			if !ok {
				return nil, false
			}
			selectors = append(selectors, selector)
		}
		return selectors, len(selectors) > 0
	default:
		return nil, false
	}
}

func parseNetplanAddresses(entries []any) ([]netip.Prefix, error) {
	addresses := make([]netip.Prefix, 0, len(entries))
	for _, entry := range entries {
		text, ok := entry.(string)
		if !ok {
			return nil, fmt.Errorf("selected Ethernet uses unsupported complex address entries")
		}
		prefix, err := netip.ParsePrefix(text)
		if err != nil {
			return nil, fmt.Errorf("selected Ethernet address entry is invalid")
		}
		addresses = append(addresses, prefix)
	}
	return addresses, nil
}

func inspectAddresses(entries []netip.Prefix, desired netip.Prefix) (bool, error) {
	present := false
	for _, prefix := range entries {
		if !prefix.Addr().Is4() {
			continue
		}
		if prefix == desired {
			if present {
				return false, fmt.Errorf("selected Ethernet declares the desired IPv4 address more than once")
			}
			present = true
		} else {
			return false, fmt.Errorf("selected Ethernet already declares a conflicting IPv4 address")
		}
	}
	return present, nil
}

func parseNetplanRoutes(entries []netplanRoute) ([]inspectedNetplanRoute, error) {
	routes := make([]inspectedNetplanRoute, 0, len(entries))
	for _, route := range entries {
		if route.To == "default" {
			return nil, fmt.Errorf("selected Ethernet already declares a default route")
		}
		destination, err := routePrefix(route.To)
		if err != nil {
			return nil, fmt.Errorf("selected Ethernet route destination is invalid")
		}
		if destination.Bits() == 0 {
			return nil, fmt.Errorf("selected Ethernet already declares a default route")
		}
		routes = append(routes, inspectedNetplanRoute{definition: route, destination: destination})
	}
	return routes, nil
}

func inspectRoutes(routes []inspectedNetplanRoute, workload netip.Prefix, gateway netip.Addr) (bool, error) {
	present := false
	for _, parsed := range routes {
		route, destination := parsed.definition, parsed.destination
		if route.Scope != "" && route.Scope != "global" {
			if destination.Addr().Is4() && destination.Overlaps(workload) {
				return false, fmt.Errorf("selected Ethernet has an overlapping route with unsupported scope")
			}
		}
		if !destination.Addr().Is4() || !destination.Overlaps(workload) {
			continue
		}
		if route.Table != nil && !mainTable(route.Table) || route.From != "" {
			return false, fmt.Errorf("selected Ethernet has an overlapping policy route")
		}
		if destination == workload {
			via, err := netip.ParseAddr(route.Via)
			if err == nil && via == gateway && (route.Type == "" || route.Type == "unicast") {
				if present {
					return false, fmt.Errorf("selected Ethernet declares the workload route more than once")
				}
				present = true
				continue
			}
			return false, fmt.Errorf("selected Ethernet has a conflicting route for the workload CIDR")
		}
		if workload.Contains(destination.Addr()) && destination.Bits() > workload.Bits() {
			return false, fmt.Errorf("selected Ethernet has a more-specific route inside the workload CIDR")
		}
	}
	return present, nil
}

func routePrefix(text string) (netip.Prefix, error) {
	if prefix, err := netip.ParsePrefix(text); err == nil {
		if prefix != prefix.Masked() {
			return netip.Prefix{}, fmt.Errorf("route prefix is not canonical")
		}
		return prefix, nil
	}
	address, err := netip.ParseAddr(text)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(address, address.BitLen()), nil
}

func mainTable(raw any) bool {
	switch value := raw.(type) {
	case string:
		return value == "main"
	case int:
		return value == 254
	case int64:
		return value == 254
	case uint64:
		return value == 254
	default:
		return false
	}
}

func makeFragment(logicalID string, mtuMatchesDesired, addressPresent, routePresent bool, mtu int, host, workload netip.Prefix, gateway netip.Addr) ([]byte, error) {
	entry := map[string]any{}
	if !mtuMatchesDesired {
		entry["mtu"] = mtu
	}
	if !addressPresent {
		entry["addresses"] = []string{host.String()}
	}
	if !routePresent {
		entry["routes"] = []map[string]string{{"to": workload.String(), "via": gateway.String()}}
	}
	if len(entry) == 0 {
		return nil, nil
	}
	data, err := yaml.Marshal(map[string]any{"network": map[string]any{"ethernets": map[string]any{logicalID: entry}}})
	if err != nil {
		return nil, fmt.Errorf("cannot encode Netplan fragment")
	}
	return data, nil
}

func lastIPv4(prefix netip.Prefix) netip.Addr {
	base := prefix.Masked().Addr().As4()
	value := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	value += uint32(1)<<uint(32-prefix.Bits()) - 1
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)})
}
