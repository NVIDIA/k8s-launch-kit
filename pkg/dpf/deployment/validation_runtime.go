// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbnruntime"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

func validationRuntimeChecks(plan *generation.Plan) ([]validation.CheckSpec, validation.RuntimeScope, error) {
	if plan == nil {
		return nil, validation.RuntimeScope{}, fmt.Errorf("retained plan required")
	}
	checks := []validation.CheckSpec{}
	for _, area := range []validation.Area{validation.AreaDevices, validation.AreaHBN, validation.AreaWorkloads} {
		checks = append(checks, validation.CheckSpec{ID: validation.CheckID(string(area) + "/coverage"), Area: area, Subject: string(area) + " selected coverage", Required: true, Coverage: true, CohortBound: true})
	}
	scope := validation.RuntimeScope{}
	for _, device := range plan.Intent().Devices {
		scope.Devices = append(scope.Devices, validation.DeviceID(device.Serial))
		scope.Hosts = append(scope.Hosts, validation.HostID(device.Serial))
		for _, area := range []validation.Area{validation.AreaDevices, validation.AreaHBN, validation.AreaWorkloads} {
			checks = append(checks, validation.CheckSpec{ID: validation.CheckID(string(area) + "/" + device.Serial), Area: area, Subject: device.Serial, Required: true, CohortBound: true})
		}
		checks = append(checks, validation.CheckSpec{ID: validation.CheckID("physical/host-network/" + device.Serial), Area: validation.AreaPhysical, Subject: device.Serial + " workload network", Required: true, FullOnly: true, CohortBound: true})
	}
	for _, a := range scope.Devices {
		for _, b := range scope.Devices {
			if a != b {
				scope.Routes = append(scope.Routes, validation.DirectedDevicePair{Source: a, Destination: b})
			}
		}
	}
	for _, a := range scope.Hosts {
		for _, b := range scope.Hosts {
			if a != b {
				scope.Traffic = append(scope.Traffic, validation.DirectedHostPair{Source: a, Destination: b})
			}
		}
	}
	return checks, scope, nil
}

// validationRuntimeTenantReader exposes only fixed current reads and bounded
// diagnostics, never the tenant transport's mutation-capable Kubernetes client.
type validationRuntimeTenantReader interface {
	VerifyIdentity(context.Context) error
	getWorkloadChild(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error)
	listCompleteNodes(context.Context) ([]unstructured.Unstructured, error)
	listRuntimeObjects(context.Context, string, string) ([]appsv1.DaemonSet, []corev1.Pod, error)
	readPod(context.Context, string, string) (*corev1.Pod, error)
	checkTenantPodExecPermissions(context.Context, string, string) []PreflightCheck
	listValidationSFCObjects(context.Context, string, string, string, string) ([]unstructured.Unstructured, error)
	readValidationHBN(context.Context, string, string, hbnRuntimeCommand, netip.Prefix) ([]byte, error)
}

func (tenant *tenantClient) readValidationHBN(ctx context.Context, namespace, pod string, command hbnRuntimeCommand, prefix netip.Prefix) ([]byte, error) {
	if tenant == nil || tenant.hbnExec == nil {
		return nil, fmt.Errorf("bound HBN diagnostic transport unavailable")
	}
	return tenant.hbnExec.read(ctx, namespace, pod, command, prefix)
}

type validationHBNBindings struct {
	hbnRuntimeBindingSnapshot
	unavailable map[string]validation.Reason
}

type validationCurrentCohort struct {
	graph     currentRuntimeGraph
	snapshot  hostWorkloadLeaseSnapshot
	leases    []hostWorkloadLease
	bindings  validationHBNBindings
	workloads map[string]string
}
type validationHostRequest struct {
	selected currentSelectedDevice
	lease    hostWorkloadLease
	request  sshprobe.HostNetworkRequest
}

