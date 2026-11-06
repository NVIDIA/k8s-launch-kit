// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package connectivity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

func TestNextHopFromRoute(t *testing.T) {
	cases := []struct {
		name, route, dst, address string
		gateway, invalid          bool
	}{
		{"IPv4 gateway", "198.51.100.20 from 192.0.2.10 via 192.0.2.1 dev net1 table 100", "198.51.100.20", "192.0.2.1", true, false},
		{"IPv4 onlink", "192.0.2.20 from 192.0.2.10 dev net1 table 100", "192.0.2.20", "192.0.2.20", false, false},
		{"IPv6 gateway", "2001:db8:2::20 from 2001:db8:1::10 via fe80::1 dev net1", "2001:db8:2::20", "fe80::1", true, false},
		{"IPv6 onlink", "2001:db8::20 dev net1", "2001:db8::20", "2001:db8::20", false, false},
		{"explicit family", "2001:db8::20 via inet6 fe80::1 dev net1", "2001:db8::20", "fe80::1", true, false},
		{"missing gateway", "198.51.100.20 dev net1 via", "198.51.100.20", "198.51.100.20", true, true},
		{"bad gateway", "198.51.100.20 via bad dev net1", "198.51.100.20", "198.51.100.20", true, true},
		{"wrong family", "198.51.100.20 via fe80::1 dev net1", "198.51.100.20", "198.51.100.20", true, true},
		{"bad destination", "bad dev net1", "bad", "bad", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := nextHopFromRoute(RouteCheck{Output: tc.route, Dev: "net1", OK: true}, tc.dst)
			assert.Equal(t, tc.address, d.Address)
			assert.Equal(t, tc.gateway, d.Gateway)
			assert.Equal(t, tc.invalid, d.Error != "")
			assert.Equal(t, "net1", d.Interface)
		})
	}
}

func TestNeighborClassification(t *testing.T) {
	cases := []struct{ name, output, status string }{
		{"failed", `[{"dst":"192.0.2.1","dev":"net1","state":["FAILED"]}]`, "unresolved"},
		{"incomplete", `[{"dst":"192.0.2.1","state":["INCOMPLETE"]}]`, "pending"},
		{"stale", `[{"dst":"192.0.2.1","lladdr":"02:00:00:00:00:01","state":["STALE"]}]`, "resolved"},
		{"reachable", `[{"dst":"192.0.2.1","lladdr":"02:00:00:00:00:01","state":["REACHABLE"]}]`, "resolved"},
		{"no mapping", `[{"dst":"192.0.2.1","state":["STALE"]}]`, "unverified"},
		{"unknown", `[{"dst":"192.0.2.1","state":["FUTURE"]}]`, "unverified"},
		{"empty", `[]`, "unverified"},
		{"malformed", `ip: unknown option -j`, "unverified"},
		{"other neighbor", `[{"dst":"192.0.2.2","state":["FAILED"]}]`, "unverified"},
		{"other interface", `[{"dst":"192.0.2.1","dev":"net2","state":["FAILED"]}]`, "unverified"},
		{"ambiguous", `[{"dst":"192.0.2.1"},{"dst":"192.0.2.1"}]`, "unverified"},
		{"ambiguous state", `[{"dst":"192.0.2.1","state":["FAILED","STALE"]}]`, "unverified"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, message := classifyNeighbor(tc.output, "192.0.2.1", "net1")
			assert.Equal(t, tc.status, status)
			assert.NotEmpty(t, message)
		})
	}
	status, _, _ := classifyNeighbor(`[{"dst":"fe80::1","state":["FAILED"]}]`, "fe80:0:0:0:0:0:0:1", "net1")
	assert.Equal(t, "unresolved", status)
}

func nextHopFailure() PingResult {
	return PingResult{Test: PingTest{Kind: ICMPCrossRail, SrcPod: "pod-a", DstPod: "pod-b", SrcIP: "192.0.2.10", DstIP: "198.51.100.20", SrcIface: "net1", Expectation: ExpectRequired},
		Route: RouteCheck{Command: "ip route get", Output: "198.51.100.20 from 192.0.2.10 via 192.0.2.1 dev net1 table 100", Dev: "net1", OK: true},
		OK:    false, ObservedOK: false, Expectation: ExpectRequired, Err: errors.New("original connection error"), Stderr: "original stderr"}
}

