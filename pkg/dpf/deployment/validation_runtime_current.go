// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

// currentSelectedDevice binds retained intent and saved physical association.
// Enrollment history and effect completion are deliberately separate.
type currentSelectedDevice struct {
	retained          generation.ResolvedDevice
	device, host      inventory.Device
	adapter           inventory.Adapter
	hostTarget        config.Target
	nodeName, dpuName string
	dpuSet            *unstructured.Unstructured
}

func (s currentSelectedDevice) deviceName() string { return strings.ToLower(s.retained.Serial) }
func currentSelection(s activationSelectedDevice) currentSelectedDevice {
	return currentSelectedDevice{retained: s.item.device, device: s.device, host: s.host, adapter: s.adapter, hostTarget: s.hostTarget, nodeName: s.nodeName, dpuName: s.dpuName, dpuSet: s.dpuSet}
}

type currentRuntimeGraph struct {
	deployment                   *unstructured.Unstructured
	selected                     []currentSelectedDevice
	sets, dpus, nodes, devices   map[string]*unstructured.Unstructured
	services, chains, interfaces []unstructured.Unstructured
}

func retainedValidationDeployment(plan *generation.Plan) (*unstructured.Unstructured, error) {
	if plan == nil {
		return nil, fmt.Errorf("retained plan required")
	}
	stage, ok := retainedPlanStage(plan, generation.StageDeployment)
	if !ok || len(stage.Documents()) != 1 {
		return nil, fmt.Errorf("retained deployment stage incomplete")
	}
	object := stage.Documents()[0].ObjectCopy()
	if object == nil || object.GetKind() != "DPUDeployment" || object.GetNamespace() != plan.Intent().Namespace {
		return nil, fmt.Errorf("retained deployment identity invalid")
	}
	return object, nil
}

func validationSelectedHosts(desired *managementDesired, request config.Request, expected *inventory.Inventory) ([]currentSelectedDevice, error) {
	if expected == nil {
		return nil, fmt.Errorf("immutable retained physical expectations unavailable")
	}
	deployment, err := retainedValidationDeployment(desired.plan)
	if err != nil {
		return nil, err
	}
	intent := desired.plan.Intent()
	// Missing host credentials do not change retained selected scope. Bind the
	// association with its declared endpoint; transport availability is separate.
	targets := append([]config.Target(nil), request.Targets...)
	if expected != nil {
		for _, host := range expected.Hosts {
			found := false
			for _, target := range targets {
				if target.Role == "hosts" && target.InputRef == host.InputRef && target.Endpoint == host.SSHEndpoint {
					found = true
				}
			}
			if !found {
				targets = append(targets, config.Target{Role: "hosts", InputRef: host.InputRef, Endpoint: host.SSHEndpoint})
			}
		}
	}
	result, _, err := bindCurrentHostAssociations(desired.rules, intent.Devices, expected, targets, deployment, false)
	return result, err
}

