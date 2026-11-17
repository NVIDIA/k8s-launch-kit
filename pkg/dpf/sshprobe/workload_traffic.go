// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/netip"
	"regexp"
	"strconv"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
)

// HostTrafficRequest binds one directed test to the host PF and current lease.
type HostTrafficRequest struct {
	HostNetworkRequest
	Destination netip.Addr `json:"destination"`
}

// PingCounters contains only parsed summary counts; command output is private.
type PingCounters struct {
	Sent     int     `json:"sent"`
	Received int     `json:"received"`
	LostPct  float64 `json:"lostPercent"`
}

const (
	TrafficProbeNotRun  = "not-run" // TrafficProbeNotRun means the command was not attempted.
	TrafficProbeUnknown = "unknown" // TrafficProbeUnknown means no definitive measurement was obtained.
	TrafficProbePassed  = "passed"  // TrafficProbePassed means all requested packets were received.
	TrafficProbeFailed  = "failed"  // TrafficProbeFailed means a parsed measurement did not meet the contract.
)

// HostTrafficProbe reports whether a probe was measured, passed, or failed.
type HostTrafficProbe struct {
	State        string        `json:"state"`
	ReasonCode   string        `json:"reasonCode,omitempty"`
	PayloadBytes int           `json:"payloadBytes,omitempty"`
	Counters     *PingCounters `json:"counters,omitempty"`
}

// HostTrafficResult describes one directed observation, not a cohort pass.
type HostTrafficResult struct {
	State               string           `json:"state"`
	ReasonCode          string           `json:"reasonCode,omitempty"`
	ExpectedSource      string           `json:"expectedSource,omitempty"`
	ExpectedDestination string           `json:"expectedDestination,omitempty"`
	ExpectedInterface   string           `json:"expectedInterface,omitempty"`
	ExpectedGateway     string           `json:"expectedGateway,omitempty"`
	MTU                 int              `json:"mtu,omitempty"`
	RouteVerified       bool             `json:"routeVerified"`
	Ordinary            HostTrafficProbe `json:"ordinary"`
	MTUProbe            HostTrafficProbe `json:"mtuProbe"`
}

const trafficOutputLimit = 16 << 10

var pingSummary = regexp.MustCompile(`(?m)^([0-9]+) packets transmitted, ([0-9]+) (?:packets )?received, ([0-9]+(?:\.[0-9]+)?)% packet loss(?:,|$)`)

// ProbeHostWorkloadTraffic performs two finite, source-bound pings over the
// natural route selected by the kernel. The caller owns cross-resource fences.
func ProbeHostWorkloadTraffic(ctx context.Context, target config.Target, request HostTrafficRequest, timeout time.Duration) (result HostTrafficResult, err error) {
	result = HostTrafficResult{State: TrafficProbeUnknown, Ordinary: HostTrafficProbe{State: TrafficProbeNotRun}, MTUProbe: HostTrafficProbe{State: TrafficProbeNotRun}}
	defer func() {
		if err == nil {
			result.State, result.ReasonCode = TrafficProbePassed, ""
			return
		}
		result.State = TrafficProbeUnknown
		if Code(err) == "HOST_NETWORK_TRAFFIC_PING_INCOMPLETE" {
			result.State = TrafficProbeFailed
		}
		result.ReasonCode = safeTrafficReason(err)
	}()
	if err := validateHostNetworkAccess(ctx, target, request.Identity, timeout); err != nil {
		return result, err
	}
	if !hostNetworkLeaseBinding.MatchString(request.LeaseBinding) {
		return result, networkError("HOST_NETWORK_LEASE_BINDING_INVALID", false)
	}
	if err := validateHostNetworkDesired(request.HostNetworkRequest); err != nil {
		return result, err
	}
	if !request.Destination.Is4() || !request.Destination.IsGlobalUnicast() || !netip.MustParsePrefix(request.WorkloadCIDR).Contains(request.Destination) ||
		request.Destination == netip.MustParsePrefix(request.HostAddressPrefix).Addr() || request.Destination == netip.MustParseAddr(request.Gateway) ||
		netip.MustParsePrefix(request.HostAddressPrefix).Contains(request.Destination) {
		return result, networkError("HOST_NETWORK_TRAFFIC_INPUT_INVALID", false)
	}
	result.ExpectedSource = netip.MustParsePrefix(request.HostAddressPrefix).Addr().String()
	result.ExpectedDestination = request.Destination.String()
	result.ExpectedInterface = request.Identity.InterfaceName
	result.ExpectedGateway = netip.MustParseAddr(request.Gateway).String()
	result.MTU = request.MTU
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	session, closeSession, err := openHostNetworkSession(ctx, target, timeout)
	if err == nil {
		err = session.probeHostWorkloadTraffic(ctx, request, &result)
		if closeErr := closeSession(); closeErr != nil && err == nil {
			err = networkError("HOST_NETWORK_SSH_CLEANUP_UNCONFIRMED", false)
		}
	}
	if err != nil {
		result.State = TrafficProbeUnknown
		if Code(err) == "HOST_NETWORK_TRAFFIC_PING_INCOMPLETE" {
			result.State = TrafficProbeFailed
		}
		result.ReasonCode = safeTrafficReason(err)
		return result, err
	}
	result.State = TrafficProbePassed
	return result, nil
}

