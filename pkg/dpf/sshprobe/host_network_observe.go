// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/netip"
	"path"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hostnetwork"
)

// HostNetworkObservation is a point-in-time backend/PF observation, not lease,
// application or readiness evidence. Private raw configuration is deliberately
// excluded from serialization, logs and receipts.
type HostNetworkObservation struct {
	Identity            HostNetworkIdentity `json:"identity"`
	MTU                 int                 `json:"mtu"`
	MinMTU              int                 `json:"minMTU"`
	MinimumWorkloadMTU  int                 `json:"minimumWorkloadMTU"`
	MaxMTU              int                 `json:"maxMTU"`
	ManagementInterface string              `json:"managementInterface"`
	BackendVersion      string              `json:"backendVersion"`
	SourceDigest        string              `json:"sourceDigest"`
	raw                 hostNetworkSnapshot
	connection          string
	merged              []byte
}

type hostNetworkSnapshot struct {
	Error                    string          `json:"error,omitempty"`
	WaitOnlineOtherMatchOnly *bool           `json:"waitOnlineOtherMatchOnly"`
	OS                       string          `json:"os"`
	NetplanVersion           string          `json:"netplanVersion"`
	SystemdVersion           string          `json:"systemdVersion"`
	IPVersion                string          `json:"ipVersion"`
	Networkd                 string          `json:"networkd"`
	OtherManager             string          `json:"otherManager"`
	UUID                     string          `json:"uuid"`
	BootID                   string          `json:"bootID"`
	VPD                      string          `json:"vpd"`
	Binding                  string          `json:"binding"`
	PhysicalPort             string          `json:"physicalPort"`
	VF                       bool            `json:"vf"`
	PermanentMAC             string          `json:"permanentMAC"`
	Link                     json.RawMessage `json:"link"`
	Addresses                json.RawMessage `json:"addresses"`
	Routes                   json.RawMessage `json:"routes"`
	Rules4                   json.RawMessage `json:"rules4"`
	Rules6                   json.RawMessage `json:"rules6"`
	DefaultRoutes6           json.RawMessage `json:"defaultRoutes6"`
	ManagementRoute          json.RawMessage `json:"managementRoute"`
	ManagementAddrs          json.RawMessage `json:"managementAddresses"`
	ManagementDevice         string          `json:"managementDevice"`
	NetworkFile              string          `json:"networkFile"`
	Merged                   string          `json:"merged"`
	SourceDigest             string          `json:"sourceDigest"`
	Fence                    string          `json:"fence"`
	RuntimeDigest            string          `json:"runtimeDigest"`
}

type hostNetworkBackendRequest struct {
	Request            HostNetworkRequest `json:"request"`
	RequestID          string             `json:"requestID"`
	Key                string             `json:"key"`
	Connection         string             `json:"connection"`
	Fence              string             `json:"fence"`
	RuntimeDigest      string             `json:"runtimeDigest"`
	LogicalID          string             `json:"logicalID"`
	Fragment           string             `json:"fragment"`
	OldMTU             int                `json:"oldMTU"`
	AddressExists      bool               `json:"addressExists"`
	RouteExists        bool               `json:"routeExists"`
	ProbeAddress       string             `json:"probeAddress"`
	WaitOnlineOptional bool               `json:"waitOnlineOptional"`
}

func hostNetworkKey(identity HostNetworkIdentity) string {
	return networkDigest([]string{strings.ToLower(identity.SystemUUID), identity.AdapterSerial, strings.ToLower(identity.PCIAddress), strings.ToLower(identity.PermanentMAC)})[:24]
}

