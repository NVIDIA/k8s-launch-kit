// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

const (
	hostLeaseNodePageSize = 64
	hostLeaseNodeMaxCount = 3000
	hostLeaseNodeMaxPages = (hostLeaseNodeMaxCount + hostLeaseNodePageSize - 1) / hostLeaseNodePageSize
)

// observeHostWorkloadLeaseSnapshot reads the current parent, child and complete
// tenant Node set through the same bound clients and retained execution receipts.
// It is an observation only: it does not configure hosts or imply reachability.
func observeHostWorkloadLeaseSnapshot(
	ctx context.Context,
	management managementLeaseReader,
	tenant tenantAllocationReader,
	plan *generation.Plan,
	reconciler *Reconciler,
	workload workloadLeaseInputs,
	requestTimeout time.Duration,
) (hostWorkloadLeaseSnapshot, error) {
	if ctx == nil || management == nil || tenant == nil || plan == nil || reconciler == nil || requestTimeout < time.Second || requestTimeout > 2*time.Minute {
		return hostWorkloadLeaseSnapshot{}, fmt.Errorf("host lease observation requires bound clients, retained plan, reconciler and bounded request timeout")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return hostWorkloadLeaseSnapshot{}, fmt.Errorf("host lease observation requires an execution deadline")
	}
	if err := ctx.Err(); err != nil {
		return hostWorkloadLeaseSnapshot{}, err
	}
	if plan.Digest() == "" || workload.bundleDigest != plan.Digest() || reconciler.BundleDigest() != plan.Digest() ||
		workload.managementDigest != plan.Digest() || workload.tenant != tenant ||
		workload.tenantUID == "" || tenant.clusterIdentity() != workload.tenantUID {
		return hostWorkloadLeaseSnapshot{}, fmt.Errorf("host lease observation receipts do not identify one retained execution")
	}
	intent := plan.Intent()
	if !management.matchesBinding(intent.Management, intent.KubernetesVersion) || !tenant.matchesTenantSource(management) {
		return hostWorkloadLeaseSnapshot{}, fmt.Errorf("host lease clients are not bound to the retained management and tenant identities")
	}
	expectedTenant, _, err := tenantPlanInputs(plan)
	if err != nil || !tenant.matchesTenantTarget(expectedTenant, workload.tenantUID) {
		return hostWorkloadLeaseSnapshot{}, fmt.Errorf("tenant API binding differs from the retained tenant plan")
	}
	desiredIPAM, err := retainedWorkloadIPAM(plan)
	if err != nil {
		return hostWorkloadLeaseSnapshot{}, err
	}
	ipamReceipt, expectedPoolUID, err := hostLeaseWorkloadReceipt(workload.resources, desiredIPAM)
	if err != nil {
		return hostWorkloadLeaseSnapshot{}, err
	}
	if ipamReceipt.Generation <= 0 || expectedPoolUID == "" {
		return hostWorkloadLeaseSnapshot{}, fmt.Errorf("workload receipt has no current IPAM generation or CIDRPool identity")
	}

	observeCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := management.VerifyIdentity(observeCtx); err != nil {
		return hostWorkloadLeaseSnapshot{}, err
	}
	if err := tenant.VerifyIdentity(observeCtx); err != nil {
		return hostWorkloadLeaseSnapshot{}, err
	}
	parentBefore, err := observeHostLeaseIPAM(observeCtx, management, reconciler, desiredIPAM, ipamReceipt)
	if err != nil {
		return hostWorkloadLeaseSnapshot{}, err
	}
	poolBefore, err := observeHostLeasePool(observeCtx, tenant, parentBefore, expectedTenant, expectedPoolUID)
	if err != nil {
		return hostWorkloadLeaseSnapshot{}, err
	}
	nodes, err := tenant.listCompleteNodes(observeCtx)
	if err != nil {
		return hostWorkloadLeaseSnapshot{}, err
	}
	poolAfter, err := observeHostLeasePool(observeCtx, tenant, parentBefore, expectedTenant, expectedPoolUID)
	if err != nil {
		return hostWorkloadLeaseSnapshot{}, err
	}
	if !sameHostLeasePoolObservation(poolBefore, poolAfter) {
		return hostWorkloadLeaseSnapshot{}, apierrors.NewServiceUnavailable("tenant CIDRPool changed during the complete Node listing")
	}
	parentAfter, err := observeHostLeaseIPAM(observeCtx, management, reconciler, desiredIPAM, ipamReceipt)
	if err != nil {
		return hostWorkloadLeaseSnapshot{}, err
	}
	if parentBefore.GetUID() != parentAfter.GetUID() || parentBefore.GetGeneration() != parentAfter.GetGeneration() ||
		!reflect.DeepEqual(parentBefore.Object["spec"], parentAfter.Object["spec"]) {
		return hostWorkloadLeaseSnapshot{}, apierrors.NewServiceUnavailable("retained DPUServiceIPAM changed during the complete Node listing")
	}
	if err := management.VerifyIdentity(observeCtx); err != nil {
		return hostWorkloadLeaseSnapshot{}, err
	}
	if err := tenant.VerifyIdentity(observeCtx); err != nil {
		return hostWorkloadLeaseSnapshot{}, err
	}
	if err := observeCtx.Err(); err != nil {
		return hostWorkloadLeaseSnapshot{}, err
	}
	return hostWorkloadLeaseSnapshot{
		DPUServiceIPAM: parentAfter.DeepCopy(),
		CIDRPool:       poolAfter.DeepCopy(),
		TenantNodes:    cloneUnstructuredNodes(nodes),
	}, nil
}