func observeCurrentRuntimeGraph(ctx context.Context, reader activationObservationReader, desired *managementDesired, selected []currentSelectedDevice) (currentRuntimeGraph, error) {
	graph := currentRuntimeGraph{selected: selected, sets: map[string]*unstructured.Unstructured{}, dpus: map[string]*unstructured.Unstructured{}, nodes: map[string]*unstructured.Unstructured{}, devices: map[string]*unstructured.Unstructured{}}
	if reader == nil || desired == nil || len(selected) == 0 {
		return graph, fmt.Errorf("selected current graph input incomplete")
	}
	if err := reader.VerifyIdentity(ctx); err != nil {
		return graph, err
	}
	expected, err := retainedValidationDeployment(desired.plan)
	if err != nil {
		return graph, err
	}
	graph.deployment, err = reader.Get(ctx, expected.DeepCopy())
	if err != nil {
		return graph, err
	}
	if !currentObjectIdentity(graph.deployment, expected) || desired.verifier.Verify(expected, graph.deployment) != nil {
		return graph, fmt.Errorf("current deployment is not desired and ready")
	}
	lists := map[string][]unstructured.Unstructured{}
	for _, kind := range []string{"DPUSet", "DPU", "DPUNodeMaintenance", "DPUService", "DPUServiceChain", "DPUServiceInterface"} {
		api := "provisioning.dpu.nvidia.com/v1alpha1"
		if strings.HasPrefix(kind, "DPUService") {
			api = "svc.dpu.nvidia.com/v1alpha1"
		}
		rows, err := reader.List(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": api, "kind": kind, "metadata": map[string]any{"namespace": expected.GetNamespace()}}}, "")
		if err != nil {
			return graph, err
		}
		lists[kind] = rows
	}
	for _, item := range selected {
		set := uniqueObjectByName(lists["DPUSet"], item.dpuSet.GetName())
		dpu := uniqueObjectByName(lists["DPU"], item.dpuName)
		if !currentObjectIdentity(set, item.dpuSet) || !activationOwnerMatches(set, expected.GetAPIVersion(), expected.GetKind(), expected.GetName(), graph.deployment.GetUID()) || !currentActivationDPUSetMatches(desired.rules, set, item) {
			return graph, fmt.Errorf("selected set identity or projection mismatch")
		}
		if dpu == nil || dpu.GetUID() == "" || dpu.GetResourceVersion() == "" || dpu.GetGeneration() < 1 || dpu.GetDeletionTimestamp() != nil || !activationOwnerMatches(dpu, set.GetAPIVersion(), set.GetKind(), set.GetName(), set.GetUID()) || !currentActivationDPUMatches(desired.rules, dpu, item) {
			return graph, fmt.Errorf("selected DPU identity or ownership mismatch")
		}
		node, err := reader.Get(ctx, dpuNodeIdentity(expected.GetNamespace(), item.nodeName))
		if err != nil {
			return graph, err
		}
		if validateCurrentEnrolledNode(node, item.retained, expected.GetNamespace()) != nil {
			return graph, validationFailure(validation.Failed, validation.ReasonHealth, "Selected DPUNode identity or exact device association differs from retained intent.")
		}
		if err := validationRuntimeReadiness(desired.rules.EnrolledNodeReadiness(node), "Selected DPUNode has not reported current enrolled readiness."); err != nil {
			return graph, err
		}
		device, err := reader.Get(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": enrollmentDeviceAPIVersion, "kind": "DPUDevice", "metadata": map[string]any{"name": item.deviceName(), "namespace": expected.GetNamespace()}}})
		if err != nil {
			return graph, err
		}
		if err := validationRuntimeReadiness(desired.rules.ResourceReadiness(device), "Selected DPUDevice has not reported current readiness."); err != nil {
			return graph, err
		}
		if validateCurrentEnrolledDevice(desired.rules, device, item.retained, expected.GetNamespace(), true) != nil {
			return graph, validationFailure(validation.Failed, validation.ReasonHealth, "Current DPUDevice serial, endpoint or hardware identity differs from retained intent.")
		}
		if err := currentDeviceSettled(desired.rules, dpu, node, lists["DPUNodeMaintenance"]); err != nil {
			return graph, err
		}
		if activationNativeNodeEffectPolicy(dpu, "spec", "nodeEffect") != nil || !activationNodeEffectMatches(dpu, set) {
			return graph, fmt.Errorf("selected DPU node effect differs from desired")
		}
		setLabels, _, e := unstructured.NestedStringMap(set.Object, "spec", "dpuTemplate", "spec", "cluster", "nodeLabels")
		dpuLabels, _, e2 := unstructured.NestedStringMap(dpu.Object, "spec", "cluster", "nodeLabels")
		if e != nil || e2 != nil || !activationDPUClusterLabelsMatch(setLabels, dpuLabels, item.nodeName, item.dpuSet.GetNamespace(), desired.operatorVersion) {
			return graph, fmt.Errorf("selected DPU cluster labels differ")
		}
		tenant := desired.plan.Intent().Bootstrap.Tenant
		name, _, _ := unstructured.NestedString(dpu.Object, "spec", "cluster", "name")
		ns, _, _ := unstructured.NestedString(dpu.Object, "spec", "cluster", "namespace")
		if name != tenant.Name || ns != tenant.Namespace {
			return graph, fmt.Errorf("selected DPU cluster differs")
		}
		graph.sets[set.GetName()] = set
		graph.dpus[dpu.GetName()] = dpu
		graph.nodes[node.GetName()] = node
		graph.devices[device.GetName()] = device
	}
	if err := verifyCurrentDPUSetMembership(desired, graph, lists["DPUSet"]); err != nil {
		return graph, err
	}
	// A complete list must not admit extra children participating in a selected
	// set. Other deployments remain outside this graph.
	for i := range lists["DPU"] {
		row := &lists["DPU"][i]
		for _, item := range selected {
			set := graph.sets[item.dpuSet.GetName()]
			if activationOwnerMatches(row, set.GetAPIVersion(), set.GetKind(), set.GetName(), set.GetUID()) && row.GetName() != item.dpuName {
				return graph, fmt.Errorf("selected set contains an extra DPU child")
			}
		}
	}
	version, complete, err := compileCurrentVersionGraph(desired.plan, desired.rules, graph.deployment, graph.sets, lists["DPUService"], lists["DPUServiceChain"])
	if err != nil || !complete {
		return graph, fmt.Errorf("current controller version graph incomplete")
	}
	for _, item := range selected {
		if !activationAdditionalRequestorsMatch(graph.sets[item.dpuSet.GetName()], version.requestors, "spec", "dpuTemplate", "spec", "nodeEffect") || !activationAdditionalRequestorsMatch(graph.dpus[item.dpuName], version.requestors, "spec", "nodeEffect") {
			return graph, fmt.Errorf("current node effect requestors differ")
		}
	}
	graph.services = lists["DPUService"]
	graph.chains = lists["DPUServiceChain"]
	graph.interfaces = lists["DPUServiceInterface"]
	return graph, reader.VerifyIdentity(ctx)
}

func verifyCurrentDPUSetMembership(desired *managementDesired, graph currentRuntimeGraph, sets []unstructured.Unstructured) error {
	deployment := graph.deployment
	expected, err := desired.rules.ActivationSetIntents(deployment, desired.plan.Intent().Devices)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for _, set := range expected {
		names[set.GetName()] = true
	}
	for i := range sets {
		set := &sets[i]
		if set.GetNamespace() != deployment.GetNamespace() {
			continue
		}
		owned := activationOwnerMatches(set, deployment.GetAPIVersion(), deployment.GetKind(), deployment.GetName(), deployment.GetUID())
		if owned && !names[set.GetName()] {
			return validationFailure(validation.Failed, validation.ReasonHealth, "An additional DPUSet is owned by the selected deployment.")
		}
		nodeSelector, nodeErr := currentDPUSetLabelSelector(set, "dpuNodeSelector")
		deviceSelector, deviceErr := currentDPUSetLabelSelector(set, "dpuDeviceSelector")
		// The pinned controller falls back to the deprecated equality map only
		// when its modern device selector is absent, and otherwise selects all.
		modern, _, _ := unstructured.NestedFieldNoCopy(set.Object, "spec", "dpuDeviceSelector")
		if modern == nil {
			legacy, _, legacyErr := unstructured.NestedStringMap(set.Object, "spec", "dpuSelector")
			if legacyErr != nil {
				deviceErr = legacyErr
			} else {
				deviceSelector, deviceErr = metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: legacy})
			}
		}
		participates := false
		for _, item := range graph.selected {
			device, node := graph.devices[item.deviceName()], graph.nodes[item.nodeName]
			if device == nil || node == nil {
				return fmt.Errorf("selected current selector inputs unavailable")
			}
			deviceLabels, _, deviceLabelsErr := unstructured.NestedStringMap(device.Object, "metadata", "labels")
			nodeLabels, _, nodeLabelsErr := unstructured.NestedStringMap(node.Object, "metadata", "labels")
			if deviceLabelsErr != nil || nodeLabelsErr != nil {
				return validationFailure(validation.Failed, validation.ReasonHealth, "Selected current device or node labels are malformed.")
			}
			// A proven disjoint axis makes the set unrelated even if the other axis
			// cannot be parsed. Relevant unresolved selection must never be ignored.
			if nodeErr == nil && !nodeSelector.Matches(labels.Set(nodeLabels)) || deviceErr == nil && !deviceSelector.Matches(labels.Set(deviceLabels)) {
				continue
			}
			if nodeErr != nil || deviceErr != nil {
				return validationFailure(validation.Failed, validation.ReasonHealth, "A potentially overlapping DPUSet has a malformed selector.")
			}
			participates = true
		}
		if participates && (!owned || !names[set.GetName()]) {
			return validationFailure(validation.Failed, validation.ReasonHealth, "An additional or foreign DPUSet participates in the retained selected device and node selection.")
		}
	}
	return nil
}

