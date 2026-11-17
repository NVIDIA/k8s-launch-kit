// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// observeOwnedResumeAllocations permits partial provisioning: an allocation
// without a current ready, exactly owned selected Node grants no allowance.
// Both pools retain original workload receipts and are reread around Node reads.
func observeOwnedResumeAllocations(ctx context.Context, client activationObservationReader, prepared *preparedControlPlaneRun, control controlPlaneReceipts, workload workloadPreparationReceipt, proof ownedResumePrerequisites, timeout time.Duration) ([]discovery.ResumeRouteAllocation, error) {
	poolNames := []string{"workload", "loopback"}
	observeCtx, cancel := context.WithTimeout(ctx, time.Duration(len(poolNames))*timeout)
	defer cancel()
	expected, _, err := tenantPlanInputs(prepared.execution.plan)
	if err != nil {
		return nil, err
	}
	objects, err := retainedWorkloadObjects(prepared.execution.verificationInputs())
	if err != nil {
		return nil, err
	}
	allocations := []discovery.ResumeRouteAllocation{}
	type poolAnchor struct{ parent, pool *unstructured.Unstructured }
	anchors := []poolAnchor{}
	for _, poolName := range poolNames {
		var desired *unstructured.Unstructured
		for _, object := range objects {
			if object.GetKind() == "DPUServiceIPAM" && object.GetName() == "hbn-"+poolName {
				if desired != nil {
					return nil, fmt.Errorf("OWNED_RESUME_IPAM_AMBIGUOUS")
				}
				desired = object
			}
		}
		if desired == nil {
			return nil, fmt.Errorf("OWNED_RESUME_IPAM_MISSING")
		}
		receipt, uid, err := hostLeaseWorkloadReceipt(workload.Resources, desired)
		if err != nil {
			return nil, err
		}
		parent, err := observeHostLeaseIPAM(observeCtx, client, prepared.execution.reconciler, desired, receipt)
		if err != nil {
			return nil, err
		}
		pool, err := observeHostLeasePool(observeCtx, control.tenant.Client, parent, expected, uid)
		if err != nil {
			return nil, err
		}
		nodes, err := control.tenant.Client.listCompleteNodes(observeCtx)
		if err != nil {
			return nil, err
		}
		rows, err := resumeAllocationPrefixes(parent, pool)
		if err != nil {
			return nil, err
		}

		if poolName == "workload" && len(rows) > 0 {
			network, size, gateway, err := retainedWorkloadNetwork(parent)
			if err != nil {
				return nil, err
			}
			if _, err = hostLeaseAllocations(pool, network, size, gateway); err != nil {
				return nil, err
			}
		}
		for _, item := range proof.selected {
			node, err := uniqueTenantNode(prepared.execution.rules, nodes, item.nodeName, item.dpuSet.GetNamespace())
			if err != nil {
				continue
			} // No current selected Node means no positive allowance.
			prefix, found := rows[node.GetName()]
			if !found {
				continue
			}
			dpu, set, physicalNode := uniqueObjectByName(proof.scope.objects("DPU"), item.dpuName), uniqueObjectByName(proof.scope.objects("DPUSet"), item.dpuSet.GetName()), uniqueObjectByName(proof.scope.objects("DPUNode"), item.nodeName)
			if dpu == nil || set == nil || physicalNode == nil || physicalNode.GetUID() != item.receipt.DPUNodeUID || !activationDPUMatches(prepared.execution.rules, dpu, item) || !activationDPUSetMatches(prepared.execution.rules, set, item) || !activationOwnerMatches(dpu, set.GetAPIVersion(), set.GetKind(), set.GetName(), set.GetUID()) || !prepared.execution.rules.EnrolledNodeReadiness(physicalNode).Ready {
				continue
			}

			clusterName, _, nameErr := unstructured.NestedString(dpu.Object, "spec", "cluster", "name")
			clusterNamespace, _, namespaceErr := unstructured.NestedString(dpu.Object, "spec", "cluster", "namespace")
			if nameErr != nil || namespaceErr != nil || clusterName != expected.Name || clusterNamespace != expected.Namespace {
				continue
			}
			allocations = append(allocations, discovery.ResumeRouteAllocation{DPUSerial: item.item.device.Serial, Pool: poolName, Prefix: prefix})
		}
		after, err := observeHostLeasePool(observeCtx, control.tenant.Client, parent, expected, uid)
		if err != nil || !sameHostLeasePoolObservation(pool, after) {
			return nil, fmt.Errorf("OWNED_RESUME_ALLOCATION_CHANGED: pool changed during complete Node read")
		}
		if _, err = observeHostLeaseIPAM(observeCtx, client, prepared.execution.reconciler, desired, receipt); err != nil {
			return nil, err
		}
		anchors = append(anchors, poolAnchor{parent: parent, pool: after})
		if poolName == "workload" {
			hostRequests := prepared.operation.hostRequests()
			if len(hostRequests) == 0 {
				continue
			}
			deployment, err := retainedDeploymentObject(prepared.execution.verificationInputs())
			if err != nil {
				return nil, err
			}
			observed, err := observeActivationScope(observeCtx, client, prepared.execution.verificationInputs(), deployment, proof.parentUID, proof.selected, prepared.operation.restoreProgress())
			if err != nil || !observed.complete {
				return nil, fmt.Errorf("OWNED_RESUME_HOST_ALLOCATION_UNVERIFIED: recorded host transaction requires complete current activation")
			}
			snapshot := hostWorkloadLeaseSnapshot{DPUServiceIPAM: parent, CIDRPool: after, TenantNodes: nodes}
			leases, err := resolveHostWorkloadLeases(prepared.execution.plan, prepared.execution.reconciler, proof.selected, observed, workload.BundleDigest, workload.Resources, snapshot)
			if err != nil {
				return nil, err
			}
			for serial, prior := range hostRequests {
				current, err := hostNetworkRequestForLease(activationStageReceipt{BundleDigest: prepared.execution.plan.Digest(), DeploymentUID: proof.parentUID}, prior.Identity, hostNetworkLeaseFence{snapshot: snapshot, leases: leases}, serial, prepared.execution.plan.Intent().Networking.WorkloadMTU, prepared.execution.plan.Intent().Networking.WorkloadCIDR)
				if err != nil || !sameOperationRequest(prior, current) {
					return nil, fmt.Errorf("OWNED_RESUME_HOST_ALLOCATION_CHANGED: original host request no longer matches the complete allocation cohort")
				}
			}
		}
	}

	// A later pool/host read must not silently invalidate an earlier prefix proof.
	for _, anchor := range anchors {
		current, err := observeHostLeasePool(observeCtx, control.tenant.Client, anchor.parent, expected, anchor.pool.GetUID())
		if err != nil || !sameHostLeasePoolObservation(anchor.pool, current) {
			return nil, fmt.Errorf("OWNED_RESUME_ALLOCATION_CHANGED: the allocation cohort changed before assessment")
		}
	}
	if err = client.VerifyIdentity(observeCtx); err != nil {
		return nil, err
	}
	if err = control.tenant.Client.VerifyIdentity(observeCtx); err != nil {
		return nil, err
	}
	return allocations, nil
}

