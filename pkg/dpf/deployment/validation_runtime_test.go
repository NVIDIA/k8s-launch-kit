// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func minimalValidationCohort() validationCurrentCohort {
	return validationCurrentCohort{graph: currentRuntimeGraph{deployment: &unstructured.Unstructured{Object: map[string]any{"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": "DPUDeployment", "metadata": map[string]any{"name": "selected", "uid": "deployment-uid", "generation": int64(1)}, "spec": map[string]any{"selected": true}}}, sets: map[string]*unstructured.Unstructured{}, dpus: map[string]*unstructured.Unstructured{}, nodes: map[string]*unstructured.Unstructured{}}, leases: []hostWorkloadLease{{Serial: "SERIAL-A", Prefix: "192.0.2.0/26", ReservedHostAddress: "192.0.2.2"}, {Serial: "SERIAL-B", Prefix: "192.0.2.64/26", ReservedHostAddress: "192.0.2.66"}}, bindings: validationHBNBindings{hbnRuntimeBindingSnapshot: hbnRuntimeBindingSnapshot{anchors: map[string]hbnPodBindingAnchor{"SERIAL-A": {PodName: "hbn-a", PodNamespace: "dpf"}, "SERIAL-B": {PodName: "hbn-b", PodNamespace: "dpf"}}, sourceDigests: map[string]string{"selected": "anchor"}}, unavailable: map[string]validation.Reason{}}}
}

func standaloneValidationSession() (*validationRuntimeSession, *int) {
	anchor := minimalValidationCohort()
	reads := 0
	session := &validationRuntimeSession{anchor: anchor, observe: func(context.Context) (validationCurrentCohort, error) { reads++; return anchor, nil }, asns: map[validation.DeviceID]uint32{"SERIAL-A": 65101, "SERIAL-B": 65102}, fabricASN: 65000, timeout: time.Second, hosts: map[validation.HostID]validationHostRequest{}}
	for i, serial := range []string{"SERIAL-A", "SERIAL-B"} {
		lease := anchor.leases[i]
		session.hosts[validation.HostID(serial)] = validationHostRequest{lease: lease, request: sshprobe.HostNetworkRequest{Identity: sshprobe.HostNetworkIdentity{AdapterSerial: serial, InterfaceName: "workload0"}, MTU: 1500, HostAddressPrefix: lease.ReservedHostAddress + "/26", Gateway: map[int]string{0: "192.0.2.1", 1: "192.0.2.65"}[i], WorkloadCIDR: "192.0.2.0/24"}}
	}
	session.scope = validation.RuntimeScope{Devices: []validation.DeviceID{"SERIAL-A", "SERIAL-B"}, Hosts: []validation.HostID{"SERIAL-A", "SERIAL-B"}, Routes: []validation.DirectedDevicePair{{Source: "SERIAL-A", Destination: "SERIAL-B"}, {Source: "SERIAL-B", Destination: "SERIAL-A"}}, Traffic: []validation.DirectedHostPair{{Source: "SERIAL-A", Destination: "SERIAL-B"}, {Source: "SERIAL-B", Destination: "SERIAL-A"}}}
	return session, &reads
}

