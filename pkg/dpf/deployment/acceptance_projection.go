// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"net/netip"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
)

// projectHBNRuntime preserves the legacy measurement view without executing a
// second runtime matrix. Validation is authoritative; invalidated observations
// are retained as prior, unfenced measurements rather than accepted passes.
func projectHBNRuntime(result validation.Result, plan *generation.Plan, hosts []HostWorkloadNetworkResult) HBNRuntimeResult {
	projected := HBNRuntimeResult{State: "failed", TrafficValidated: result.Verdict == validation.VerdictPassed && result.ExitCode() == 0}
	if projected.TrafficValidated {
		projected.State = "passed"
	} else if onlySingleHostCoverageIncomplete(result) {
		projected.State, projected.ReasonCode = "unmeasured", "HBN_RUNTIME_CROSS_HOST_UNMEASURED"
	}
	rows := make(map[validation.CheckID]validation.CheckResult, len(result.Checks))
	for _, row := range result.Checks {
		rows[row.ID] = row
	}
	devices := make(map[string]HostWorkloadNetworkResult, len(hosts))
	for _, host := range hosts {
		devices[host.DPUSerial] = host
	}
	measurement := func(row validation.CheckResult) (validation.Outcome, bool) {
		if row.PriorOutcome != nil {
			return *row.PriorOutcome, false
		}
		return row.Outcome, row.Outcome.State == validation.Passed || row.Outcome.State == validation.Failed
	}
	intent := plan.Intent()
	for _, device := range intent.Devices {
		row, ok := rows[validation.BGPCheckID(validation.DeviceID(device.Serial))]
		if !ok {
			continue
		}
		observed, fenced := measurement(row)
		if observed.BGP == nil {
			continue
		}
		projected.BGP = append(projected.BGP, HBNBGPResult{DPUSerial: device.Serial, HostInputRef: devices[device.Serial].HostInputRef, LocalASN: device.ASN, FabricASN: intent.Networking.FabricASN, ObservedASN: observed.BGP.LocalASN, State: string(observed.State), ReasonCode: string(observed.Reason), Fenced: fenced})
	}
	for _, pair := range result.Scope.Routes {
		row := rows[validation.RouteCheckID(pair)]
		observed, fenced := measurement(row)
		if observed.Route == nil {
			continue
		}
		projected.Routes = append(projected.Routes, HBNRouteResult{DPUSerial: string(pair.Source), RemoteDPUSerial: string(pair.Destination), Prefix: observed.Route.Prefix.String(), State: string(observed.State), ReasonCode: string(observed.Reason), Fenced: fenced})
	}
	probe := func(outcome validation.Outcome) sshprobe.HostTrafficProbe {
		value := sshprobe.HostTrafficProbe{State: sshprobe.TrafficProbeUnknown, ReasonCode: string(outcome.Reason)}
		switch outcome.State {
		case validation.Passed:
			value.State = sshprobe.TrafficProbePassed
		case validation.Failed:
			value.State = sshprobe.TrafficProbeFailed
		}
		if facts := outcome.Traffic; facts != nil {
			value.PayloadBytes = int(facts.PayloadBytes)
			value.Counters = &sshprobe.PingCounters{Sent: int(facts.Transmitted), Received: int(facts.Received)}
			if facts.Transmitted > 0 {
				value.Counters.LostPct = 100 * (float64(facts.Transmitted) - float64(facts.Received)) / float64(facts.Transmitted)
			}
		}
		return value
	}
	for _, pair := range result.Scope.Traffic {
		ordinary, ordinaryFenced := measurement(rows[validation.TrafficCheckID(pair, false)])
		df, dfFenced := measurement(rows[validation.TrafficCheckID(pair, true)])
		if ordinary.Traffic == nil && df.Traffic == nil {
			continue
		}
		source, destination := devices[string(pair.Source)], devices[string(pair.Destination)]
		sourceAddress, _ := netip.ParsePrefix(source.HostAddressPrefix)
		destinationAddress, _ := netip.ParsePrefix(destination.HostAddressPrefix)
		value := sshprobe.HostTrafficResult{State: sshprobe.TrafficProbeUnknown, MTU: source.MTU, ExpectedInterface: source.Interface, ExpectedGateway: source.Gateway, RouteVerified: ordinary.State == validation.Passed || df.State == validation.Passed, Ordinary: probe(ordinary), MTUProbe: probe(df)}
		if sourceAddress.IsValid() {
			value.ExpectedSource = sourceAddress.Addr().String()
		}
		if destinationAddress.IsValid() {
			value.ExpectedDestination = destinationAddress.Addr().String()
		}
		if ordinary.State == validation.Passed && df.State == validation.Passed {
			value.State = sshprobe.TrafficProbePassed
		} else if ordinary.State == validation.Failed || df.State == validation.Failed {
			value.State = sshprobe.TrafficProbeFailed
		}
		projected.Traffic = append(projected.Traffic, HBNHostTrafficResult{SourceHost: source.HostInputRef, DestinationHost: destination.HostInputRef, SourceDPUSerial: string(pair.Source), DestinationDPUSerial: string(pair.Destination), Measurement: value, Fenced: ordinaryFenced && dfFenced})
	}
	return projected
}