func (s *hostNetworkSession) observe(ctx context.Context, identity HostNetworkIdentity) (HostNetworkObservation, error) {
	connection, err := s.remote.Run(ctx, "host-network-ssh-path", `printf '%s' "$SSH_CONNECTION"`)
	if err != nil {
		return HostNetworkObservation{}, err
	}
	parts := strings.Fields(connection)
	if len(parts) != 4 {
		return HostNetworkObservation{}, networkError("HOST_NETWORK_MANAGEMENT_PATH_UNKNOWN", false)
	}
	for _, i := range []int{0, 2} {
		if _, err := netip.ParseAddr(parts[i]); err != nil {
			return HostNetworkObservation{}, networkError("HOST_NETWORK_MANAGEMENT_PATH_UNKNOWN", false)
		}
	}
	payload := hostNetworkBackendRequest{Request: HostNetworkRequest{Identity: identity}, Key: hostNetworkKey(identity), Connection: strings.Join(parts, " ")}
	data, err := s.run(ctx, "snapshot", payload)
	if err != nil {
		return HostNetworkObservation{}, err
	}
	var snapshot hostNetworkSnapshot
	if err := decodeNetworkJSON(data, &snapshot); err != nil {
		return HostNetworkObservation{}, err
	}
	if snapshot.Error != "" {
		return HostNetworkObservation{}, networkError(snapshot.Error, false)
	}
	return validateHostNetworkSnapshot(identity, snapshot, payload.Connection)
}

func validateHostNetworkSnapshot(identity HostNetworkIdentity, raw hostNetworkSnapshot, connection string) (HostNetworkObservation, error) {
	fail := func(code string) (HostNetworkObservation, error) {
		return HostNetworkObservation{}, networkError(code, false)
	}
	if raw.OS != "ubuntu:24.04" || !strings.HasPrefix(raw.NetplanVersion, "1.1.2-") || !strings.HasPrefix(raw.SystemdVersion, "systemd 255 ") ||
		!strings.Contains(raw.IPVersion, "iproute2-6.1.0") || raw.Networkd != "active" || raw.OtherManager != "inactive" {
		return fail("HOST_NETWORK_BACKEND_UNSUPPORTED")
	}
	vpdBytes, err := base64.StdEncoding.DecodeString(raw.VPD)
	if err != nil {
		return fail("HOST_NETWORK_IDENTITY_INVALID")
	}
	serial, _, err := vpd(string(vpdBytes))
	mac, macErr := parsePermanentMAC(raw.PermanentMAC)
	expectedMAC, expectedErr := parsePermanentMAC("Permanent address: " + identity.PermanentMAC)
	if err != nil || serial != identity.AdapterSerial || !strings.EqualFold(raw.UUID, identity.SystemUUID) || !validUUID(raw.BootID) ||
		macErr != nil || expectedErr != nil || mac != expectedMAC || raw.PhysicalPort != identity.PhysicalPort || raw.VF ||
		!strings.HasPrefix(raw.Binding, "/sys/devices/") || strings.ContainsAny(raw.Binding, "\x00\r\n\t ") || path.Clean(raw.Binding) != raw.Binding ||
		!strings.HasSuffix(raw.Binding, "/"+strings.ToLower(identity.PCIAddress)) {
		return fail("HOST_NETWORK_IDENTITY_CHANGED")
	}
	var links []struct {
		Name   string         `json:"ifname"`
		Type   string         `json:"link_type"`
		MTU    int            `json:"mtu"`
		Min    int            `json:"min_mtu"`
		Max    int            `json:"max_mtu"`
		Master string         `json:"master"`
		Flags  []string       `json:"flags"`
		Info   map[string]any `json:"linkinfo"`
	}
	if json.Unmarshal(raw.Link, &links) != nil || len(links) != 1 || links[0].Name != identity.InterfaceName || links[0].Type != "ether" || links[0].MTU <= 0 || links[0].Master != "" || len(links[0].Info) != 0 {
		return fail("HOST_NETWORK_LINK_UNSUPPORTED")
	}
	if links[0].MTU < MinimumHostWorkloadMTU {
		return fail("HOST_NETWORK_MTU_UNSUPPORTED")
	}
	if links[0].Min <= 0 || links[0].Max < links[0].Min {
		return fail("HOST_NETWORK_MTU_CAPABILITY_UNAVAILABLE")
	}
	up := false
	for _, flag := range links[0].Flags {
		up = up || flag == "UP"
	}
	if !up || raw.ManagementDevice == "" || raw.ManagementDevice == identity.InterfaceName || !hostInterfaceName.MatchString(raw.ManagementDevice) {
		return fail("HOST_NETWORK_MANAGEMENT_PATH_CONFLICT")
	}
	if !standardHostNetworkRules(raw.Rules4) || !standardHostNetworkRules(raw.Rules6) {
		return fail("HOST_NETWORK_ROUTING_POLICY_UNSUPPORTED")
	}
	merged, err := base64.StdEncoding.DecodeString(raw.Merged)
	if err != nil || len(merged) == 0 || len(merged) > 1<<20 || !hostNetworkToken.MatchString(raw.SourceDigest) || !hostNetworkToken.MatchString(raw.Fence) {
		return fail("HOST_NETWORK_SOURCE_INVALID")
	}
	selected, err := hostnetwork.InspectNetplanInterface(identity.InterfaceName, identity.PermanentMAC, merged)
	if err != nil {
		return fail("HOST_NETWORK_NETPLAN_UNSUPPORTED")
	}
	if raw.WaitOnlineOtherMatchOnly == nil {
		return fail("HOST_NETWORK_RESPONSE_INVALID")
	}
	if *raw.WaitOnlineOtherMatchOnly && !selected.WaitOnlineOptional {
		return fail("HOST_NETWORK_WAIT_ONLINE_CONTEXT_UNSUPPORTED")
	}
	if !hostNetworkLogicalID.MatchString(selected.LogicalID) || raw.NetworkFile != "/run/systemd/network/10-netplan-"+selected.LogicalID+".network" {
		return fail("HOST_NETWORK_BACKEND_OWNERSHIP_UNAVAILABLE")
	}
	for _, routes := range []json.RawMessage{raw.Routes, raw.DefaultRoutes6} {
		var entries []struct {
			Destination string `json:"dst"`
			Device      string `json:"dev"`
		}
		if json.Unmarshal(routes, &entries) != nil || entries == nil {
			return fail("HOST_NETWORK_ROUTE_OBSERVATION_INVALID")
		}
		for _, route := range entries {
			if route.Destination == "" {
				return fail("HOST_NETWORK_ROUTE_OBSERVATION_INVALID")
			}
			if route.Device == identity.InterfaceName && (route.Destination == "default" || route.Destination == "0.0.0.0/0" || route.Destination == "::/0") {
				return fail("HOST_NETWORK_MANAGEMENT_PATH_CONFLICT")
			}
		}
	}
	raw.Merged = ""
	return HostNetworkObservation{Identity: identity, MTU: links[0].MTU, MinMTU: links[0].Min, MaxMTU: links[0].Max, MinimumWorkloadMTU: MinimumHostWorkloadMTU,
		ManagementInterface: raw.ManagementDevice, BackendVersion: raw.NetplanVersion, SourceDigest: raw.SourceDigest, raw: raw, connection: connection, merged: merged}, nil
}