func TestValidationRuntimeHealthFailuresKeepIndependentMeasurements(t *testing.T) {
	session, fences := standaloneValidationSession()
	commands, packets := 0, 0
	session.read = func(_ context.Context, _ string, _ string, command hbnRuntimeCommand, prefix netip.Prefix) ([]byte, error) {
		commands++
		if command == hbnReadBGPSummary {
			return []byte(`{"as":65101,"vrfName":"default","peerCount":2,"displayedPeers":2,"totalPeers":2,"failedPeers":2,"peers":{"p0_if":{"remoteAs":65000,"localAs":65101,"state":"Idle","idType":"interface"},"p1_if":{"remoteAs":65000,"localAs":65101,"state":"Idle","idType":"interface"}}}`), nil
		}
		return []byte(`{"192.0.2.64/26":[]}`), nil
	}
	session.probe = func(_ context.Context, _ config.Target, request sshprobe.HostTrafficRequest, _ time.Duration) (sshprobe.HostTrafficResult, error) {
		packets++
		return runtimePassedTraffic(request), nil
	}
	bgp, err := session.ObserveBGP(context.Background(), "SERIAL-A")
	if err != nil || bgp.Outcome.State != validation.Failed {
		t.Fatalf("BGP=%+v err=%v", bgp, err)
	}
	route, err := session.ObserveRoute(context.Background(), validation.DirectedDevicePair{Source: "SERIAL-A", Destination: "SERIAL-B"})
	if err != nil || route.Outcome.State != validation.Failed {
		t.Fatalf("route=%+v err=%v", route, err)
	}
	traffic, err := session.ProbeTraffic(context.Background(), validation.DirectedHostPair{Source: "SERIAL-A", Destination: "SERIAL-B"})
	if err != nil || traffic.Ordinary.State != validation.Passed || traffic.DF.State != validation.Passed {
		t.Fatalf("traffic=%+v err=%v", traffic, err)
	}
	if err := session.VerifyCohort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if commands != 2 || packets != 1 || *fences != 7 || session.poisoned != nil {
		t.Fatalf("commands=%d traffic=%d fences=%d poison=%v", commands, packets, *fences, session.poisoned)
	}
}

func TestValidationRuntimeDriftPermanentlyPoisonsSession(t *testing.T) {
	for _, driftAfter := range []int{1, 2} {
		t.Run(fmt.Sprint(driftAfter), func(t *testing.T) {
			session, _ := standaloneValidationSession()
			calls, measurements := 0, 0
			original := session.anchor
			session.observe = func(context.Context) (validationCurrentCohort, error) {
				calls++
				current := original
				if calls == driftAfter {
					current.leases = append([]hostWorkloadLease(nil), original.leases...)
					current.leases[0].PoolUID = "replacement"
				}
				return current, nil
			}
			session.read = func(context.Context, string, string, hbnRuntimeCommand, netip.Prefix) ([]byte, error) {
				measurements++
				return nil, errors.New("independent read failure")
			}
			_, err := session.ObserveBGP(context.Background(), "SERIAL-A")
			var classified *validation.ObservationError
			if !errors.As(err, &classified) || classified.Category != validation.FatalIdentity {
				t.Fatalf("drift error=%v", err)
			}
			if _, err := session.ObserveBGP(context.Background(), "SERIAL-B"); err == nil {
				t.Fatal("poisoned session resumed")
			}
			if err := session.VerifyCohort(context.Background()); err == nil {
				t.Fatal("poisoned final fence passed")
			}
			if calls != driftAfter || measurements != driftAfter-1 {
				t.Fatalf("calls=%d measurements=%d", calls, measurements)
			}
		})
	}
}

func TestValidationRuntimeUnavailablePodDoesNotShrinkScope(t *testing.T) {
	session, _ := standaloneValidationSession()
	delete(session.anchor.bindings.anchors, "SERIAL-A")
	session.anchor.bindings.unavailable["SERIAL-A"] = validation.ReasonDependency
	stable := session.anchor
	session.observe = func(context.Context) (validationCurrentCohort, error) { return stable, nil }
	calls := 0
	session.read = func(context.Context, string, string, hbnRuntimeCommand, netip.Prefix) ([]byte, error) {
		calls++
		return nil, errors.New("read unavailable")
	}
	blocked, err := session.ObserveBGP(context.Background(), "SERIAL-A")
	if err != nil || blocked.Outcome.State != validation.Unverified || blocked.Outcome.Reason != validation.ReasonDependency {
		t.Fatalf("blocked=%+v err=%v", blocked, err)
	}
	_, err = session.ObserveBGP(context.Background(), "SERIAL-B")
	if err != nil || calls != 1 {
		t.Fatalf("independent source not measured: calls=%d err=%v", calls, err)
	}
	scope := session.Scope()
	scope.Devices[0] = "changed"
	scope.Traffic[0].Source = "changed"
	if len(session.Scope().Devices) != 2 || session.Scope().Devices[0] != "SERIAL-A" || session.Scope().Traffic[0].Source != "SERIAL-A" {
		t.Fatal("scope reduced or mutable")
	}
}

