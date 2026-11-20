// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/discovery"
)

// validationSelectedTenantNodes establishes the selected provisioned node set
// independently of HBN health. A broken App must not hide system workloads.
func (o *validationObserver) validationSelectedTenantNodes(ctx context.Context, management validationManagementReader, tenant *tenantClient) (map[string]corev1.Node, error) {
	if tenant == nil {
		return nil, fmt.Errorf("bound tenant unavailable")
	}
	selected, err := validationSelectedHosts(o.desired, o.request, o.physicalScope.expected)
	if err != nil {
		return nil, err
	}
	nodes, err := tenant.listCompleteNodes(ctx)
	if err != nil {
		return nil, err
	}
	result := map[string]corev1.Node{}
	seen := map[string]bool{}
	expectedDD, err := retainedValidationDeployment(o.desired.plan)
	if err != nil {
		return nil, err
	}
	deployment, err := management.Get(ctx, expectedDD)
	if err != nil {
		return nil, err
	}
	if !currentObjectIdentity(deployment, expectedDD) || o.desired.verifier.Verify(expectedDD, deployment) != nil {
		return nil, fmt.Errorf("selected deployment projection differs")
	}
	sets, err := management.List(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": enrollmentDeviceAPIVersion, "kind": "DPUSet", "metadata": map[string]any{"namespace": expectedDD.GetNamespace()}}}, "")
	if err != nil {
		return nil, err
	}
	for _, item := range selected {
		node, err := management.Get(ctx, dpuNodeIdentity(expectedDD.GetNamespace(), item.nodeName))
		if err != nil {
			return nil, err
		}
		if validateCurrentEnrolledNode(node, item.retained, expectedDD.GetNamespace()) != nil || !o.desired.rules.EnrolledNodeReadiness(node).Ready {
			return nil, fmt.Errorf("selected DPUNode unavailable")
		}
		set := uniqueObjectByName(sets, item.dpuSet.GetName())
		if !currentObjectIdentity(set, item.dpuSet) || !currentActivationDPUSetMatches(o.desired.rules, set, item) || !activationOwnerMatches(set, deployment.GetAPIVersion(), deployment.GetKind(), deployment.GetName(), deployment.GetUID()) {
			return nil, fmt.Errorf("selected DPUSet ownership differs")
		}
		dpu, err := management.Get(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": enrollmentDeviceAPIVersion, "kind": "DPU", "metadata": map[string]any{"name": item.dpuName, "namespace": expectedDD.GetNamespace()}}})
		if err != nil {
			return nil, err
		}
		if dpu == nil || !currentActivationDPUMatches(o.desired.rules, dpu, item) || !activationOwnerMatches(dpu, set.GetAPIVersion(), set.GetKind(), set.GetName(), set.GetUID()) {
			return nil, fmt.Errorf("selected DPU ownership differs")
		}
		current, err := uniqueTenantNode(o.desired.rules, nodes, node.GetName(), node.GetNamespace())
		if err != nil {
			return nil, err
		}
		if seen[current.GetName()] {
			return nil, fmt.Errorf("selected tenant Node binding ambiguous")
		}
		seen[current.GetName()] = true
		var typed corev1.Node
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(current.Object, &typed); err != nil {
			return nil, err
		}
		result[item.retained.Serial] = typed
	}
	return result, tenant.VerifyIdentity(ctx)
}

func (o *validationObserver) observeValidationRuntimeResources(ctx context.Context, management validationManagementReader, tenant *tenantClient, r *validationRecorder) error {
	return o.observeValidationRuntimeResourcesWithImage(ctx, management, tenant, r, resolveHBNRuntimeImage)
}

// The image boundary resolves immutable OCI metadata; all resource/readiness
// decisions remain in this shared native observation path.
func (o *validationObserver) observeValidationRuntimeResourcesWithImage(ctx context.Context, management validationManagementReader, tenant *tenantClient, r *validationRecorder, resolveImage hbnImageResolver) error {
	selected, selectErr := validationSelectedHosts(o.desired, o.request, o.physicalScope.expected)
	if selectErr != nil {
		for _, id := range []validation.CheckID{"devices/coverage", "hbn/coverage", "workloads/coverage"} {
			r.recordError(id, selectErr)
		}
		return nil
	}
	// Device-only observations have their own accepted management region; tenant
	// failure must not erase independently collected device health.
	deviceRows := r.bufferCompletions()
	deviceScopeErr := withValidationCurrentReads(ctx, management, nil, func(management validationCurrentManagementReader, _ validationCurrentTenantReader) error {
		r := deviceRows
		// Device subjects remain visible even when a different selected DPU is bad.
		allDevices := true
		var deviceErr error
		for _, item := range selected {
			err := r.run(validation.CheckID("devices/"+item.retained.Serial), func() error {
				_, err := observeCurrentRuntimeGraph(ctx, management, o.desired, []currentSelectedDevice{item})
				return err
			})
			if err != nil {
				allDevices = false
				if deviceErr == nil {
					deviceErr = err
				}
				if validationIdentityChanged(err) {
					return err
				}
			}
		}
		if allDevices {
			r.record("devices/coverage", validation.Passed, validation.ReasonNone, "Every retained selected device has a current settled owner-bound controller graph.", "")
		} else {
			r.recordError("devices/coverage", deviceErr)
		}
		return nil
	})
	r.acceptBuffered(deviceRows, deviceScopeErr)
	if deviceScopeErr != nil {
		return validationRuntimeError(ctx, deviceScopeErr, false)
	}
	var image hbnPodImageIdentity
	if tenant != nil {
		imageCtx, cancelImage := context.WithTimeout(ctx, o.request.RequestTimeout)
		var imageErr error
		image, imageErr = resolveImage(imageCtx, hbnImageReference(o.desired.plan.Intent().Generation), o.request.RequestTimeout, nil)
		cancelImage()
		if imageErr != nil {
			image = hbnPodImageIdentity{}
		}
	}
	var tenantDependencyErr error
	if tenant == nil {
		tenantDependencyErr = r.tenantDependencyError()
	}
	rows := r.bufferCompletions()
	var graph currentRuntimeGraph
	var snapshot hostWorkloadLeaseSnapshot
	var leases []hostWorkloadLease
	var sfcBefore map[string]string
	var sfcErr, hbnErr error
	complete := false
	var tenantSource validationTenantReadSource
	if tenant != nil {
		tenantSource = tenant
	}
	err := withValidationCurrentReads(ctx, management, tenantSource, func(management validationCurrentManagementReader, tenant validationCurrentTenantReader) error {
		r := rows
		var graphErr error
		graph, graphErr = observeCurrentRuntimeGraph(ctx, management, o.desired, selected)
		if graphErr != nil {
			r.recordError("hbn/coverage", graphErr)
			r.recordError("workloads/coverage", graphErr)
			recordRuntimeIncompleteDependencies(r, graphErr)
			return nil
		}
		// Retained immutable prerequisites are compared independently of runtime.
		workloadErr := r.run("workloads/coverage", func() error {
			err := o.observeRetainedWorkloads(ctx, management, tenant, graph)
			if errors.Is(err, errValidationTenantDependency) {
				return tenantDependencyErr
			}
			return err
		})
		if validationIdentityChanged(workloadErr) {
			return workloadErr
		}
		if tenant == nil {
			// Only the absent tenant boundary supplies this dependency. Actual
			// device/declaration failures above retain their measured outcomes.
			r.recordError("hbn/coverage", tenantDependencyErr)
			recordRuntimePendingDependencies(r, tenantDependencyErr)
			return nil
		}
		var leaseErr error
		snapshot, leases, leaseErr = observeCurrentAllocation(ctx, management, tenant, o.desired, graph)
		if leaseErr != nil {
			r.recordError("hbn/coverage", leaseErr)
			recordRuntimeIncompleteDependencies(r, leaseErr)
			return nil
		}
		sfcBefore, sfcErr = currentSFCAnchors(ctx, tenant, o.desired, graph, snapshot)
		for _, item := range selected {
			err := r.run(validation.CheckID("workloads/"+item.retained.Serial), func() error { return o.observeTenantSFC(ctx, tenant, graph, snapshot, item.retained.Serial) })
			if validationIdentityChanged(err) {
				return err
			}
		}
		var bindings validationHBNBindings
		hbnErr = r.run("hbn/coverage", func() error {
			var err error
			bindings, err = observeCurrentHBNBindings(ctx, management, tenant, o.desired, graph, snapshot, leases, image, false)
			if err == nil && len(bindings.unavailable) > 0 {
				return validationFailure(validation.Unverified, validation.ReasonIncomplete, "The complete selected HBN workload or exact platform image is unavailable.")
			}
			return err
		})
		for _, item := range selected {
			id := validation.CheckID("hbn/" + item.retained.Serial)
			r.start(id)
			if _, ok := bindings.anchors[item.retained.Serial]; ok {
				r.record(id, validation.Passed, validation.ReasonNone, "Current HBN App, selected-node DaemonSet Pod and exact platform image are bound.", "")
			} else if hbnErr != nil {
				r.recordError(id, hbnErr)
			} else {
				r.record(id, validation.Unverified, validation.ReasonDependency, "The selected current HBN execution target or exact image is unavailable.", "Inspect this selected Pod, image and parent health.")
			}
		}
		if validationIdentityChanged(hbnErr) {
			return hbnErr
		}
		complete = true
		return nil
	})
	if err != nil || !complete {
		r.acceptBuffered(rows, err)
		if err != nil {
			return validationRuntimeError(ctx, err, false)
		}
		return nil
	}
	// Slow host qualification runs between accepted current-data snapshots.
	// Its outcomes remain provisional until the semantic comparison closes.
	hostErr := func() error {
		r := rows
		if o.mode == validation.ModeFull {
			hosts, err := currentHostRequests(o.desired, graph, leases)
			if err == nil {
				for _, host := range hosts {
					hostErr := r.run(validation.CheckID("physical/host-network/"+host.lease.Serial), func() error {
						if host.selected.hostTarget.SSH == nil {
							return validationFailure(validation.Unverified, validation.ReasonUnavailable, "Explicit selected-host SSH access is unavailable.")
						}
						observed, err := sshprobe.VerifyHostWorkloadNetwork(ctx, host.selected.hostTarget, host.request, o.request.RequestTimeout)
						if err != nil {
							return validationHostNetworkError(err)
						}
						if !observed.Qualified() {
							return validationFailure(validation.Failed, validation.ReasonHealth, "Selected host runtime or persistent workload configuration differs.")
						}
						return nil
					})
					if validationIdentityChanged(hostErr) {
						return validationRuntimeError(ctx, hostErr, false)
					}
				}
			}
		}
		return nil
	}()
	if hostErr != nil {
		r.acceptBuffered(rows, hostErr)
		return hostErr
	}
	err = withValidationCurrentReads(ctx, management, tenantSource, func(management validationCurrentManagementReader, tenant validationCurrentTenantReader) error {
		r := rows
		graphAfter, err := observeCurrentRuntimeGraph(ctx, management, o.desired, selected)
		if err != nil {
			return err
		}
		after, afterLeases, err := observeCurrentAllocation(ctx, management, tenant, o.desired, graphAfter)
		if err != nil {
			return err
		}
		if currentGraphDigest(graph) != currentGraphDigest(graphAfter) || !sameCurrentLeaseSnapshot(snapshot, after) || !reflect.DeepEqual(leases, afterLeases) {
			return errValidationResourceChanged
		}
		if sfcErr == nil {
			sfcAfter, err := currentSFCAnchors(ctx, tenant, o.desired, graphAfter, after)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(sfcBefore, sfcAfter) {
				return errValidationResourceChanged
			}
		} else {
			r.recordError("workloads/coverage", sfcErr)
		}
		return nil
	})
	r.acceptBuffered(rows, err)
	if err != nil {
		return validationRuntimeError(ctx, err, false)
	}
	return nil
}

// An incomplete semantic collection cannot accept provisional passes. Keep
// measured failures/pending evidence; a known progressing cause must also keep
// dependent rows Pending so the resource runner can observe convergence.
func recordRuntimeIncompleteDependencies(r *validationRecorder, cause error) {
	var check *validationCheckError
	pending := errors.As(cause, &check) && check.state == validation.Pending
	for id, out := range r.outcomes {
		if out.State != validation.Passed {
			continue
		}
		spec := r.specs[id]
		if spec.Area != validation.AreaHBN && spec.Area != validation.AreaWorkloads && !strings.HasPrefix(string(id), "physical/host-network/") {
			continue
		}
		if pending {
			r.record(id, validation.Pending, validation.ReasonProgressing, "Waiting for the selected current controller prerequisite before this collection can be accepted.", "Allow the selected controller to settle and repeat the observation.")
		} else {
			r.record(id, validation.Unverified, validation.ReasonDependency, "The current collection could not complete the semantic comparison required for this check.", "Resolve the reported dependency and repeat validation.")
		}
	}
	recordRuntimePendingDependencies(r, cause)
}

// Only a proven progressing prerequisite makes unperformed dependent rows
// pending. Denied, malformed, unsupported and terminal states remain blockers.
func recordRuntimePendingDependencies(r *validationRecorder, cause error) {
	var check *validationCheckError
	if !errors.As(cause, &check) || check.state != validation.Pending {
		return
	}
	for id, spec := range r.specs {
		if _, done := r.outcomes[id]; done {
			continue
		}
		if spec.Area == validation.AreaHBN || spec.Area == validation.AreaWorkloads || strings.HasPrefix(string(id), "physical/host-network/") {
			r.start(id)
			r.record(id, validation.Pending, validation.ReasonProgressing, "Waiting for the selected current controller prerequisite to reconcile.", "Allow the selected controller to settle and repeat the observation.")
		}
	}
}

var errValidationTenantDependency = errors.New("selected tenant children are not available")

func (o *validationObserver) observeRetainedWorkloads(ctx context.Context, management resourceReader, tenant validationCurrentTenantReader, graph currentRuntimeGraph) error {
	objects, err := retainedPlanWorkloadObjects(o.desired.plan)
	if err != nil {
		return err
	}
	tenantMissing := false
	for _, expected := range objects {
		current, err := management.Get(ctx, expected.DeepCopy())
		if err != nil {
			return err
		}
		if !currentObjectIdentity(current, expected) || o.desired.verifier.Verify(expected, current) != nil {
			return fmt.Errorf("retained workload prerequisite differs")
		}
		switch current.GetKind() {
		case "DPUFlavor", "DPUServiceConfiguration": // These APIs do not define readiness status.
		case "BlueFieldSoftware":
			if err := validationRuntimeReadiness(o.desired.rules.ResourceReadiness(current), "Retained software is not currently Ready and Downloaded."); err != nil {
				return err
			}
		case "DPUServiceTemplate":
			if err := validationRuntimeReadiness(o.desired.rules.WorkloadReadiness(current, controllercontract.WorkloadTemplate), "Retained HBN template has not reconciled its current generation."); err != nil {
				return err
			}
		case "DPUServiceIPAM":
			if tenant == nil {
				tenantMissing = true // Keep inspecting independent declarations.
			} else {
				child, err := tenant.getWorkloadChild(ctx, current)
				if err != nil {
					return err
				}
				expectedTenant, _, err := tenantPlanInputs(o.desired.plan)
				if err != nil {
					return err
				}
				if err := verifyWorkloadChild(current, child, expectedTenant); err != nil {
					return err
				}
			}
			if err := validationRuntimeReadiness(o.desired.rules.WorkloadReadiness(current, controllercontract.WorkloadIPAM), "Retained workload IPAM has not reconciled its current generation."); err != nil {
				return err
			}
		case "DPUServiceInterface":
			if err := validationRuntimeReadiness(o.desired.rules.ValidationWorkloadReadiness(current), "Selected workload has not reconciled its current generation."); err != nil {
				return err
			}
		}
	}
	// Graph compilation independently proves actual Flavor/Configuration use.
	if len(graph.sets) != len(graph.selected) {
		return fmt.Errorf("selected prerequisite consumption incomplete")
	}
	if tenantMissing {
		return errValidationTenantDependency
	}
	return nil
}

// tenantSFCObjects is a finite read-only collection on the already source-bound
// tenant. Explicit kind/resource pairs cannot be widened to arbitrary exec/API.
func (tenant *tenantClient) listValidationSFCObjects(ctx context.Context, kind, namespace, name, selector string) ([]unstructured.Unstructured, error) {
	if _, err := tenant.validationSFCResource(kind, namespace, name); err != nil {
		return nil, err
	}
	if err := tenant.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	rows, err := tenant.listCurrentValidationSFCObjects(ctx, kind, namespace, name, selector)
	if err != nil {
		return nil, err
	}
	return rows, tenant.VerifyIdentity(ctx)
}

func (tenant *tenantClient) validationSFCResource(kind, namespace, name string) (string, error) {
	resources := map[string]string{"ServiceChainSet": "servicechainsets", "ServiceChain": "servicechains", "ServiceInterfaceSet": "serviceinterfacesets", "NodeServiceInterfaces": "nodeserviceinterfaces"}
	resource, ok := resources[kind]
	if !ok || tenant == nil || tenant.api == nil || len(kvalidation.IsDNS1123Label(namespace)) != 0 || name != "" && len(kvalidation.IsDNS1123Subdomain(name)) != 0 {
		return "", fmt.Errorf("selected tenant child input invalid")
	}
	return resource, nil
}

func (tenant *tenantClient) listCurrentValidationSFCObjects(ctx context.Context, kind, namespace, name, selector string) ([]unstructured.Unstructured, error) {
	resource, err := tenant.validationSFCResource(kind, namespace, name)
	if err != nil {
		return nil, err
	}
	served, err := discovery.ToServerResourcesInterfaceWithContext(tenant.api.Discovery()).ServerResourcesForGroupVersionWithContext(ctx, "svc.dpu.nvidia.com/v1alpha1")
	if err != nil {
		return nil, err
	}
	matches := 0
	if served == nil {
		return nil, fmt.Errorf("selected tenant child discovery unavailable")
	}
	for _, api := range served.APIResources {
		if api.Kind == kind && api.Name == resource && api.Namespaced {
			matches++
		}
	}
	if matches != 1 {
		return nil, fmt.Errorf("selected tenant child API unavailable or ambiguous")
	}
	path := "/apis/svc.dpu.nvidia.com/v1alpha1/namespaces/" + namespace + "/" + resource
	if name != "" {
		var object unstructured.Unstructured
		if err := tenant.api.Discovery().RESTClient().Get().AbsPath(path + "/" + name).Do(ctx).Into(&object); err != nil {
			return nil, err
		}
		if object.GetKind() != kind || object.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" || object.GetNamespace() != namespace || object.GetName() != name || object.GetUID() == "" || object.GetResourceVersion() == "" || object.GetDeletionTimestamp() != nil {
			return nil, fmt.Errorf("selected tenant child identity invalid")
		}
		return []unstructured.Unstructured{object}, nil
	}
	rows := []unstructured.Unstructured{}
	cursor, rv := "", ""
	seenNames, seenUIDs, seenCursors := map[string]bool{}, map[types.UID]bool{}, map[string]bool{}
	for range hbnRuntimeMaxPages {
		var page unstructured.UnstructuredList
		request := tenant.api.Discovery().RESTClient().Get().AbsPath(path).Param("limit", "64").Param("labelSelector", selector)
		if cursor != "" {
			request.Param("continue", cursor)
		}
		if err := request.Do(ctx).Into(&page); err != nil {
			return nil, err
		}
		if page.GetKind() != kind+"List" || page.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" || page.GetResourceVersion() == "" || len(page.Items) > hbnRuntimePageSize || rv != "" && rv != page.GetResourceVersion() {
			return nil, fmt.Errorf("selected tenant child list incomplete")
		}
		rv = page.GetResourceVersion()
		for i := range page.Items {
			object := &page.Items[i]
			if object.GetKind() != kind || object.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" || object.GetNamespace() != namespace || object.GetName() == "" || object.GetUID() == "" || object.GetResourceVersion() == "" || seenNames[object.GetName()] || seenUIDs[object.GetUID()] {
				return nil, fmt.Errorf("selected tenant child list identity incomplete or duplicated")
			}
			seenNames[object.GetName()] = true
			seenUIDs[object.GetUID()] = true
			rows = append(rows, *object.DeepCopy())
		}
		cursor = page.GetContinue()
		if cursor == "" {
			return rows, nil
		}
		if seenCursors[cursor] {
			return nil, fmt.Errorf("selected tenant child list repeated page")
		}
		seenCursors[cursor] = true
	}
	return nil, fmt.Errorf("selected tenant child list exceeds bound")
}

