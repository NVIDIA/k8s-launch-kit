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
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func runtimeCurrentCorrectionFixture(t *testing.T) (*validationObserver, *currentGraphReadFixture, []currentSelectedDevice) {
	t.Helper()
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
	for _, group := range [][]unstructured.Unstructured{base.client.sets, base.client.dpus, base.client.maint} {
		for i := range group {
			object := group[i].DeepCopy()
			reader.objects[tenantStageKey(object)] = object
		}
	}
	for _, object := range base.client.versions {
		reader.objects[tenantStageKey(&object)] = object.DeepCopy()
	}
	return &validationObserver{desired: desired, request: base.request, physicalScope: validationPhysicalScope{expected: base.inventory}, mode: validation.ModeResources}, reader, selected
}

func TestValidationCurrentSetMembershipRejectsExtraAndAllowsUnrelated(t *testing.T) {
	o, reader, selected := runtimeCurrentCorrectionFixture(t)
	current := reader.objects[tenantStageKey(selected[0].dpuSet)].DeepCopy()
	for _, test := range []struct {
		name     string
		selected bool
		owned    bool
		reject   bool
	}{
		{"same selected DD child", true, true, true},
		{"DD child with another selector", false, true, true},
		{"foreign owner on selected selector", true, false, true},
		{"unrelated deployment and device", false, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			extra := current.DeepCopy()
			extra.SetName("another-set")
			extra.SetUID("another-set-uid")
			if !test.selected {
				_ = unstructured.SetNestedStringMap(extra.Object, map[string]string{"inventory.example/serial": "unrelated"}, "spec", "dpuDeviceSelector", "matchLabels")
			}
			if !test.owned {
				owners := extra.GetOwnerReferences()
				owners[0].Name = "unrelated"
				owners[0].UID = "unrelated-dd"
				extra.SetOwnerReferences(owners)
			}
			reader.objects[tenantStageKey(extra)] = extra
			defer delete(reader.objects, tenantStageKey(extra))
			_, err := observeCurrentRuntimeGraph(context.Background(), reader, o.desired, selected)
			if (err != nil) != test.reject {
				t.Fatalf("membership rejected=%v err=%v", test.reject, err)
			}
		})
	}
}

func TestValidationSelectedHostsUsesOnlyRetainedPhysicalExpectation(t *testing.T) {
	o, _, _ := runtimeCurrentCorrectionFixture(t)
	expected := o.physicalScope.expected
	before, err := validationSelectedHosts(o.desired, o.request, expected)
	if err != nil {
		t.Fatal(err)
	}
	changed := *expected
	changed.Hosts = append([]inventory.Device(nil), expected.Hosts...)
	changed.Hosts[0].Adapters = append([]inventory.Adapter(nil), expected.Hosts[0].Adapters...)
	changed.Hosts[0].Adapters[0].SerialNumber = "untrusted-current-serial"
	o.request.Input.Site.Inventory = &changed
	after, err := validationSelectedHosts(o.desired, o.request, expected)
	if err != nil {
		t.Fatal(err)
	}
	if after[0].adapter.SerialNumber != before[0].adapter.SerialNumber {
		t.Fatal("caller current inventory redefined selected physical identity")
	}
	if _, err := validationSelectedHosts(o.desired, o.request, nil); err == nil {
		t.Fatal("missing retained physical expectations fell back to caller inventory")
	}
}