func (s *hostNetworkSession) probeHostWorkloadTraffic(ctx context.Context, request HostTrafficRequest, result *HostTrafficResult) error {
	before, err := s.observe(ctx, request.Identity)
	if err != nil {
		return err
	}
	defer clearHostObservation(&before)
	if before.MTU != request.MTU || request.MTU < before.MinimumWorkloadMTU || request.MTU > before.MaxMTU {
		return networkError("HOST_NETWORK_MTU_UNSUPPORTED", false)
	}
	addressExists, routeExists, err := inspectHostNetworkRuntime(request.HostNetworkRequest, before)
	if err != nil || !addressExists || !routeExists {
		return networkError("HOST_NETWORK_RUNTIME_CONFLICT", false)
	}
	if err := s.checkTrafficRoute(ctx, request); err != nil {
		return err
	}
	result.Ordinary.State = TrafficProbeUnknown
	result.Ordinary.PayloadBytes = 56
	ordinary, err := s.pingHost(ctx, request, 56, false)
	setTrafficProbe(&result.Ordinary, ordinary, err)
	if err != nil {
		return err
	}
	if err := s.checkTrafficRoute(ctx, request); err != nil {
		return err
	}
	result.MTUProbe.State = TrafficProbeUnknown
	result.MTUProbe.PayloadBytes = request.MTU - 28
	mtu, err := s.pingHost(ctx, request, request.MTU-28, true)
	setTrafficProbe(&result.MTUProbe, mtu, err)
	if err != nil {
		return err
	}
	if err := s.checkTrafficRoute(ctx, request); err != nil {
		return err
	}
	after, err := s.observe(ctx, request.Identity)
	if err != nil {
		return err
	}
	defer clearHostObservation(&after)
	if before.raw.RuntimeDigest != after.raw.RuntimeDigest || before.raw.SourceDigest != after.raw.SourceDigest || before.raw.Fence != after.raw.Fence ||
		before.raw.BootID != after.raw.BootID || before.Identity != after.Identity || before.MTU != after.MTU || before.connection != after.connection ||
		before.raw.NetworkFile != after.raw.NetworkFile || before.ManagementInterface != after.ManagementInterface {
		return networkError("HOST_NETWORK_RUNTIME_CHANGED", false)
	}
	result.RouteVerified = true
	return nil
}

func setTrafficProbe(probe *HostTrafficProbe, counters *PingCounters, err error) {
	probe.Counters = counters
	if err == nil {
		probe.State = TrafficProbePassed
		probe.ReasonCode = ""
		return
	}
	probe.ReasonCode = safeTrafficReason(err)
	probe.State = TrafficProbeUnknown
	if Code(err) == "HOST_NETWORK_TRAFFIC_PING_INCOMPLETE" {
		probe.State = TrafficProbeFailed
	}
}

func safeTrafficReason(err error) string {
	var hostErr *HostNetworkError
	if errors.As(err, &hostErr) {
		return hostErr.ReasonCode
	}
	var sshErr *Error
	if errors.As(err, &sshErr) {
		return sshErr.Code
	}
	if errors.Is(err, context.Canceled) {
		return "SSH_OBSERVATION_CANCELED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "SSH_OBSERVATION_TIMEOUT"
	}
	return "HOST_NETWORK_TRAFFIC_FAILED"
}