func TestValidationRuntimeCancellationStopsBeforeMeasurement(t *testing.T) {
	session, fences := standaloneValidationSession()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	session.read = func(context.Context, string, string, hbnRuntimeCommand, netip.Prefix) ([]byte, error) {
		calls++
		return nil, nil
	}
	_, err := session.ObserveBGP(ctx, "SERIAL-A")
	var classified *validation.ObservationError
	if !errors.As(err, &classified) || classified.Category != validation.FatalCanceled || calls != 0 || *fences != 0 {
		t.Fatalf("error=%v reads=%d fences=%d", err, calls, *fences)
	}
}

func TestValidationRuntimeFinalPhysicalFencePrecedesFinalAPIRead(t *testing.T) {
	session, _ := standaloneValidationSession()
	order := []string{}
	session.verifyPhysical = func(context.Context) error { order = append(order, "physical"); return nil }
	stable := session.anchor
	session.observe = func(context.Context) (validationCurrentCohort, error) {
		order = append(order, "api")
		return stable, nil
	}
	if err := session.VerifyCohort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"physical", "api"}) {
		t.Fatalf("final order=%v", order)
	}
	order = nil
	var classified *validation.ObservationError
	var absentContext context.Context
	if err := session.VerifyCohort(absentContext); !errors.As(err, &classified) || classified.Category != validation.FatalInput || len(order) != 0 {
		t.Fatalf("nil final context reached transport: err=%v order=%v", err, order)
	}
}