func TestValidationHBNMembershipRejectsExpandedNodesAndAllowsUnrelated(t *testing.T) {
	intent, _ := hbnBindingTestIntent()
	ds, pod := bindingFixture(intent, "cluster-dpu-hbn-ds")
	leases := []hostWorkloadLease{{Serial: "SERIAL-A", TenantNodeName: intent.TenantNodeName}}
	for _, test := range []struct {
		name   string
		edit   func(*appsv1.DaemonSet, *corev1.Pod)
		reject bool
	}{
		{"healthy selected singleton", func(*appsv1.DaemonSet, *corev1.Pod) {}, false},
		{"foreign scheduled node", func(_ *appsv1.DaemonSet, p *corev1.Pod) { p.Spec.NodeName = "unselected" }, true},
		{"expanded schedule count", func(d *appsv1.DaemonSet, _ *corev1.Pod) {
			d.Status.DesiredNumberScheduled = 2
			d.Status.CurrentNumberScheduled = 2
			d.Status.UpdatedNumberScheduled = 2
			d.Status.NumberReady = 2
			d.Status.NumberAvailable = 2
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, p := ds.DeepCopy(), pod.DeepCopy()
			test.edit(d, p)
			err := verifyHBNRuntimeMembership(intent, []appsv1.DaemonSet{*d}, []corev1.Pod{*p}, leases)
			if (err != nil) != test.reject {
				t.Fatalf("membership reject=%v err=%v", test.reject, err)
			}
		})
	}
	extra := pod.DeepCopy()
	extra.Name = "foreign-node-pod"
	extra.UID = "extra-pod"
	extra.Spec.NodeName = "unselected"
	if err := verifyHBNRuntimeMembership(intent, []appsv1.DaemonSet{ds}, []corev1.Pod{pod, *extra}, leases); err == nil {
		t.Fatal("same-owner Pod on unselected Node was ignored")
	}
	unrelatedDS := ds.DeepCopy()
	unrelatedDS.Name = "unrelated"
	unrelatedDS.UID = "unrelated-ds"
	unrelatedDS.Labels[hbnServiceIDLabel] = "unrelated"
	unrelatedDS.Spec.Template.Labels[hbnServiceIDLabel] = "unrelated"
	unrelatedPod := extra.DeepCopy()
	unrelatedPod.Labels[hbnServiceIDLabel] = "unrelated"
	owners := unrelatedPod.OwnerReferences
	owners[0].UID = unrelatedDS.UID
	owners[0].Name = unrelatedDS.Name
	unrelatedPod.OwnerReferences = owners
	if err := verifyHBNRuntimeMembership(intent, []appsv1.DaemonSet{ds, *unrelatedDS}, []corev1.Pod{pod, *unrelatedPod}, leases); err != nil {
		t.Fatalf("unrelated service objects became selected: %v", err)
	}
}

type correctionHBNManagement struct {
	*validationReadFake
	app *unstructured.Unstructured
}

type correctionHBNObjects struct {
	validationRuntimeTenantReader
	daemonSets []appsv1.DaemonSet
	pods       []corev1.Pod
}

func (r *correctionHBNObjects) listRuntimeObjects(context.Context, string, string) ([]appsv1.DaemonSet, []corev1.Pod, error) {
	return r.daemonSets, r.pods, nil
}

func (r *correctionHBNManagement) currentValidationReads() validationCurrentManagementReader {
	return r
}

func (r *correctionHBNManagement) getHBNRuntimeApplication(context.Context, string, string) (*unstructured.Unstructured, error) {
	return r.app.DeepCopy(), nil
}

