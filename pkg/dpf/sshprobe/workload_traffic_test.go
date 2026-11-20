// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
)

type trafficFakeRemote struct {
	snapshot     hostNetworkSnapshot
	calls        []string
	jumboOK      bool
	route        string
	change       bool
	routeErr     bool
	remoteErr    error
	jumboErr     error
	jumboOutput  string
	ordinaryLoss bool
}

func (f *trafficFakeRemote) Run(ctx context.Context, stage, command string) (string, error) {
	f.calls = append(f.calls, stage+" "+command)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	switch stage {
	case "host-network-ssh-path":
		return "192.0.2.10 40000 192.0.2.20 22", nil
	case "host-network-snapshot":
		snapshot := f.snapshot
		if f.change && strings.Count(strings.Join(f.calls, "\n"), "host-network-snapshot") > 1 {
			snapshot.RuntimeDigest = strings.Repeat("e", 64)
		}
		data, _ := json.Marshal(snapshot)
		return string(data), nil
	case "host-traffic-route":
		if f.routeErr {
			if f.remoteErr != nil {
				return "", f.remoteErr
			}
			return "", errors.New("ip unavailable")
		}
		if f.route != "" {
			return f.route, nil
		}
		return `[{"dst":"172.25.1.2","from":"172.25.0.2","gateway":"172.25.0.1","dev":"test0","flags":[],"uid":0,"cache":[]}]`, nil
	case "host-traffic-ping":
		if f.ordinaryLoss && !strings.Contains(command, "-M do") {
			exit := 1
			return "3 packets transmitted, 1 received, 66.6667% packet loss, time 4ms\n", &Error{Code: "SSH_COMMAND_FAILED", Stage: stage, Tool: "/usr/bin/ping", ExitStatus: &exit}
		}
		if strings.Contains(command, "-M do") && !f.jumboOK {
			output := f.jumboOutput
			if output == "" {
				output = "PING summary\n3 packets transmitted, 1 received, 66.6667% packet loss, time 4ms\n"
			}
			err := f.jumboErr
			if err == nil {
				exit := 1
				err = &Error{Code: "SSH_COMMAND_FAILED", Stage: stage, Tool: "/usr/bin/ping", ExitStatus: &exit}
			}
			return output, err
		}
		return "PING summary\n3 packets transmitted, 3 received, 0% packet loss, time 2ms\nrtt min/avg/max/mdev = 0.1/0.2/0.3/0.0 ms\n", nil
	default:
		return "", errors.New("unexpected stage")
	}
}

func TestHostTrafficOrdinaryLossStillMeasuresDFAndFinalFences(t *testing.T) {
	for _, dfPass := range []bool{true, false} {
		t.Run(fmt.Sprint(dfPass), func(t *testing.T) {
			remote := &trafficFakeRemote{snapshot: trafficTestSnapshot(), ordinaryLoss: true, jumboOK: dfPass}
			result, err := runTrafficProbeForTest(remote, trafficTestRequest())
			if Code(err) != "HOST_NETWORK_TRAFFIC_PING_INCOMPLETE" || result.Ordinary.State != TrafficProbeFailed || result.Ordinary.Counters == nil || result.Ordinary.Counters.Received != 1 || !result.RouteVerified {
				t.Fatalf("ordinary loss lost counters or final qualification: %+v %v", result, err)
			}
			expected := TrafficProbeFailed
			if dfPass {
				expected = TrafficProbePassed
			}
			if result.MTUProbe.State != expected || result.MTUProbe.Counters == nil {
				t.Fatalf("required DF not measured: %+v", result)
			}
			assertTrafficCommands(t, remote.calls, 3)
			if strings.Count(strings.Join(remote.calls, "\n"), "host-network-snapshot") != 2 {
				t.Fatal("health failure skipped final host identity snapshot")
			}
		})
	}
	remote := &trafficFakeRemote{snapshot: trafficTestSnapshot(), ordinaryLoss: true, jumboOK: true, change: true}
	result, err := runTrafficProbeForTest(remote, trafficTestRequest())
	if Code(err) != "HOST_NETWORK_RUNTIME_CHANGED" || result.RouteVerified || result.Ordinary.Counters == nil || result.MTUProbe.State != TrafficProbePassed {
		t.Fatalf("final drift did not override packet health: %+v %v", result, err)
	}
}