func (o *validationObserver) observeTenantSFC(ctx context.Context, tenant validationCurrentTenantReader, graph currentRuntimeGraph, snapshot hostWorkloadLeaseSnapshot, serial string) error {
	var node *unstructured.Unstructured
	for _, item := range graph.selected {
		if item.retained.Serial == serial {
			var err error
			node, err = uniqueTenantNode(o.desired.rules, snapshot.TenantNodes, item.nodeName, graph.deployment.GetNamespace())
			if err != nil {
				return err
			}
		}
	}
	if node == nil {
		return fmt.Errorf("selected tenant node unavailable")
	}
	parent := graph.deployment.GetNamespace() + "_" + graph.deployment.GetName()
	chainCount := 0
	for i := range graph.chains {
		chain := &graph.chains[i]
		if chain.GetLabels()[activationParentLabel] != parent {
			continue
		}
		chainCount++
		if err := validationRuntimeReadiness(o.desired.rules.ValidationWorkloadReadiness(chain), "Selected workload has not reconciled its current generation."); err != nil {
			return err
		}
		sets, err := tenant.listValidationSFCObjects(ctx, "ServiceChainSet", chain.GetNamespace(), chain.GetName(), "")
		if err != nil {
			return err
		}
		set := &sets[0]
		expected, found, e := unstructured.NestedMap(chain.Object, "spec", "template", "spec")
		if e != nil || !found || !jsonEqual(expected, set.Object["spec"]) || len(set.GetOwnerReferences()) != 0 || len(set.GetLabels()) != 0 || len(set.GetAnnotations()) != 0 {
			return fmt.Errorf("current chain set differs from parent")
		}
		if err := validationRuntimeReadiness(o.desired.rules.ValidationWorkloadReadiness(set), "Selected workload has not reconciled its current generation."); err != nil {
			return err
		}
		if err := verifySelectedSetNodes(o.desired.rules, set, snapshot.TenantNodes, graph); err != nil {
			return err
		}
		rows, err := tenant.listValidationSFCObjects(ctx, "ServiceChain", set.GetNamespace(), "", labels.Set{"svc.dpu.nvidia.com/servicechainset-name": set.GetName(), "svc.dpu.nvidia.com/servicechainset-namespace": set.GetNamespace()}.AsSelector().String())
		if err != nil {
			return err
		}
		if err := verifySelectedChainChildren(o.desired.rules, set, rows, snapshot.TenantNodes, graph); err != nil {
			return err
		}
	}
	if chainCount != 1 {
		return fmt.Errorf("selected chain parent missing or ambiguous")
	}
	interfaces := []*unstructured.Unstructured{}
	objects, err := retainedPlanWorkloadObjects(o.desired.plan)
	if err != nil {
		return err
	}
	// Retained physical/PF roots and current service-port roots form six sets.
	for _, expected := range objects {
		if expected.GetKind() == "DPUServiceInterface" {
			for i := range graph.interfaces {
				row := &graph.interfaces[i]
				if row.GetName() == expected.GetName() {
					if o.desired.verifier.Verify(expected, row) != nil {
						return fmt.Errorf("retained interface differs")
					}
					interfaces = append(interfaces, row)
				}
			}
		}
	}
	for i := range graph.interfaces {
		row := &graph.interfaces[i]
		if row.GetLabels()[activationParentLabel] == parent && row.GetLabels()[hbnServiceReferenceLabel] == hbnContainerName {
			interfaces = append(interfaces, row)
		}
	}
	if len(interfaces) != 6 {
		return fmt.Errorf("selected interface parent coverage incomplete or ambiguous")
	}
	shards, err := tenant.listValidationSFCObjects(ctx, "NodeServiceInterfaces", o.desired.operatorNamespace, "", labels.Set{"svc.dpu.nvidia.com/node": node.GetName(), "svc.dpu.nvidia.com/nsi-type": "sfc"}.AsSelector().String())
	if err != nil {
		return err
	}
	if len(shards) != 1 {
		return fmt.Errorf("selected node NSI shard missing or ambiguous")
	}
	shard := &shards[0]
	shardNode, _, _ := unstructured.NestedString(shard.Object, "spec", "node")
	shardType, _, _ := unstructured.NestedString(shard.Object, "spec", "type")
	if shardNode != node.GetName() || shardType != "sfc" || len(shard.GetOwnerReferences()) != 0 {
		return fmt.Errorf("selected NSI shard node or type differs")
	}
	for _, interfaceParent := range interfaces {
		if err := validationRuntimeReadiness(o.desired.rules.ValidationWorkloadReadiness(interfaceParent), "Selected workload has not reconciled its current generation."); err != nil {
			return err
		}
		sets, err := tenant.listValidationSFCObjects(ctx, "ServiceInterfaceSet", interfaceParent.GetNamespace(), interfaceParent.GetName(), "")
		if err != nil {
			return err
		}
		set := &sets[0]
		if verifyServiceInterfaceSetChild(interfaceParent, set) != nil || len(set.GetOwnerReferences()) != 0 || set.GetAnnotations()[workloadInterfaceModeAnnotation] != "nsi" {
			return fmt.Errorf("selected interface set projection or stable NSI mode differs")
		}
		if err := validationRuntimeReadiness(o.desired.rules.ValidationWorkloadReadiness(set), "Selected workload has not reconciled its current generation."); err != nil {
			return err
		}
		if err := verifySelectedSetNodes(o.desired.rules, set, snapshot.TenantNodes, graph); err != nil {
			return err
		}
		if err := verifySelectedNSIEntry(o.desired.rules, set, shard); err != nil {
			return err
		}
	}
	return nil
}

