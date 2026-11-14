// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package addresslookup resolves explicitly selected MAC locators without contacting candidates.
package addresslookup

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

type Source struct {
	File   string `yaml:"file"`
	Format string `yaml:"format"`
}
type Selector struct {
	Name string `yaml:"name"`
	Role string `yaml:"role"`
	MAC  string `yaml:"mac"`
}
type Config struct {
	Sources []Source   `yaml:"sources,omitempty"`
	Devices []Selector `yaml:"devices"`
}

// Request contains normalized selectors and absolute source paths, no credentials.
type Request struct {
	Sources []Source
	Devices []Selector
}
type Neighbor struct{ Address, MAC, State, Source, EvidenceRef string }

func MAC(value string) (string, error) {
	m, err := net.ParseMAC(value)
	if err != nil || len(m) != 6 || m[0]&1 != 0 || m.String() == "00:00:00:00:00:00" {
		return "", fmt.Errorf("expected a unicast six-byte MAC address")
	}
	return m.String(), nil
}
func Validate(c Config) (Request, error) {
	r := Request{Sources: slices.Clone(c.Sources), Devices: slices.Clone(c.Devices)}
	if len(r.Devices) == 0 || len(r.Devices) > 256 || len(r.Sources) > 16 {
		return r, fmt.Errorf("addressLookup needs 1-256 devices and at most 16 sources")
	}
	names := map[string]bool{}
	for i, d := range r.Devices {
		if d.Name == "" || len(d.Name) > 63 || strings.ContainsAny(d.Name, "/\\\n\r\t ") || names[d.Name] {
			return r, fmt.Errorf("addressLookup device names must be unique nonempty labels")
		}
		names[d.Name] = true
		if d.Role != "arm" && d.Role != "bmc" {
			return r, fmt.Errorf("addressLookup device role must be arm or bmc")
		}
		mac, err := MAC(d.MAC)
		if err != nil {
			return r, err
		}
		r.Devices[i].MAC = mac
	}
	for _, s := range r.Sources {
		if s.File == "" || s.Format != "dnsmasq-leases" {
			return r, fmt.Errorf("addressLookup source needs file and format: dnsmasq-leases")
		}
	}
	return r, nil
}

type lease struct {
	mac, address string
	expired      bool
}

func leases(data []byte, now time.Time) ([]lease, error) {
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("lease source exceeds 1 MiB")
	}
	out := []lease{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		// The five-column dnsmasq IPv4 lease format; never guess other formats.
		if len(f) != 5 {
			return nil, fmt.Errorf("invalid dnsmasq lease record")
		}
		expiry, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil || expiry < 0 {
			return nil, fmt.Errorf("invalid lease expiry")
		}
		mac, err := MAC(f[1])
		if err != nil {
			return nil, err
		}
		ip, err := netip.ParseAddr(f[2])
		if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() {
			return nil, fmt.Errorf("invalid lease address")
		}
		out = append(out, lease{mac, ip.String(), expiry != 0 && expiry <= now.Unix()})
		if len(out) > 4096 {
			return nil, fmt.Errorf("too many lease records")
		}
	}
	return out, nil
}

// Collect uses a bounded reader supplied by the caller. Invalid sources contribute
// no candidates. Valid candidates remain hints even when other sources fail.
func Collect(r Request, now time.Time, read func(string, int64) ([]byte, error), neighbors []Neighbor) (*inventory.AddressLookup, map[string]inventory.Evidence) {
	out := &inventory.AddressLookup{Candidates: []inventory.AddressCandidate{}, Observations: map[string]inventory.Observation{}}
	evidence := map[string]inventory.Evidence{}
	expired := map[string]bool{}
	add := func(mac, address, source, ref string) {
		for _, d := range r.Devices {
			if d.MAC == mac {
				out.Candidates = append(out.Candidates, inventory.AddressCandidate{Name: d.Name, Role: d.Role, MAC: mac, Address: address, Source: source, EvidenceRef: ref})
			}
		}
	}
	for i, s := range r.Sources {
		key := fmt.Sprintf("source.%d", i)
		data, err := read(s.File, 1<<20)
		if err != nil {
			out.Observations[key] = inventory.Observation{State: "unavailable", ReasonCode: "ADDRESS_SOURCE_UNREADABLE", Message: "Cannot read the configured bounded lease file; check that it exists and is readable."}
			continue
		}
		rows, err := leases(data, now)
		if err != nil {
			out.Observations[key] = inventory.Observation{State: "unavailable", ReasonCode: "ADDRESS_SOURCE_INVALID", Message: "The lease file is not a bounded supported dnsmasq IPv4 lease table."}
			continue
		}
		ref := inventory.Digest("dpf-address-source-v1", []string{key, inventory.Digest("lease-bytes-v1", data)})
		evidence[ref] = inventory.Evidence{Kind: "lease-file", InputRef: fmt.Sprintf("/discovery/addressLookup/sources/%d", i), ObservedAt: now, Vantage: "executor", Property: "MAC/IP/expiry"}
		out.Observations[key] = inventory.Observation{State: "observed"}
		for _, row := range rows {
			if row.expired {
				expired[row.mac] = true
				continue
			}
			add(row.mac, row.address, key, ref)
		}
	}
	for _, n := range neighbors {
		mac, err := MAC(n.MAC)
		if err != nil {
			continue
		}
		ip, err := netip.ParseAddr(n.Address)
		if err != nil || !ip.IsGlobalUnicast() {
			continue
		}
		// Only usable kernel entries; stale entries are not proof of current reachability.
		if n.State != "REACHABLE" && n.State != "PERMANENT" {
			continue
		}
		add(mac, ip.String(), n.Source, n.EvidenceRef)
	}
	slices.SortFunc(out.Candidates, func(a, b inventory.AddressCandidate) int {
		return strings.Compare(a.Name+"/"+a.Address+"/"+a.Source, b.Name+"/"+b.Address+"/"+b.Source)
	})
	out.Candidates = slices.Compact(out.Candidates)
	for _, d := range r.Devices {
		ips := map[string]bool{}
		for _, c := range out.Candidates {
			if c.Name == d.Name {
				ips[c.Address] = true
			}
		}
		o := inventory.Observation{State: "unknown", ReasonCode: "ADDRESS_NOT_FOUND", Message: "No usable lease or reachable neighbor matches the requested MAC; provide an address or refresh the source."}
		if expired[d.MAC] {
			o.ReasonCode = "ADDRESS_LEASE_EXPIRED"
			o.Message = "Only expired matching lease evidence is available; refresh the lease source."
		}
		if len(ips) > 1 {
			o.ReasonCode = "ADDRESS_AMBIGUOUS"
			o.Message = "Sources report multiple addresses for this MAC; verify the intended endpoint before connecting."
		}
		if len(ips) == 1 {
			o.State = "candidate"
			o.ReasonCode = "ADDRESS_UNVERIFIED"
			o.Message = "One address candidate was found; verify endpoint identity and configure explicit credentials and trust before connecting."
			for ip := range ips {
				o.Values = []string{ip}
			}
		}
		out.Observations["device."+d.Name] = o
	}
	return out, evidence
}