func TestCurrentSettledDeviceNeedsNoHistoricalHoldReceipt(t *testing.T) {
	rules := testRules(t)
	dpu := &unstructured.Unstructured{Object: map[string]any{"apiVersion": enrollmentDeviceAPIVersion, "kind": "DPU", "metadata": map[string]any{"name": "selected", "generation": int64(2)}, "status": map[string]any{"phase": "Ready", "observedGeneration": int64(2), "conditions": []any{conditionMap("Ready", "True", "DPUReady", 2), conditionMap("NodeEffectRemoved", "True", "Removed", 2)}}}}
	node := dpuNodeIdentity("dpf", "selected-node")
	if err := currentDeviceSettled(rules, dpu, node, nil); err != nil {
		t.Fatalf("fresh completed hold-free cluster rejected: %v", err)
	}
	if activationRemovalComplete(rules, activationObservation{dpus: map[string]*unstructured.Unstructured{"selected": dpu}}, activationNodeProgress{}) {
		t.Fatal("deployment historical gate was weakened")
	}
	for _, test := range []struct {
		name   string
		mutate func(*unstructured.Unstructured, *unstructured.Unstructured)
		holds  []unstructured.Unstructured
	}{
		{name: "pending reboot", mutate: func(_ *unstructured.Unstructured, node *unstructured.Unstructured) {
			node.SetAnnotations(map[string]string{activationExternalRebootKey: "true"})
		}},
		{name: "stale effect removed", mutate: func(dpu, _ *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(dpu.Object, []any{conditionMap("Ready", "True", "DPUReady", 2), conditionMap("NodeEffectRemoved", "True", "Removed", 1)}, "status", "conditions")
		}},
		{name: "future effect removed", mutate: func(dpu, _ *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(dpu.Object, []any{conditionMap("Ready", "True", "DPUReady", 2), conditionMap("NodeEffectRemoved", "True", "Removed", 3)}, "status", "conditions")
		}},
		{name: "active hold", mutate: func(*unstructured.Unstructured, *unstructured.Unstructured) {}, holds: []unstructured.Unstructured{{Object: map[string]any{"metadata": map[string]any{"name": "selected-node-hold"}, "spec": map[string]any{"dpuNodeName": "selected-node"}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, n := dpu.DeepCopy(), node.DeepCopy()
			test.mutate(d, n)
			if err := currentDeviceSettled(rules, d, n, test.holds); err == nil {
				t.Fatal("unsettled selected node passed")
			}
		})
	}
}

func TestCurrentAllocationJoinSharesDeploymentCoreWithoutHistory(t *testing.T) {
	base := newActivationIntegrationFixture(t)
	fixture := makeHostLeaseResolverFixture(t, base)
	desired, err := prepareManagementDesiredFromPlan(base.prepared.execution.retained, fixture.plan)
	if err != nil {
		t.Fatal(err)
	}
	graph := currentRuntimeGraph{selected: make([]currentSelectedDevice, len(fixture.selected)), sets: fixture.activation.sets, dpus: fixture.activation.dpus, nodes: fixture.activation.nodes}
	for i, item := range fixture.selected {
		graph.selected[i] = currentSelection(item)
	}
	actual, err := currentGraphLeases(desired, graph, fixture.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	historic, err := resolveHostWorkloadLeases(fixture.plan, fixture.reconciler, fixture.selected, fixture.activation, fixture.workload.BundleDigest, fixture.workload.Resources, fixture.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, historic) {
		t.Fatalf("fresh=%+v historical=%+v", actual, historic)
	}
	ambiguous := fixture.snapshot
	ambiguous.TenantNodes = append(cloneUnstructuredNodes(fixture.snapshot.TenantNodes), *fixture.snapshot.TenantNodes[0].DeepCopy())
	ambiguous.TenantNodes[len(ambiguous.TenantNodes)-1].SetName("duplicate-selected-node")
	if _, err := currentGraphLeases(desired, graph, ambiguous); err == nil {
		t.Fatal("ambiguous current node allocation passed")
	}
}

type currentGraphReadFixture struct {
	objects map[string]*unstructured.Unstructured
	listErr error
}

func (f *currentGraphReadFixture) Get(_ context.Context, want *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	object := f.objects[tenantStageKey(want)]
	if object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: want.GetKind()}, want.GetName())
	}
	return object.DeepCopy(), nil
}
func (f *currentGraphReadFixture) List(_ context.Context, want *unstructured.Unstructured, _ string) ([]unstructured.Unstructured, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	result := []unstructured.Unstructured{}
	for _, object := range f.objects {
		if object.GetKind() == want.GetKind() && object.GetNamespace() == want.GetNamespace() {
			result = append(result, *object.DeepCopy())
		}
	}
	return result, nil
}
func (*currentGraphReadFixture) VerifyIdentity(context.Context) error { return nil }