func observeCurrentAllocation(ctx context.Context, management resourceReader, tenant validationRuntimeTenantReader, desired *managementDesired, graph currentRuntimeGraph) (hostWorkloadLeaseSnapshot, []hostWorkloadLease, error) {
	var empty hostWorkloadLeaseSnapshot
	if tenant == nil {
		return empty, nil, fmt.Errorf("bound tenant unavailable")
	}
	expected, err := retainedWorkloadIPAM(desired.plan)
	if err != nil {
		return empty, nil, err
	}
	parent, err := management.Get(ctx, expected.DeepCopy())
	if err != nil {
		return empty, nil, err
	}
	if !currentObjectIdentity(parent, expected) || desired.verifier.Verify(expected, parent) != nil {
		return empty, nil, fmt.Errorf("current IPAM projection differs")
	}
	pool, err := tenant.getWorkloadChild(ctx, parent)
	if err != nil {
		return empty, nil, err
	}
	nodes, err := tenant.listCompleteNodes(ctx)
	if err != nil {
		return empty, nil, err
	}
	poolAfter, err := tenant.getWorkloadChild(ctx, parent)
	if err != nil {
		return empty, nil, err
	}
	parentAfter, err := management.Get(ctx, expected.DeepCopy())
	if err != nil {
		return empty, nil, err
	}
	first := hostWorkloadLeaseSnapshot{DPUServiceIPAM: parent, CIDRPool: pool, TenantNodes: nodes}
	after := hostWorkloadLeaseSnapshot{DPUServiceIPAM: parentAfter, CIDRPool: poolAfter, TenantNodes: nodes}
	if !sameCurrentLeaseSnapshot(first, after) {
		return empty, nil, fmt.Errorf("current allocation changed during observation: %w", errValidationResourceChanged)
	}
	leases, err := currentGraphLeases(desired, graph, after)
	return after, leases, err
}

func currentHostRequests(desired *managementDesired, graph currentRuntimeGraph, leases []hostWorkloadLease) ([]validationHostRequest, error) {
	results := make([]validationHostRequest, 0, len(graph.selected))
	hosts := map[string]bool{}
	for _, item := range graph.selected {
		if hosts[item.host.ID] {
			return nil, fmt.Errorf("runtime requires one selected DPU per physical host")
		}
		hosts[item.host.ID] = true
		identity, err := sshprobe.SelectHostWorkloadNetworkIdentity(item.host, item.adapter.Key, item.retained.Serial)
		if err != nil {
			return nil, err
		}
		var selected *hostWorkloadLease
		for i := range leases {
			if leases[i].Serial == item.retained.Serial {
				if selected != nil {
					return nil, fmt.Errorf("current lease ambiguous")
				}
				selected = &leases[i]
			}
		}
		if selected == nil {
			return nil, fmt.Errorf("selected allocation missing")
		}
		request, err := currentHostNetworkRequest(identity, *selected, desired.plan.Intent().Networking.WorkloadMTU, desired.plan.Intent().Networking.WorkloadCIDR)
		if err != nil {
			return nil, err
		}
		results = append(results, validationHostRequest{selected: item, lease: *selected, request: request})
	}
	return results, nil
}

func currentHostNetworkRequest(identity sshprobe.HostNetworkIdentity, lease hostWorkloadLease, mtu int, workloadCIDR string) (sshprobe.HostNetworkRequest, error) {
	prefix, err := netip.ParsePrefix(lease.Prefix)
	address, addressErr := netip.ParseAddr(lease.ReservedHostAddress)
	if err != nil || addressErr != nil || !prefix.Contains(address) || identity.AdapterSerial != lease.Serial {
		return sshprobe.HostNetworkRequest{}, fmt.Errorf("current reserved address identity invalid")
	}
	return sshprobe.HostNetworkRequest{Identity: identity, MTU: mtu, HostAddressPrefix: netip.PrefixFrom(address, prefix.Bits()).String(), Gateway: lease.Gateway, WorkloadCIDR: workloadCIDR}, nil
}