func currentDPUSetLabelSelector(set *unstructured.Unstructured, field string) (labels.Selector, error) {
	raw, _, err := unstructured.NestedMap(set.Object, "spec", field)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return labels.Everything(), nil
	}
	for key := range raw {
		if key != "matchLabels" && key != "matchExpressions" {
			return nil, fmt.Errorf("unrecognized label selector field")
		}
	}
	var selector metav1.LabelSelector
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &selector); err != nil {
		return nil, err
	}
	return metav1.LabelSelectorAsSelector(&selector)
}

func currentObjectIdentity(current, expected *unstructured.Unstructured) bool {
	return current != nil && expected != nil && current.GroupVersionKind() == expected.GroupVersionKind() && current.GetName() == expected.GetName() && current.GetNamespace() == expected.GetNamespace() && current.GetUID() != "" && current.GetResourceVersion() != "" && current.GetGeneration() > 0 && current.GetDeletionTimestamp() == nil
}

// A completed hold need not exist. Current readiness never proves this process
// released a hold or rebooted a host, and deploy still requires its own history.
func currentDeviceSettled(rules *controllercontract.Rules, dpu, node *unstructured.Unstructured, maintenance []unstructured.Unstructured) error {
	if dpu == nil || node == nil {
		return fmt.Errorf("selected DPU or node unavailable")
	}
	if err := validationRuntimeReadiness(rules.ResourceReadiness(dpu), "Selected DPU has not reached Ready at its current generation."); err != nil {
		return err
	}
	if err := validationRuntimeReadiness(rules.NodeEffectRemoved(dpu), "Selected DPU has not reported NodeEffectRemoved at its current generation."); err != nil {
		return err
	}
	if _, pending := node.GetAnnotations()[activationExternalRebootKey]; pending {
		return validationFailure(validation.Pending, validation.ReasonProgressing, "Selected node still reports a pending external reboot.")
	}
	for i := range maintenance {
		row := &maintenance[i]
		name, _, err := unstructured.NestedString(row.Object, "spec", "dpuNodeName")
		if err != nil {
			return fmt.Errorf("maintenance node binding malformed")
		}
		if name == node.GetName() || row.GetLabels()[activationDPUNodeLabel] == node.GetName() || row.GetName() == node.GetName()+"-hold" {
			return validationFailure(validation.Pending, validation.ReasonProgressing, "Selected node still has active or deleting maintenance; observation does not release it.")
		}
	}
	return nil
}