func (f *trafficFakeRemote) RunWithInput(ctx context.Context, stage, command string, _ []byte) (string, error) {
	return f.Run(ctx, stage, command)
}

func (f *trafficFakeRemote) RunWithInputTimeout(ctx context.Context, stage, command string, input []byte, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return f.RunWithInput(ctx, stage, command, input)
}

func trafficTestRequest() HostTrafficRequest {
	r := HostTrafficRequest{HostNetworkRequest: hostNetworkTestRequest()}
	r.MTU = MinimumHostWorkloadMTU
	r.Destination = netip.MustParseAddr("172.25.1.2")
	return r
}

func trafficTestResult(request HostTrafficRequest) HostTrafficResult {
	return HostTrafficResult{
		State:               TrafficProbeUnknown,
		ExpectedSource:      "172.25.0.2",
		ExpectedDestination: request.Destination.String(),
		ExpectedInterface:   "test0",
		ExpectedGateway:     "172.25.0.1",
		MTU:                 request.MTU,
		Ordinary:            HostTrafficProbe{State: TrafficProbeNotRun},
		MTUProbe:            HostTrafficProbe{State: TrafficProbeNotRun},
	}
}

func runTrafficProbeForTest(f *trafficFakeRemote, request HostTrafficRequest) (HostTrafficResult, error) {
	result := trafficTestResult(request)
	s := hostNetworkSession{remote: f}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	err := s.probeHostWorkloadTraffic(ctx, request, &result)
	return result, err
}

func trafficTestSnapshot() hostNetworkSnapshot {
	s := hostNetworkTestSnapshot()
	s.Link = json.RawMessage(`[{"ifname":"test0","link_type":"ether","mtu":1280,"min_mtu":68,"max_mtu":9700,"flags":["UP"]}]`)
	s.Addresses = json.RawMessage(`[{"addr_info":[{"family":"inet","local":"172.25.0.2","prefixlen":24}]}]`)
	s.Routes = json.RawMessage(`[{"dst":"default","gateway":"192.0.2.1","dev":"eth0"},{"dst":"172.25.0.0/16","gateway":"172.25.0.1","dev":"test0","table":254,"type":"unicast"}]`)
	return s
}

func TestHostTrafficPreservesOrdinaryResultOnMTUFailure(t *testing.T) {
	f := &trafficFakeRemote{snapshot: trafficTestSnapshot()}
	result, err := runTrafficProbeForTest(f, trafficTestRequest())
	if err == nil || Code(err) != "HOST_NETWORK_TRAFFIC_PING_INCOMPLETE" || result.Ordinary.State != TrafficProbePassed || result.Ordinary.PayloadBytes != 56 || result.Ordinary.Counters == nil || *result.Ordinary.Counters != (PingCounters{Sent: 3, Received: 3, LostPct: 0}) || result.MTUProbe.State != TrafficProbeFailed || result.MTUProbe.PayloadBytes != 1252 || result.MTUProbe.Counters == nil || *result.MTUProbe.Counters != (PingCounters{Sent: 3, Received: 1, LostPct: 66.6667}) {
		t.Fatalf("expected ordinary success and MTU failure, got result=%+v err=%v", result, err)
	}
	assertTrafficCommands(t, f.calls, 1)
}