func observeCurrentHBNBindings(ctx context.Context, management validationManagementReader, tenant validationRuntimeTenantReader, desired *managementDesired, graph currentRuntimeGraph, snapshot hostWorkloadLeaseSnapshot, leases []hostWorkloadLease, image hbnPodImageIdentity, requireExec bool) (validationHBNBindings, error) {
	empty := validationHBNBindings{}
	if tenant == nil {
		return empty, fmt.Errorf("bound tenant unavailable")
	}
	if err := tenant.VerifyIdentity(ctx); err != nil {
		return empty, err
	}
	var service *unstructured.Unstructured
	var intent hbnPodBindingIntent
	for i := range graph.services {
		row := &graph.services[i]
		candidate, err := compileCurrentHBNBinding(desired.plan, desired.rules, desired.operatorNamespace, graph.deployment.GetUID(), graph.deployment, leases, snapshot.TenantNodes, row, graph.interfaces, image)
		if err != nil {
			continue
		}
		if service != nil {
			return empty, fmt.Errorf("current HBN service ambiguous")
		}
		service = row
		intent = candidate
	}
	if service == nil {
		return empty, fmt.Errorf("current HBN service binding unavailable")
	}
	app, err := management.getHBNRuntimeApplication(ctx, desired.operatorNamespace, dpfApplicationName(intent.DPUClusterName, intent.DPUServiceName))
	if err != nil {
		return empty, err
	}
	if app == nil {
		return empty, fmt.Errorf("current HBN application unavailable")
	}
	daemonSets, pods, err := tenant.listRuntimeObjects(ctx, intent.TargetNamespace, labels.Set{hbnServiceIDLabel: intent.ServiceID}.AsSelector().String())
	if err != nil {
		return empty, err
	}
	result := validationHBNBindings{hbnRuntimeBindingSnapshot: hbnRuntimeBindingSnapshot{anchors: map[string]hbnPodBindingAnchor{}, sourceDigests: map[string]string{}}, unavailable: map[string]validation.Reason{}}
	// Bind actual running identities even when a source is unhealthy or absent.
	// Missing source Pods block only dependent exec measurements; no ready subset
	// replaces the retained selected scope.
	for _, ds := range daemonSets {
		result.sourceDigests["DaemonSet/"+ds.Name] = hbnBindingDigest(struct {
			UID        types.UID
			Generation int64
			Spec       any
			Owners     any
		}{ds.UID, ds.Generation, ds.Spec, ds.OwnerReferences})
	}
	for _, pod := range pods {
		result.sourceDigests["Pod/"+pod.Name] = hbnBindingDigest(struct {
			UID            types.UID
			Spec           any
			Owners         any
			Containers     any
			InitContainers any
		}{pod.UID, pod.Spec, pod.OwnerReferences, pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses})
	}
	result.sourceDigests["Application/"+app.GetName()] = hbnBindingDigest(struct {
		UID  types.UID
		Spec any
	}{app.GetUID(), app.Object["spec"]})
	serviceHealthy := desired.rules.ValidationWorkloadReadiness(service) == nil
	for _, object := range graph.interfaces {
		if object.GetLabels()[activationParentLabel] != graph.deployment.GetNamespace()+"_"+graph.deployment.GetName() {
			continue
		}
		result.sourceDigests[object.GetName()] = hbnBindingDigest(struct {
			UID        string
			Generation int64
			Spec       any
			Owners     any
		}{string(object.GetUID()), object.GetGeneration(), object.Object["spec"], object.GetOwnerReferences()})
	}
	for _, lease := range leases {
		expected := intent
		expected.TenantNodeName = lease.TenantNodeName
		anchor, err := validateHBNPodBinding(expected, service, app, daemonSets, pods, time.Now().UTC())
		if err != nil || !serviceHealthy {
			result.unavailable[lease.Serial] = validation.ReasonDependency
			continue
		}
		if requireExec && !runtimePermissionsPassed(tenant.checkTenantPodExecPermissions(ctx, intent.TargetNamespace, anchor.PodName)) {
			result.unavailable[lease.Serial] = validation.ReasonDenied
			continue
		}
		pod, err := tenant.readPod(ctx, anchor.PodNamespace, anchor.PodName)
		if err != nil {
			return empty, err
		}
		if pod == nil {
			return empty, fmt.Errorf("current HBN pod unavailable")
		}
		named, err := validateHBNPodBinding(expected, service, app, daemonSets, []corev1.Pod{*pod}, time.Now().UTC())
		if err != nil || !sameHBNPodBinding(anchor, named) {
			return empty, fmt.Errorf("current HBN pod changed: %w", errValidationResourceChanged)
		}
		result.anchors[lease.Serial] = anchor
	}
	return result, tenant.VerifyIdentity(ctx)
}

