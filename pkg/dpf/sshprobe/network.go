// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package sshprobe

import (
	"context"
	"encoding/json"
	"net"
	"regexp"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

const dnsmasqRead = `sh -c 'found=0; for f in /etc/dnsmasq.conf /etc/dnsmasq.d/*.conf; do if [ -e "$f" ]; then found=1; [ -r "$f" ] || exit 77; awk '\''/^[[:space:]]*(server=|no-resolv([[:space:]]|$))/ {print}'\'' "$f" || exit; fi; done; [ "$found" -eq 1 ] || exit 66'`

func collectNetwork(ctx context.Context, remote runner, runtime *inventory.RuntimeObservation, add func(string, string) string) {
	n := &inventory.NetworkObservation{Links: []inventory.NetworkLink{}, Neighbors: []inventory.Neighbor{}, LLDP: []inventory.LLDPNeighbor{}, Resolver: inventory.ResolverObservation{Owner: "unknown", Search: []string{}, PerLink: map[string][]string{}, Domains: map[string][]string{}, DNSMasqServers: []string{}}}
	runtime.Network = n
	for _, p := range []struct{ stage, command string }{
		{"network-links", "ip -j link show"}, {"network-link-details", "ip -j -d link show"}, {"neighbors", "ip -j neighbor show"}, {"resolver-path", "readlink -f /etc/resolv.conf"},
		{"resolver-dns", "resolvectl dns"}, {"resolver-domains", "resolvectl domain"}, {"dnsmasq", dnsmasqRead}, {"lldp", "lldpctl -f keyvalue"},
	} {
		text, err := remote.Run(ctx, p.stage, p.command)
		if err == nil {
			switch p.stage {
			case "network-links":
				n.Links, err = parseLinks(text)
			case "network-link-details":
				var details []inventory.NetworkLink
				details, err = parseLinks(text)
				if err == nil {
					err = mergeLinkDetails(n.Links, details)
				}
			case "neighbors":
				n.Neighbors, err = parseNeighbors(text)
			case "resolver-path":
				value := strings.TrimSpace(text)
				if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\r\n\x00") {
					err = &Error{Code: "SSH_RUNTIME_INVALID"}
					break
				}
				n.Resolver.Path = value
				switch {
				case strings.Contains(value, "/systemd/resolve/"):
					n.Resolver.Owner = "systemd-resolved"
				case strings.Contains(value, "/NetworkManager/"):
					n.Resolver.Owner = "NetworkManager"
				case strings.Contains(value, "/resolvconf/"):
					n.Resolver.Owner = "resolvconf"
				default:
					n.Resolver.Owner = "file"
				}
			case "resolver-dns":
				n.Resolver.PerLink, err = parseResolved(text, true)
			case "resolver-domains":
				n.Resolver.Domains, err = parseResolved(text, false)
			case "dnsmasq":
				n.Resolver.DNSMasqServers, err = parseDNSMasq(text)
				if err == nil {
					noResolv := false
					for _, line := range strings.Split(text, "\n") {
						if strings.TrimSpace(line) == "no-resolv" {
							noResolv = true
						}
					}
					n.Resolver.DNSMasqNoResolv = &noResolv
				}
			case "lldp":
				n.LLDP, err = parseLLDP(text)
			}
		}
		if err != nil {
			runtime.Observations[p.stage] = unavailable(err, commandTool(p.command))
		} else {
			runtime.Observations[p.stage] = observed(commandTool(p.command))
			add(p.stage, p.stage)
		}
	}
}

func parseLinks(text string) ([]inventory.NetworkLink, error) {
	var rows []struct {
		Name   string `json:"ifname"`
		MAC    string `json:"address"`
		Master string `json:"master"`
		Info   struct {
			Kind string `json:"info_kind"`
		} `json:"linkinfo"`
	}
	if err := json.Unmarshal([]byte(text), &rows); err != nil || rows == nil || len(rows) > 4096 {
		return nil, &Error{Code: "SSH_RUNTIME_INVALID"}
	}
	out := []inventory.NetworkLink{}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Name == "" || seen[r.Name] {
			return nil, &Error{Code: "SSH_RUNTIME_INVALID"}
		}
		seen[r.Name] = true
		// Non-Ethernet link-layer addresses (e.g. InfiniBand) are retained as facts.
		out = append(out, inventory.NetworkLink{Name: r.Name, MAC: r.MAC, Master: r.Master, Kind: r.Info.Kind})
	}
	return out, nil
}
func parseNeighbors(text string) ([]inventory.Neighbor, error) {
	var rows []struct {
		Address   string   `json:"dst"`
		MAC       string   `json:"lladdr"`
		Interface string   `json:"dev"`
		State     []string `json:"state"`
	}
	if err := json.Unmarshal([]byte(text), &rows); err != nil || rows == nil || len(rows) > 8192 {
		return nil, &Error{Code: "SSH_RUNTIME_INVALID"}
	}
	out := []inventory.Neighbor{}
	for _, r := range rows {
		if net.ParseIP(r.Address) == nil || r.Interface == "" {
			return nil, &Error{Code: "SSH_RUNTIME_INVALID"}
		}
		if r.MAC != "" {
			if mac, err := net.ParseMAC(r.MAC); err != nil || len(mac) != 6 {
				continue
			}
		}
		out = append(out, inventory.Neighbor{Address: r.Address, MAC: strings.ToLower(r.MAC), Interface: r.Interface, State: r.State})
	}
	return out, nil
}

