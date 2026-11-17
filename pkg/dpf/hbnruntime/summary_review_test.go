// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package hbnruntime

import (
	"encoding/json"
	"testing"
)

func TestSummarySemanticMismatchesBlockReadiness(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		reason string
	}{
		{"local ASN", func(v map[string]any) { v["as"] = 65102 }, "LOCAL_ASN_MISMATCH"},
		{"local VRF", func(v map[string]any) { v["vrfName"] = "fabric" }, "VRF_MISMATCH"},
		{"missing peer", func(v map[string]any) { delete(v["peers"].(map[string]any), "p1_if") }, "PEER_MISSING"},
		{"extra peer", func(v map[string]any) { v["peers"].(map[string]any)["p2_if"] = summaryPeer(v) }, "PEER_COUNT_MISMATCH"},
		{"peer local ASN", func(v map[string]any) { summaryPeer(v)["localAs"] = 65102 }, "PEER_LOCAL_ASN_MISMATCH"},
		{"peer remote ASN", func(v map[string]any) { summaryPeer(v)["remoteAs"] = 65001 }, "PEER_REMOTE_ASN_MISMATCH"},
		{"numbered peer", func(v map[string]any) { summaryPeer(v)["idType"] = "ipv4" }, "PEER_ID_TYPE_MISMATCH"},
		{"peer count", func(v map[string]any) { v["peerCount"] = 1 }, "PEER_COUNTER_MISMATCH"},
		{"displayed count", func(v map[string]any) { v["displayedPeers"] = 1 }, "PEER_COUNTER_MISMATCH"},
		{"total count", func(v map[string]any) { v["totalPeers"] = 1 }, "PEER_COUNTER_MISMATCH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := summaryJSON(t)
			tc.mutate(value)
			summary, err := Parse(marshalSummary(t, value))
			if err != nil {
				t.Fatalf("well-formed mismatch became a parse failure: %v", err)
			}
			for _, mismatch := range Validate(summary, Expected{LocalASN: 65101, FabricASN: 65000}) {
				if mismatch.Reason == tc.reason {
					return
				}
			}
			t.Fatalf("missing readiness blocker %s", tc.reason)
		})
	}
}

func TestRequiredSummaryFieldsCannotBecomeSuccessfulZeroValues(t *testing.T) {
	for _, field := range []string{"as", "vrfName", "peers", "peerCount", "displayedPeers", "totalPeers", "failedPeers"} {
		for _, absent := range []bool{false, true} {
			t.Run(field+map[bool]string{false: "/null", true: "/missing"}[absent], func(t *testing.T) {
				value := summaryJSON(t)
				if absent {
					delete(value, field)
				} else {
					value[field] = nil
				}
				if _, err := Parse(marshalSummary(t, value)); err == nil {
					t.Fatal("accepted incomplete required summary")
				}
			})
		}
	}
	for _, field := range []string{"localAs", "remoteAs", "state", "idType"} {
		for _, replacement := range []struct {
			name   string
			value  any
			remove bool
		}{{name: "missing", remove: true}, {name: "null", value: nil}, {name: "wrong type", value: false}} {
			t.Run(field+"/"+replacement.name, func(t *testing.T) {
				value := summaryJSON(t)
				peer := summaryPeer(value)
				if replacement.remove {
					delete(peer, field)
				} else {
					peer[field] = replacement.value
				}
				if _, err := Parse(marshalSummary(t, value)); err == nil {
					t.Fatal("accepted incomplete required peer")
				}
			})
		}
	}
}

func summaryJSON(t *testing.T) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(validFixture("Established")), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func summaryPeer(value map[string]any) map[string]any {
	return value["peers"].(map[string]any)["p0_if"].(map[string]any)
}

func marshalSummary(t *testing.T, value map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
