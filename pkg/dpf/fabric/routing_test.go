// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package fabric

import (
	"encoding/json"
	yaml "gopkg.in/yaml.v3"
	"strings"
	"testing"
)

func TestParseOperationalL3FactsPreservesFamiliesTablesAndDefaults(t *testing.T) {
	in := validInputs()
	in.OperationalAddresses = []byte(`[
			{"ifindex":2,"ifname":"eth0","addr_info":[{"family":"inet","local":"192.0.2.10","prefixlen":24},{"family":"inet6","local":"2001:db8::10","prefixlen":64}]},
			{"ifindex":3,"ifname":"swp1","addr_info":[]}
		]`)
	in.OperationalRoutesIPv4 = []byte(`[
			{"dst":"default","gateway":"192.0.2.1","dev":"eth0","table":"mgmt","protocol":"static"},
			{"dst":"198.51.100.7","table":254,"protocol":2,"type":"unicast"},
			{"dst":"default","type":"unreachable"}
		]`)
	in.OperationalRoutesIPv6 = []byte(`[{"dst":"2001:db8:1::/48","table":"main","protocol":"boot","type":"unicast"},{"dst":"default","table":"1001","type":"unreachable"}]`)
	snapshot := ParseNVUE(in)
	if len(snapshot.Issues) != 0 {
		t.Fatalf("unexpected issues: %#v", snapshot.Issues)
	}
	if len(*snapshot.Operational.Addresses) != 2 {
		t.Fatalf("address facts = %#v", snapshot.Operational.Addresses)
	}
	if got := (*snapshot.Operational.Addresses)[0]; got.Interface != "eth0" || got.Family != "ipv4" || got.Prefix != "192.0.2.10/24" {
		t.Fatalf("IPv4 interface address = %#v", got)
	}
	if got := (*snapshot.Operational.Addresses)[1]; got.Family != "ipv6" || got.Prefix != "2001:db8::10/64" {
		t.Fatalf("IPv6 interface address = %#v", got)
	}
	if len(*snapshot.Operational.IPv4Routes) != 3 || len(*snapshot.Operational.IPv6Routes) != 2 {
		t.Fatalf("route facts lost family distinction: v4=%#v v6=%#v", snapshot.Operational.IPv4Routes, snapshot.Operational.IPv6Routes)
	}
	if got := (*snapshot.Operational.IPv4Routes)[0]; got.Table != "254" || got.Prefix != "198.51.100.7/32" || got.Protocol != "2" || got.Type != "unicast" {
		t.Fatalf("IPv4 default route = %#v", got)
	}
	if got := (*snapshot.Operational.IPv4Routes)[1]; got.Prefix != "0.0.0.0/0" || got.Table != "main" || got.Type != "unreachable" {
		t.Fatalf("implicit default/main route facts = %#v", got)
	}
	if got := (*snapshot.Operational.IPv4Routes)[2]; got.Family != "ipv4" || got.Prefix != "0.0.0.0/0" || got.Table != "mgmt" || got.Protocol != "static" {
		t.Fatalf("named table default route = %#v", got)
	}
	if got := (*snapshot.Operational.IPv6Routes)[0]; got.Family != "ipv6" || got.Prefix != "::/0" || got.Table != "1001" {
		t.Fatalf("IPv6 default route = %#v", got)
	}
}

func TestParseOperationalL3UnknownDiffersFromKnownEmpty(t *testing.T) {
	unknown := ParseNVUE(NVUEInputs{})
	if unknown.Operational.Addresses != nil || unknown.Operational.IPv4Routes != nil || unknown.Operational.IPv6Routes != nil {
		t.Fatalf("missing sources became facts: %#v", unknown.Operational)
	}
	for _, input := range []string{"operationalAddresses", "operationalRoutesIPv4", "operationalRoutesIPv6"} {
		if _, ok := issueByInput(unknown.Issues, input); !ok {
			t.Errorf("missing issue for %s: %#v", input, unknown.Issues)
		}
	}
	emptyInputs := validInputs()
	emptyInputs.OperationalAddresses = []byte(`[]`)
	emptyInputs.OperationalRoutesIPv4 = []byte(`[]`)
	emptyInputs.OperationalRoutesIPv6 = []byte(`[]`)
	empty := ParseNVUE(emptyInputs)
	if len(empty.Issues) != 0 {
		t.Fatalf("known-empty outputs have issues: %#v", empty.Issues)
	}
	if empty.Operational.Addresses == nil || empty.Operational.IPv4Routes == nil || empty.Operational.IPv6Routes == nil {
		t.Fatalf("known-empty source became unknown: %#v", empty.Operational)
	}
}