func verifySelectedSetNodes(rules *controllercontract.Rules, set *unstructured.Unstructured, nodes []unstructured.Unstructured, graph currentRuntimeGraph) error {
	selectorMap, found, err := unstructured.NestedMap(set.Object, "spec", "nodeSelector")
	if err != nil || !found {
		return fmt.Errorf("selected tenant set selector unavailable")
	}
	selectorSpec := &metav1.LabelSelector{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(selectorMap, selectorSpec); err != nil {
		return err
	}
	selector, err := metav1.LabelSelectorAsSelector(selectorSpec)
	if err != nil || selector.Empty() {
		return fmt.Errorf("selected tenant set selector invalid")
	}
	expected := map[string]types.UID{}
	for _, item := range graph.selected {
		node, err := uniqueTenantNode(rules, nodes, item.nodeName, set.GetNamespace())
		if err != nil {
			return err
		}
		expected[node.GetName()] = node.GetUID()
	}
	matched := 0
	for i := range nodes {
		node := &nodes[i]
		if !selector.Matches(labels.Set(node.GetLabels())) {
			continue
		}
		if uid, ok := expected[node.GetName()]; !ok || uid != node.GetUID() {
			return fmt.Errorf("selected tenant set includes foreign node")
		}
		matched++
	}
	applied, found, err := unstructured.NestedInt64(set.Object, "status", "numberApplied")
	if err != nil || !found || applied != int64(len(expected)) || matched != len(expected) {
		return fmt.Errorf("selected tenant set node coverage incomplete")
	}
	return nil
}

// currentSFCAnchors captures only selected semantic identities from shared NSI
// shards. Unrelated entries can change without becoming selected evidence.
func currentSFCAnchors(ctx context.Context, tenant validationCurrentTenantReader, desired *managementDesired, graph currentRuntimeGraph, snapshot hostWorkloadLeaseSnapshot) (map[string]string, error) {
	result := map[string]string{}
	parent := graph.deployment.GetNamespace() + "_" + graph.deployment.GetName()
	selectedSets := map[string]bool{}
	add := func(o *unstructured.Unstructured) {
		result[o.GetKind()+"/"+o.GetNamespace()+"/"+o.GetName()] = hbnBindingDigest(struct {
			UID                 types.UID
			Generation          int64
			Spec                any
			Owners              any
			Labels, Annotations map[string]string
			Status              any
		}{o.GetUID(), o.GetGeneration(), o.Object["spec"], o.GetOwnerReferences(), o.GetLabels(), o.GetAnnotations(), o.Object["status"]})
	}
	for i := range graph.interfaces {
		source := &graph.interfaces[i]
		applicable := source.GetLabels()[activationParentLabel] == parent
		if !applicable {
			objects, err := retainedPlanWorkloadObjects(desired.plan)
			if err != nil {
				return nil, err
			}
			for _, expected := range objects {
				if expected.GetKind() == source.GetKind() && expected.GetName() == source.GetName() && expected.GetNamespace() == source.GetNamespace() {
					applicable = true
				}
			}
		}
		if !applicable {
			continue
		}
		rows, err := tenant.listValidationSFCObjects(ctx, "ServiceInterfaceSet", source.GetNamespace(), source.GetName(), "")
		if err != nil {
			return nil, err
		}
		add(&rows[0])
		selectedSets[source.GetNamespace()+"_"+source.GetName()] = true
	}
	for i := range graph.chains {
		source := &graph.chains[i]
		if source.GetLabels()[activationParentLabel] != parent {
			continue
		}
		sets, err := tenant.listValidationSFCObjects(ctx, "ServiceChainSet", source.GetNamespace(), source.GetName(), "")
		if err != nil {
			return nil, err
		}
		set := &sets[0]
		add(set)
		children, err := tenant.listValidationSFCObjects(ctx, "ServiceChain", set.GetNamespace(), "", labels.Set{"svc.dpu.nvidia.com/servicechainset-name": set.GetName(), "svc.dpu.nvidia.com/servicechainset-namespace": set.GetNamespace()}.AsSelector().String())
		if err != nil {
			return nil, err
		}
		for j := range children {
			add(&children[j])
		}
	}
	for _, item := range graph.selected {
		node, err := uniqueTenantNode(desired.rules, snapshot.TenantNodes, item.nodeName, graph.deployment.GetNamespace())
		if err != nil {
			return nil, err
		}
		shards, err := tenant.listValidationSFCObjects(ctx, "NodeServiceInterfaces", desired.operatorNamespace, "", labels.Set{"svc.dpu.nvidia.com/node": node.GetName(), "svc.dpu.nvidia.com/nsi-type": "sfc"}.AsSelector().String())
		if err != nil {
			return nil, err
		}
		for i := range shards {
			shard := &shards[i]
			entries, _, err := unstructured.NestedSlice(shard.Object, "spec", "interfaces")
			if err != nil {
				return nil, err
			}
			statuses, _, err := unstructured.NestedSlice(shard.Object, "status", "interfaceStatuses")
			if err != nil {
				return nil, err
			}
			selectedEntries, selectedStatuses := []any{}, []any{}
			for _, raw := range entries {
				row, ok := raw.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("selected shard entry malformed")
				}
				name, _ := row["name"].(string)
				if selectedSets[name] {
					selectedEntries = append(selectedEntries, row)
				}
			}
			for _, raw := range statuses {
				row, ok := raw.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("selected shard status malformed")
				}
				name, _ := row["name"].(string)
				if selectedSets[name] {
					selectedStatuses = append(selectedStatuses, row)
				}
			}
			result["NodeServiceInterfaces/"+shard.GetNamespace()+"/"+shard.GetName()] = hbnBindingDigest(struct {
				UID               types.UID
				Generation        int64
				Node              string
				Entries, Statuses []any
			}{shard.GetUID(), shard.GetGeneration(), node.GetName(), selectedEntries, selectedStatuses})
		}
	}
	return result, nil
}

