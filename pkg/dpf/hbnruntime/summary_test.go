// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package hbnruntime

import (
	"fmt"
	"strings"
	"testing"
)

const idleFixture = `{"routerId":"10.0.124.1","as":65101,"vrfId":0,"vrfName":"default","tableVersion":0,"ribCount":1,"ribMemory":128,"peerCount":2,"peerMemory":40992,"peers":{"p0_if":{"remoteAs":65000,"localAs":65101,"version":4,"msgRcvd":0,"msgSent":0,"tableVersion":0,"outq":0,"inq":0,"peerUptime":"never","peerUptimeMsec":0,"pfxRcd":0,"pfxSnt":0,"state":"Idle","peerState":"OK","connectionsEstablished":0,"connectionsDropped":0,"idType":"interface"},"p1_if":{"remoteAs":65000,"localAs":65101,"version":4,"msgRcvd":0,"msgSent":0,"tableVersion":0,"outq":0,"inq":0,"peerUptime":"never","peerUptimeMsec":0,"pfxRcd":0,"pfxSnt":0,"state":"Idle","peerState":"OK","connectionsEstablished":0,"connectionsDropped":0,"idType":"interface"}},"failedPeers":2,"displayedPeers":2,"totalPeers":2,"dynamicPeers":0,"bestPath":{"multiPathRelax":"false"}}`

func validFixture(state string) string {
	return fmt.Sprintf(`{"as":65101,"vrfName":"default","peerCount":2,"displayedPeers":2,"totalPeers":2,"failedPeers":%d,"peers":{"p0_if":{"remoteAs":65000,"localAs":65101,"state":%q,"idType":"interface","newTelemetry":null},"p1_if":{"remoteAs":65000,"localAs":65101,"state":%q,"idType":"interface"}}}`, 0, state, state)
}
func TestActualRetainedIdleIsBlocked(t *testing.T) {
	s, e := Parse([]byte(idleFixture))
	if e != nil {
		t.Fatal(e)
	}
	got := Validate(s, Expected{65101, 65000})
	states := 0
	failedCounter := false
	for _, m := range got {
		states += boolInt(m.Reason == "PEER_NOT_ESTABLISHED")
		failedCounter = failedCounter || m.Reason == "FAILED_PEER_COUNTER_NONZERO"
	}
	if states != 2 || !failedCounter {
		t.Fatalf("got %#v", got)
	}
}
func TestValidSyntheticSummary(t *testing.T) {
	s, e := Parse([]byte(validFixture("Established")))
	if e != nil {
		t.Fatal(e)
	}
	if got := Validate(s, Expected{65101, 65000}); len(got) != 0 {
		t.Fatalf("got %#v", got)
	}
}

func TestNonEstablishedPeerWithZeroFailedCounterStillBlocked(t *testing.T) {
	s, err := Parse([]byte(validFixture("Idle")))
	if err != nil {
		t.Fatal(err)
	}
	got := Validate(s, Expected{65101, 65000})
	states := 0
	for _, m := range got {
		if m.Reason == "PEER_NOT_ESTABLISHED" {
			states++
		}
	}
	if states != 2 {
		t.Fatalf("expected both Idle peers blocked, got %#v", got)
	}
}
func TestRejectMalformedAndAmbiguous(t *testing.T) {
	base := validFixture("Established")
	cases := []string{`null`, `{} {}`, strings.Replace(base, `"as":65101`, `"as":"65101"`, 1), strings.Replace(base, `"as":65101`, `"as":null`, 1), strings.Replace(base, `"as":65101`, `"as":1.2`, 1), strings.Replace(base, `"as":65101`, `"as":4294967296`, 1), strings.Replace(base, `"as":65101`, `"as":0`, 1), strings.Replace(base, `"as":65101`, `"as":65101,"as":65101`, 1), strings.Replace(base, `"state":"Established"`, `"state":null`, 1), strings.Replace(base, `"state":"Established"`, `"state":2`, 1), strings.Replace(base, `"failedPeers":0`, `"failedPeers":1`, 1)}
	for i, in := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			s, e := Parse([]byte(in))
			if e != nil {
				return
			}
			if i == 10 && len(Validate(s, Expected{65101, 65000})) > 0 {
				return
			}
			t.Fatalf("accepted invalid JSON/schema %s", in)
		})
	}
}
func TestCounterMismatch(t *testing.T) {
	in := strings.Replace(validFixture("Established"), `"failedPeers":0`, `"failedPeers":1`, 1)
	s, e := Parse([]byte(in))
	if e != nil {
		t.Fatal(e)
	}
	got := Validate(s, Expected{65101, 65000})
	if len(got) == 0 || got[0].Reason != "FAILED_PEER_COUNTER_NONZERO" {
		t.Fatalf("got %#v", got)
	}
}
func TestInvalidExpectedASNs(t *testing.T) {
	s, e := Parse([]byte(validFixture("Established")))
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []Expected{{0, 65000}, {65101, 0}, {65101, 65101}, {65535, 65000}} {
		got := Validate(s, want)
		if len(got) != 1 || got[0].Reason != "INVALID_EXPECTED_ASN" {
			t.Errorf("%#v => %#v", want, got)
		}
	}
}
func TestOversize(t *testing.T) {
	if _, e := Parse([]byte(strings.Repeat(" ", MaxSummaryBytes+1))); e == nil {
		t.Fatal("accepted oversized input")
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