func TestParsePingCountersAcceptsPinnedIputilsPercentages(t *testing.T) {
	for _, tc := range []struct {
		name           string
		sent, received int
		loss           float64
	}{
		{"zero loss", 3, 3, 0},
		{"one lost", 3, 2, 33.3333},
		{"two lost", 3, 1, 66.6667},
		{"all lost", 3, 0, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := fmt.Sprintf("3 packets transmitted, %d received, %g%% packet loss, time 3ms\n", tc.received, tc.loss)
			counters, err := parsePingCounters([]byte(line))
			if err != nil || counters == nil || counters.Sent != tc.sent || counters.Received != tc.received || counters.LostPct != tc.loss {
				t.Fatalf("parse mismatch: counters=%+v err=%v", counters, err)
			}
		})
	}
}

func TestParsePingCountersRejectsImpossibleOrMalformedSummary(t *testing.T) {
	for _, tc := range []struct{ name, summary string }{
		{"contradictory percentage", "3 packets transmitted, 1 received, 33.3333% packet loss"},
		{"negative", "3 packets transmitted, 1 received, -66.6667% packet loss"},
		{"nan", "3 packets transmitted, 1 received, NaN% packet loss"},
		{"over one hundred", "3 packets transmitted, 0 received, 100.0001% packet loss"},
		{"oversized percentage", "3 packets transmitted, 0 received, 999999999999999999999999999999999999% packet loss"},
		{"sent count mismatch", "2 packets transmitted, 1 received, 50% packet loss"},
		{"received exceeds sent", "3 packets transmitted, 4 received, 0% packet loss"},
		{"rounded beyond iputils precision", "3 packets transmitted, 1 received, 66.6668% packet loss"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePingCounters([]byte(tc.summary + ", time 3ms\n"))
			if Code(err) != "HOST_NETWORK_TRAFFIC_RESPONSE_INVALID" {
				t.Fatalf("expected response-invalid, got %v", err)
			}
		})
	}
}

func TestHostTrafficRequiresVerifiedExitStatusForPingLossClassification(t *testing.T) {
	for _, tc := range []struct {
		name       string
		exitStatus *int
		wantCode   string
		wantState  string
	}{
		{"normal loss exit", intPointer(1), "HOST_NETWORK_TRAFFIC_PING_INCOMPLETE", TrafficProbeFailed},
		{"other ping exit", intPointer(2), "SSH_COMMAND_FAILED", TrafficProbeUnknown},
		{"missing exit status", nil, "SSH_COMMAND_FAILED", TrafficProbeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &trafficFakeRemote{
				snapshot:    trafficTestSnapshot(),
				jumboErr:    &Error{Code: "SSH_COMMAND_FAILED", Stage: "host-traffic-ping", Tool: "/usr/bin/ping", ExitStatus: tc.exitStatus},
				jumboOutput: "PING summary\n3 packets transmitted, 1 received, 66.6667% packet loss, time 4ms\n",
			}
			result, err := runTrafficProbeForTest(f, trafficTestRequest())
			if Code(err) != tc.wantCode || result.MTUProbe.State != tc.wantState || result.MTUProbe.Counters == nil || result.MTUProbe.Counters.LostPct != 66.6667 {
				t.Fatalf("classification mismatch: result=%+v err=%v", result, err)
			}
		})
	}
}

func intPointer(value int) *int { return &value }

func TestHostTrafficNaturalRouteAndSuccessfulPair(t *testing.T) {
	f := &trafficFakeRemote{snapshot: trafficTestSnapshot(), jumboOK: true}
	result, err := runTrafficProbeForTest(f, trafficTestRequest())
	if err != nil || !result.RouteVerified || result.Ordinary.State != TrafficProbePassed || result.MTUProbe.State != TrafficProbePassed || result.Ordinary.Counters == nil || result.MTUProbe.Counters == nil || result.Ordinary.Counters.Received != 3 || result.MTUProbe.Counters.Received != 3 {
		t.Fatalf("probe failed: %+v %v", result, err)
	}
	assertTrafficCommands(t, f.calls, 2)
}