func observeHostLeaseIPAM(ctx context.Context, management resourceReader, reconciler *Reconciler, desired *unstructured.Unstructured, receipt workloadResourceReceipt) (*unstructured.Unstructured, error) {
	current, err := management.Get(ctx, desired.DeepCopy())
	if err != nil {
		return nil, err
	}
	if current == nil || current.GroupVersionKind() != desired.GroupVersionKind() || current.GetName() != desired.GetName() || current.GetNamespace() != desired.GetNamespace() ||
		current.GetUID() != receipt.UID || current.GetResourceVersion() == "" || current.GetGeneration() != receipt.Generation || current.GetDeletionTimestamp() != nil ||
		reconciler.Verify(desired, current) != nil {
		return nil, apierrors.NewServiceUnavailable("retained DPUServiceIPAM is missing, replaced, stale or changed")
	}
	if ready := workloadResourceReadiness(current, workloadIPAMReadiness); !ready.Ready {
		return nil, apierrors.NewServiceUnavailable("retained DPUServiceIPAM is not currently reconciled")
	}
	return current, nil
}

func observeHostLeasePool(ctx context.Context, tenant tenantVerificationReader, parent *unstructured.Unstructured, expected TenantReadinessExpected, uid types.UID) (*unstructured.Unstructured, error) {
	pool, err := tenant.getWorkloadChild(ctx, parent)
	if err != nil {
		return nil, safeAPIError("read tenant CIDRPool", err)
	}
	if pool == nil || pool.GetUID() != uid || pool.GetResourceVersion() == "" || pool.GetGeneration() <= 0 || pool.GetDeletionTimestamp() != nil {
		return nil, apierrors.NewServiceUnavailable("tenant CIDRPool is missing, replaced, deleting or lacks fresh metadata")
	}
	if err := verifyWorkloadChild(parent, pool, expected); err != nil {
		return nil, fmt.Errorf("tenant CIDRPool differs from the retained workload IPAM: %w", err)
	}
	return pool, nil
}

func sameHostLeasePoolObservation(first, second *unstructured.Unstructured) bool {
	if first == nil || second == nil || first.GetUID() != second.GetUID() || first.GetResourceVersion() != second.GetResourceVersion() || first.GetGeneration() != second.GetGeneration() {
		return false
	}
	firstAllocations, _, firstErr := unstructured.NestedFieldNoCopy(first.Object, "status", "allocations")
	secondAllocations, _, secondErr := unstructured.NestedFieldNoCopy(second.Object, "status", "allocations")
	return firstErr == nil && secondErr == nil && reflect.DeepEqual(first.Object["spec"], second.Object["spec"]) && reflect.DeepEqual(firstAllocations, secondAllocations)
}

func listCompleteTenantNodes(ctx context.Context, client typedcorev1.CoreV1Interface) ([]unstructured.Unstructured, error) {
	if ctx == nil || client == nil {
		return nil, fmt.Errorf("tenant Core API client and context are required")
	}
	var nodes []unstructured.Unstructured
	cursor := ""
	seenCursors := map[string]bool{}
	seenNames := map[string]bool{}
	seenUIDs := map[types.UID]bool{}
	resourceVersion := ""
	for pageNumber := 0; pageNumber < hostLeaseNodeMaxPages; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := client.Nodes().List(ctx, metav1.ListOptions{Limit: hostLeaseNodePageSize, Continue: cursor})
		if err != nil {
			return nil, safeAPIError("list tenant Nodes", err)
		}
		if page == nil || len(page.Items) > hostLeaseNodePageSize {
			return nil, fmt.Errorf("tenant Node list returned an invalid or oversized page")
		}
		if page.APIVersion != "" && page.APIVersion != "v1" || page.Kind != "" && page.Kind != "NodeList" {
			return nil, fmt.Errorf("tenant Node list returned the wrong API identity")
		}
		if page.ResourceVersion == "" {
			return nil, fmt.Errorf("tenant Node list page has no resourceVersion")
		}
		if resourceVersion != "" && page.ResourceVersion != resourceVersion {
			return nil, apierrors.NewServiceUnavailable("tenant Node list changed resourceVersion between pages")
		}
		resourceVersion = page.ResourceVersion
		if len(nodes)+len(page.Items) > hostLeaseNodeMaxCount {
			return nil, fmt.Errorf("tenant Node list exceeds the complete observation limit")
		}
		for i := range page.Items {
			node := page.Items[i].DeepCopy()
			if node.APIVersion != "" && node.APIVersion != "v1" || node.Kind != "" && node.Kind != "Node" {
				return nil, fmt.Errorf("tenant Node list contains an object with the wrong API identity")
			}
			if node.Name == "" || node.UID == "" || node.ResourceVersion == "" || node.Namespace != "" || seenNames[node.Name] || seenUIDs[node.UID] {
				return nil, fmt.Errorf("tenant Node list contains an incomplete or duplicate identity")
			}
			node.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Node"}
			object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(node)
			if err != nil {
				return nil, fmt.Errorf("tenant Node list contains an invalid object")
			}
			nodes = append(nodes, unstructured.Unstructured{Object: object})
			seenNames[node.Name] = true
			seenUIDs[node.UID] = true
		}
		cursor = page.GetContinue()
		if cursor == "" {
			return nodes, nil
		}
		if seenCursors[cursor] {
			return nil, fmt.Errorf("tenant Node list repeated a continuation token")
		}
		seenCursors[cursor] = true
	}
	return nil, fmt.Errorf("tenant Node list exceeds the complete page bound")
}

func cloneUnstructuredNodes(nodes []unstructured.Unstructured) []unstructured.Unstructured {
	cloned := make([]unstructured.Unstructured, len(nodes))
	for i := range nodes {
		cloned[i] = *nodes[i].DeepCopy()
	}
	return cloned
}