// resumeAllocationPrefixes validates the complete allocation set against the
// retained IPAM network, including empty in-progress status. It never allocates.
func resumeAllocationPrefixes(parent, pool *unstructured.Unstructured) (map[string]string, error) {
	network, found, err := unstructured.NestedString(parent.Object, "spec", "ipv4Network", "network")
	if err != nil || !found {
		return nil, fmt.Errorf("OWNED_RESUME_POOL_INVALID")
	}
	prefixValue, found, err := unstructured.NestedFieldNoCopy(parent.Object, "spec", "ipv4Network", "prefixSize")
	size, ok := numericInt64(prefixValue)
	whole, parseErr := netip.ParsePrefix(network)
	if err != nil || !found || !ok || parseErr != nil || !whole.Addr().Is4() || whole != whole.Masked() || size < int64(whole.Bits()) || size > 32 {
		return nil, fmt.Errorf("OWNED_RESUME_POOL_INVALID")
	}
	rows, _, err := unstructured.NestedSlice(pool.Object, "status", "allocations")
	if err != nil {
		return nil, fmt.Errorf("OWNED_RESUME_ALLOCATIONS_INVALID")
	}
	result := map[string]string{}
	prefixes := []netip.Prefix{}
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("OWNED_RESUME_ALLOCATIONS_INVALID")
		}
		name, nameOK := row["nodeName"].(string)
		text, textOK := row["prefix"].(string)
		prefix, err := netip.ParsePrefix(text)
		if !nameOK || name == "" || !textOK || err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() || int64(prefix.Bits()) != size || !whole.Contains(prefix.Addr()) || !whole.Contains(prefixLastAddress(prefix)) {
			return nil, fmt.Errorf("OWNED_RESUME_ALLOCATIONS_INVALID")
		}
		if _, duplicate := result[name]; duplicate {
			return nil, fmt.Errorf("OWNED_RESUME_ALLOCATIONS_AMBIGUOUS")
		}
		for _, prior := range prefixes {
			if prior.Overlaps(prefix) {
				return nil, fmt.Errorf("OWNED_RESUME_ALLOCATIONS_OVERLAP")
			}
		}
		result[name] = prefix.String()
		prefixes = append(prefixes, prefix)
	}
	return result, nil
}
