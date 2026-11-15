// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package hbnruntime

import (
	"fmt"
	"strings"
	"testing"
)

// This exact FRR 10.0.3 output is captured in planning evidence
// hbn-frr-fib-qualification/r2-learned-route.json (SHA256
// 4bef3aaea2133aad4905d7f0178af8af22f27ea19ce30c304576b488fbe14b0e).
const capturedRoute = `{"198.18.114.1/32":[{"protocol":"bgp","selected":true,"destSelected":true,"distance":20,"metric":0,"installed":true,"nexthopGroupId":4,"uptime":"00:00:48","routeUptimeEstablishedEpoch":1794678027,"prefix":"198.18.114.1/32","prefixLen":32,"vrfId":0,"vrfName":"default","table":254,"internalStatus":16,"internalFlags":8,"internalNextHopNum":1,"internalNextHopActiveNum":1,"installedNexthopGroupId":4,"nexthops":[{"flags":3,"fib":true,"ip":"172.30.114.19","afi":"ipv4","interfaceIndex":11,"interfaceName":"eth0","active":true,"weight":1}]}]}`

func TestCapturedFRR10RouteInstalledInFIB(t *testing.T) {
	doc, err := ParseFIBRoute([]byte(capturedRoute))
	if err != nil {
		t.Fatal(err)
	}
	if got := ValidateFIBRoute(doc, FIBExpectation{Prefix: "198.18.114.1/32", EgressInterfaces: []string{"eth0"}}); len(got) != 0 {
		t.Fatalf("captured route rejected: %#v", got)
	}
}

// syntheticRoute is a unit-only example with multiple expected ECMP ports.
func syntheticRoute() string {
	return `{"198.18.114.0/24":[{"protocol":"bgp","selected":true,"destSelected":true,"installed":true,"prefix":"198.18.114.0/24","prefixLen":24,"vrfId":0,"vrfName":"default","table":254,"nexthops":[{"ip":"fe80::2","interfaceName":"p0_if","active":true,"fib":true},{"ip":"fe80::3","interfaceName":"p1_if","active":true,"fib":true}],"extraTelemetry":null}]}`
}

func TestSyntheticECMPOnAllowedPorts(t *testing.T) {
	doc, err := ParseFIBRoute([]byte(syntheticRoute()))
	if err != nil {
		t.Fatal(err)
	}
	if got := ValidateFIBRoute(doc, FIBExpectation{Prefix: "198.18.114.0/24", EgressInterfaces: []string{"p0_if", "p1_if"}}); len(got) != 0 {
		t.Fatalf("synthetic ECMP route rejected: %#v", got)
	}
}

