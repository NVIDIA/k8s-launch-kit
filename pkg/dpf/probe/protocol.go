// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package probe contains the public diagnostic protocol and Kubernetes runner.
package probe

import (
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

const ProtocolVersion = "v1alpha1"

// collectorScript adapts the existing image's tools to our public result format.
// It is fixed release code, never site-supplied commands or a downloaded helper.
//
//go:embed collect.sh
var collectorScript string

type Destination struct {
	ID                 string `json:"id"`
	Endpoint           string `json:"endpoint"`
	Path               string `json:"path"`
	ServerName         string `json:"serverName,omitempty"`
	CA                 []byte `json:"ca,omitempty"`
	InsecureSkipVerify bool   `json:"insecureSkipVerify,omitempty"`
}
type Request struct {
	TenantVIP      string        `json:"tenantVIP,omitempty"`
	RequestTimeout time.Duration `json:"requestTimeout"`
	Version        string        `json:"version"`
	RunID          string        `json:"runID"`
	Deadline       time.Time     `json:"deadline"`
	Destinations   []Destination `json:"destinations"`
}
type Response struct {
	TenantVIPRoute *inventory.RouteLookup  `json:"tenantVIPRoute,omitempty"`
	StartedAt      time.Time               `json:"startedAt"`
	FinishedAt     time.Time               `json:"finishedAt"`
	Version        string                  `json:"version"`
	RunID          string                  `json:"runID"`
	NodeUID        string                  `json:"nodeUID"`
	PodUID         string                  `json:"podUID"`
	NetworkMode    string                  `json:"networkMode"`
	Results        []inventory.ProbeResult `json:"results"`
	Interfaces     []inventory.Interface   `json:"interfaces,omitempty"`
	Routes         []inventory.Route       `json:"routes,omitempty"`
}

type toolDestination struct {
	ID                 string `json:"id"`
	Hostname           string `json:"hostname"`
	Port               string `json:"port"`
	Literal            bool   `json:"literal"`
	URL                string `json:"url"`
	Authority          string `json:"authority"`
	VerifyName         string `json:"verifyName"`
	CAFile             string `json:"caFile"`
	InsecureSkipVerify bool   `json:"insecureSkipVerify,omitempty"`
}

// requestFiles resolves URL/trust coordinates in Go. The script consumes JSON
// values as quoted arguments; it never parses URLs or evaluates site input.
func requestFiles(request Request) (map[string]string, error) {
	if request.Version != ProtocolVersion || request.RunID == "" || len(request.Destinations) == 0 || len(request.Destinations) > 257 || time.Until(request.Deadline) > time.Hour || time.Until(request.Deadline) <= 0 || request.RequestTimeout < time.Second || request.RequestTimeout > 2*time.Minute {
		return nil, fmt.Errorf("invalid probe request scope/deadline")
	}
	if request.TenantVIP != "" {
		vip, err := netip.ParseAddr(request.TenantVIP)
		if err != nil || !vip.Is4() || vip.Is4In6() || !vip.IsGlobalUnicast() || vip.String() != request.TenantVIP {
			return nil, fmt.Errorf("invalid tenant VIP route lookup target")
		}
	}
	files := map[string]string{"collect.sh": collectorScript}
	destinations := []toolDestination{}
	seen := map[string]bool{}
	for i, destination := range request.Destinations {
		u, err := url.Parse(destination.Endpoint)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || strings.ContainsAny(u.Host, "\r\n") || !strings.HasPrefix(destination.Path, "/") || destination.ID == "" || seen[destination.ID] {
			return nil, fmt.Errorf("invalid or duplicate probe destination")
		}
		if destination.InsecureSkipVerify && (destination.ID == "management-api" || len(destination.CA) != 0) {
			return nil, fmt.Errorf("TLS verification bypass is not allowed for the management API or with a CA")
		}
		seen[destination.ID] = true
		port := u.Port()
		if port == "" {
			port = "443"
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return nil, fmt.Errorf("invalid destination port")
		}
		name := destination.ServerName
		if name == "" {
			name = u.Hostname()
		}
		if strings.ContainsAny(name, "\r\n /@?#%") {
			return nil, fmt.Errorf("invalid TLS server name")
		}
		host := name
		if strings.Contains(host, ":") {
			if _, err := netip.ParseAddr(host); err != nil {
				return nil, fmt.Errorf("invalid TLS server name")
			}
			host = "[" + host + "]"
		}
		probeURL := url.URL{Scheme: "https", Host: host + ":" + port, Path: destination.Path}
		_, literalErr := netip.ParseAddr(u.Hostname())
		d := toolDestination{ID: destination.ID, Hostname: u.Hostname(), Port: port, Literal: literalErr == nil, URL: probeURL.String(), Authority: u.Host, VerifyName: name, InsecureSkipVerify: destination.InsecureSkipVerify}
		if len(destination.CA) > 0 {
			if !x509.NewCertPool().AppendCertsFromPEM(destination.CA) {
				return nil, fmt.Errorf("invalid public probe CA")
			}
			key := fmt.Sprintf("ca-%d.pem", i)
			files[key] = string(destination.CA)
			d.CAFile = "/request/" + key
		}
		destinations = append(destinations, d)
	}
	document := struct {
		TenantVIP      string            `json:"tenantVIP,omitempty"`
		Version        string            `json:"version"`
		RunID          string            `json:"runID"`
		Deadline       time.Time         `json:"deadline"`
		RequestTimeout time.Duration     `json:"requestTimeout"`
		Destinations   []toolDestination `json:"destinations"`
	}{request.TenantVIP, request.Version, request.RunID, request.Deadline.UTC(), request.RequestTimeout, destinations}
	data, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	files["request.json"] = string(data)
	size := 0
	for key, value := range files {
		size += len(key) + len(value)
	}
	if size > 900<<10 {
		return nil, fmt.Errorf("probe request exceeds ConfigMap budget")
	}
	return files, nil
}

// ValidateNetworkResponse validates the host-network portion independently of
// connectivity rows, so callers cannot accept absent or contradictory facts.
func ValidateNetworkResponse(out Response, req Request, mode string) error {
	if mode != "pod-network" && mode != "node-network" || out.NetworkMode != mode {
		return fmt.Errorf("invalid network observation mode")
	}
	if req.TenantVIP != "" {
		vip, err := netip.ParseAddr(req.TenantVIP)
		if err != nil || !vip.Is4() || vip.Is4In6() || !vip.IsGlobalUnicast() || vip.String() != req.TenantVIP {
			return fmt.Errorf("invalid tenant VIP route lookup request")
		}
	}
	if mode == "pod-network" {
		if len(out.Interfaces) != 0 || len(out.Routes) != 0 || out.TenantVIPRoute != nil {
			return fmt.Errorf("pod-network response contains host-network facts")
		}
		return nil
	}
	if len(out.Interfaces) == 0 || out.Routes == nil {
		return fmt.Errorf("node-network response is missing interface or route facts")
	}
	if len(out.Interfaces) > maxNetworkInterfaces || len(out.Routes) > maxNetworkRoutes {
		return fmt.Errorf("node-network response exceeds fact bounds")
	}
	byIndex := make(map[int]struct{}, len(out.Interfaces))
	byName := make(map[string]struct{}, len(out.Interfaces))
	for _, iface := range out.Interfaces {
		if iface.Name == "" || len(iface.Name) > maxInterfaceNameLength || hasControl(iface.Name) || iface.Index <= 0 || iface.Index > maxInterfaceIndex || iface.MTU <= 0 || iface.MTU > maxInterfaceMTU || iface.AdminUp == nil || iface.OperState == "" || len(iface.OperState) > maxOperStateLength || hasControl(iface.OperState) {
			return fmt.Errorf("node-network response contains incomplete interface facts")
		}
		if _, ok := byIndex[iface.Index]; ok {
			return fmt.Errorf("node-network response contains duplicate interface index")
		}
		if _, ok := byName[iface.Name]; ok {
			return fmt.Errorf("node-network response contains duplicate interface name")
		}
		byIndex[iface.Index] = struct{}{}
		byName[iface.Name] = struct{}{}
		if len(iface.Addresses) > maxInterfaceAddresses {
			return fmt.Errorf("node-network response exceeds interface address bound")
		}
		seenPrefixes := make(map[string]struct{}, len(iface.Addresses))
		for _, value := range iface.Addresses {
			prefix, err := netip.ParsePrefix(value)
			if err != nil || prefix.Addr().Is4In6() || prefix.String() != value {
				return fmt.Errorf("node-network response contains invalid interface prefix")
			}
			if _, ok := seenPrefixes[value]; ok {
				return fmt.Errorf("node-network response contains duplicate interface prefix")
			}
			seenPrefixes[value] = struct{}{}
		}
	}
	for _, route := range out.Routes {
		if !validRouteDestination(route.Destination) || route.InterfaceIndex < 0 || route.InterfaceIndex > maxInterfaceIndex || route.Table < 0 || route.Table > maxRouteTable {
			return fmt.Errorf("node-network response contains invalid route facts")
		}
		if route.InterfaceIndex > 0 {
			if _, ok := byIndex[route.InterfaceIndex]; !ok {
				return fmt.Errorf("node-network response route refers to an unknown interface")
			}
		}
		if route.Gateway != "" && !validRouteIP(route.Gateway) {
			return fmt.Errorf("node-network response contains invalid route gateway")
		}
	}
	if req.TenantVIP == "" {
		if out.TenantVIPRoute != nil {
			return fmt.Errorf("unexpected tenant VIP route lookup")
		}
		return nil
	}
	lookup := out.TenantVIPRoute
	if lookup == nil || lookup.Destination != req.TenantVIP {
		return fmt.Errorf("missing or mismatched tenant VIP route lookup")
	}
	if lookup.ReasonCode != "" || lookup.Message != "" {
		if !validRouteFailure(lookup.ReasonCode, lookup.Message) || lookup.InterfaceIndex != 0 || lookup.Source != "" || lookup.Gateway != "" {
			return fmt.Errorf("invalid failed tenant VIP route lookup")
		}
		return nil
	}
	if lookup.InterfaceIndex <= 0 || lookup.InterfaceIndex > maxInterfaceIndex || lookup.Source == "" || !validRouteIPv4(lookup.Source) {
		return fmt.Errorf("incomplete tenant VIP route lookup")
	}
	if _, ok := byIndex[lookup.InterfaceIndex]; !ok {
		return fmt.Errorf("tenant VIP route lookup refers to an unknown interface")
	}
	if lookup.Gateway != "" && !validRouteIPv4(lookup.Gateway) {
		return fmt.Errorf("invalid tenant VIP route gateway")
	}
	return nil
}

const (
	maxNetworkInterfaces   = 256
	maxNetworkRoutes       = 4096
	maxInterfaceAddresses  = 256
	maxInterfaceNameLength = 255
	maxOperStateLength     = 32
	maxInterfaceIndex      = 1<<32 - 1
	maxInterfaceMTU        = 1 << 20
	maxRouteTable          = 1<<32 - 1
	maxReasonCodeLength    = 64
	maxRouteFailureMessage = 192
)

func validRouteDestination(value string) bool {
	if value == "default" {
		return true
	}
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return !prefix.Addr().Is4In6() && prefix.String() == value
	}
	if addr, err := netip.ParseAddr(value); err == nil {
		return !addr.Is4In6() && addr.String() == value
	}
	return false
}

func validRouteIP(value string) bool {
	addr, err := netip.ParseAddr(value)
	return err == nil && !addr.Is4In6() && !addr.IsUnspecified() && !addr.IsMulticast() && !isIPv4Broadcast(addr)
}

func validRouteIPv4(value string) bool {
	addr, err := netip.ParseAddr(value)
	return err == nil && addr.Is4() && !addr.Is4In6() && !addr.IsUnspecified() && !addr.IsMulticast() && !isIPv4Broadcast(addr)
}

func isIPv4Broadcast(addr netip.Addr) bool {
	return addr.Is4() && addr.String() == "255.255.255.255"
}

func validRouteFailure(reason, message string) bool {
	if len(reason) > maxReasonCodeLength || len(message) > maxRouteFailureMessage || hasControl(reason) || hasControl(message) {
		return false
	}
	switch reason {
	case "ROUTE_LOOKUP_TIMEOUT":
		return message == "IPv4 host route lookup timed out"
	case "ROUTE_LOOKUP_FAILED":
		return message == "IPv4 host route lookup failed"
	case "ROUTE_INTERFACE_UNAVAILABLE":
		return message == "IPv4 route interface was not present in the host interface snapshot"
	default:
		return false
	}
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
