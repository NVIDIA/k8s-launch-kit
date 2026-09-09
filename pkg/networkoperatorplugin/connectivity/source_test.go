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
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestShellWithTimeoutPrefersNativeTimeout(t *testing.T) {
	cmd := shellWithTimeout(`ping -c 1 -W 1 -I "192.168.0.10" "192.168.0.11"`, 5*time.Second)

	assert.Contains(t, cmd, `command -v timeout`)
	assert.Contains(t, cmd, `timeout -s TERM -k 2 5 sh -c`)
	assert.Contains(t, cmd, `ping -c 1 -W 1 -I \"192.168.0.10\" \"192.168.0.11\"`)
	assert.Contains(t, cmd, `else ping -c 1 -W 1 -I "192.168.0.10" "192.168.0.11" & pid=$!`)
}

func TestShellWithTimeoutUsesMinimumOneSecond(t *testing.T) {
	cmd := shellWithTimeout(`true`, 0)

	assert.Contains(t, cmd, `timeout -s TERM -k 2 1 sh -c "true"`)
	assert.Contains(t, cmd, `sleep 1;`)
}

func TestCheckSourceRoutesReusesCachedRoute(t *testing.T) {
	test := PingTest{
		Kind: ICMPSameRail, SrcPod: "pod-a", DstPod: "pod-b",
		SrcIP: "192.0.2.10", DstIP: "192.0.2.20", SrcIface: "net1",
		Expectation: ExpectRequired,
	}
	key := routeCacheKey{
		namespace: "default", pod: "pod-a", container: "netshoot",
		srcIP: test.SrcIP, dstIP: test.DstIP,
	}
	cache := routeCache{key: {
		Command: "ip route get", Output: "192.0.2.20 dev net1 src 192.0.2.10", Dev: "net1", OK: true,
	}}

	got := checkSourceRoutes(context.Background(), nil,
		map[string]string{"pod-a": "default"}, map[string]string{"pod-a": "netshoot"}, []PingTest{test}, cache)

	assert.Len(t, got, 1)
	assert.Equal(t, "net1", got[0].sourceRoute.Dev)
	assert.NoError(t, got[0].sourceRouteErr)
}

func TestCheckSourceRoutesQualifiesNonRequiredICMP(t *testing.T) {
	for _, expectation := range []Expectation{ExpectObserve, ExpectForbidden} {
		t.Run(string(expectation), func(t *testing.T) {
			test := PingTest{
				Kind: ICMPCrossRail, SrcPod: "pod-a", DstPod: "pod-b",
				SrcIP: "192.0.2.10", DstIP: "198.51.100.20", SrcIface: "net1",
				Expectation: expectation,
			}
			key := routeCacheKey{
				namespace: "default", pod: "pod-a", container: "netshoot",
				srcIP: test.SrcIP, dstIP: test.DstIP,
			}
			cache := routeCache{key: {
				Command: "ip route get", Output: "198.51.100.20 dev net2 src 192.0.2.10", Dev: "net2", OK: true,
			}}

			got := checkSourceRoutes(context.Background(), nil,
				map[string]string{"pod-a": "default"}, map[string]string{"pod-a": "netshoot"}, []PingTest{test}, cache)

			assert.Len(t, got, 1)
			assert.Equal(t, "net2", got[0].sourceRoute.Dev)
			assert.EqualError(t, got[0].sourceRouteErr,
				`source route selected dev "net2", expected "net1" (route: 198.51.100.20 dev net2 src 192.0.2.10)`)
		})
	}
}

func TestCheckSourceRoutesKeepsObservedRDMABehavior(t *testing.T) {
	test := PingTest{
		Kind: RDMAPingCrossRail, SrcPod: "pod-a", DstPod: "pod-b",
		SrcIP: "192.0.2.10", DstIP: "198.51.100.20", SrcIface: "net1",
		Expectation: ExpectObserve,
	}

	got := checkSourceRoutes(context.Background(), nil,
		map[string]string{"pod-a": "default"}, map[string]string{"pod-a": "netshoot"}, []PingTest{test}, routeCache{})

	assert.Len(t, got, 1)
	assert.Empty(t, got[0].sourceRoute.Command)
	assert.NoError(t, got[0].sourceRouteErr)
}