func TestHostTrafficRouteMismatchStopsBeforePing(t *testing.T) {
	f := &trafficFakeRemote{snapshot: trafficTestSnapshot(), route: `[{"dst":"172.25.1.2","from":"172.25.0.2","gateway":"172.25.0.1","dev":"eth1"}]`}
	result, err := runTrafficProbeForTest(f, trafficTestRequest())
	if Code(err) != "HOST_NETWORK_TRAFFIC_ROUTE_MISMATCH" || result.RouteVerified || result.Ordinary.State != TrafficProbeNotRun {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, call := range f.calls {
		if strings.Contains(call, "host-traffic-ping") {
			t.Fatal("ping ran despite natural route mismatch")
		}
	}
}

func TestHostTrafficMissingRouteToolIsUnknownAndStops(t *testing.T) {
	f := &trafficFakeRemote{snapshot: trafficTestSnapshot(), routeErr: true}
	result, err := runTrafficProbeForTest(f, trafficTestRequest())
	if Code(err) != "HOST_NETWORK_TRAFFIC_ROUTE_FAILED" || result.RouteVerified || result.Ordinary.State != TrafficProbeNotRun {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, call := range f.calls {
		if strings.Contains(call, "host-traffic-ping") {
			t.Fatal("ping ran without route evidence")
		}
	}
}

func TestHostTrafficCanceledBeforeObservationDoesNotProbe(t *testing.T) {
	f := &trafficFakeRemote{snapshot: trafficTestSnapshot(), jumboOK: true}
	s := hostNetworkSession{remote: f}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := trafficTestResult(trafficTestRequest())
	err := s.probeHostWorkloadTraffic(ctx, trafficTestRequest(), &result)
	if err == nil {
		t.Fatal("accepted canceled operation")
	}
	for _, call := range f.calls {
		if strings.Contains(call, "host-traffic-ping") {
			t.Fatal("ping ran after cancellation")
		}
	}
}

func TestHostTrafficInvalidDestinationIsRejectedBeforeOpeningSSH(t *testing.T) {
	request := trafficTestRequest()
	request.Destination = netip.Addr{}
	target := config.Target{Role: "hosts", SSH: &config.SSHAccess{}, Endpoint: "invalid"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := ProbeHostWorkloadTraffic(ctx, target, request, time.Second)
	if Code(err) != "HOST_NETWORK_TRAFFIC_INPUT_INVALID" || result.State != TrafficProbeUnknown || result.ReasonCode != "HOST_NETWORK_TRAFFIC_INPUT_INVALID" || result.Ordinary.State != TrafficProbeNotRun {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHostTrafficRuntimeChangeRevokesSuccess(t *testing.T) {
	f := &trafficFakeRemote{snapshot: trafficTestSnapshot(), jumboOK: true, change: true}
	result, err := runTrafficProbeForTest(f, trafficTestRequest())
	if Code(err) != "HOST_NETWORK_RUNTIME_CHANGED" || result.RouteVerified || result.Ordinary.State != TrafficProbePassed || result.MTUProbe.State != TrafficProbePassed {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNaturalTrafficRouteRejectsAmbiguousAndUnpinnedFields(t *testing.T) {
	r := trafficTestRequest()
	valid := `[{"dst":"172.25.1.2","from":"172.25.0.2","gateway":"172.25.0.1","dev":"test0","flags":[],"uid":0,"cache":[]}]`
	for _, tc := range []struct{ name, data string }{
		{"extra route", valid[:len(valid)-1] + `,{"dst":"172.25.1.2"}]`},
		{"wrong source", strings.Replace(valid, "172.25.0.2", "172.25.0.3", 1)},
		{"wrong gateway", strings.Replace(valid, "172.25.0.1", "172.25.0.9", 1)},
		{"wrong table", strings.Replace(valid, `"flags":[]`, `"table":100,"flags":[]`, 1)},
		{"forced oif marker", strings.Replace(valid, `"flags":[]`, `"oif":"test0","flags":[]`, 1)},
		{"nexthops", strings.Replace(valid, `"flags":[]`, `"nexthops":[],"flags":[]`, 1)},
		{"encapsulation", strings.Replace(valid, `"flags":[]`, `"encap":{},"flags":[]`, 1)},
		{"nhid", strings.Replace(valid, `"flags":[]`, `"nhid":1,"flags":[]`, 1)},
		{"null flags", strings.Replace(valid, `"flags":[]`, `"flags":null`, 1)},
		{"null cache", strings.Replace(valid, `"cache":[]`, `"cache":null`, 1)},
		{"empty route type", strings.Replace(valid, `"flags":[]`, `"type":"","flags":[]`, 1)},
		{"default destination", strings.Replace(valid, "172.25.1.2", "default", 1)},
		{"malformed", "[] trailing"},
		{"null table", strings.Replace(valid, `"flags":[]`, `"table":null,"flags":[]`, 1)},
		{"null route type", strings.Replace(valid, `"flags":[]`, `"type":null,"flags":[]`, 1)},
		{"null uid", strings.Replace(valid, `"uid":0`, `"uid":null`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if validNaturalTrafficRoute([]byte(tc.data), r) {
				t.Fatal("accepted invalid route")
			}
		})
	}
}

func TestHostTrafficRouteOutputCapUsesOtherwiseValidJSON(t *testing.T) {
	valid := `[ {"dst":"172.25.1.2","from":"172.25.0.2","gateway":"172.25.0.1","dev":"test0","flags":[],"uid":0,"cache":[]} ]`
	f := &trafficFakeRemote{snapshot: trafficTestSnapshot(), route: valid + strings.Repeat(" ", trafficOutputLimit-len(valid)+1)}
	result, err := runTrafficProbeForTest(f, trafficTestRequest())
	if Code(err) != "HOST_NETWORK_TRAFFIC_ROUTE_MISMATCH" || result.RouteVerified || result.Ordinary.State != TrafficProbeNotRun {
		t.Fatalf("oversized otherwise-valid route evidence was accepted: %+v %v", result, err)
	}
}

func TestHostTrafficPreservesTypedRouteFailures(t *testing.T) {
	for _, code := range []string{"TOOL_NOT_AVAILABLE", "PERMISSION_DENIED", "SSH_TIMEOUT_OR_CANCELLED"} {
		t.Run(code, func(t *testing.T) {
			f := &trafficFakeRemote{snapshot: trafficTestSnapshot(), routeErr: true, remoteErr: &Error{Code: code, Stage: "host-traffic-route", Tool: "ip"}}
			result, err := runTrafficProbeForTest(f, trafficTestRequest())
			if Code(err) != code || result.RouteVerified || result.Ordinary.State != TrafficProbeNotRun {
				t.Fatalf("typed failure was lost: result=%+v err=%v", result, err)
			}
		})
	}
}

func assertTrafficCommands(t *testing.T, calls []string, minimumRoutes int) {
	t.Helper()
	routes, pings := 0, 0
	for _, call := range calls {
		if strings.Contains(call, "host-traffic-route") {
			routes++
			if strings.Contains(call, " oif ") || strings.Contains(call, "sudo") {
				t.Fatalf("route was forced or privileged: %s", call)
			}
		}
		if strings.Contains(call, "host-traffic-ping") {
			pings++
			if strings.Contains(call, "sudo") || !strings.Contains(call, "-B -I 172.25.0.2") || !strings.Contains(call, " -i 1 -c 3 -W 2 172.25.1.2") {
				t.Fatalf("unexpected ping argv: %s", call)
			}
		}
	}
	if routes < minimumRoutes || pings != 2 {
		t.Fatalf("route/ping calls = %d/%d; expected at least %d/2", routes, pings, minimumRoutes)
	}
}
