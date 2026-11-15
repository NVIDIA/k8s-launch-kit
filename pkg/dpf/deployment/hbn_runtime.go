// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"sort"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbnruntime"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

const hbnRuntimeStage = "hbn-runtime-validation"

// HBNRuntimeResult retains individual measurements on every later failure.
// Fenced measurements are point-in-time observations, never a cohort pass.
type HBNRuntimeResult struct {
	State            string                 `json:"state"`
	ReasonCode       string                 `json:"reasonCode,omitempty"`
	TrafficValidated bool                   `json:"trafficValidated"`
	BGP              []HBNBGPResult         `json:"bgp,omitempty"`
	Routes           []HBNRouteResult       `json:"routes,omitempty"`
	Traffic          []HBNHostTrafficResult `json:"traffic,omitempty"`
}
type HBNBGPResult struct {
	DPUSerial    string `json:"dpuSerial"`
	HostInputRef string `json:"hostInputRef"`
	LocalASN     uint32 `json:"localASN"`
	FabricASN    uint32 `json:"fabricASN"`
	ObservedASN  uint32 `json:"observedASN,omitempty"`
	State        string `json:"state"`
	ReasonCode   string `json:"reasonCode,omitempty"`
	Fenced       bool   `json:"fenced"`
}
type HBNRouteResult struct {
	DPUSerial       string `json:"dpuSerial"`
	RemoteDPUSerial string `json:"remoteDpuSerial"`
	Prefix          string `json:"prefix"`
	State           string `json:"state"`
	ReasonCode      string `json:"reasonCode,omitempty"`
	Fenced          bool   `json:"fenced"`
}
type HBNHostTrafficResult struct {
	SourceHost           string                     `json:"sourceHost"`
	DestinationHost      string                     `json:"destinationHost"`
	SourceDPUSerial      string                     `json:"sourceDpuSerial"`
	DestinationDPUSerial string                     `json:"destinationDpuSerial"`
	Measurement          sshprobe.HostTrafficResult `json:"measurement"`
	Fenced               bool                       `json:"fenced"`
}

type hbnRuntimeRead func(context.Context, string, string, hbnRuntimeCommand, netip.Prefix) ([]byte, error)
type hbnHostTrafficProbe func(context.Context, config.Target, sshprobe.HostTrafficRequest, time.Duration) (sshprobe.HostTrafficResult, error)
type hbnRuntimeTransports struct {
	readLeases     hostLeaseSnapshotReader
	readCommand    hbnRuntimeRead
	probeTraffic   hbnHostTrafficProbe
	imageTransport http.RoundTripper
	resolveImage   func(context.Context, string, time.Duration, http.RoundTripper) (hbnPodImageIdentity, error)
}
type hbnRuntimeFence struct {
	allocation hostNetworkLeaseFence
	bindings   hbnRuntimeBindingSnapshot
}

