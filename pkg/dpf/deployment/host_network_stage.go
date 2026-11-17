// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"sort"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

const hostWorkloadNetworkStage = "host-workload-network"

// HostWorkloadNetworkResult retains a host-local transaction result even when
// a later Kubernetes fence or another selected host fails. It contains no SSH
// access, raw configuration or claim of BGP/traffic readiness.
type HostWorkloadNetworkResult struct {
	DPUSerial         string                      `json:"dpuSerial"`
	HostInputRef      string                      `json:"hostInputRef"`
	Interface         string                      `json:"interface"`
	HostAddressPrefix string                      `json:"hostAddressPrefix"`
	Gateway           string                      `json:"gateway"`
	MTU               int                         `json:"mtu"`
	Configuration     sshprobe.HostNetworkReceipt `json:"configuration"`
	LeaseReverified   bool                        `json:"leaseReverified"`
}

// These seams isolate read-only API transport and one host-local effect. The
// stage still executes its real cohort validation and pure lease resolver.
type hostLeaseSnapshotReader func(context.Context) (hostWorkloadLeaseSnapshot, error)
type hostNetworkConfigurer func(context.Context, config.Target, sshprobe.HostNetworkRequest, time.Duration) (sshprobe.HostNetworkReceipt, error)

type hostNetworkLeaseFence struct {
	snapshot   hostWorkloadLeaseSnapshot
	leases     []hostWorkloadLease
	activation activationObservation
}