func (o *validationObserver) captureValidationCohort(ctx context.Context, management validationManagementReader, tenant validationRuntimeTenantReader, selected []currentSelectedDevice, image hbnPodImageIdentity, requireExec bool) (validationCurrentCohort, error) {
	var result validationCurrentCohort
	graph, err := observeCurrentRuntimeGraph(ctx, management, o.desired, selected)
	if err != nil {
		return result, err
	}
	snapshot, leases, err := observeCurrentAllocation(ctx, management, tenant, o.desired, graph)
	if err != nil {
		return result, err
	}
	bindings, err := observeCurrentHBNBindings(ctx, management, tenant, o.desired, graph, snapshot, leases, image, requireExec)
	if err != nil {
		return result, err
	}
	workloads, err := currentSFCAnchors(ctx, tenant, o.desired, graph, snapshot)
	if err != nil {
		return result, err
	}
	after, afterLeases, err := observeCurrentAllocation(ctx, management, tenant, o.desired, graph)
	if err != nil {
		return result, err
	}
	if !sameCurrentLeaseSnapshot(snapshot, after) || !reflect.DeepEqual(leases, afterLeases) {
		return result, fmt.Errorf("current allocation changed during HBN observation: %w", errValidationResourceChanged)
	}
	graphAfter, err := observeCurrentRuntimeGraph(ctx, management, o.desired, selected)
	if err != nil {
		return result, err
	}
	if currentGraphDigest(graph) != currentGraphDigest(graphAfter) {
		return result, fmt.Errorf("current selected graph changed during observation: %w", errValidationResourceChanged)
	}
	if err := tenant.VerifyIdentity(ctx); err != nil {
		return result, err
	}
	if err := management.VerifyIdentity(ctx); err != nil {
		return result, err
	}
	return validationCurrentCohort{graph: graphAfter, snapshot: after, leases: afterLeases, bindings: bindings, workloads: workloads}, nil
}

type validationRuntimeSession struct {
	mu             sync.Mutex
	scope          validation.RuntimeScope
	anchor         validationCurrentCohort
	observe        func(context.Context) (validationCurrentCohort, error)
	read           hbnRuntimeRead
	probe          hbnHostTrafficProbe
	hosts          map[validation.HostID]validationHostRequest
	asns           map[validation.DeviceID]uint32
	fabricASN      uint32
	timeout        time.Duration
	poisoned       error
	verifyPhysical func(context.Context) error
}