func safeTrafficRemoteError(err error, fallback string) error {
	var sshErr *Error
	if errors.As(err, &sshErr) {
		return sshErr
	}
	return networkError(fallback, false)
}

func clearHostObservation(observation *HostNetworkObservation) {
	clear(observation.merged)
	observation.merged = nil
	observation.raw = hostNetworkSnapshot{}
}

func (s *hostNetworkSession) checkTrafficRoute(ctx context.Context, request HostTrafficRequest) error {
	source := netip.MustParsePrefix(request.HostAddressPrefix).Addr()
	destination := request.Destination
	command := "/usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C /usr/sbin/ip -4 -j route get " + destination.String() + " from " + source.String()
	execCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := s.remote.Run(execCtx, "host-traffic-route", command)
	if err != nil {
		if Code(err) == "SSH_TIMEOUT_OR_CANCELLED" {
			return err
		}
		if ctx.Err() != nil {
			return networkError("HOST_NETWORK_TRAFFIC_CANCELED", false)
		}
		return safeTrafficRemoteError(err, "HOST_NETWORK_TRAFFIC_ROUTE_FAILED")
	}
	if ctx.Err() != nil {
		return networkError("HOST_NETWORK_TRAFFIC_CANCELED", false)
	}
	if execCtx.Err() != nil {
		return networkError("HOST_NETWORK_TRAFFIC_TIMEOUT", false)
	}
	data := []byte(output)
	defer clear(data)
	if len(data) == 0 || len(data) > trafficOutputLimit || !validNaturalTrafficRoute(data, request) {
		return networkError("HOST_NETWORK_TRAFFIC_ROUTE_MISMATCH", false)
	}
	return nil
}

func validNaturalTrafficRoute(data []byte, request HostTrafficRequest) bool {
	var rows []map[string]json.RawMessage
	if json.Unmarshal(data, &rows) != nil || len(rows) != 1 {
		return false
	}
	row := rows[0]
	allowed := map[string]bool{"dst": true, "from": true, "gateway": true, "dev": true, "flags": true, "uid": true, "cache": true, "table": true, "type": true}
	for key := range row {
		if !allowed[key] {
			return false
		}
	}
	for _, raw := range row {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return false
		}
	}
	read := func(key string) string { var value string; _ = json.Unmarshal(row[key], &value); return value }
	flags, flagsPresent := row["flags"]
	var routeFlags []string
	if !flagsPresent || bytes.Equal(bytes.TrimSpace(flags), []byte("null")) || json.Unmarshal(flags, &routeFlags) != nil || len(routeFlags) != 0 {
		return false
	}
	table := "main" // iproute2 6.1 omits the default table for main-table results.
	if raw, ok := row["table"]; ok {
		if len(raw) > 0 && raw[0] == '"' {
			if json.Unmarshal(raw, &table) != nil {
				return false
			}
		} else {
			var number int
			if json.Unmarshal(raw, &number) != nil || number != 254 {
				return false
			}
			table = "254"
		}
	}
	if table != "main" && table != "254" {
		return false
	}
	if raw, ok := row["uid"]; ok {
		var uid uint32
		if json.Unmarshal(raw, &uid) != nil {
			return false
		}
	}
	if raw, ok := row["cache"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return false
		}
		var cache []any
		if json.Unmarshal(raw, &cache) != nil || len(cache) != 0 {
			return false
		}
	}
	typ := "unicast" // iproute2 6.1 omits the default route type.
	if raw, ok := row["type"]; ok {
		if json.Unmarshal(raw, &typ) != nil || typ != "unicast" {
			return false
		}
	}
	if typ != "unicast" {
		return false
	}
	return read("dst") == request.Destination.String() && read("from") == netip.MustParsePrefix(request.HostAddressPrefix).Addr().String() &&
		read("gateway") == request.Gateway && read("dev") == request.Identity.InterfaceName
}