func TestIPv6LinkLocalNexthopIsAccepted(t *testing.T) {
	input := strings.Replace(syntheticRoute(), `"p1_if","active":true,"fib":true}`, `"p1_if","active":false,"fib":false}`, 1)
	doc, err := ParseFIBRoute([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if got := ValidateFIBRoute(doc, FIBExpectation{Prefix: "198.18.114.0/24", EgressInterfaces: []string{"p0_if"}}); len(got) != 0 {
		t.Fatalf("IPv6 link-local nexthop rejected: %#v", got)
	}
}

func TestParseRejectsMalformedJSONAndRequiredFields(t *testing.T) {
	base := capturedRoute
	cases := map[string]string{
		"duplicate key":            strings.Replace(base, `"protocol":"bgp"`, `"protocol":"bgp","protocol":"bgp"`, 1),
		"trailing JSON":            base + `{}`,
		"null selected":            strings.Replace(base, `"selected":true`, `"selected":null`, 1),
		"quoted prefix length":     strings.Replace(base, `"prefixLen":32`, `"prefixLen":"32"`, 1),
		"null table":               strings.Replace(base, `"table":254`, `"table":null`, 1),
		"quoted boolean":           strings.Replace(base, `"installed":true`, `"installed":"true"`, 1),
		"null next hops":           strings.Replace(base, `"nexthops":[`, `"nexthops":null,"unused":[`, 1),
		"missing prefix":           strings.Replace(base, `,"prefix":"198.18.114.1/32"`, ``, 1),
		"null VRF":                 strings.Replace(base, `"vrfName":"default"`, `"vrfName":null`, 1),
		"null next hop flag":       strings.Replace(base, `"fib":true`, `"fib":null`, 1),
		"invalid next hop address": strings.Replace(base, `"ip":"172.30.114.19"`, `"ip":"not-an-ip"`, 1),
		"null next hop address":    strings.Replace(base, `"ip":"172.30.114.19"`, `"ip":null`, 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseFIBRoute([]byte(input)); err == nil {
				t.Fatal("accepted malformed route JSON")
			}
		})
	}
}

func TestParseRejectsOversizeAndInvalidRoot(t *testing.T) {
	if _, err := ParseFIBRoute([]byte(strings.Repeat(" ", MaxSummaryBytes+1))); err == nil {
		t.Fatal("accepted oversized route JSON")
	}
	for _, input := range []string{`null`, `{}`, `{"198.18.114.1/32":null}`, `{"198.18.114.1/32":{}}`, `{"0.0.0.0/0":[] , "198.18.114.1/32":[]}`} {
		if _, err := ParseFIBRoute([]byte(input)); err == nil {
			t.Errorf("accepted invalid root %s", input)
		}
	}
}

func TestRouteMismatchCases(t *testing.T) {
	valid := syntheticRoute()
	cases := []struct {
		name, input, prefix string
		ports               []string
		reason              string
	}{
		{"wrong key", strings.Replace(valid, `"198.18.114.0/24"`, `"198.18.114.1/32"`, 1), "198.18.114.0/24", []string{"p0_if", "p1_if"}, "PREFIX_KEY_MISMATCH"},
		{"covering route", strings.Replace(strings.Replace(valid, `"198.18.114.0/24"`, `"0.0.0.0/0"`, 1), `"prefix":"198.18.114.0/24","prefixLen":24`, `"prefix":"0.0.0.0/0","prefixLen":0`, 1), "198.18.114.0/24", []string{"p0_if"}, "ROUTE_PREFIX_MISMATCH"},
		{"no routes", `{"198.18.114.0/24":[]}`, "198.18.114.0/24", []string{"p0_if"}, "ROUTE_NOT_SELECTED"},
		{"not selected", strings.Replace(valid, `"selected":true`, `"selected":false`, 1), "198.18.114.0/24", []string{"p0_if"}, "ROUTE_NOT_SELECTED"},
		{"missing selection flag", strings.Replace(valid, `"selected":true,`, ``, 1), "198.18.114.0/24", []string{"p0_if"}, "ROUTE_NOT_SELECTED"},
		{"missing destination selection flag", strings.Replace(valid, `"destSelected":true,`, ``, 1), "198.18.114.0/24", []string{"p0_if"}, "ROUTE_NOT_DEST_SELECTED"},
		{"missing installed flag", strings.Replace(valid, `"installed":true,`, ``, 1), "198.18.114.0/24", []string{"p0_if"}, "ROUTE_NOT_INSTALLED"},
		{"wrong prefix length", strings.Replace(valid, `"prefixLen":24`, `"prefixLen":25`, 1), "198.18.114.0/24", []string{"p0_if"}, "ROUTE_PREFIX_MISMATCH"},
		{"foreign protocol", strings.Replace(valid, `"protocol":"bgp"`, `"protocol":"connected"`, 1), "198.18.114.0/24", []string{"p0_if"}, "ROUTE_PROTOCOL_MISMATCH"},
		{"wrong VRF", strings.Replace(valid, `"vrfName":"default"`, `"vrfName":"blue"`, 1), "198.18.114.0/24", []string{"p0_if"}, "ROUTE_TABLE_MISMATCH"},
		{"wrong VRF id", strings.Replace(valid, `"vrfId":0`, `"vrfId":1`, 1), "198.18.114.0/24", []string{"p0_if"}, "ROUTE_TABLE_MISMATCH"},
		{"wrong table", strings.Replace(valid, `"table":254`, `"table":100`, 1), "198.18.114.0/24", []string{"p0_if"}, "ROUTE_TABLE_MISMATCH"},
		{"not dest selected", strings.Replace(valid, `"destSelected":true`, `"destSelected":false`, 1), "198.18.114.0/24", []string{"p0_if"}, "ROUTE_NOT_DEST_SELECTED"},
		{"not installed", strings.Replace(valid, `"installed":true`, `"installed":false`, 1), "198.18.114.0/24", []string{"p0_if"}, "ROUTE_NOT_INSTALLED"},
		{"not fib", strings.Replace(valid, `"fib":true`, `"fib":false`, 1), "198.18.114.0/24", []string{"p0_if"}, "FIB_NEXTHOP_MISSING"},
		{"inactive", strings.Replace(valid, `"active":true`, `"active":false`, 1), "198.18.114.0/24", []string{"p0_if"}, "FIB_NEXTHOP_MISSING"},
		{"wrong egress", valid, "198.18.114.0/24", []string{"eth0"}, "NEXTHOP_INTERFACE_MISMATCH"},
		{"fib inactive contradiction", strings.Replace(valid, `"active":true,"fib":true`, `"active":false,"fib":true`, 1), "198.18.114.0/24", []string{"p0_if"}, "NEXTHOP_FLAGS_CONTRADICTORY"},
		{"uninstalled fib contradiction", strings.Replace(valid, `"installed":true`, `"installed":false`, 1), "198.18.114.0/24", []string{"p0_if"}, "NEXTHOP_FLAGS_CONTRADICTORY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := ParseFIBRoute([]byte(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			if reason := routeReason(ValidateFIBRoute(doc, FIBExpectation{Prefix: tc.prefix, EgressInterfaces: tc.ports}), tc.reason); !reason {
				t.Fatalf("missing %s in %#v", tc.reason, ValidateFIBRoute(doc, FIBExpectation{Prefix: tc.prefix, EgressInterfaces: tc.ports}))
			}
		})
	}
}