func verifySelectedChainChildren(rules *controllercontract.Rules, set *unstructured.Unstructured, rows []unstructured.Unstructured, tenantNodes []unstructured.Unstructured, graph currentRuntimeGraph) error {
	expectedNodes := map[string]bool{}
	for _, item := range graph.selected {
		node, err := uniqueTenantNode(rules, tenantNodes, item.nodeName, set.GetNamespace())
		if err != nil {
			return err
		}
		expectedNodes[node.GetName()] = true
	}
	if len(rows) != len(expectedNodes) {
		return fmt.Errorf("selected chain child coverage incomplete")
	}
	switches, found, err := unstructured.NestedSlice(set.Object, "spec", "template", "spec", "switches")
	if err != nil || !found {
		return fmt.Errorf("selected chain set template malformed")
	}
	for i := range rows {
		row := &rows[i]
		node, _, err := unstructured.NestedString(row.Object, "spec", "node")
		if err != nil || !expectedNodes[node] {
			return fmt.Errorf("selected chain child node duplicate or foreign")
		}
		delete(expectedNodes, node)
		if row.GetDeletionTimestamp() != nil || len(row.GetOwnerReferences()) != 1 || !activationOwnerMatches(row, set.GetAPIVersion(), set.GetKind(), set.GetName(), set.GetUID()) || !jsonEqual(row.Object["spec"], map[string]any{"node": node, "switches": switches}) {
			return fmt.Errorf("selected chain child ownership or spec differs")
		}
		if err := validationRuntimeReadiness(rules.ValidationWorkloadReadiness(row), "Selected workload has not reconciled its current generation."); err != nil {
			return err
		}
	}
	return nil
}

