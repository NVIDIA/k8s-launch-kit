// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRouteNextHopsRetainEveryExplicitInterfaceAndGateway(t *testing.T) {
	for _, test := range []struct {
		name, raw string
		want      []RouteNextHop
	}{
		{"IPv4", `{"dev":"swp1","gateway":"192.0.2.1"}`, []RouteNextHop{{Interface: "swp1", Gateway: "192.0.2.1"}}},
		{"IPv6 via", `{"dev":"swp1","via":{"family":"inet6","host":"fe80::1"}}`, []RouteNextHop{{Interface: "swp1", Gateway: "fe80::1"}}},
		{"complete sorted ECMP", `{"nexthops":[{"dev":"swp2","gateway":"fe80::2","flags":["onlink"]},{"dev":"swp1","gateway":"fe80::1"}]}`, []RouteNextHop{{Interface: "swp1", Gateway: "fe80::1"}, {Interface: "swp2", Gateway: "fe80::2"}}},
		{"old data", `{}`, nil},
		{"gateway without interface", `{"gateway":"192.0.2.1"}`, nil},
		{"interface without gateway", `{"dev":"swp1"}`, nil},
		{"unknown gateway", `{"dev":"swp1","gateway":"0.0.0.0"}`, nil},
		{"mixed missing hop", `{"nexthops":[{"dev":"swp1","gateway":"fe80::1"},{"gateway":"fe80::2"}]}`, nil},
		{"duplicate hop", `{"nexthops":[{"dev":"swp1","gateway":"fe80::1"},{"dev":"swp1","gateway":"fe80::1"}]}`, nil},
		{"dead hop", `{"nexthops":[{"dev":"swp1","gateway":"fe80::1","flags":["dead"]}]}`, nil},
		{"dead route", `{"flags":["dead"],"nexthops":[{"dev":"swp1","gateway":"fe80::1"}]}`, nil},
		{"ambiguous via", `{"dev":"swp1","via":{"family":"inet6","host":"fe80::1"},"gateway":"192.0.2.1"}`, nil},
		{"wrong via family", `{"dev":"swp1","via":{"family":"inet","host":"fe80::1"}}`, nil},
		{"bad interface", `{"dev":"swp1 other","gateway":"fe80::1"}`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var row map[string]json.RawMessage
			if err := json.Unmarshal([]byte(test.raw), &row); err != nil {
				t.Fatal(err)
			}
			if got := routeNextHops(row); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("next hops=%+v want %+v", got, test.want)
			}
		})
	}
	input := validInputs()
	input.OperationalRoutesIPv4 = []byte(`[{"dst":"198.51.100.0/29","protocol":"bgp","nexthops":[{"dev":"swp1","via":{"family":"inet6","host":"fe80::1"}}]}]`)
	got := ParseNVUE(input)
	if got.Operational.IPv4Routes == nil || len(*got.Operational.IPv4Routes) != 1 || len((*got.Operational.IPv4Routes)[0].NextHops) != 1 {
		t.Fatalf("actual route parser discarded owned-source facts: %+v", got)
	}
}

func TestLegacyRouteCodecDoesNotAddAbsentFields(t *testing.T) {
	// Exact legacy encoding is part of sealed source/fingerprint compatibility.
	const old = `{"family":"ipv4","prefix":"198.51.100.0/29","table":"main","protocol":"bgp","type":"unicast"}`
	var route Route
	if err := json.Unmarshal([]byte(old), &route); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(route)
	if err != nil || string(encoded) != old || route.NextHops != nil {
		t.Fatalf("legacy route changed: %s (%v)", encoded, err)
	}
}