func TestParseOperationalL3MalformedOrWrongFamilyNeverProducesPartialSuccess(t *testing.T) {
	tests := []struct {
		name, input, field, code, data string
	}{
		{"address missing interface", "operationalAddresses", "$[0].ifname", "missing_field", `[{"addr_info":[]}]`},
		{"address wrong prefix type", "operationalAddresses", "$[0].addr_info[0].prefixlen", "invalid_type", `[{"ifname":"eth0","addr_info":[{"family":"inet","local":"192.0.2.1","prefixlen":"24"}]}]`},
		{"address family mismatch", "operationalAddresses", "$[0].addr_info[0]", "invalid_value", `[{"ifname":"eth0","addr_info":[{"family":"inet","local":"2001:db8::1","prefixlen":64}]}]`},
		{"IPv4 route with IPv6 prefix", "operationalRoutesIPv4", "$[0].dst", "invalid_value", `[{"dst":"2001:db8::/64"}]`},
		{"route wrong table type", "operationalRoutesIPv6", "$[0].table", "invalid_type", `[{"dst":"::/0","table":true}]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := validInputs()
			switch test.input {
			case "operationalAddresses":
				in.OperationalAddresses = []byte(test.data)
			case "operationalRoutesIPv4":
				in.OperationalRoutesIPv4 = []byte(test.data)
			case "operationalRoutesIPv6":
				in.OperationalRoutesIPv6 = []byte(test.data)
			}
			snapshot := ParseNVUE(in)
			found := false
			for _, issue := range snapshot.Issues {
				if issue.Input == test.input && issue.Field == test.field && issue.Code == test.code {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected %s %s %s in %#v", test.input, test.field, test.code, snapshot.Issues)
			}
			switch test.input {
			case "operationalAddresses":
				if snapshot.Operational.Addresses != nil {
					t.Fatalf("partial address list exposed: %#v", snapshot.Operational.Addresses)
				}
			case "operationalRoutesIPv4":
				if snapshot.Operational.IPv4Routes != nil {
					t.Fatalf("partial IPv4 route list exposed: %#v", snapshot.Operational.IPv4Routes)
				}
			case "operationalRoutesIPv6":
				if snapshot.Operational.IPv6Routes != nil {
					t.Fatalf("partial IPv6 route list exposed: %#v", snapshot.Operational.IPv6Routes)
				}
			}
		})
	}
}

func TestParseOperationalL3EntryLimitAndSnapshotStability(t *testing.T) {
	base := validInputs()
	row := `{"ifindex":2,"ifname":"eth0","addr_info":[]}`
	tooMany := `[` + strings.TrimSuffix(strings.Repeat(row+",", maxL3Entries+1), ",") + `]`
	base.OperationalAddresses = []byte(tooMany)
	snapshot := ParseNVUE(base)
	if snapshot.Operational.Addresses != nil || !hasIssue(snapshot.Issues, "operationalAddresses", "$", "entry_limit") {
		t.Fatalf("oversized input did not fail closed: addresses=%#v issues=%#v", snapshot.Operational.Addresses, snapshot.Issues)
	}

	in := validInputs()
	in.OperationalAddresses = []byte(`[{"ifindex":2,"ifname":"eth1","addr_info":[{"family":"inet","local":"192.0.2.2","prefixlen":24}]},{"ifindex":1,"ifname":"eth0","addr_info":[]}]`)
	in.OperationalRoutesIPv4 = []byte(`[{"dst":"198.51.100.0/24","table":"z"},{"dst":"192.0.2.0/24","table":"a"}]`)
	in.OperationalRoutesIPv6 = []byte(`[]`)
	first, err := json.Marshal(ParseNVUE(in))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(ParseNVUE(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("L3 facts are nondeterministic:\n%s\n%s", first, second)
	}
}

func issueByInput(issues []Issue, input string) (Issue, bool) {
	for _, issue := range issues {
		if issue.Input == input {
			return issue, true
		}
	}
	return Issue{}, false
}

func TestOperationalL3KnownEmptySurvivesJSONAndYAML(t *testing.T) {
	for _, codec := range []struct {
		name      string
		marshal   func(any) ([]byte, error)
		unmarshal func([]byte, any) error
	}{
		{"json", json.Marshal, json.Unmarshal}, {"yaml", yaml.Marshal, yaml.Unmarshal},
	} {
		for _, known := range []bool{false, true} {
			facts := OperationalFacts{}
			if known {
				facts.Addresses = &[]InterfaceAddress{}
				facts.IPv4Routes = &[]Route{}
				facts.IPv6Routes = &[]Route{}
			}
			data, err := codec.marshal(facts)
			if err != nil {
				t.Fatal(err)
			}
			var restored OperationalFacts
			if err := codec.unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			if (restored.Addresses != nil) != known || (restored.IPv4Routes != nil) != known || (restored.IPv6Routes != nil) != known {
				t.Fatalf("%s lost known-empty=%v: %s", codec.name, known, data)
			}
		}
	}
}

func TestMissingRouteDestinationDoesNotBecomeDefault(t *testing.T) {
	input := validInputs()
	input.OperationalRoutesIPv4 = []byte(`[{"dst":"192.0.2.0/24","dev":"eth0"},{"dev":"swp1","table":"main"}]`)
	snapshot := ParseNVUE(input)
	if snapshot.Operational.IPv4Routes != nil || !hasIssue(snapshot.Issues, "operationalRoutesIPv4", "$[1].dst", "missing_field") {
		t.Fatalf("incomplete route source became a passing default: %#v", snapshot)
	}
}