func verifySelectedNSIEntry(rules *controllercontract.Rules, set, shard *unstructured.Unstructured) error {
	name := set.GetNamespace() + "_" + set.GetName()
	template, found, err := unstructured.NestedMap(set.Object, "spec", "template", "spec")
	if err != nil || !found {
		return fmt.Errorf("selected interface template malformed")
	}
	kind, _ := template["interfaceType"].(string)
	if kind != "physical" && kind != "pf" && kind != "service" {
		return fmt.Errorf("selected interface type unsupported")
	}
	expected := runtime.DeepCopyJSON(template)
	expected["name"] = name
	expectedLabels, _, err := unstructured.NestedStringMap(set.Object, "spec", "template", "metadata", "labels")
	if err != nil {
		return err
	}
	expectedLabels = maps.Clone(expectedLabels)
	if expectedLabels == nil {
		expectedLabels = map[string]string{}
	}
	expectedLabels["svc.dpu.nvidia.com/serviceinterfaceset-name"] = set.GetName()
	expectedLabels["svc.dpu.nvidia.com/serviceinterfaceset-namespace"] = set.GetNamespace()
	if kind == "service" {
		id, _, err := unstructured.NestedString(template, "service", "serviceID")
		if err != nil || id == "" {
			return fmt.Errorf("selected service interface identity missing")
		}
		expectedLabels["svc.dpu.nvidia.com/service-id"] = id
	}
	rows, found, err := unstructured.NestedSlice(shard.Object, "spec", "interfaces")
	if err != nil || !found {
		return fmt.Errorf("selected NSI entry unavailable")
	}
	matches := 0
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("NSI entry malformed")
		}
		if row["name"] != name {
			continue
		}
		matches++
		copy := runtime.DeepCopyJSON(row)
		if terminating, exists := copy["terminating"]; exists {
			if terminating != false {
				return fmt.Errorf("selected NSI entry terminating")
			}
			delete(copy, "terminating")
		}
		labelsMap := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"labels": copy["labels"], "annotations": copy["annotations"]}}}
		if !maps.Equal(expectedLabels, labelsMap.GetLabels()) {
			return fmt.Errorf("selected NSI entry labels differ")
		}
		expectedAnnotations, _, err := unstructured.NestedStringMap(set.Object, "spec", "template", "metadata", "annotations")
		if err != nil || !maps.Equal(expectedAnnotations, labelsMap.GetAnnotations()) {
			return fmt.Errorf("selected NSI entry annotations differ")
		}
		delete(copy, "labels")
		delete(copy, "annotations")
		if !reflect.DeepEqual(copy, expected) {
			return fmt.Errorf("selected NSI entry exact spec differs")
		}
	}
	if matches != 1 {
		return fmt.Errorf("selected NSI entry missing or duplicated")
	}
	return validationRuntimeReadiness(rules.ValidationNSIEntryReady(shard, name), "Selected NSI entry has not reconciled its current generation.")
}

func validationHostNetworkError(err error) error {
	if validationHostObservedDrift(err) {
		return fmt.Errorf("host observation changed: %w", errValidationResourceChanged)
	}
	code := sshprobe.Code(err)
	switch code {
	case "HOST_NETWORK_RUNTIME_MISMATCH", "HOST_NETWORK_PERSISTENT_MISMATCH":
		return validationFailure(validation.Failed, validation.ReasonHealth, sshprobe.Reason(err)+" ("+code+")")
	default:
		return validationFailure(validation.Unverified, validation.ReasonUnavailable, sshprobe.Reason(err)+" ("+code+")")
	}
}