func currentGraphLeases(desired *managementDesired, graph currentRuntimeGraph, snapshot hostWorkloadLeaseSnapshot) ([]hostWorkloadLease, error) {
	ipam := snapshot.DPUServiceIPAM
	pool := snapshot.CIDRPool
	if ipam == nil || pool == nil {
		return nil, fmt.Errorf("current allocation sources unavailable")
	}
	expected, err := retainedWorkloadIPAM(desired.plan)
	if err != nil {
		return nil, err
	}
	if !currentObjectIdentity(ipam, expected) || desired.verifier.Verify(expected, ipam) != nil {
		return nil, validationFailure(validation.Failed, validation.ReasonHealth, "Current workload IPAM differs from retained intent.")
	}
	if err := validationRuntimeReadiness(desired.rules.WorkloadReadiness(ipam, controllercontract.WorkloadIPAM), "Current workload IPAM has not reconciled its current generation."); err != nil {
		return nil, err
	}
	tenant, _, err := tenantPlanInputs(desired.plan)
	if err != nil {
		return nil, err
	}
	if verifyWorkloadChild(ipam, pool, tenant) != nil {
		return nil, fmt.Errorf("current pool projection differs")
	}
	network, size, gateway, err := retainedWorkloadNetwork(ipam)
	if err != nil {
		return nil, err
	}
	allocations, err := hostLeaseAllocations(pool, network, size, gateway)
	if err != nil {
		return nil, err
	}
	intent := desired.plan.Intent()
	reservations, err := hbn.HostReservations(intent.Networking, intent.Generation)
	if err != nil {
		return nil, err
	}
	leases := make([]hostWorkloadLease, 0, len(graph.selected))
	seen := map[string]bool{}
	for _, item := range graph.selected {
		node := graph.nodes[item.nodeName]
		dpu := graph.dpus[item.dpuName]
		tenantNode, err := uniqueTenantNode(desired.rules, snapshot.TenantNodes, node.GetName(), node.GetNamespace())
		if err != nil {
			return nil, err
		}
		if seen[tenantNode.GetName()] {
			return nil, fmt.Errorf("tenant node allocation association ambiguous")
		}
		seen[tenantNode.GetName()] = true
		lease, err := currentHostLease(desired.rules, item.retained.Serial, dpu, node, snapshot.TenantNodes, pool, allocations, reservations, intent.Generation.HostAddressIndex)
		if err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	sort.Slice(leases, func(i, j int) bool { return leases[i].Serial < leases[j].Serial })
	return leases, nil
}

func currentGraphDigest(graph currentRuntimeGraph) string {
	type anchor struct {
		Kind, Name, UID     string
		Generation          int64
		Spec                any
		Labels, Annotations map[string]string
		Owners              any
	}
	rows := []anchor{}
	add := func(o *unstructured.Unstructured) {
		rows = append(rows, anchor{o.GetKind(), o.GetName(), string(o.GetUID()), o.GetGeneration(), o.Object["spec"], o.GetLabels(), o.GetAnnotations(), o.GetOwnerReferences()})
	}
	add(graph.deployment)
	for _, group := range []map[string]*unstructured.Unstructured{graph.sets, graph.dpus, graph.nodes, graph.devices} {
		for _, o := range group {
			add(o)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Kind != rows[j].Kind {
			return rows[i].Kind < rows[j].Kind
		}
		return rows[i].Name < rows[j].Name
	})
	return hbnBindingDigest(rows)
}

func sameCurrentLeaseSnapshot(a, b hostWorkloadLeaseSnapshot) bool {
	if a.DPUServiceIPAM == nil || b.DPUServiceIPAM == nil || a.CIDRPool == nil || b.CIDRPool == nil {
		return false
	}
	return a.DPUServiceIPAM.GetUID() == b.DPUServiceIPAM.GetUID() && a.DPUServiceIPAM.GetGeneration() == b.DPUServiceIPAM.GetGeneration() && reflect.DeepEqual(a.DPUServiceIPAM.Object["spec"], b.DPUServiceIPAM.Object["spec"]) && sameHostLeasePoolObservation(a.CIDRPool, b.CIDRPool)
}

func validationRuntimeReadiness(ready controllercontract.Readiness, summary string) error {
	err := validationReady(ready)
	if err == nil {
		return nil
	}
	var check *validationCheckError
	if errors.As(err, &check) {
		return validationFailure(check.state, check.reason, summary)
	}
	return err
}
