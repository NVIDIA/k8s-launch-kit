// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

// Package hbnruntime validates bounded FRR BGP summary JSON. Established
// sessions do not prove route installation or forwarding.
package hbnruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

const MaxSummaryBytes = 1 << 20
const SummaryCommand = "show bgp vrf default ipv4 unicast summary json"

// Expected holds explicit resolved ASN expectations for one DPU summary.
type Expected struct {
	LocalASN  uint32
	FabricASN uint32
}

// Peer is the semantic FRR peer subset used by this check.
type Peer struct {
	RemoteAS uint32
	LocalAS  uint32
	State    string
	IDType   string
}

// Summary contains required FRR summary fields and validated peer records.
type Summary struct {
	ASN            uint32
	VRF            string
	PeerCount      uint32
	DisplayedPeers uint32
	TotalPeers     uint32
	FailedPeers    uint32
	Peers          map[string]Peer
}

// Mismatch is a stable, compact validation reason.
type Mismatch struct {
	Reason string
	Peer   string
}

// Parse rejects oversized, ambiguous, malformed, or incomplete JSON.
func Parse(data []byte) (Summary, error) {
	if len(data) > MaxSummaryBytes {
		return Summary{}, errors.New("summary exceeds 1 MiB limit")
	}
	if err := checkJSON(data); err != nil {
		return Summary{}, fmt.Errorf("invalid summary JSON: %w", err)
	}
	var raw struct {
		ASN       json.RawMessage `json:"as"`
		VRF       *string         `json:"vrfName"`
		PeerCount json.RawMessage `json:"peerCount"`
		Displayed json.RawMessage `json:"displayedPeers"`
		Total     json.RawMessage `json:"totalPeers"`
		Failed    json.RawMessage `json:"failedPeers"`
		Peers     map[string]struct {
			Remote json.RawMessage `json:"remoteAs"`
			Local  json.RawMessage `json:"localAs"`
			State  *string         `json:"state"`
			IDType *string         `json:"idType"`
		} `json:"peers"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(&raw); err != nil {
		return Summary{}, fmt.Errorf("decode summary: %w", err)
	}
	asn, e := requiredNumber(raw.ASN, false)
	if e != nil {
		return Summary{}, fmt.Errorf("invalid top-level as: %w", e)
	}
	counters := make([]uint32, 4)
	for i, m := range []json.RawMessage{raw.PeerCount, raw.Displayed, raw.Total, raw.Failed} {
		counters[i], e = requiredNumber(m, true)
		if e != nil {
			return Summary{}, fmt.Errorf("invalid summary counter: %w", e)
		}
	}
	if raw.VRF == nil || *raw.VRF == "" || raw.Peers == nil {
		return Summary{}, errors.New("summary missing vrfName or peers")
	}
	out := Summary{ASN: asn, VRF: *raw.VRF, PeerCount: counters[0], DisplayedPeers: counters[1], TotalPeers: counters[2], FailedPeers: counters[3], Peers: make(map[string]Peer, len(raw.Peers))}
	for name, p := range raw.Peers {
		r, x := requiredNumber(p.Remote, false)
		if x != nil {
			return Summary{}, fmt.Errorf("peer %q invalid remoteAs: %w", name, x)
		}
		l, x := requiredNumber(p.Local, false)
		if x != nil {
			return Summary{}, fmt.Errorf("peer %q invalid localAs: %w", name, x)
		}
		if p.State == nil || p.IDType == nil || *p.State == "" || *p.IDType == "" {
			return Summary{}, fmt.Errorf("peer %q missing state or idType", name)
		}
		out.Peers[name] = Peer{r, l, *p.State, *p.IDType}
	}
	return out, nil
}
func requiredNumber(raw json.RawMessage, allowZero bool) (uint32, error) {
	if len(raw) == 0 || raw[0] < '0' || raw[0] > '9' {
		return 0, errors.New("expected unsigned integer JSON number")
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, errors.New("expected unsigned integer JSON number")
		}
	}
	v, e := strconv.ParseUint(string(raw), 10, 32)
	if e != nil || (!allowZero && v == 0) {
		return 0, errors.New("expected unsigned 32-bit integer in range")
	}
	return uint32(v), nil
}
func validASN(asn uint32) bool {
	return (asn >= 64512 && asn <= 65534) || (asn >= 4200000000 && asn <= 4294967294)
}

// Validate checks one DPU's expected ASN, the exact peer set, counters, and states.
func Validate(s Summary, expected Expected) []Mismatch {
	var out []Mismatch
	add := func(reason, peer string) { out = append(out, Mismatch{reason, peer}) }
	if !validASN(expected.LocalASN) || !validASN(expected.FabricASN) || expected.LocalASN == expected.FabricASN {
		add("INVALID_EXPECTED_ASN", "")
		return out
	}
	if s.ASN != expected.LocalASN {
		add("LOCAL_ASN_MISMATCH", "")
	}
	if s.VRF != "default" {
		add("VRF_MISMATCH", "")
	}
	if len(s.Peers) != 2 {
		add("PEER_COUNT_MISMATCH", "")
	}
	count := uint32(len(s.Peers))
	if s.PeerCount != count || s.DisplayedPeers != count || s.TotalPeers != count {
		add("PEER_COUNTER_MISMATCH", "")
	}
	// FRR's failedPeers counter has semantics beyond the displayed state string.
	// Require zero for a passing summary without deriving it from peer states.
	if s.FailedPeers != 0 {
		add("FAILED_PEER_COUNTER_NONZERO", "")
	}
	for _, name := range []string{"p0_if", "p1_if"} {
		p, ok := s.Peers[name]
		if !ok {
			add("PEER_MISSING", name)
			continue
		}
		if p.LocalAS != expected.LocalASN {
			add("PEER_LOCAL_ASN_MISMATCH", name)
		}
		if p.RemoteAS != expected.FabricASN {
			add("PEER_REMOTE_ASN_MISMATCH", name)
		}
		if p.State != "Established" {
			add("PEER_NOT_ESTABLISHED", name)
		}
		if p.IDType != "interface" {
			add("PEER_ID_TYPE_MISMATCH", name)
		}
	}
	return out
}

// checkJSON rejects duplicate keys and trailing values while allowing null in
// unknown telemetry fields. Required-field nulls are rejected during parsing.
func checkJSON(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	t, e := d.Token()
	if e != nil {
		return e
	}
	if t != json.Delim('{') {
		return errors.New("top-level value must be object")
	}
	if e = walkObject(d); e != nil {
		return e
	}
	if _, e = d.Token(); e != io.EOF {
		if e == nil {
			return errors.New("trailing JSON value")
		}
		return e
	}
	return nil
}
func walkObject(d *json.Decoder) error {
	seen := map[string]bool{}
	for d.More() {
		t, e := d.Token()
		if e != nil {
			return e
		}
		k, ok := t.(string)
		if !ok {
			return errors.New("object key is not string")
		}
		if seen[k] {
			return fmt.Errorf("duplicate object key %q", k)
		}
		seen[k] = true
		if e = walkValue(d); e != nil {
			return e
		}
	}
	t, e := d.Token()
	if e != nil {
		return e
	}
	if t != json.Delim('}') {
		return errors.New("unterminated object")
	}
	return nil
}
func walkValue(d *json.Decoder) error {
	t, e := d.Token()
	if e != nil {
		return e
	}
	switch t {
	case json.Delim('{'):
		return walkObject(d)
	case json.Delim('['):
		for d.More() {
			if e = walkValue(d); e != nil {
				return e
			}
		}
		end, e := d.Token()
		if e != nil {
			return e
		}
		if end != json.Delim(']') {
			return errors.New("unterminated array")
		}
	}
	return nil
}