func TestNextHopAnnotationsPreserveResultsAndRefreshEachStage(t *testing.T) {
	namespaces := map[string]string{"pod-a": "default"}
	containers := map[string]string{"pod-a": "netshoot"}
	first := nextHopFailure()
	second := first
	second.Test.DstIP = "198.51.100.30"
	results := []PingResult{first, second}
	calls := 0
	exec := func(ctx context.Context, _ *rest.Config, ns, pod, container string, cmd []string) (kubeclient.ExecResult, error) {
		calls++
		assert.Equal(t, "default", ns)
		assert.Equal(t, "pod-a", pod)
		assert.Equal(t, "netshoot", container)
		assert.Equal(t, []string{"ip", "-j", "neigh", "show", "to", "192.0.2.1", "dev", "net1"}, cmd)
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		assert.LessOrEqual(t, time.Until(deadline), nextHopTimeout)
		state := "FAILED"
		if calls > 1 {
			state = "STALE"
		}
		return kubeclient.ExecResult{Stdout: `[{"dst":"192.0.2.1","lladdr":"02:00:00:00:00:01","state":["` + state + `"]}]`}, nil
	}
	annotateNextHops(context.Background(), nil, namespaces, containers, results, exec)
	assert.Equal(t, 1, calls)
	require.NotNil(t, results[0].NextHop)
	assert.Equal(t, "unresolved", results[0].NextHop.Status)
	for i, want := range []PingResult{first, second} {
		got := results[i]
		got.NextHop = nil
		assert.Equal(t, want, got)
	}
	annotateNextHops(context.Background(), nil, namespaces, containers, results, exec)
	assert.Equal(t, 2, calls)
	assert.Equal(t, "resolved", results[0].NextHop.Status)
}

func TestNextHopSkipsSuccessBandwidthAndRouteFailures(t *testing.T) {
	for _, scenario := range []string{"success", "threshold", "route-error", "route-mismatch", "missing-route", "precomputed-error"} {
		t.Run(scenario, func(t *testing.T) {
			r := nextHopFailure()
			switch scenario {
			case "success":
				r.ObservedOK = true
				r.OK = true
			case "threshold":
				r.BandwidthGbps = 100
			case "route-error":
				r.Route.Err = "route unavailable"
			case "route-mismatch":
				r.Route.Dev = "net2"
			case "missing-route":
				r.Route.OK = false
			case "precomputed-error":
				r.Test.sourceRouteErr = errors.New("guard rejected path")
			}
			results := []PingResult{r}
			annotateNextHops(context.Background(), nil, nil, nil, results, func(context.Context, *rest.Config, string, string, string, []string) (kubeclient.ExecResult, error) {
				t.Fatal("unexpected pod exec")
				return kubeclient.ExecResult{}, nil
			})
			assert.Nil(t, results[0].NextHop)
			assert.Equal(t, r, results[0])
		})
	}
}

func TestNextHopUnavailableEvidenceDoesNotChangeVerdict(t *testing.T) {
	for _, scenario := range []string{"denied", "malformed", "oversized", "cancelled", "missing-container"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := nextHopFailure()
			r.OK = true
			r.Expectation = ExpectForbidden
			r.Test.Expectation = ExpectForbidden
			containers := map[string]string{"pod-a": "netshoot"}
			calls := 0
			if scenario == "cancelled" {
				cancel()
			}
			if scenario == "missing-container" {
				containers = nil
			}
			results := []PingResult{r}
			annotateNextHops(ctx, nil, map[string]string{"pod-a": "default"}, containers, results, func(context.Context, *rest.Config, string, string, string, []string) (kubeclient.ExecResult, error) {
				calls++
				switch scenario {
				case "denied":
					return kubeclient.ExecResult{Stderr: "forbidden"}, errors.New("exec denied")
				case "oversized":
					return kubeclient.ExecResult{Stdout: strings.Repeat("x", traceOutputLimit+1)}, nil
				default:
					return kubeclient.ExecResult{Stdout: "malformed"}, nil
				}
			})
			require.NotNil(t, results[0].NextHop)
			assert.Equal(t, "unverified", results[0].NextHop.Status)
			got := results[0]
			got.NextHop = nil
			assert.Equal(t, r, got)
			if scenario == "cancelled" || scenario == "missing-container" {
				assert.Zero(t, calls)
			}
		})
	}
}

func TestNextHopHonorsCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	results := []PingResult{nextHopFailure()}
	annotateNextHops(ctx, nil, map[string]string{"pod-a": "default"}, map[string]string{"pod-a": "netshoot"}, results,
		func(ctx context.Context, _ *rest.Config, _, _, _ string, _ []string) (kubeclient.ExecResult, error) {
			<-ctx.Done()
			return kubeclient.ExecResult{}, ctx.Err()
		})
	require.NotNil(t, results[0].NextHop)
	assert.Contains(t, results[0].NextHop.Error, "deadline exceeded")
	assert.EqualError(t, results[0].Err, "original connection error")
}

func TestNextHopReportsRemainAdvisoryAndEscapeEvidence(t *testing.T) {
	r := nextHopFailure()
	r.NextHop = &NextHopDiagnostic{Address: "192.0.2.1", Interface: "net1", Gateway: true, Status: "unresolved", State: "FAILED", Message: "next-hop address resolution failed", Output: "<script>unsafe</script>"}
	data, err := json.Marshal(r)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"NextHop"`)
	assert.Contains(t, string(data), `"Error":"original connection error"`)
	plain := nextHopFailure()
	data, err = json.Marshal(plain)
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"NextHop"`)
	matrix := &MatrixResult{PingResults: []PingResult{r}}
	output := &captureOutput{}
	RenderMatrixText(output, matrix)
	assert.Contains(t, strings.Join(output.lines, "\n"), "192.0.2.1 dev net1, unresolved (FAILED)")
	var html bytes.Buffer
	report := fixtureData()
	report.Matrix = matrix
	require.NoError(t, RenderHTML(&html, report))
	assert.Contains(t, html.String(), "Next-hop observations after failed tests (advisory)")
	assert.Contains(t, html.String(), "original connection error")
	assert.Contains(t, html.String(), "&lt;script&gt;unsafe&lt;/script&gt;")
	assert.NotContains(t, html.String(), "<script>unsafe</script>")
}

func TestNextHopReportsShowKindAndObservationTime(t *testing.T) {
	for _, tc := range []struct {
		name       string
		gateway    bool
		observedAt time.Time
		wantTime   string
	}{
		{name: "gateway", gateway: true, observedAt: time.Date(2026, 11, 6, 15, 4, 5, 123000000, time.FixedZone("offset", 2*60*60)), wantTime: "2026-11-06T13:04:05.123Z"},
		{name: "on-link destination", gateway: false, observedAt: time.Date(2026, 11, 6, 13, 4, 5, 0, time.UTC), wantTime: "2026-11-06T13:04:05Z"},
		{name: "uncollected", gateway: true, observedAt: time.Time{}, wantTime: "not collected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := nextHopFailure()
			r.NextHop = &NextHopDiagnostic{Address: "192.0.2.1", Interface: "net1", Gateway: tc.gateway, Status: "unresolved", State: "FAILED", ObservedAt: tc.observedAt, Message: "resolution failed"}
			if tc.observedAt.IsZero() {
				r.NextHop.Status = "unverified"
				r.NextHop.State = ""
				r.NextHop.Message = "observation unavailable"
			}
			matrix := &MatrixResult{PingResults: []PingResult{r}}
			output := &captureOutput{}
			RenderMatrixText(output, matrix)
			kind := "gateway"
			if !tc.gateway {
				kind = "on-link destination"
			}
			text := strings.Join(output.lines, "\n")
			assert.Contains(t, text, kind+" 192.0.2.1 dev net1")
			assert.Contains(t, text, "observed at "+tc.wantTime)
			report := fixtureData()
			report.Matrix = matrix
			var html bytes.Buffer
			require.NoError(t, RenderHTML(&html, report))
			assert.Contains(t, html.String(), kind+" <code>192.0.2.1</code>")
			assert.Contains(t, html.String(), "Observed at: "+tc.wantTime)
		})
	}
}
