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

	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
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

func TestCheckSourceRoutesKeepsICMPRouteMismatchDiagnostic(t *testing.T) {
	for _, expectation := range []Expectation{ExpectRequired, ExpectObserve, ExpectForbidden} {
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
			assert.NoError(t, got[0].sourceRouteErr)
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

func TestCheckSourceRoutesKeepsRequiredRDMAGuard(t *testing.T) {
	test := PingTest{
		Kind: RDMAPingCrossRail, SrcPod: "pod-a", DstPod: "pod-b",
		SrcIP: "192.0.2.10", DstIP: "198.51.100.20", SrcIface: "net1",
		Expectation: ExpectRequired,
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
	assert.EqualError(t, got[0].sourceRouteErr,
		`source route selected dev "net2", expected "net1" (route: 198.51.100.20 dev net2 src 192.0.2.10)`)
}

func TestBoundedTraceOutput(t *testing.T) {
	input := strings.Repeat("x", traceOutputLimit+100)
	got := boundedTraceOutput(input)

	assert.Less(t, len(got), len(input))
	assert.Contains(t, got, "truncated 100 bytes")
}

func TestRunICMPExecutesProbeDespitePrecomputedRouteError(t *testing.T) {
	for _, expectation := range []Expectation{ExpectRequired, ExpectObserve, ExpectForbidden} {
		t.Run(string(expectation), func(t *testing.T) {
			test := PingTest{
				Kind: ICMPSameRail, SrcIface: "net1", SrcIP: "192.0.2.10", DstIP: "198.51.100.20", Expectation: expectation,
				sourceRoute:    RouteCheck{Command: "ip route get", Err: "command terminated with exit code 1"},
				sourceRouteErr: errors.New("route lookup failed"),
			}
			called := false
			execInPod := func(_ context.Context, _ *rest.Config, _, _, _ string, command []string) (kubeclient.ExecResult, error) {
				called = true
				require.Equal(t, []string{"/bin/sh", "-c", shellWithTimeout(icmpCommand(test), commandTimeoutFor(test, icmpCommandTimeout))}, command)
				return kubeclient.ExecResult{Stdout: "1 packets transmitted, 1 received"}, nil
			}

			result := runICMP(context.Background(), nil, "default", "pod-a", "netshoot", test, execInPod)

			assert.True(t, called)
			assert.True(t, result.ObservedOK)
			assert.Equal(t, expectation != ExpectForbidden, result.OK)
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
		})
	}
}

func TestRunICMPUsesObservedFailureDespiteRouteLookupFailure(t *testing.T) {
	test := PingTest{
		Kind:        ICMPCrossRail,
		SrcIface:    "net1",
		SrcIP:       "192.0.2.10",
		DstIP:       "198.51.100.20",
		Expectation: ExpectForbidden,
		sourceRoute: RouteCheck{
			Command: "ip route get",
			Err:     "command terminated with exit code 1",
		},
	}
	pingErr := errors.New("ping exited with code 1")
	execInPod := func(_ context.Context, _ *rest.Config, _, _, _ string, _ []string) (kubeclient.ExecResult, error) {
		return kubeclient.ExecResult{}, pingErr
	}

	result := runICMP(context.Background(), nil, "default", "pod-a", "netshoot", test, execInPod)

	assert.True(t, result.OK)
	assert.False(t, result.ObservedOK)
	assert.NoError(t, result.Err)
}

func TestRunICMPUsesObservedSuccessDespiteRouteMismatch(t *testing.T) {
	test := PingTest{
		Kind: ICMPCrossRail, SrcIface: "net1", SrcIP: "192.0.2.10", DstIP: "198.51.100.20", Expectation: ExpectObserve,
		sourceRoute: RouteCheck{
			Command: "ip route get", Output: "198.51.100.20 dev net2 src 192.0.2.10", Dev: "net2", OK: true,
		},
	}
	called := false
	execInPod := func(_ context.Context, _ *rest.Config, _, _, _ string, _ []string) (kubeclient.ExecResult, error) {
		called = true
		return kubeclient.ExecResult{}, nil
	}

	result := runICMP(context.Background(), nil, "default", "pod-a", "netshoot", test, execInPod)

	assert.True(t, called)
	assert.True(t, result.OK)
	assert.True(t, result.ObservedOK)
	assert.NoError(t, result.Err)
}

func TestRunICMPUsesObservedSuccessForForbiddenRouteMismatch(t *testing.T) {
	test := PingTest{
		Kind: ICMPCrossRail, SrcIface: "net1", SrcIP: "192.0.2.10", DstIP: "198.51.100.20", Expectation: ExpectForbidden,
		sourceRoute: RouteCheck{
			Command: "ip route get", Output: "198.51.100.20 dev net2 src 192.0.2.10", Dev: "net2", OK: true,
		},
	}
	execInPod := func(_ context.Context, _ *rest.Config, _, _, _ string, _ []string) (kubeclient.ExecResult, error) {
		return kubeclient.ExecResult{}, nil
	}

	result := runICMP(context.Background(), nil, "default", "pod-a", "netshoot", test, execInPod)

	assert.False(t, result.OK)
	assert.True(t, result.ObservedOK)
	assert.EqualError(t, result.Err, "cross-rail traffic succeeded but profile routing expects isolation")
}