// runHostWorkloadNetworkStage consumes successful activation bindings without
// resolving a second SSH authority. Each effect gets fresh API fences; neither
// those reads nor the host-local journal make the two systems atomic.
func runHostWorkloadNetworkStage(ctx context.Context, client activationBoundaryReader, prepared *preparedControlPlaneRun,
	controlPlane controlPlaneReceipts, workload workloadPreparationReceipt, activation activationStageReceipt,
	journal *executionJournal, requestTimeout time.Duration, readLeases hostLeaseSnapshotReader, configure, inspect hostNetworkConfigurer,
) ([]HostWorkloadNetworkResult, error) {
	var results []HostWorkloadNetworkResult
	fail := func(code, message string, cause error) ([]HostWorkloadNetworkResult, error) {
		err := fmt.Errorf("%s: %s", code, message)
		if cause != nil {
			err = errors.Join(err, cause)
		}
		if journal != nil {
			err = errors.Join(err, journal.record(executionEvent{Stage: hostWorkloadNetworkStage, Operation: "configure", State: "failed", ReasonCode: code, Message: message}))
		}
		return results, err
	}
	if ctx == nil || client == nil || prepared == nil || prepared.execution == nil || prepared.execution.plan == nil ||
		prepared.execution.reconciler == nil || controlPlane.tenant.Client == nil || journal == nil || readLeases == nil || configure == nil ||
		requestTimeout < time.Second || requestTimeout > 2*time.Minute || len(activation.selected) == 0 || len(activation.groups) == 0 || activation.DeploymentUID == "" {
		return fail("HOST_NETWORK_STAGE_INPUT_INVALID", "Host networking requires the retained execution, completed activation cohort and bounded transports.", nil)
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return fail("HOST_NETWORK_STAGE_DEADLINE_REQUIRED", "Host networking requires an execution deadline.", nil)
	}
	execution := prepared.execution
	if activation.BundleDigest != execution.plan.Digest() || journal.digest != activation.BundleDigest || workload.BundleDigest != activation.BundleDigest {
		return fail("HOST_NETWORK_STAGE_RECEIPT_MISMATCH", "Host networking receipts do not identify the retained plan.", nil)
	}
	// This profile installs one workload-pool route per host. Equal-prefix paths
	// through several DPUs need an explicit multipath policy, never list order.
	seenIndexes := make(map[int]bool, len(activation.selected))
	for hostID, indexes := range activation.groups {
		if len(indexes) != 1 {
			return fail("HOST_NETWORK_MULTIPLE_DPUS_UNSUPPORTED", "The flat host workload route requires one selected DPU per physical host.", nil)
		}
		index := indexes[0]
		if index < 0 || index >= len(activation.selected) || seenIndexes[index] || activation.selected[index].host.ID != hostID {
			return fail("HOST_NETWORK_STAGE_COHORT_INVALID", "The activated host groups do not identify each selected DPU exactly once.", nil)
		}
		seenIndexes[index] = true
	}
	if len(seenIndexes) != len(activation.selected) {
		return fail("HOST_NETWORK_STAGE_COHORT_INVALID", "The activated host groups omit a selected DPU.", nil)
	}
	selected := append([]activationSelectedDevice(nil), activation.selected...)
	sort.Slice(selected, func(i, j int) bool { return selected[i].item.device.Serial < selected[j].item.device.Serial })
	identities := make(map[string]sshprobe.HostNetworkIdentity, len(selected))
	for _, item := range selected {
		identity, err := sshprobe.SelectHostWorkloadNetworkIdentity(item.host, item.adapter.Key, item.adapter.SerialNumber)
		if err != nil {
			return fail("HOST_NETWORK_PF_IDENTITY_UNAVAILABLE", "A selected host has no unique physical p0, PCI and permanent-MAC binding.", err)
		}
		if _, duplicate := identities[item.item.device.Serial]; duplicate {
			return fail("HOST_NETWORK_STAGE_COHORT_INVALID", "The activated host cohort contains duplicate DPU identities.", nil)
		}
		identities[item.item.device.Serial] = identity
	}
	observe, err := newHostNetworkFenceObserver(ctx, client, prepared, controlPlane, workload, activation, journal, requestTimeout, readLeases, hostWorkloadNetworkStage, "HOST_NETWORK")
	if err != nil {
		return fail("HOST_NETWORK_WORKLOAD_PLAN_INVALID", "The retained workload or deployment plan is invalid.", err)
	}
	var anchor *hostNetworkLeaseFence
	for _, item := range selected {
		if err := ctx.Err(); err != nil {
			return fail("HOST_NETWORK_STAGE_CANCELED", "Host networking stopped before the next selected host.", err)
		}
		before, err := observe()
		if err != nil {
			return fail("HOST_NETWORK_LEASE_UNAVAILABLE", "Fresh activation and tenant workload allocations could not be established before a host change.", err)
		}
		if anchor != nil && !sameHostNetworkAllocation(*anchor, before) {
			return fail("HOST_NETWORK_COHORT_ALLOCATION_CHANGED", "The selected allocation cohort changed after an earlier host was configured; inspect existing host results.", nil)
		}
		if anchor == nil {
			anchor = &before
		}
		request, err := hostNetworkRequestForLease(activation, identities[item.item.device.Serial], before, item.item.device.Serial, execution.plan.Intent().Networking.WorkloadMTU, execution.plan.Intent().Networking.WorkloadCIDR)
		if err != nil {
			return fail("HOST_NETWORK_LEASE_REQUEST_INVALID", "The selected allocation cannot produce one exact reserved host request.", err)
		}
		subject := item.item.device.Serial + " @ " + item.host.InputRef
		apply := configure
		action, reason, message := "configure", "HOST_NETWORK_CONFIGURATION_STARTED", "Configure only the selected p0 MTU, reserved workload address and workload-pool route; preserve management and other ports."
		if prepared.operation != nil {
			prior, known := prepared.operation.state.Hosts[item.item.device.Serial]
			if known {
				if !sameOperationRequest(prior.Request, request) {
					return fail("HOST_NETWORK_OPERATION_CHANGED", "Saved host request differs from fresh cohort; no host write permitted.", nil)
				}
				if inspect == nil {
					return fail("HOST_NETWORK_INSPECTION_REQUIRED", "Recovery requires inspect-only host transport.", nil)
				}
				apply = inspect
				action, reason, message = "inspect", "HOST_NETWORK_INSPECTION_STARTED", "Inspect the original host transaction without configuration fallback; reverify current persistent/runtime state and allocation."
			} else {
				prepared.operation.state.Hosts[item.item.device.Serial] = operationHost{Request: request}
				if err := prepared.operation.save(ctx); err != nil {
					return results, err
				}
			}
		}
		if err := journal.record(executionEvent{Stage: hostWorkloadNetworkStage, Operation: action, Subject: subject, State: "attempted", ReasonCode: reason, Message: message}); err != nil {
			return results, err
		}
		ui.FromContext(ctx).Info("[RUNNING] Host %s: %s %s address %s and workload route with MTU %d", item.host.InputRef, action, request.Identity.InterfaceName, request.HostAddressPrefix, request.MTU)
		receipt, err := apply(ctx, item.hostTarget, request, requestTimeout)
		results = append(results, HostWorkloadNetworkResult{DPUSerial: item.item.device.Serial, HostInputRef: item.host.InputRef, Interface: request.Identity.InterfaceName,
			HostAddressPrefix: request.HostAddressPrefix, Gateway: request.Gateway, MTU: request.MTU, Configuration: receipt})
		localResult := results[len(results)-1]
		if journalErr := journal.record(executionEvent{Stage: hostWorkloadNetworkStage, Operation: "host-local-result", Subject: subject, State: "observed",
			ReasonCode: "HOST_NETWORK_LOCAL_RESULT", Message: "Host-local transaction result retained; its post-change API fence is not yet established.", HostNetwork: &localResult}); journalErr != nil {
			return results, journalErr
		}
		if err != nil {
			code := "HOST_NETWORK_CONFIGURATION_FAILED"
			var failure *sshprobe.HostNetworkError
			if errors.As(err, &failure) && safeWorkloadReasonCode(failure.ReasonCode) {
				code = failure.ReasonCode
			}
			return fail(code, "The host transaction did not complete; retain its receipt and inspect actual state before retrying.", err)
		}
		if receipt.Blocking || receipt.State != "complete" && receipt.State != "verified-external" || !receipt.RuntimeVerified || !receipt.FileVerified {
			return fail("HOST_NETWORK_RECEIPT_INCOMPLETE", "The host did not return a definitive verified runtime and persistent-file result.", nil)
		}
		after, err := observe()
		if err != nil || !sameHostNetworkAllocation(before, after) || !sameHostLeasePoolObservation(before.snapshot.CIDRPool, after.snapshot.CIDRPool) {
			return fail("HOST_NETWORK_POST_FENCE_UNCERTAIN", "The post-change activation or allocation fence failed; preserve the host receipt and stop before another host.", err)
		}
		results[len(results)-1].LeaseReverified = true
		confirmedResult := results[len(results)-1]
		if prepared.operation != nil && prepared.operation.state.Hosts[item.item.device.Serial].Result == nil {
			value := prepared.operation.state.Hosts[item.item.device.Serial]
			value.Result = &confirmedResult
			prepared.operation.state.Hosts[item.item.device.Serial] = value
			if err := prepared.operation.save(ctx); err != nil {
				return results, err
			}
		}
		if err := journal.record(executionEvent{Stage: hostWorkloadNetworkStage, Operation: action, Subject: subject, State: "confirmed",
			ReasonCode: "HOST_NETWORK_CONFIGURATION_VERIFIED", Message: "Host runtime and persistent file checks passed and the selected allocation cohort was reverified; BGP and traffic remain untested.", HostNetwork: &confirmedResult}); err != nil {
			return results, err
		}
		ui.FromContext(ctx).Info("[PASS] Host %s: selected %s configuration and fresh DPF allocation verified", item.host.InputRef, request.Identity.InterfaceName)
	}
	return results, nil
}