// runHBNRuntimeStage uses one measurement at a time (concurrency bound 1).
// This keeps the complete-cohort fences and durable result ordering explicit;
// each SSH operation is itself finite and bounded by the shared deadline.
func runHBNRuntimeStage(ctx context.Context, client hbnRuntimeStageClient, prepared *preparedControlPlaneRun,
	controlPlane controlPlaneReceipts, workload workloadPreparationReceipt, activation activationStageReceipt, hosts []HostWorkloadNetworkResult,
	journal *executionJournal, timeout time.Duration, transports hbnRuntimeTransports,
) (result HBNRuntimeResult, stageErr error) {
	result.State = "incomplete"
	fail := func(code, message string) (HBNRuntimeResult, error) {
		if journal != nil && journal.failed != nil {
			code = "HBN_RUNTIME_REPORT_UNCERTAIN"
			message = "Runtime evidence could not be durably recorded; no cohort pass is retained."
		}
		result.State = "failed"
		result.ReasonCode = code
		result.TrafficValidated = false
		err := fmt.Errorf("%s: %s", code, message)
		if journal != nil {
			if journalErr := journal.record(executionEvent{Stage: hbnRuntimeStage, Operation: "validate", State: "failed", ReasonCode: code, Message: message}); journalErr != nil {
				return result, fmt.Errorf("%w: %w", err, journalErr)
			}
		}
		if ctx != nil {
			ui.FromContext(ctx).Info("[FAIL] HBN runtime: %s", message)
		}
		return result, err
	}
	if ctx == nil || client == nil || prepared == nil || prepared.execution == nil || prepared.execution.plan == nil || prepared.execution.reconciler == nil || journal == nil || controlPlane.tenant.Client == nil ||
		timeout < time.Second || timeout > 2*time.Minute || transports.readLeases == nil || transports.readCommand == nil || transports.probeTraffic == nil || len(activation.selected) == 0 || len(activation.selected) != len(hosts) || activation.BundleDigest != prepared.execution.plan.Digest() || journal.digest != activation.BundleDigest || workload.BundleDigest != activation.BundleDigest {
		return fail("HBN_RUNTIME_INPUT_INVALID", "Runtime checks require the retained plan, complete activation/host receipts and bounded transports.")
	}
	if _, ok := ctx.Deadline(); !ok {
		return fail("HBN_RUNTIME_DEADLINE_REQUIRED", "Runtime checks require an execution deadline.")
	}
	if err := ctx.Err(); err != nil {
		return fail("HBN_RUNTIME_CANCELED", "Runtime validation was canceled.")
	}
	if err := validateHostNetworkCohort(activation); err != nil {
		return fail("HBN_RUNTIME_COHORT_INVALID", "Runtime traffic requires one complete selected DPU binding per physical host.")
	}
	execution := prepared.execution
	desired := execution.plan.Intent()
	selected := append([]activationSelectedDevice(nil), activation.selected...)
	sort.Slice(selected, func(i, j int) bool { return selected[i].item.device.Serial < selected[j].item.device.Serial })
	asns := map[string]uint32{}
	for _, device := range desired.Devices {
		asns[device.Serial] = device.ASN
	}
	reference := desired.Generation.ImageRepository + ":" + desired.Generation.ImageTag + "@" + desired.Generation.ImageDigest
	ui.FromContext(ctx).Info("[RUNNING] HBN runtime: verify retained Linux/arm64 image identity")
	resolveImage := transports.resolveImage
	if resolveImage == nil {
		resolveImage = resolveHBNRuntimeImage
	}
	image, err := resolveImage(ctx, reference, timeout, transports.imageTransport)
	if err != nil || image.Reference != reference || !image.valid() {
		return fail("HBN_RUNTIME_IMAGE_IDENTITY_UNAVAILABLE", "The immutable HBN platform image identity could not be verified.")
	}
	allocationObserver, err := newHostNetworkFenceObserver(ctx, client, prepared, controlPlane, workload, activation, journal, timeout, transports.readLeases, hbnRuntimeStage, "HBN_RUNTIME")
	if err != nil {
		return fail("HBN_RUNTIME_PLAN_INVALID", "The retained activation/allocation plan is invalid.")
	}
	var anchor *hbnRuntimeFence
	observe := func() (hbnRuntimeFence, error) {
		if err := ctx.Err(); err != nil {
			return hbnRuntimeFence{}, err
		}
		before, err := allocationObserver()
		if err != nil {
			return hbnRuntimeFence{}, err
		}
		bindings, err := observeHBNRuntimeBindings(ctx, client, prepared, controlPlane.tenant.Client, activation, before, image, timeout)
		if err != nil {
			return hbnRuntimeFence{}, err
		}
		after, err := allocationObserver()
		if err != nil || !sameHostNetworkAllocation(before, after) {
			return hbnRuntimeFence{}, fmt.Errorf("HBN_RUNTIME_ALLOCATION_CHANGED")
		}
		current := hbnRuntimeFence{allocation: after, bindings: bindings}
		if anchor != nil && !sameHBNRuntimeFence(*anchor, current) {
			return current, fmt.Errorf("HBN_RUNTIME_COHORT_CHANGED")
		}
		if err := validateHBNRuntimeHostReceipts(activation, selected, current.allocation, hosts, desired.Networking.WorkloadMTU, desired.Networking.WorkloadCIDR); err != nil {
			return current, err
		}
		return current, nil
	}
	first, err := observe()
	if err != nil {
		return fail("HBN_RUNTIME_FENCE_UNAVAILABLE", "Fresh current HBN containers, permissions, host receipts and complete allocations could not be established.")
	}
	anchor = &first
	// All measurement attempts/results are durable before proceeding. A post-read
	// fence failure retains the measurement with Fenced=false and stops the stage.
	measure := func(operation, subject string, read func() error, event func() executionEvent, confirm func()) error {
		if _, err := observe(); err != nil {
			return fmt.Errorf("HBN_RUNTIME_PRE_FENCE_FAILED")
		}
		if err := journal.record(executionEvent{Stage: hbnRuntimeStage, Operation: operation, Subject: subject, State: "attempted", ReasonCode: "HBN_RUNTIME_MEASUREMENT_STARTED", Message: "Read the selected current runtime; no cohort pass has been established."}); err != nil {
			return err
		}
		readErr := read()
		local := event()
		local.Stage = hbnRuntimeStage
		local.Operation = operation
		local.Subject = subject
		local.State = "observed"
		local.Message = "Partial runtime measurement retained; its post-read cohort fence is not yet established."
		if err := journal.record(local); err != nil {
			return err
		}
		if readErr != nil {
			return readErr
		}
		if _, err := observe(); err != nil {
			return fmt.Errorf("HBN_RUNTIME_POST_FENCE_FAILED")
		}
		confirm()
		confirmed := event()
		confirmed.Stage = hbnRuntimeStage
		confirmed.Operation = operation
		confirmed.Subject = subject
		confirmed.State = "confirmed"
		confirmed.Message = "This runtime measurement and the complete selected cohort were reverified."
		return journal.record(confirmed)
	}
	leaseBySerial := map[string]hostWorkloadLease{}
	for _, lease := range first.allocation.leases {
		leaseBySerial[lease.Serial] = lease
	}
	for _, device := range selected {
		serial := device.item.device.Serial
		binding := first.bindings.anchors[serial]
		ui.FromContext(ctx).Info("[RUNNING] HBN %s @ %s: BGP AS%d → fabric AS%d on p0_if and p1_if", serial, device.host.InputRef, asns[serial], desired.Networking.FabricASN)
		var measurement *HBNBGPResult
		err = measure("bgp", serial, func() error {
			row := HBNBGPResult{DPUSerial: serial, HostInputRef: device.host.InputRef, LocalASN: asns[serial], FabricASN: desired.Networking.FabricASN, State: "unknown"}
			data, err := transports.readCommand(ctx, binding.PodNamespace, binding.PodName, hbnReadBGPSummary, netip.Prefix{})
			if err != nil {
				row.ReasonCode = "HBN_BGP_READ_UNAVAILABLE"
			} else {
				summary, parseErr := hbnruntime.Parse(data)
				if parseErr != nil {
					row.ReasonCode = "HBN_BGP_RESPONSE_INVALID"
				} else {
					row.ObservedASN = summary.ASN
					mismatches := hbnruntime.Validate(summary, hbnruntime.Expected{LocalASN: row.LocalASN, FabricASN: row.FabricASN})
					if len(mismatches) > 0 {
						row.State = "failed"
						row.ReasonCode = "HBN_BGP_" + mismatches[0].Reason
					} else {
						row.State = "passed"
					}
				}
			}
			result.BGP = append(result.BGP, row)
			measurement = &result.BGP[len(result.BGP)-1]
			if row.State != "passed" {
				return fmt.Errorf("%s", row.ReasonCode)
			}
			return nil
		}, func() executionEvent {
			return executionEvent{ReasonCode: runtimeMeasurementReason(measurement.State, measurement.ReasonCode), BGP: measurement}
		}, func() { measurement.Fenced = true })
		if err != nil {
			return fail(runtimeStageFailureCode(err), "BGP verification did not complete for the selected cohort; retain partial measurements.")
		}
		ui.FromContext(ctx).Info("[PASS] HBN %s: both BGP fabric sessions established with expected ASNs", serial)
		for _, remote := range selected {
			remoteSerial := remote.item.device.Serial
			if remoteSerial == serial {
				continue
			}
			prefix := netip.MustParsePrefix(leaseBySerial[remoteSerial].Prefix)
			var route *HBNRouteResult
			ui.FromContext(ctx).Info("[RUNNING] HBN %s: installed remote workload route %s for %s", serial, prefix.String(), remoteSerial)
			err = measure("route", serial+" → "+remoteSerial, func() error {
				row := HBNRouteResult{DPUSerial: serial, RemoteDPUSerial: remoteSerial, Prefix: prefix.String(), State: "unknown"}
				data, readErr := transports.readCommand(ctx, binding.PodNamespace, binding.PodName, hbnReadRemoteRoute, prefix)
				if readErr != nil {
					row.ReasonCode = "HBN_FIB_READ_UNAVAILABLE"
				} else {
					document, parseErr := hbnruntime.ParseFIBRoute(data)
					if parseErr != nil {
						row.ReasonCode = "HBN_FIB_RESPONSE_INVALID"
					} else {
						mismatches := hbnruntime.ValidateFIBRoute(document, hbnruntime.FIBExpectation{Prefix: prefix.String(), EgressInterfaces: []string{"p0_if", "p1_if"}})
						if len(mismatches) > 0 {
							row.State = "failed"
							row.ReasonCode = "HBN_FIB_" + mismatches[0].Reason
						} else {
							row.State = "passed"
						}
					}
				}
				result.Routes = append(result.Routes, row)
				route = &result.Routes[len(result.Routes)-1]
				if row.State != "passed" {
					return fmt.Errorf("%s", row.ReasonCode)
				}
				return nil
			}, func() executionEvent {
				return executionEvent{ReasonCode: runtimeMeasurementReason(route.State, route.ReasonCode), Route: route}
			}, func() { route.Fenced = true })
			if err != nil {
				return fail(runtimeStageFailureCode(err), "An exact remote workload prefix was not verified in HBN's installed BGP FIB.")
			}
			ui.FromContext(ctx).Info("[PASS] HBN %s: remote prefix %s installed through allowed uplinks", serial, prefix.String())
		}
	}
	for _, source := range selected {
		serial := source.item.device.Serial
		identity, err := sshprobe.SelectHostWorkloadNetworkIdentity(source.host, source.adapter.Key, source.adapter.SerialNumber)
		if err != nil {
			return fail("HBN_RUNTIME_HOST_IDENTITY_UNAVAILABLE", "The retained host PF identity is unavailable.")
		}
		request, err := hostNetworkRequestForLease(activation, identity, first.allocation, serial, desired.Networking.WorkloadMTU, desired.Networking.WorkloadCIDR)
		if err != nil {
			return fail("HBN_RUNTIME_HOST_LEASE_INVALID", "The current host allocation could not produce an exact traffic request.")
		}
		for _, destination := range selected {
			remoteSerial := destination.item.device.Serial
			if serial == remoteSerial {
				continue
			}
			trafficRequest := sshprobe.HostTrafficRequest{HostNetworkRequest: request, Destination: netip.MustParseAddr(leaseBySerial[remoteSerial].ReservedHostAddress)}
			var traffic *HBNHostTrafficResult
			ui.FromContext(ctx).Info("[RUNNING] Host %s → %s: natural workload route, ordinary ping and DF ping (%d-byte payload)", source.host.InputRef, destination.host.InputRef, request.MTU-28)
			err = measure("traffic", source.host.InputRef+" → "+destination.host.InputRef, func() error {
				measurement, probeErr := transports.probeTraffic(ctx, source.hostTarget, trafficRequest, timeout)
				result.Traffic = append(result.Traffic, HBNHostTrafficResult{SourceHost: source.host.InputRef, DestinationHost: destination.host.InputRef, SourceDPUSerial: serial, DestinationDPUSerial: remoteSerial, Measurement: measurement})
				traffic = &result.Traffic[len(result.Traffic)-1]
				if probeErr != nil || !validHBNRuntimeTraffic(measurement, trafficRequest) {
					return fmt.Errorf("HBN_RUNTIME_TRAFFIC_NOT_VERIFIED")
				}
				return nil
			}, func() executionEvent {
				return executionEvent{ReasonCode: "HBN_RUNTIME_TRAFFIC_RESULT", Traffic: traffic}
			}, func() { traffic.Fenced = true })
			if err != nil {
				return fail(runtimeStageFailureCode(err), "A directed host ordinary/DF traffic check did not complete; a partial pair pass is not a cohort pass.")
			}
			ui.FromContext(ctx).Info("[PASS] Host %s → %s: natural route and both 3/3 packet probes verified", source.host.InputRef, destination.host.InputRef)
		}
	}
	if _, err := observe(); err != nil {
		return fail("HBN_RUNTIME_FINAL_FENCE_FAILED", "The complete final activation, allocation or runtime identity observation failed.")
	}
	if len(selected) < 2 {
		result.State = "unmeasured"
		result.ReasonCode = "HBN_RUNTIME_CROSS_HOST_UNMEASURED"
		if err := journal.record(executionEvent{Stage: hbnRuntimeStage, Operation: "complete", State: "unmeasured", ReasonCode: result.ReasonCode, Message: "The selected single-host BGP check passed; cross-host routes and traffic cannot be measured with fewer than two hosts."}); err != nil {
			return fail("HBN_RUNTIME_REPORT_UNCERTAIN", "The single-host runtime result was not durably recorded.")
		}
		ui.FromContext(ctx).Info("[UNMEASURED] Cross-host traffic requires at least two selected physical hosts")
		return result, nil
	}
	if err := journal.record(executionEvent{Stage: hbnRuntimeStage, Operation: "complete", State: "passed", ReasonCode: "HBN_RUNTIME_COHORT_VALIDATED", Message: "Every selected DPU BGP check, exact directed remote-prefix FIB check and directed host ordinary/DF probe passed with final full-cohort fences."}); err != nil {
		return fail("HBN_RUNTIME_REPORT_UNCERTAIN", "The cohort runtime pass was not durably recorded.")
	}
	if ctx.Err() != nil {
		return fail("HBN_RUNTIME_CANCELED", "Runtime validation was canceled before its final result.")
	}
	result.State = "passed"
	result.TrafficValidated = true
	ui.FromContext(ctx).Info("[PASS] HBN runtime: %d DPUs, %d remote routes and all %d directed host pairs validated", len(result.BGP), len(result.Routes), len(result.Traffic))
	return result, nil
}