func TestBoundedTraceOutput(t *testing.T) {
	input := strings.Repeat("x", traceOutputLimit+100)
	got := boundedTraceOutput(input)

	assert.Less(t, len(got), len(input))
	assert.Contains(t, got, "truncated 100 bytes")
}

func TestRunICMPPreservesPrecomputedRouteErrorForEveryExpectation(t *testing.T) {
	for _, expectation := range []Expectation{ExpectRequired, ExpectObserve, ExpectForbidden} {
		t.Run(string(expectation), func(t *testing.T) {
			test := PingTest{
				Kind: ICMPSameRail, SrcIface: "net1", Expectation: expectation,
				sourceRouteErr: errors.New("route lookup failed"),
			}

			result := RunICMP(context.Background(), nil, "default", "pod-a", "netshoot", test)

			assert.False(t, result.OK)
			assert.False(t, result.ObservedOK)
			assert.EqualError(t, result.Err, "route lookup failed")
		})
	}
}

func TestSourceRouteValidationErrorRejectsLookupAndParseFailures(t *testing.T) {
	test := PingTest{SrcIface: "net1"}
	tests := []struct {
		name  string
		route RouteCheck
		want  string
	}{
		{
			name:  "lookup failed",
			route: RouteCheck{Command: "ip route get", Err: "command terminated with exit code 1"},
			want:  "source route check failed: command terminated with exit code 1",
		},
		{
			name:  "device missing",
			route: RouteCheck{Command: "ip route get", Output: "unparseable route"},
			want:  "source route check returned no device (route: unparseable route)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := sourceRouteValidationError(tc.route, test)

			assert.EqualError(t, err, tc.want)
			assert.False(t, isSourceRouteMismatchError(err))
		})
	}
}

func TestRunICMPTreatsForbiddenRouteLookupFailureAsValidationFailure(t *testing.T) {
	test := PingTest{
		Kind:        ICMPCrossRail,
		SrcIface:    "net1",
		Expectation: ExpectForbidden,
		sourceRoute: RouteCheck{
			Command: "ip route get",
			Err:     "command terminated with exit code 1",
		},
	}

	result := RunICMP(context.Background(), nil, "default", "pod-a", "netshoot", test)

	assert.False(t, result.OK)
	assert.False(t, result.ObservedOK)
	assert.EqualError(t, result.Err,
		"source route check failed: command terminated with exit code 1")
}

func TestRunICMPTreatsObservedRouteMismatchAsDisconnected(t *testing.T) {
	test := PingTest{
		Kind: ICMPCrossRail, SrcIface: "net1", Expectation: ExpectObserve,
		sourceRoute: RouteCheck{
			Command: "ip route get", Output: "198.51.100.20 dev net2 src 192.0.2.10", Dev: "net2", OK: true,
		},
	}

	result := RunICMP(context.Background(), nil, "default", "pod-a", "netshoot", test)

	assert.True(t, result.OK)
	assert.False(t, result.ObservedOK)
	assert.EqualError(t, result.Err,
		`source route selected dev "net2", expected "net1" (route: 198.51.100.20 dev net2 src 192.0.2.10)`)
}

func TestRunICMPTreatsForbiddenRouteMismatchAsDisconnected(t *testing.T) {
	test := PingTest{
		Kind: ICMPCrossRail, SrcIface: "net1", Expectation: ExpectForbidden,
		sourceRoute: RouteCheck{
			Command: "ip route get", Output: "198.51.100.20 dev net2 src 192.0.2.10", Dev: "net2", OK: true,
		},
	}

	result := RunICMP(context.Background(), nil, "default", "pod-a", "netshoot", test)

	assert.True(t, result.OK)
	assert.False(t, result.ObservedOK)
	assert.NoError(t, result.Err)
}