func (o *validationObserver) OpenRuntime(ctx context.Context, _ validation.Progress) (validation.RuntimeSession, error) {
	if ctx == nil || o.mode != validation.ModeFull {
		return nil, &validation.ObservationError{Category: validation.FatalInput}
	}
	physicalFence, err := o.openValidationPhysicalFence(ctx)
	if err != nil {
		return nil, err
	}
	management, err := o.newManagement(ctx)
	if err != nil {
		return nil, validationRuntimeError(ctx, err, false)
	}
	tenant, err := o.openValidationTenant(ctx, management)
	if err != nil {
		return nil, validationRuntimeError(ctx, err, false)
	}
	selected, err := validationSelectedHosts(o.desired, o.request)
	if err != nil {
		return nil, validationRuntimeError(ctx, err, false)
	}
	image, imageErr := resolveHBNRuntimeImage(ctx, hbnImageReference(o.desired.plan.Intent().Generation), o.request.RequestTimeout, nil)
	if imageErr != nil {
		image = hbnPodImageIdentity{}
	}
	observe := func(ctx context.Context) (validationCurrentCohort, error) {
		return o.captureValidationCohort(ctx, management, tenant, selected, image, true)
	}
	anchor, err := observe(ctx)
	if err != nil {
		return nil, validationRuntimeError(ctx, err, false)
	}
	hosts, err := currentHostRequests(o.desired, anchor.graph, anchor.leases)
	if err != nil {
		return nil, validationRuntimeError(ctx, err, false)
	}
	session := &validationRuntimeSession{scope: o.runtimeScope, anchor: anchor, observe: observe, read: tenant.readValidationHBN, probe: sshprobe.ProbeHostWorkloadTraffic, hosts: map[validation.HostID]validationHostRequest{}, asns: map[validation.DeviceID]uint32{}, fabricASN: o.desired.plan.Intent().Networking.FabricASN, timeout: o.request.RequestTimeout, verifyPhysical: physicalFence}
	for _, host := range hosts {
		session.hosts[validation.HostID(host.lease.Serial)] = host
	}
	for _, device := range o.desired.plan.Intent().Devices {
		session.asns[validation.DeviceID(device.Serial)] = device.ASN
	}
	return session, nil
}

func validationRuntimeError(ctx context.Context, err error, drift bool) error {
	category := validation.FatalObservation
	if drift || validationIdentityChanged(err) {
		category = validation.FatalIdentity
	}
	if errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled {
		category = validation.FatalCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		category = validation.FatalTimeout
	}
	return &validation.ObservationError{Category: category, Cause: err}
}