func standardHostNetworkRules(data []byte) bool {
	var rules []struct {
		Priority int             `json:"priority"`
		Source   string          `json:"src"`
		Table    json.RawMessage `json:"table"`
	}
	if decodeNetworkJSON(data, &rules) != nil || len(rules) < 2 || len(rules) > 3 {
		return false
	}
	seen := map[string]bool{}
	for _, rule := range rules {
		table := strings.Trim(string(rule.Table), `"`)
		standard := rule.Priority == 0 && table == "local" || rule.Priority == 32766 && table == "main" || rule.Priority == 32767 && table == "default"
		if rule.Source != "all" || seen[table] || !standard {
			return false
		}
		seen[table] = true
	}
	return seen["local"] && seen["main"]
}

func (s *hostNetworkSession) configure(ctx context.Context, request HostNetworkRequest) (HostNetworkReceipt, error) {
	if !hostNetworkLeaseBinding.MatchString(request.LeaseBinding) {
		return HostNetworkReceipt{}, networkError("HOST_NETWORK_LEASE_BINDING_INVALID", false)
	}
	if err := validateHostNetworkDesired(request); err != nil {
		return HostNetworkReceipt{}, err
	}
	observation, err := s.observe(ctx, request.Identity)
	if err != nil {
		return HostNetworkReceipt{}, err
	}
	defer clear(observation.merged)
	if request.MTU < observation.MinMTU || request.MTU > observation.MaxMTU {
		return HostNetworkReceipt{}, networkError("HOST_NETWORK_MTU_UNSUPPORTED", false)
	}
	plan, err := hostnetwork.PlanNetplan(hostnetwork.Desired{InterfaceName: request.Identity.InterfaceName, PermanentMAC: request.Identity.PermanentMAC,
		MTU: request.MTU, HostAddressPrefix: request.HostAddressPrefix, Gateway: request.Gateway, WorkloadCIDR: request.WorkloadCIDR, MergedNetplanYAML: observation.merged})
	if err != nil || !hostNetworkLogicalID.MatchString(plan.LogicalID) || observation.raw.NetworkFile != "/run/systemd/network/10-netplan-"+plan.LogicalID+".network" {
		return HostNetworkReceipt{}, networkError("HOST_NETWORK_NETPLAN_CONFLICT", false)
	}
	addressExists, routeExists, err := inspectHostNetworkRuntime(request, observation)
	if err != nil {
		return HostNetworkReceipt{}, err
	}
	id := networkDigest(request)
	workload := netip.MustParsePrefix(request.WorkloadCIDR)
	host := netip.MustParsePrefix(request.HostAddressPrefix)
	probe := workload.Addr().Next()
	if host.Contains(probe) {
		probe = hostLastAddress(host).Next().Next()
	}
	payload := hostNetworkBackendRequest{Request: request, RequestID: id, Key: hostNetworkKey(request.Identity), Connection: observation.connection,
		Fence: observation.raw.Fence, RuntimeDigest: observation.raw.RuntimeDigest, LogicalID: plan.LogicalID, Fragment: base64.StdEncoding.EncodeToString(plan.DesiredFragment), OldMTU: observation.MTU, AddressExists: addressExists, RouteExists: routeExists, ProbeAddress: probe.String(), WaitOnlineOptional: plan.WaitOnlineOptional}
	data, err := s.run(ctx, "configure", payload)
	if err != nil {
		return unknownHostNetworkReceipt(id, payload.Key, "HOST_NETWORK_ACKNOWLEDGEMENT_UNKNOWN"), networkError("HOST_NETWORK_ACKNOWLEDGEMENT_UNKNOWN", true)
	}
	var receipt HostNetworkReceipt
	if err := decodeNetworkJSON(data, &receipt); err != nil || receipt.RequestID != id {
		return unknownHostNetworkReceipt(id, payload.Key, "HOST_NETWORK_ACKNOWLEDGEMENT_UNKNOWN"), networkError("HOST_NETWORK_ACKNOWLEDGEMENT_UNKNOWN", true)
	}
	readFailure := strings.HasPrefix(receipt.ReasonCode, "HOST_NETWORK_READ_")
	invalidReadReason := readFailure && (!validHostNetworkReadReason(receipt.ReasonCode) || receipt.State == "complete" || receipt.State == "verified-external")
	if receipt.ReasonCode != "" && !hostNetworkReason.MatchString(receipt.ReasonCode) || invalidReadReason || len(receipt.JournalPath) > 512 || len(receipt.FragmentPath) > 512 ||
		(receipt.FragmentSHA256 != "" && !hostNetworkToken.MatchString(receipt.FragmentSHA256)) ||
		(receipt.JournalPath != "" && receipt.JournalPath != "/var/lib/l8k-host-network/"+payload.Key+".json") || (receipt.FragmentPath != "" && receipt.FragmentPath != "/etc/netplan/99-l8k-workload-"+payload.Key+".yaml") {
		return unknownHostNetworkReceipt(id, payload.Key, "HOST_NETWORK_RESPONSE_INVALID"), networkError("HOST_NETWORK_RESPONSE_INVALID", true)
	}
	switch receipt.State {
	case "complete", "verified-external", "refused":
	case "partial", "rolled-back":
		receipt.Blocking = true
	default:
		return unknownHostNetworkReceipt(id, payload.Key, "HOST_NETWORK_RESPONSE_INVALID"), networkError("HOST_NETWORK_RESPONSE_INVALID", true)
	}
	if receipt.State != "complete" && receipt.State != "verified-external" {
		if receipt.ReasonCode == "" {
			return unknownHostNetworkReceipt(id, payload.Key, "HOST_NETWORK_RESPONSE_INVALID"), networkError("HOST_NETWORK_RESPONSE_INVALID", true)
		}
		return receipt, networkError(receipt.ReasonCode, receipt.Blocking)
	}
	if !receipt.RuntimeVerified || !receipt.FileVerified || receipt.Blocking {
		return unknownHostNetworkReceipt(id, payload.Key, "HOST_NETWORK_READBACK_FAILED"), networkError("HOST_NETWORK_READBACK_FAILED", true)
	}
	return receipt, nil
}