func (s *hostNetworkSession) pingHost(ctx context.Context, request HostTrafficRequest, size int, df bool) (*PingCounters, error) {
	source := netip.MustParsePrefix(request.HostAddressPrefix).Addr()
	mode := ""
	if df {
		mode = " -M do"
	}
	command := "/usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C /usr/bin/ping -4 -n -q -B -I " + source.String() + mode + " -s " + strconv.Itoa(size) +
		" -i " + strconv.FormatFloat(hbn.RuntimePingInterval.Seconds(), 'f', -1, 64) + " -c " + strconv.Itoa(hbn.RuntimePacketCount) + " -W 2 " + request.Destination.String()
	execCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	output, err := s.remote.Run(execCtx, "host-traffic-ping", command)
	runErr := err
	if runErr != nil && Code(runErr) == "SSH_TIMEOUT_OR_CANCELLED" {
		return nil, runErr
	}
	if ctx.Err() != nil {
		return nil, networkError("HOST_NETWORK_TRAFFIC_CANCELED", false)
	}
	if execCtx.Err() != nil && runErr == nil {
		return nil, networkError("HOST_NETWORK_TRAFFIC_TIMEOUT", false)
	}
	if runErr != nil && len(output) == 0 {
		return nil, safeTrafficRemoteError(runErr, "HOST_NETWORK_TRAFFIC_PING_FAILED")
	}
	data := []byte(output)
	defer clear(data)
	if len(data) == 0 || len(data) > trafficOutputLimit {
		return nil, networkError("HOST_NETWORK_TRAFFIC_RESPONSE_INVALID", false)
	}
	counters, parseErr := parsePingCounters(data)
	if parseErr != nil {
		return nil, parseErr
	}
	if runErr != nil {
		var sshErr *Error
		if errors.As(runErr, &sshErr) && sshErr.Code == "SSH_COMMAND_FAILED" && sshErr.ExitStatus != nil && *sshErr.ExitStatus == 1 && counters.Sent == hbn.RuntimePacketCount && (counters.Received != hbn.RuntimePacketCount || counters.LostPct != 0) {
			return counters, networkError("HOST_NETWORK_TRAFFIC_PING_INCOMPLETE", false)
		}
		return counters, safeTrafficRemoteError(runErr, "HOST_NETWORK_TRAFFIC_PING_FAILED")
	}
	if counters.Received != hbn.RuntimePacketCount || counters.LostPct != 0 {
		return counters, networkError("HOST_NETWORK_TRAFFIC_PING_INCOMPLETE", false)
	}
	return counters, nil
}

func parsePingCounters(data []byte) (*PingCounters, error) {
	matches := pingSummary.FindAllSubmatch(data, -1)
	if len(matches) != 1 || len(matches[0]) != 4 {
		return nil, networkError("HOST_NETWORK_TRAFFIC_RESPONSE_INVALID", false)
	}
	match := matches[0]
	sent, err := strconv.Atoi(string(match[1]))
	if err != nil {
		return nil, networkError("HOST_NETWORK_TRAFFIC_RESPONSE_INVALID", false)
	}
	received, err := strconv.Atoi(string(match[2]))
	if err != nil {
		return nil, networkError("HOST_NETWORK_TRAFFIC_RESPONSE_INVALID", false)
	}
	lost, err := strconv.ParseFloat(string(match[3]), 64)
	if err != nil || math.IsNaN(lost) || math.IsInf(lost, 0) {
		return nil, networkError("HOST_NETWORK_TRAFFIC_RESPONSE_INVALID", false)
	}
	if sent != hbn.RuntimePacketCount || received < 0 || received > sent || lost < 0 || lost > 100 {
		return nil, networkError("HOST_NETWORK_TRAFFIC_RESPONSE_INVALID", false)
	}
	expected := 100 * float64(sent-received) / float64(sent)
	if !lossWithinIPUtilsRounding(lost, expected) {
		return nil, networkError("HOST_NETWORK_TRAFFIC_RESPONSE_INVALID", false)
	}
	return &PingCounters{Sent: sent, Received: received, LostPct: lost}, nil
}

func lossWithinIPUtilsRounding(reported, expected float64) bool {
	if expected == 0 || expected == 100 {
		return reported == expected
	}
	// iputils prints this percentage with C %g's default six significant digits.
	exponent := math.Floor(math.Log10(expected))
	tolerance := 0.5*math.Pow(10, exponent-5) + 1e-12
	return math.Abs(reported-expected) <= tolerance
}