func (s *validationRuntimeSession) Scope() validation.RuntimeScope {
	return validation.RuntimeScope{Devices: append([]validation.DeviceID(nil), s.scope.Devices...), Hosts: append([]validation.HostID(nil), s.scope.Hosts...), Routes: append([]validation.DirectedDevicePair(nil), s.scope.Routes...), Traffic: append([]validation.DirectedHostPair(nil), s.scope.Traffic...)}
}
func (s *validationRuntimeSession) fence(ctx context.Context) error {
	if s.poisoned != nil {
		return s.poisoned
	}
	if ctx == nil {
		return &validation.ObservationError{Category: validation.FatalInput}
	}
	if err := ctx.Err(); err != nil {
		return validationRuntimeError(ctx, err, false)
	}
	current, err := s.observe(ctx)
	if err == nil && (currentGraphDigest(s.anchor.graph) != currentGraphDigest(current.graph) || !reflect.DeepEqual(s.anchor.leases, current.leases) || !reflect.DeepEqual(s.anchor.bindings, current.bindings) || !reflect.DeepEqual(s.anchor.workloads, current.workloads)) {
		err = fmt.Errorf("current runtime cohort changed: %w", errValidationResourceChanged)
	}
	if err != nil {
		s.poisoned = validationRuntimeError(ctx, err, validationIdentityChanged(err))
		return s.poisoned
	}
	return nil
}
func (s *validationRuntimeSession) VerifyCohort(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil {
		return &validation.ObservationError{Category: validation.FatalInput}
	}
	if s.poisoned != nil {
		return s.poisoned
	}
	if s.verifyPhysical != nil {
		if err := s.verifyPhysical(ctx); err != nil {
			s.poisoned = err
			return err
		}
	}
	return s.fence(ctx)
}
func (s *validationRuntimeSession) measure(ctx context.Context, fn func()) error {
	if err := s.fence(ctx); err != nil {
		return err
	}
	fn()
	return s.fence(ctx)
}
func runtimeReadOutcome(id validation.CheckID, err error) validation.Outcome {
	if err != nil {
		return validation.Outcome{ID: id, State: validation.Unverified, Reason: validation.ReasonUnavailable, Summary: "The selected runtime measurement could not be read or parsed."}
	}
	return validation.Outcome{ID: id, State: validation.Passed, Summary: "The selected runtime measurement matches retained intent."}
}
func (s *validationRuntimeSession) ObserveBGP(ctx context.Context, device validation.DeviceID) (validation.BGPObservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := validation.BGPObservation{}
	binding, ok := s.anchor.bindings.anchors[string(device)]
	if !ok {
		if _, selected := s.asns[device]; selected {
			result.Outcome = validation.Outcome{ID: validation.BGPCheckID(device), State: validation.Unverified, Reason: validation.ReasonDependency, Summary: "A current exact-image HBN execution target is unavailable for this selected source."}
			return result, s.measure(ctx, func() {})
		}
		return result, &validation.ObservationError{Category: validation.FatalInput}
	}
	err := s.measure(ctx, func() {
		data, readErr := s.read(ctx, binding.PodNamespace, binding.PodName, hbnReadBGPSummary, netip.Prefix{})
		result.Outcome = runtimeReadOutcome(validation.BGPCheckID(device), readErr)
		if readErr != nil {
			return
		}
		summary, parseErr := hbnruntime.Parse(data)
		if parseErr != nil {
			result.Outcome = runtimeReadOutcome(validation.BGPCheckID(device), parseErr)
			return
		}
		result.Outcome.BGP = &validation.BGPFacts{ExpectedPeers: 2, LocalASN: summary.ASN}
		for _, peer := range summary.Peers {
			if peer.State == "Established" {
				result.Outcome.BGP.EstablishedPeers++
			}
		}
		if len(hbnruntime.Validate(summary, hbnruntime.Expected{LocalASN: s.asns[device], FabricASN: s.fabricASN})) != 0 {
			result.Outcome.State = validation.Failed
			result.Outcome.Reason = validation.ReasonHealth
			result.Outcome.Summary = "Expected fabric BGP peers are not established with retained ASNs."
		}
	})
	return result, err
}
func (s *validationRuntimeSession) ObserveRoute(ctx context.Context, pair validation.DirectedDevicePair) (validation.RouteObservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := validation.RouteObservation{}
	binding, ok := s.anchor.bindings.anchors[string(pair.Source)]
	if pair.Source == pair.Destination {
		return result, &validation.ObservationError{Category: validation.FatalInput}
	}
	if !ok {
		if _, sourceSelected := s.asns[pair.Source]; sourceSelected {
			if _, destinationSelected := s.asns[pair.Destination]; destinationSelected {
				result.Outcome = validation.Outcome{ID: validation.RouteCheckID(pair), State: validation.Unverified, Reason: validation.ReasonDependency, Summary: "A current exact-image HBN execution target is unavailable for this selected source."}
				return result, s.measure(ctx, func() {})
			}
		}
		return result, &validation.ObservationError{Category: validation.FatalInput}
	}
	var prefix netip.Prefix
	for _, lease := range s.anchor.leases {
		if lease.Serial == string(pair.Destination) {
			prefix, _ = netip.ParsePrefix(lease.Prefix)
		}
	}
	if !prefix.IsValid() {
		return result, &validation.ObservationError{Category: validation.FatalInput}
	}
	err := s.measure(ctx, func() {
		data, readErr := s.read(ctx, binding.PodNamespace, binding.PodName, hbnReadRemoteRoute, prefix)
		result.Outcome = runtimeReadOutcome(validation.RouteCheckID(pair), readErr)
		if readErr != nil {
			return
		}
		document, parseErr := hbnruntime.ParseFIBRoute(data)
		if parseErr != nil {
			result.Outcome = runtimeReadOutcome(validation.RouteCheckID(pair), parseErr)
			return
		}
		installed := len(hbnruntime.ValidateFIBRoute(document, hbnruntime.FIBExpectation{Prefix: prefix.String(), EgressInterfaces: []string{"p0_if", "p1_if"}})) == 0
		result.Outcome.Route = &validation.RouteFacts{Prefix: prefix, Installed: installed}
		if !installed {
			result.Outcome.State = validation.Failed
			result.Outcome.Reason = validation.ReasonHealth
			result.Outcome.Summary = "The exact remote allocation is not installed through the expected uplinks."
		}
	})
	return result, err
}
func (s *validationRuntimeSession) ProbeTraffic(ctx context.Context, pair validation.DirectedHostPair) (validation.TrafficObservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := validation.TrafficObservation{}
	source, ok := s.hosts[pair.Source]
	destination, found := s.hosts[pair.Destination]
	if !ok || !found || pair.Source == pair.Destination {
		return result, &validation.ObservationError{Category: validation.FatalInput}
	}
	request := sshprobe.HostTrafficRequest{HostNetworkRequest: source.request, Destination: netip.MustParseAddr(destination.lease.ReservedHostAddress)}
	request.LeaseBinding = hbnBindingDigest(struct {
		Graph  string
		Leases []hostWorkloadLease
	}{currentGraphDigest(s.anchor.graph), s.anchor.leases})
	err := s.measure(ctx, func() {
		measured, probeErr := s.probe(ctx, source.selected.hostTarget, request, s.timeout)
		result.Ordinary = validationTrafficOutcome(pair, false, measured.Ordinary, probeErr, 56)
		result.DF = validationTrafficOutcome(pair, true, measured.MTUProbe, probeErr, request.MTU-28)
		expectedSource := netip.MustParsePrefix(request.HostAddressPrefix).Addr().String()
		identityMatches := measured.ExpectedSource == expectedSource && measured.ExpectedDestination == request.Destination.String() && measured.ExpectedInterface == request.Identity.InterfaceName && measured.ExpectedGateway == request.Gateway && measured.MTU == request.MTU
		if !measured.RouteVerified || !identityMatches || probeErr != nil && measured.State != sshprobe.TrafficProbeFailed {
			for _, row := range []*validation.Outcome{&result.Ordinary, &result.DF} {
				if row.State == validation.Failed && identityMatches {
					continue
				}
				row.State = validation.Unverified
				row.Reason = validation.ReasonIncomplete
				row.Summary = "The intended natural source route was not verified."
			}
		}
	})
	return result, err
}
func validationTrafficOutcome(pair validation.DirectedHostPair, df bool, probe sshprobe.HostTrafficProbe, probeErr error, expectedPayload int) validation.Outcome {
	outcome := runtimeReadOutcome(validation.TrafficCheckID(pair, df), probeErr)
	if probe.Counters != nil {
		outcome.Traffic = &validation.TrafficFacts{Transmitted: uint32(max(probe.Counters.Sent, 0)), Received: uint32(max(probe.Counters.Received, 0)), PayloadBytes: uint32(max(probe.PayloadBytes, 0))}
	}
	switch probe.State {
	case sshprobe.TrafficProbePassed:
		if probe.Counters == nil || probe.Counters.Sent != hbn.RuntimePacketCount || probe.Counters.Received != hbn.RuntimePacketCount || probe.Counters.LostPct != 0 || probe.PayloadBytes != expectedPayload {
			outcome.State = validation.Unverified
			outcome.Reason = validation.ReasonIncomplete
			outcome.Summary = "Diagnostic packet counters or payload do not establish the requested measurement."
			return outcome
		}
		outcome.State = validation.Passed
		outcome.Reason = validation.ReasonNone
		outcome.Summary = "All selected source-bound diagnostic packets were received."
	case sshprobe.TrafficProbeFailed:
		outcome.State = validation.Failed
		outcome.Reason = validation.ReasonHealth
		outcome.Summary = "The selected source-bound diagnostic packets experienced loss."
	default:
		outcome.State = validation.Unverified
		outcome.Reason = validation.ReasonIncomplete
		outcome.Summary = "The selected diagnostic packet measurement did not complete."
	}
	return outcome
}