func sameHostNetworkAllocation(first, second hostNetworkLeaseFence) bool {
	a, b := first.snapshot.DPUServiceIPAM, second.snapshot.DPUServiceIPAM
	p, q := first.snapshot.CIDRPool, second.snapshot.CIDRPool
	return a != nil && b != nil && p != nil && q != nil && a.GetUID() == b.GetUID() && a.GetGeneration() == b.GetGeneration() &&
		p.GetUID() == q.GetUID() && p.GetGeneration() == q.GetGeneration() && reflect.DeepEqual(a.Object["spec"], b.Object["spec"]) &&
		reflect.DeepEqual(p.Object["spec"], q.Object["spec"]) && reflect.DeepEqual(first.leases, second.leases)
}

func hostNetworkRequestForLease(activation activationStageReceipt, identity sshprobe.HostNetworkIdentity, fence hostNetworkLeaseFence, serial string, mtu int, workloadCIDR string) (sshprobe.HostNetworkRequest, error) {
	var selected *hostWorkloadLease
	for i := range fence.leases {
		if fence.leases[i].Serial == serial {
			if selected != nil {
				return sshprobe.HostNetworkRequest{}, fmt.Errorf("selected host allocation is ambiguous")
			}
			selected = &fence.leases[i]
		}
	}
	if selected == nil || fence.snapshot.DPUServiceIPAM == nil || fence.snapshot.CIDRPool == nil || identity.AdapterSerial != serial {
		return sshprobe.HostNetworkRequest{}, fmt.Errorf("selected host allocation or identity is unavailable")
	}
	prefix, err := netip.ParsePrefix(selected.Prefix)
	address, addressErr := netip.ParseAddr(selected.ReservedHostAddress)
	if err != nil || addressErr != nil || !prefix.Contains(address) {
		return sshprobe.HostNetworkRequest{}, fmt.Errorf("selected reserved address is outside its allocation")
	}
	request := sshprobe.HostNetworkRequest{Identity: identity, MTU: mtu, HostAddressPrefix: netip.PrefixFrom(address, prefix.Bits()).String(), Gateway: selected.Gateway, WorkloadCIDR: workloadCIDR}
	// Resource versions fence each individual read/effect/read sequence below.
	// The durable operation key uses semantic identity, so an unrelated status
	// refresh cannot turn an exact completed repeat into a different transaction.
	request.LeaseBinding = inventory.Digest("dpf-hbn-host-network-v1", struct {
		BundleDigest   string
		DeploymentUID  string
		IPAMUID        string
		IPAMGeneration int64
		IPAMSpec       any
		PoolUID        string
		PoolGeneration int64
		PoolSpec       any
		Leases         []hostWorkloadLease
		Request        sshprobe.HostNetworkRequest
	}{BundleDigest: activation.BundleDigest, DeploymentUID: string(activation.DeploymentUID),
		IPAMUID: string(fence.snapshot.DPUServiceIPAM.GetUID()), IPAMGeneration: fence.snapshot.DPUServiceIPAM.GetGeneration(), IPAMSpec: fence.snapshot.DPUServiceIPAM.Object["spec"],
		PoolUID: string(fence.snapshot.CIDRPool.GetUID()), PoolGeneration: fence.snapshot.CIDRPool.GetGeneration(), PoolSpec: fence.snapshot.CIDRPool.Object["spec"],
		Leases: fence.leases, Request: request})
	return request, nil
}
