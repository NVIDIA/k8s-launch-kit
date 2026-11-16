// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Synthetic identities retain the actual kernel wire shape: a route references
// a flat ECMP object whose two IPv6 gateways belong to physical DPU uplinks.
const kernelNextHopFixture = `[
 {"id":100,"group":[{"id":101},{"id":102}],"protocol":"zebra","flags":[]},
 {"id":101,"gateway":"fe80::1","dev":"swp1","scope":"link","protocol":"zebra","flags":[]},
 {"id":102,"gateway":"fe80::2","dev":"swp2","scope":"link","protocol":"zebra","flags":[]},
 {"id":200,"dev":"eth0","scope":"host","protocol":"zebra","flags":[]}
]`

func TestKernelNextHopReferencesAreCompleteAndStable(t *testing.T) {
	const route = `[{"dst":"198.51.100.0/29","nhid":100,"table":"tenant","protocol":"bgp","metric":20,"flags":[]}]`
	want := []RouteNextHop{{Interface: "swp1", Gateway: "fe80::1"}, {Interface: "swp2", Gateway: "fe80::2"}}
	for _, test := range []struct {
		name, before, after, route string
		want                       []RouteNextHop
	}{
		{"flat ECMP", kernelNextHopFixture, kernelNextHopFixture, route, want},
		{"direct leaf", kernelNextHopFixture, kernelNextHopFixture, strings.Replace(route, `"nhid":100`, `"nhid":101`, 1), want[:1]},
		{"default and unequal weights", strings.Replace(kernelNextHopFixture, `{"id":102}`, `{"id":102,"weight":256}`, 1), strings.Replace(kernelNextHopFixture, `{"id":102}`, `{"id":102,"weight":256}`, 1), route, want},
		{"unrelated churn", kernelNextHopFixture, strings.Replace(kernelNextHopFixture, `"dev":"eth0"`, `"dev":"eth1"`, 1), route, want},
		{"whitespace and key order", kernelNextHopFixture, strings.Replace(kernelNextHopFixture, `"gateway":"fe80::1","dev":"swp1"`, `"dev": "swp1", "gateway": "fe80::1"`, 1), route, want},
		{"gateway changed", kernelNextHopFixture, strings.Replace(kernelNextHopFixture, "fe80::2", "fe80::3", 1), route, nil},
		{"member changed", kernelNextHopFixture, strings.Replace(kernelNextHopFixture, `{"id":102}`, `{"id":200}`, 1), route, nil},
		{"weight changed", kernelNextHopFixture, strings.Replace(kernelNextHopFixture, `{"id":102}`, `{"id":102,"weight":2}`, 1), route, nil},
		{"flags changed", kernelNextHopFixture, strings.Replace(kernelNextHopFixture, `"scope":"link","protocol":"zebra","flags":[]`, `"scope":"link","protocol":"zebra","flags":["onlink"]`, 1), route, nil},
		{"missing before", `[]`, kernelNextHopFixture, route, nil},
		{"missing after", kernelNextHopFixture, `[]`, route, nil},
		{"dangling child", strings.Replace(kernelNextHopFixture, `{"id":102}`, `{"id":999}`, 1), strings.Replace(kernelNextHopFixture, `{"id":102}`, `{"id":999}`, 1), route, nil},
		{"duplicate member", strings.Replace(kernelNextHopFixture, `{"id":102}`, `{"id":101}`, 1), strings.Replace(kernelNextHopFixture, `{"id":102}`, `{"id":101}`, 1), route, nil},
		{"self cycle", `[{"id":100,"group":[{"id":100}]}]`, `[{"id":100,"group":[{"id":100}]}]`, route, nil},
		{"nested group cycle", `[{"id":100,"group":[{"id":101}]},{"id":101,"group":[{"id":100}]}]`, `[{"id":100,"group":[{"id":101}]},{"id":101,"group":[{"id":100}]}]`, route, nil},
		{"inline conflict", kernelNextHopFixture, kernelNextHopFixture, strings.Replace(route, `"nhid":100`, `"nhid":100,"dev":"swp1","gateway":"fe80::1"`, 1), nil},
		{"route encapsulation", kernelNextHopFixture, kernelNextHopFixture, strings.Replace(route, `"nhid":100`, `"nhid":100,"encap":{"type":"mpls"}`, 1), nil},
		{"invalid ID", kernelNextHopFixture, kernelNextHopFixture, strings.Replace(route, `"nhid":100`, `"nhid":0`, 1), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := validInputs()
			in.NextHopCollectionRequested = true
			in.OperationalNextHopsBefore, in.OperationalNextHopsAfter = []byte(test.before), []byte(test.after)
			in.OperationalRoutesIPv4 = []byte(test.route)
			before, _ := json.Marshal(in)
			got := ParseNVUE(in)
			if got.Operational.IPv4Routes == nil || len(*got.Operational.IPv4Routes) != 1 {
				t.Fatalf("lost route prefix: %+v", got)
			}
			if hops := (*got.Operational.IPv4Routes)[0].NextHops; !reflect.DeepEqual(hops, test.want) {
				t.Fatalf("hops=%+v want %+v", hops, test.want)
			}
			after, _ := json.Marshal(in)
			if !bytes.Equal(before, after) {
				t.Fatal("mutated raw collection inputs")
			}
		})
	}
}