func runtimeStageFailureCode(err error) string {
	if err != nil && safeWorkloadReasonCode(err.Error()) {
		return err.Error()
	}
	return "HBN_RUNTIME_MEASUREMENT_INCOMPLETE"
}
func runtimeMeasurementReason(state, reason string) string {
	if reason != "" {
		return reason
	}
	if state == "passed" {
		return "HBN_RUNTIME_MEASUREMENT_PASSED"
	}
	return "HBN_RUNTIME_MEASUREMENT_UNKNOWN"
}
func sameHBNRuntimeFence(a, b hbnRuntimeFence) bool {
	if !maps.Equal(a.bindings.sourceDigests, b.bindings.sourceDigests) || !sameHostNetworkAllocation(a.allocation, b.allocation) || len(a.bindings.anchors) != len(b.bindings.anchors) || len(a.bindings.anchors) == 0 {
		return false
	}
	for serial, anchor := range a.bindings.anchors {
		other, found := b.bindings.anchors[serial]
		if !found || !sameHBNPodBinding(anchor, other) {
			return false
		}
	}
	return true
}
func validateHBNRuntimeHostReceipts(activation activationStageReceipt, selected []activationSelectedDevice, fence hostNetworkLeaseFence, hosts []HostWorkloadNetworkResult, mtu int, cidr string) error {
	if len(selected) != len(hosts) || len(fence.leases) != len(selected) {
		return fmt.Errorf("HBN_RUNTIME_HOST_RECEIPT_INVALID")
	}
	seen := map[string]bool{}
	for _, device := range selected {
		serial := device.item.device.Serial
		var receipt *HostWorkloadNetworkResult
		for i := range hosts {
			if hosts[i].DPUSerial == serial {
				if receipt != nil {
					return fmt.Errorf("HBN_RUNTIME_HOST_RECEIPT_INVALID")
				}
				receipt = &hosts[i]
			}
		}
		identity, err := sshprobe.SelectHostWorkloadNetworkIdentity(device.host, device.adapter.Key, device.adapter.SerialNumber)
		if err != nil {
			return err
		}
		request, err := hostNetworkRequestForLease(activation, identity, fence, serial, mtu, cidr)
		if err != nil {
			return err
		}
		if seen[device.host.ID] || receipt == nil || !receipt.LeaseReverified || receipt.HostInputRef != device.host.InputRef || receipt.Interface != request.Identity.InterfaceName || receipt.HostAddressPrefix != request.HostAddressPrefix || receipt.Gateway != request.Gateway || receipt.MTU != mtu ||
			receipt.Configuration.RequestID != hbnBindingDigest(request) || receipt.Configuration.Blocking || !receipt.Configuration.RuntimeVerified || !receipt.Configuration.FileVerified || receipt.Configuration.State != "complete" && receipt.Configuration.State != "verified-external" {
			return fmt.Errorf("HBN_RUNTIME_HOST_RECEIPT_INVALID")
		}
		seen[device.host.ID] = true
	}
	return nil
}
func validHBNRuntimeTraffic(result sshprobe.HostTrafficResult, request sshprobe.HostTrafficRequest) bool {
	host, err := netip.ParsePrefix(request.HostAddressPrefix)
	if err != nil {
		return false
	}
	if result.State != sshprobe.TrafficProbePassed || !result.RouteVerified || result.ExpectedSource != host.Addr().String() || result.ExpectedDestination != request.Destination.String() || result.ExpectedInterface != request.Identity.InterfaceName || result.ExpectedGateway != request.Gateway || result.MTU != request.MTU || result.Ordinary.PayloadBytes != 56 || result.MTUProbe.PayloadBytes != request.MTU-28 {
		return false
	}
	for _, probe := range []sshprobe.HostTrafficProbe{result.Ordinary, result.MTUProbe} {
		if probe.State != sshprobe.TrafficProbePassed || probe.Counters == nil || probe.Counters.Sent != 3 || probe.Counters.Received != 3 || probe.Counters.LostPct != 0 {
			return false
		}
	}
	return true
}