var resolvedLine = regexp.MustCompile(`^(Global|Link [0-9]+ \(([^)]+)\)):\s*(.*)$`)

func parseResolved(text string, addresses bool) (map[string][]string, error) {
	out := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		m := resolvedLine.FindStringSubmatch(line)
		if m == nil {
			return nil, &Error{Code: "SSH_RUNTIME_INVALID"}
		}
		key := m[2]
		if key == "" {
			key = "global"
		}
		if _, exists := out[key]; exists {
			return nil, &Error{Code: "SSH_RUNTIME_INVALID"}
		}
		values := strings.Fields(m[3])
		for _, v := range values {
			if addresses {
				ip, _, _ := strings.Cut(v, "%")
				if net.ParseIP(ip) == nil {
					return nil, &Error{Code: "SSH_RUNTIME_INVALID"}
				}
			}
		}
		out[key] = values
	}
	if len(out) == 0 {
		return nil, &Error{Code: "SSH_RUNTIME_INVALID"}
	}
	return out, nil
}

var dnsmasqValue = regexp.MustCompile(`^[A-Za-z0-9._:/#%~@+-]+$`)

func parseDNSMasq(text string) ([]string, error) {
	out := []string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "no-resolv" || strings.HasPrefix(line, "no-resolv ") {
			continue
		}
		value, ok := strings.CutPrefix(line, "server=")
		if !ok || !dnsmasqValue.MatchString(value) {
			return nil, &Error{Code: "SSH_RUNTIME_INVALID"}
		}
		out = append(out, value)
	}
	return out, nil
}
func parseLLDP(text string) ([]inventory.LLDPNeighbor, error) {
	byKey := map[string]inventory.LLDPNeighbor{}
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		key, ok = strings.CutPrefix(key, "lldp.")
		if !ok {
			continue
		}
		for _, suffix := range []string{".chassis.name", ".port.ifname", ".port.descr"} {
			iface, ok := strings.CutSuffix(key, suffix)
			if !ok {
				continue
			}
			if iface == "" || len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
				return nil, &Error{Code: "SSH_RUNTIME_INVALID"}
			}
			n := byKey[iface]
			n.Interface = iface
			if suffix == ".chassis.name" {
				if n.Chassis != "" && n.Chassis != value {
					return nil, &Error{Code: "SSH_RUNTIME_INVALID"}
				}
				n.Chassis = value
			} else if suffix == ".port.ifname" || n.Port == "" {
				n.Port = value
			}
			byKey[iface] = n
		}
	}
	keys := []string{}
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []inventory.LLDPNeighbor{}
	for _, k := range keys {
		out = append(out, byKey[k])
	}
	if len(out) == 0 && strings.TrimSpace(text) != "" {
		return nil, &Error{Code: "SSH_RUNTIME_INVALID"}
	}
	return out, nil
}

// Detailed iproute output is optional: some releases emit invalid JSON for
// VXLAN attributes. Base links remain useful without detailed link kinds. Verify
// a complete unchanged name/MAC/master projection before enriching those facts.
func mergeLinkDetails(base, details []inventory.NetworkLink) error {
	if len(base) != len(details) {
		return &Error{Code: "SSH_RUNTIME_INVALID", Stage: "network-link-details"}
	}
	byName := map[string]inventory.NetworkLink{}
	for _, d := range details {
		byName[d.Name] = d
	}
	for _, b := range base {
		d, exists := byName[b.Name]
		if !exists || d.MAC != b.MAC || d.Master != b.Master {
			return &Error{Code: "SSH_RUNTIME_INVALID", Stage: "network-link-details"}
		}
	}
	for i := range base {
		base[i].Kind = byName[base[i].Name].Kind
	}
	return nil
}