func TestKernelNextHopUnsupportedMembersNeverDisappear(t *testing.T) {
	for _, replacement := range []string{
		`{"id":102,"group":[{"id":101}]}`,
		`{"id":102,"blackhole":true}`,
		`{"id":102,"dev":"swp2"}`,
		`{"id":102,"dev":"swp2","gateway":"fe80::2","flags":["dead"]}`,
		`{"id":102,"dev":"swp2","gateway":"fe80::2","flags":["offload"]}`,
		`{"id":102,"dev":"swp2","gateway":"fe80::2","flags":null}`,
		`{"id":102,"dev":"swp2","gateway":"fe80::2","encap":{"type":"mpls"}}`,
		`{"id":102,"dev":"swp1","gateway":"fe80::1"}`,
	} {
		t.Run(replacement, func(t *testing.T) {
			var rows []jsonObject
			if err := json.Unmarshal([]byte(kernelNextHopFixture), &rows); err != nil {
				t.Fatal(err)
			}
			rows[2] = nil
			if err := json.Unmarshal([]byte(replacement), &rows[2]); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(rows)
			in := validInputs()
			in.OperationalNextHopsBefore, in.OperationalNextHopsAfter = raw, raw
			in.OperationalRoutesIPv4 = []byte(`[{"dst":"198.51.100.0/29","nhid":100}]`)
			got := ParseNVUE(in)
			if len((*got.Operational.IPv4Routes)[0].NextHops) != 0 {
				t.Fatalf("silently discarded unsupported member: %+v", got.Operational.IPv4Routes)
			}
		})
	}
	for _, member := range []string{`{"id":102,"weight":0}`, `{"id":102,"weight":257}`, `{"id":102,"weight":1.5}`, `{"id":102,"weight":null}`, `{"id":102,"weight":-1}`, `{"id":102,"unknown":true}`} {
		in := validInputs()
		raw := []byte(strings.Replace(kernelNextHopFixture, `{"id":102}`, member, 1))
		in.OperationalNextHopsBefore, in.OperationalNextHopsAfter = raw, raw
		in.OperationalRoutesIPv4 = []byte(`[{"dst":"198.51.100.0/29","nhid":100}]`)
		if got := ParseNVUE(in); len((*got.Operational.IPv4Routes)[0].NextHops) != 0 {
			t.Fatalf("unsupported group member accepted: %s", member)
		}
	}
}

func TestKernelNextHopCapabilityRequiredEvenWithoutRoutes(t *testing.T) {
	for _, raw := range []string{"", `null`, `{}`, `[`, `[{"id":0}]`, `[{"id":1},{"id":1}]`, `[{"id":4294967296}]`} {
		in := validInputs()
		in.NextHopCollectionRequested = true
		in.OperationalNextHopsBefore, in.OperationalNextHopsAfter = []byte(raw), []byte(raw)
		got := ParseNVUE(in)
		for _, input := range []string{"operationalNextHopsBefore", "operationalNextHopsAfter"} {
			found := false
			for _, issue := range got.Issues {
				found = found || issue.Input == input
			}
			if !found {
				t.Fatalf("missing capability failure %s for %s: %+v", input, raw, got.Issues)
			}
		}
	}
	in := validInputs()
	in.NextHopCollectionRequested = true
	in.OperationalNextHopsBefore, in.OperationalNextHopsAfter = []byte(`[]`), []byte(`[]`)
	if got := ParseNVUE(in); len(got.Issues) != 0 {
		t.Fatalf("measured empty capability blocked: %+v", got.Issues)
	}
}

func TestKernelNextHopReaderBounds(t *testing.T) {
	for _, count := range []int{maxRouteNextHops, maxRouteNextHops + 1} {
		members := make([]map[string]int, 0, count)
		rows := make([]map[string]any, 0, count+1)
		for i := 1; i <= count; i++ {
			members = append(members, map[string]int{"id": i + 1})
			rows = append(rows, map[string]any{"id": i + 1, "dev": fmt.Sprintf("swp%d", i), "gateway": "fe80::1"})
		}
		rows = append(rows, map[string]any{"id": 1, "group": members})
		raw, err := json.Marshal(rows)
		if err != nil {
			t.Fatal(err)
		}
		in := validInputs()
		in.OperationalNextHopsBefore, in.OperationalNextHopsAfter = raw, raw
		in.OperationalRoutesIPv4 = []byte(`[{"dst":"198.51.100.0/29","nhid":1}]`)
		got := ParseNVUE(in)
		if (len((*got.Operational.IPv4Routes)[0].NextHops) == count) != (count <= maxRouteNextHops) {
			t.Fatalf("group size %d was not bounded", count)
		}
	}
	var issues []Issue
	rows := make([]map[string]int, maxL3Entries+1)
	for i := range rows {
		rows[i] = map[string]int{"id": i + 1}
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if parseNextHopTable(raw, "operationalNextHopsBefore", &issues) != nil || !hasIssue(issues, "operationalNextHopsBefore", "$", "entry_limit") {
		t.Fatal("nexthop table entry budget not enforced")
	}
}