func TestValidationFreshCurrentGraphNoJournalOrHistoricalProof(t *testing.T) {
	base, _, _ := hostNetworkStageFixture(t)
	desired, err := prepareManagementDesiredFromPlan(base.prepared.execution.retained, base.prepared.execution.plan)
	if err != nil {
		t.Fatal(err)
	}
	wanted, err := retainedValidationDeployment(desired.plan)
	if err != nil {
		t.Fatal(err)
	}
	selected, _, err := bindCurrentHostAssociations(desired.rules, desired.plan.Intent().Devices, base.inventory, base.request.Targets, wanted, false)
	if err != nil {
		t.Fatal(err)
	}
	reader := &currentGraphReadFixture{objects: map[string]*unstructured.Unstructured{}}
	for key, object := range base.client.base.resources {
		reader.objects[key] = object.DeepCopy()
	}
	reader.objects[tenantStageKey(base.client.deployment)] = base.client.deployment.DeepCopy()
	for _, collection := range [][]unstructured.Unstructured{base.client.sets, base.client.dpus, base.client.maint} {
		for i := range collection {
			object := collection[i].DeepCopy()
			reader.objects[tenantStageKey(object)] = object
		}
	}
	for _, object := range base.client.versions {
		reader.objects[tenantStageKey(&object)] = object.DeepCopy()
	}
	first, err := observeCurrentRuntimeGraph(base.ctx, reader, desired, selected)
	if err != nil {
		t.Fatal(err)
	}
	// Current bindings have no operation, activation progress, enrollment receipt
	// or journal fields. Routine RV churn preserves semantic acceptance.
	for _, object := range reader.objects {
		object.SetResourceVersion("changed-status-rv")
	}
	second, err := observeCurrentRuntimeGraph(base.ctx, reader, desired, selected)
	if err != nil {
		t.Fatal(err)
	}
	if currentGraphDigest(first) != currentGraphDigest(second) {
		t.Fatal("routine resourceVersion churn changed current semantic cohort")
	}
	t.Run("known pending DPU settles on next snapshot", func(t *testing.T) {
		key := "DPU/" + wanted.GetNamespace() + "/" + selected[0].dpuName
		healthy := reader.objects[key].DeepCopy()
		_ = unstructured.SetNestedField(reader.objects[key].Object, "DPU Config", "status", "phase")
		id := validation.CheckID("devices/" + selected[0].retained.Serial)
		observe := func() *validationRecorder {
			r := newValidationRecorder([]validation.CheckSpec{{ID: id, Area: validation.AreaDevices, Subject: selected[0].retained.Serial, Required: true}}, nil)
			_ = r.run(id, func() error { _, err := observeCurrentRuntimeGraph(base.ctx, reader, desired, selected); return err })
			return r
		}
		pending := observe().snapshot().Checks[0]
		if pending.State != validation.Pending || pending.Reason != validation.ReasonProgressing {
			t.Fatalf("pending selected DPU classified as %+v", pending)
		}
		reader.objects[key] = healthy
		settled := observe().snapshot().Checks[0]
		if settled.State != validation.Passed {
			t.Fatalf("fresh settled snapshot classified as %+v", settled)
		}
	})
	for _, test := range []struct {
		name   string
		mutate func(*currentGraphReadFixture)
	}{
		{name: "replaced DD owner", mutate: func(reader *currentGraphReadFixture) {
			reader.objects[tenantStageKey(base.client.deployment)].SetUID("replacement-dd")
		}},
		{name: "replaced DPU owner", mutate: func(reader *currentGraphReadFixture) {
			for _, object := range reader.objects {
				if object.GetKind() == "DPUSet" {
					object.SetUID("replacement-set")
				}
			}
		}},
		{name: "denied required list", mutate: func(reader *currentGraphReadFixture) {
			reader.listErr = apierrors.NewForbidden(schema.GroupResource{Group: "provisioning.dpu.nvidia.com", Resource: "dpus"}, "", errors.New("denied"))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := &currentGraphReadFixture{objects: map[string]*unstructured.Unstructured{}}
			for key, object := range reader.objects {
				copy.objects[key] = object.DeepCopy()
			}
			test.mutate(copy)
			if _, err := observeCurrentRuntimeGraph(base.ctx, copy, desired, selected); err == nil {
				t.Fatal("inconsistent current graph passed")
			}
		})
	}
}

func TestValidationHostDiagnosticsClassifyMismatchAndSafeUnavailable(t *testing.T) {
	for _, test := range []struct {
		code   string
		state  validation.State
		reason validation.Reason
	}{
		{"HOST_NETWORK_RUNTIME_MISMATCH", validation.Failed, validation.ReasonHealth},
		{"HOST_NETWORK_PERSISTENT_MISMATCH", validation.Failed, validation.ReasonHealth},
		{"HOST_NETWORK_TOOL_MISSING_IP", validation.Unverified, validation.ReasonUnavailable},
		{"HOST_NETWORK_SSH_CLEANUP_UNCONFIRMED", validation.Unverified, validation.ReasonUnavailable},
	} {
		err := validationHostNetworkError(&sshprobe.HostNetworkError{ReasonCode: test.code})
		var check *validationCheckError
		if !errors.As(err, &check) || check.state != test.state || check.reason != test.reason || len(check.summary) < len(test.code) {
			t.Fatalf("host code %s classified as %v", test.code, err)
		}
	}
	if err := validationHostNetworkError(&sshprobe.HostNetworkError{ReasonCode: "HOST_NETWORK_OBSERVATION_CHANGED"}); !validationIdentityChanged(err) {
		t.Fatal("host drift did not revoke current cohort")
	}
}