func TestAmbiguousSelectedRoutesRejected(t *testing.T) {
	valid := syntheticRoute()
	array := strings.TrimSuffix(strings.TrimPrefix(valid, `{"198.18.114.0/24":[`), `]}`)
	input := fmt.Sprintf(`{"198.18.114.0/24":[%s,%s]}`, array, array)
	doc, err := ParseFIBRoute([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if !routeReason(ValidateFIBRoute(doc, FIBExpectation{Prefix: "198.18.114.0/24", EgressInterfaces: []string{"p0_if"}}), "AMBIGUOUS_SELECTED_ROUTES") {
		t.Fatal("multiple selected routes not rejected")
	}
}

func TestExpectedRouteValidation(t *testing.T) {
	doc, err := ParseFIBRoute([]byte(syntheticRoute()))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []FIBExpectation{{Prefix: "0.0.0.0/0", EgressInterfaces: []string{"p0_if"}}, {Prefix: "198.18.114.0/24"}, {Prefix: "198.18.114.1/24", EgressInterfaces: []string{"p0_if"}}, {Prefix: "198.18.114.0/24", EgressInterfaces: []string{"p0_if", "p0_if"}}} {
		if !routeReason(ValidateFIBRoute(doc, expected), "INVALID_EXPECTED_ROUTE") {
			t.Errorf("invalid expectation accepted: %#v", expected)
		}
	}
}

func routeReason(mismatches []FIBMismatch, reason string) bool {
	for _, mismatch := range mismatches {
		if mismatch.Reason == reason {
			return true
		}
	}
	return false
}