func inspectHostNetworkRuntime(request HostNetworkRequest, observation HostNetworkObservation) (bool, bool, error) {
	fail := func() (bool, bool, error) { return false, false, networkError("HOST_NETWORK_RUNTIME_CONFLICT", false) }
	host := netip.MustParsePrefix(request.HostAddressPrefix)
	workload := netip.MustParsePrefix(request.WorkloadCIDR)
	for _, field := range strings.Fields(observation.connection) {
		if address, err := netip.ParseAddr(field); err == nil && address.Is4() && workload.Contains(address) {
			return fail()
		}
	}
	var addresses []struct {
		Info []struct {
			Family string `json:"family"`
			Local  string `json:"local"`
			Prefix int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if json.Unmarshal(observation.raw.Addresses, &addresses) != nil || addresses == nil || len(addresses) > 1 {
		return fail()
	}
	addressExists := false
	for _, link := range addresses {
		for _, address := range link.Info {
			if address.Family != "inet" {
				return fail()
			}
			if address.Local != host.Addr().String() || address.Prefix != host.Bits() || addressExists {
				return fail()
			}
			addressExists = true
		}
	}
	var routes []map[string]json.RawMessage
	if json.Unmarshal(observation.raw.Routes, &routes) != nil || routes == nil {
		return fail()
	}
	routeExists := false
	for _, route := range routes {
		str := func(key string) string { var value string; _ = json.Unmarshal(route[key], &value); return value }
		destination := str("dst")
		if destination == "default" {
			if str("dev") == request.Identity.InterfaceName {
				return fail()
			}
			continue
		}
		prefix, err := netip.ParsePrefix(destination)
		if err != nil {
			if address, e := netip.ParseAddr(destination); e == nil {
				prefix = netip.PrefixFrom(address, address.BitLen())
			} else {
				return fail()
			}
		}
		if !prefix.Addr().Is4() || !prefix.Overlaps(workload) {
			continue
		}
		if prefix.Bits() == 0 && str("dev") == request.Identity.InterfaceName {
			return fail()
		}
		if prefix == workload {
			table := strings.Trim(string(route["table"]), `"`)
			if str("dev") != request.Identity.InterfaceName || str("gateway") != request.Gateway || routeExists || len(route["multipath"]) != 0 ||
				(table != "" && table != "main" && table != "254") || str("type") != "" && str("type") != "unicast" {
				return fail()
			}
			routeExists = true
		} else if workload.Contains(prefix.Addr()) {
			// The address's own kernel connected/local routes are the only
			// more-specific overlapping entries that this transaction permits.
			kernelConnected := prefix == host.Masked() && str("type") == "" && str("table") == ""
			kernelLocal := prefix.Bits() == 32 && (prefix.Addr() == host.Addr() || prefix.Addr() == hostLastAddress(host)) && str("table") == "local" && (str("type") == "local" || str("type") == "broadcast")
			if str("dev") != request.Identity.InterfaceName || !addressExists || !kernelConnected && !kernelLocal || str("gateway") != "" || str("protocol") != "kernel" {
				return fail()
			}
		}
	}
	return addressExists, routeExists, nil
}