func TestValidationNativeHBNBindingRejectsExtraParticipatingNode(t *testing.T) {
	fixture := newRuntimeStageFixture(t, []string{"SERIAL-A"})
	admitN3cHBNPods(fixture)
	execution := fixture.prepared.execution
	desired, err := prepareManagementDesiredFromPlan(execution.retained, execution.plan)
	if err != nil {
		t.Fatal(err)
	}
	wanted, err := retainedDeploymentObject(execution.verificationInputs())
	if err != nil {
		t.Fatal(err)
	}
	activation, err := observeActivationScope(fixture.ctx, fixture.client, execution.verificationInputs(), wanted, fixture.activation.DeploymentUID, fixture.activation.selected, fixture.activation.progress)
	if err != nil {
		t.Fatal(err)
	}
	leases, err := resolveHostWorkloadLeases(execution.plan, execution.reconciler, fixture.activation.selected, activation, fixture.workload.BundleDigest, fixture.workload.Resources, fixture.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	graph := currentRuntimeGraph{deployment: activation.deployment, services: []unstructured.Unstructured{*fixture.client.service.DeepCopy()}, interfaces: deepCopyUnstructuredList(fixture.client.interfaces)}
	graph.services[0].Object["status"] = map[string]any{"observedGeneration": int64(1), "conditions": []any{conditionMap("Ready", "True", "Ready", 1), conditionMap("ApplicationsReconciled", "True", "Ready", 1), conditionMap("ApplicationsReady", "True", "Ready", 1)}}
	management := &correctionHBNManagement{validationReadFake: &validationReadFake{}, app: fixture.client.app}
	tenant := testTenant(fixture.controlPlane.tenant.Client)
	baseline, err := observeCurrentHBNBindings(fixture.ctx, management, tenant, desired, graph, fixture.snapshot, leases, fixture.image, false)
	if err != nil || len(baseline.anchors) != 1 {
		t.Fatalf("baseline selected membership failed: %v", err)
	}
	unrelatedDS := fixture.ds.DeepCopy()
	unrelatedDS.Name = "unrelated-ds"
	unrelatedDS.UID = "unrelated-ds"
	unrelatedDS.Labels[hbnServiceIDLabel] = "unrelated"
	unrelatedDS.Spec.Template.Labels[hbnServiceIDLabel] = "unrelated"
	unrelatedPod := fixture.pods[0].DeepCopy()
	unrelatedPod.Name = "unrelated-pod"
	unrelatedPod.UID = "unrelated-pod"
	unrelatedPod.Labels[hbnServiceIDLabel] = "unrelated"
	unrelatedPod.Spec.NodeName = "unselected-node"
	unrelatedPod.OwnerReferences[0].Name = unrelatedDS.Name
	unrelatedPod.OwnerReferences[0].UID = unrelatedDS.UID
	unrelated := &correctionHBNObjects{validationRuntimeTenantReader: tenant, daemonSets: []appsv1.DaemonSet{fixture.ds, *unrelatedDS}, pods: []corev1.Pod{fixture.pods[0], *unrelatedPod}}
	withUnrelated, err := observeCurrentHBNBindings(fixture.ctx, management, unrelated, desired, graph, fixture.snapshot, leases, fixture.image, false)
	if err != nil || !reflect.DeepEqual(baseline, withUnrelated) {
		t.Fatalf("unrelated service objects altered selected membership/fence: %v", err)
	}
	extra := fixture.pods[0].DeepCopy()
	extra.Name = "hbn-unselected"
	extra.UID = "unselected-pod"
	extra.Spec.NodeName = "unselected-node"
	fixture.pods = append(fixture.pods, *extra)
	if _, err := observeCurrentHBNBindings(fixture.ctx, management, tenant, desired, graph, fixture.snapshot, leases, fixture.image, false); err == nil {
		t.Fatal("native observer accepted a stable extra same-owner Pod on an unselected Node")
	}
	fixture.ds.Status.DesiredNumberScheduled = 2
	fixture.ds.Status.CurrentNumberScheduled = 2
	fixture.ds.Status.UpdatedNumberScheduled = 2
	fixture.ds.Status.NumberReady = 2
	fixture.ds.Status.NumberAvailable = 2
	if _, err := observeCurrentHBNBindings(fixture.ctx, management, tenant, desired, graph, fixture.snapshot, leases, fixture.image, false); err == nil {
		t.Fatal("native observer accepted expanded DS/Pod coverage against smaller retained selection")
	}
}

type runtimeCorrectionObserver struct {
	snapshot func() validation.ResourceSnapshot
	open     func(context.Context) (validation.RuntimeSession, error)
	calls    int
}

func (o *runtimeCorrectionObserver) ObserveResources(context.Context, validation.Progress) (validation.ResourceSnapshot, error) {
	o.calls++
	return o.snapshot(), nil
}
func (o *runtimeCorrectionObserver) OpenRuntime(ctx context.Context, _ validation.Progress) (validation.RuntimeSession, error) {
	return o.open(ctx)
}

func TestValidationPendingAdapterSnapshotLetsRunnerReachReadySnapshot(t *testing.T) {
	o, reader, selected := runtimeCurrentCorrectionFixture(t)
	key := "DPU/" + o.desired.plan.Intent().Namespace + "/" + selected[0].dpuName
	_ = unstructured.SetNestedField(reader.objects[key].Object, "DPU Config", "status", "phase")
	checks, scope, err := validationRuntimeChecks(o.desired.plan)
	if err != nil {
		t.Fatal(err)
	}
	active := []validation.CheckSpec{}
	for _, check := range checks {
		if !check.FullOnly {
			active = append(active, check)
		}
	}
	recorder := newValidationRecorder(active, nil)
	if err := o.observeValidationRuntimeResources(context.Background(), &validationReadFake{get: reader.Get, list: reader.List}, nil, recorder); err != nil {
		t.Fatal(err)
	}
	recorder.blockRemaining()
	pending := recorder.snapshot()
	for _, row := range pending.Checks {
		if row.State != validation.Pending || row.Reason != validation.ReasonProgressing {
			t.Fatalf("known progressing prerequisite left a nonpollable dependent: %+v", row)
		}
	}
	checks = append(checks, o.validationResourceChecks()...)
	checks = append(checks, o.physicalValidationChecks()...)
	plan, err := validation.NewPlan(validation.PlanInput{Mode: validation.ModeResources, Checks: checks, Runtime: scope})
	if err != nil {
		t.Fatal(err)
	}
	present := map[validation.CheckID]bool{}
	for _, row := range pending.Checks {
		present[row.ID] = true
	}
	ready := validation.ResourceSnapshot{}
	for _, check := range plan.Input().Checks {
		if check.FullOnly {
			continue
		}
		row := validation.Outcome{ID: check.ID, State: validation.Passed}
		ready.Checks = append(ready.Checks, row)
		if !present[check.ID] {
			pending.Checks = append(pending.Checks, row)
		}
	}
	observer := &runtimeCorrectionObserver{}
	observer.snapshot = func() validation.ResourceSnapshot {
		if observer.calls == 1 {
			return pending
		}
		return ready
	}
	runner, err := validation.NewRunner(plan, observer, validation.Options{Wait: time.Second, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	result := runner.Run(context.Background())
	if observer.calls != 2 || result.Verdict != validation.VerdictResourcesPassed {
		t.Fatalf("adapter Pending did not converge via Runner: calls=%d result=%+v", observer.calls, result)
	}
}

func TestValidationPendingDependencyPropagationDoesNotRetryBlockers(t *testing.T) {
	for _, cause := range []error{validationFailure(validation.Failed, validation.ReasonHealth, "Terminal selected failure."), validationFailure(validation.Unverified, validation.ReasonDenied, "Read denied."), validationFailure(validation.Unverified, validation.ReasonUnsupported, "Unsupported selected resource."), errors.New("malformed current object")} {
		r := newValidationRecorder([]validation.CheckSpec{{ID: "hbn/selected", Area: validation.AreaHBN, Subject: "selected", Required: true}}, nil)
		recordRuntimePendingDependencies(r, cause)
		r.blockRemaining()
		if got := r.snapshot().Checks[0]; got.State == validation.Pending {
			t.Fatalf("nonprogressing cause became pollable: %+v", got)
		}
	}
}

func TestValidationHostObservedDriftPoisonsBeforeLaterMeasurements(t *testing.T) {
	for _, code := range []string{"HOST_NETWORK_RUNTIME_CHANGED", "HOST_NETWORK_IDENTITY_CHANGED", "HOST_NETWORK_SOURCE_CHANGED", "HOST_NETWORK_OBSERVATION_CHANGED", "HOST_NETWORK_READ_PCI_BINDING_CHANGED"} {
		t.Run(code, func(t *testing.T) {
			if !validationIdentityChanged(validationHostNetworkError(&sshprobe.HostNetworkError{ReasonCode: code})) {
				t.Fatal("qualification path lost observed drift")
			}
			session, _ := standaloneValidationSession()
			commands := 0
			session.probe = func(context.Context, config.Target, sshprobe.HostTrafficRequest, time.Duration) (sshprobe.HostTrafficResult, error) {
				return sshprobe.HostTrafficResult{}, &sshprobe.HostNetworkError{ReasonCode: code}
			}
			session.read = func(context.Context, string, string, hbnRuntimeCommand, netip.Prefix) ([]byte, error) {
				commands++
				return nil, nil
			}
			_, err := session.ProbeTraffic(context.Background(), validation.DirectedHostPair{Source: "SERIAL-A", Destination: "SERIAL-B"})
			var fatal *validation.ObservationError
			if !errors.As(err, &fatal) || fatal.Category != validation.FatalIdentity || session.poisoned == nil {
				t.Fatalf("typed host drift was swallowed: %v", err)
			}
			if _, err := session.ObserveBGP(context.Background(), "SERIAL-B"); err == nil || commands != 0 {
				t.Fatal("selected exec resumed after observed host drift")
			}
		})
	}
}

func TestValidationHostPacketLossAndAccessRemainIndependent(t *testing.T) {
	for _, test := range []struct {
		name       string
		probeError error
		state      string
	}{
		{"packet loss", &sshprobe.HostNetworkError{ReasonCode: "HOST_NETWORK_TRAFFIC_PING_INCOMPLETE"}, sshprobe.TrafficProbeFailed},
		{"access unavailable", &sshprobe.Error{Code: "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED"}, sshprobe.TrafficProbeUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, _ := standaloneValidationSession()
			commands := 0
			session.probe = func(_ context.Context, _ config.Target, request sshprobe.HostTrafficRequest, _ time.Duration) (sshprobe.HostTrafficResult, error) {
				result := runtimePassedTraffic(request)
				result.State = test.state
				if test.state == sshprobe.TrafficProbeFailed {
					result.Ordinary.State = sshprobe.TrafficProbeFailed
					result.Ordinary.Counters = &sshprobe.PingCounters{Sent: 3, Received: 1, LostPct: 66.6667}
				}
				return result, test.probeError
			}
			session.read = func(context.Context, string, string, hbnRuntimeCommand, netip.Prefix) ([]byte, error) {
				commands++
				return nil, errors.New("unavailable")
			}
			observed, err := session.ProbeTraffic(context.Background(), validation.DirectedHostPair{Source: "SERIAL-A", Destination: "SERIAL-B"})
			if err != nil || session.poisoned != nil {
				t.Fatalf("independent result poisoned session: %+v %v", observed, err)
			}
			if test.state == sshprobe.TrafficProbeUnknown && observed.Ordinary.Reason != validation.ReasonDenied {
				t.Fatalf("access failure was confused with drift or packet loss: %+v", observed)
			}
			if _, err := session.ObserveBGP(context.Background(), "SERIAL-B"); err != nil || commands != 1 {
				t.Fatal("independent selected source was not measured")
			}
		})
	}
}

func TestValidationHostProbeCancellationPoisonsWithoutIdentityClaim(t *testing.T) {
	session, _ := standaloneValidationSession()
	session.probe = func(context.Context, config.Target, sshprobe.HostTrafficRequest, time.Duration) (sshprobe.HostTrafficResult, error) {
		return sshprobe.HostTrafficResult{}, context.Canceled
	}
	_, err := session.ProbeTraffic(context.Background(), validation.DirectedHostPair{Source: "SERIAL-A", Destination: "SERIAL-B"})
	var fatal *validation.ObservationError
	if !errors.As(err, &fatal) || fatal.Category != validation.FatalCanceled || session.poisoned == nil {
		t.Fatalf("canceled probe did not stop safely: %v", err)
	}
	if err := session.VerifyCohort(context.Background()); err == nil {
		t.Fatal("canceled session resumed with a fresh context")
	}
}

func TestValidationHostNetworkQualificationRunsAtEveryBoundary(t *testing.T) {
	session, _ := standaloneValidationSession()
	events := []string{}
	stable := session.anchor
	session.observe = func(context.Context) (validationCurrentCohort, error) {
		events = append(events, "api")
		return stable, nil
	}
	session.verifyHostNetwork = func(context.Context) error { events = append(events, "network"); return nil }
	session.read = func(context.Context, string, string, hbnRuntimeCommand, netip.Prefix) ([]byte, error) {
		events = append(events, "measurement")
		return nil, errors.New("health")
	}
	if _, err := session.ObserveBGP(context.Background(), "SERIAL-A"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(events) != "[network api measurement network api]" {
		t.Fatalf("missing current qualification/API boundary: %v", events)
	}
	session.verifyHostNetwork = func(context.Context) error {
		return fmt.Errorf("persistent source changed: %w", errValidationResourceChanged)
	}
	if err := session.VerifyCohort(context.Background()); err == nil || session.poisoned == nil {
		t.Fatal("post-resource persistent configuration drift retained acceptance")
	}
}

func correctionSessionPlan(t *testing.T, session *validationRuntimeSession) validation.Plan {
	t.Helper()
	checks := []validation.CheckSpec{}
	for _, area := range []validation.Area{validation.AreaManagement, validation.AreaOperator, validation.AreaDefinitions, validation.AreaSystem, validation.AreaTenant, validation.AreaDevices, validation.AreaSoftware, validation.AreaHBN, validation.AreaWorkloads, validation.AreaPhysical} {
		checks = append(checks, validation.CheckSpec{ID: validation.CheckID(string(area) + "/coverage"), Area: area, Subject: string(area), Required: true, Coverage: true, FullOnly: area == validation.AreaPhysical, CohortBound: area != validation.AreaManagement})
	}
	plan, err := validation.NewPlan(validation.PlanInput{Mode: validation.ModeFull, Checks: checks, Runtime: session.Scope()})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestValidationHostDriftReachesRunnerAndRevokesPriorCohortPasses(t *testing.T) {
	session, _ := standaloneValidationSession()
	plan := correctionSessionPlan(t, session)
	session.read = func(_ context.Context, _ string, pod string, command hbnRuntimeCommand, prefix netip.Prefix) ([]byte, error) {
		if command == hbnReadBGPSummary {
			asn := 65101
			if pod == "hbn-b" {
				asn = 65102
			}
			return []byte(fmt.Sprintf(`{"as":%d,"vrfName":"default","peerCount":2,"displayedPeers":2,"totalPeers":2,"failedPeers":0,"peers":{"p0_if":{"remoteAs":65000,"localAs":%d,"state":"Established","idType":"interface"},"p1_if":{"remoteAs":65000,"localAs":%d,"state":"Established","idType":"interface"}}}`, asn, asn, asn)), nil
		}
		return []byte(fmt.Sprintf(`{"%s":[{"protocol":"bgp","selected":true,"destSelected":true,"installed":true,"prefix":"%s","prefixLen":%d,"vrfId":0,"vrfName":"default","table":254,"nexthops":[{"ip":"fe80::2","interfaceName":"p0_if","active":true,"fib":true},{"ip":"fe80::3","interfaceName":"p1_if","active":true,"fib":true}]}]}`, prefix, prefix, prefix.Bits())), nil
	}
	session.probe = func(_ context.Context, _ config.Target, request sshprobe.HostTrafficRequest, _ time.Duration) (sshprobe.HostTrafficResult, error) {
		return runtimePassedTraffic(request), &sshprobe.HostNetworkError{ReasonCode: "HOST_NETWORK_RUNTIME_CHANGED"}
	}
	observer := &runtimeCorrectionObserver{open: func(context.Context) (validation.RuntimeSession, error) { return session, nil }}
	observer.snapshot = func() validation.ResourceSnapshot {
		snapshot := validation.ResourceSnapshot{}
		for _, check := range plan.Input().Checks {
			snapshot.Checks = append(snapshot.Checks, validation.Outcome{ID: check.ID, State: validation.Passed})
		}
		return snapshot
	}
	runner, err := validation.NewRunner(plan, observer, validation.Options{})
	if err != nil {
		t.Fatal(err)
	}
	result := runner.Run(context.Background())
	if result.Fatal != validation.FatalIdentity || result.Verdict == validation.VerdictPassed {
		t.Fatalf("host drift did not reach acceptance: %+v", result)
	}
	found := false
	for _, row := range result.Checks {
		if row.ID == validation.BGPCheckID("SERIAL-A") {
			found = true
			if row.Outcome.State == validation.Passed || row.PriorOutcome == nil || row.PriorOutcome.State != validation.Passed {
				t.Fatalf("earlier BGP pass not retained/revoked: %+v", row)
			}
		}
		if row.ID == "management/coverage" && row.Outcome.State != validation.Passed {
			t.Fatal("host drift erased independent management evidence")
		}
	}
	if !found {
		t.Fatal("missing prior BGP obligation")
	}
}

func TestValidationRuntimeEntryRejectsPersistenceLostAfterResourcePass(t *testing.T) {
	session, _ := standaloneValidationSession()
	plan := correctionSessionPlan(t, session)
	hosts := []validationHostRequest{session.hosts["SERIAL-A"]}
	hosts[0].selected.hostTarget.SSH = &config.SSHAccess{}
	reads := 0
	observer := &runtimeCorrectionObserver{}
	observer.snapshot = func() validation.ResourceSnapshot {
		snapshot := validation.ResourceSnapshot{}
		for _, check := range plan.Input().Checks {
			snapshot.Checks = append(snapshot.Checks, validation.Outcome{ID: check.ID, State: validation.Passed})
		}
		return snapshot
	}
	observer.open = func(ctx context.Context) (validation.RuntimeSession, error) {
		_, err := openCurrentHostNetworkFence(ctx, hosts, time.Second, func(context.Context, config.Target, sshprobe.HostNetworkRequest, time.Duration) (sshprobe.HostNetworkValidation, error) {
			reads++
			return sshprobe.HostNetworkValidation{RuntimeVerified: true}, &sshprobe.HostNetworkError{ReasonCode: "HOST_NETWORK_PERSISTENT_MISMATCH"}
		})
		if err == nil {
			t.Fatal("entry accepted stale resource boolean without current persistence")
		}
		return nil, validationRuntimeError(ctx, err, false)
	}
	runner, err := validation.NewRunner(plan, observer, validation.Options{})
	if err != nil {
		t.Fatal(err)
	}
	result := runner.Run(context.Background())
	if reads != 1 || result.Verdict == validation.VerdictPassed || result.Fatal == "" {
		t.Fatalf("post-resource persistence loss retained full acceptance: reads=%d result=%+v", reads, result)
	}
}

func TestValidationRuntimeRejectsOrdinaryLossWithMeasuredDF(t *testing.T) {
	session, _ := standaloneValidationSession()
	attempts := 0
	session.probe = func(_ context.Context, _ config.Target, request sshprobe.HostTrafficRequest, _ time.Duration) (sshprobe.HostTrafficResult, error) {
		attempts++
		result := runtimePassedTraffic(request)
		result.State = sshprobe.TrafficProbeFailed
		result.Ordinary.State = sshprobe.TrafficProbeFailed
		result.Ordinary.Counters = &sshprobe.PingCounters{Sent: 3, Received: 1, LostPct: 66.6667}
		return result, &sshprobe.HostNetworkError{ReasonCode: "HOST_NETWORK_TRAFFIC_PING_INCOMPLETE"}
	}
	result, err := session.ProbeTraffic(context.Background(), validation.DirectedHostPair{Source: "SERIAL-A", Destination: "SERIAL-B"})
	if err != nil || attempts != 1 || result.Ordinary.State != validation.Failed || result.DF.State != validation.Passed || result.Ordinary.Traffic == nil || result.Ordinary.Traffic.Received != 1 {
		t.Fatalf("canonical loss/DF evidence: %+v %v", result, err)
	}
}
