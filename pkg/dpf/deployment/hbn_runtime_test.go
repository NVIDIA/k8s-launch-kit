// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

type runtimeStageFake struct {
	*activationIntegrationFake
	service, app    *unstructured.Unstructured
	interfaces      []unstructured.Unstructured
	denyManagement  bool
	physicalChecks  int
	physicalCohorts [][]string
	physicalHook    func(int) error
}

func (f *runtimeStageFake) verifyEnrollmentBMCs(ctx context.Context, selected []enrollmentBMC, timeout time.Duration, journal *executionJournal) error {
	f.physicalChecks++
	serials := make([]string, 0, len(selected))
	for _, item := range selected {
		serials = append(serials, item.device.Serial)
	}
	f.physicalCohorts = append(f.physicalCohorts, serials)
	if err := f.activationIntegrationFake.verifyEnrollmentBMCs(ctx, selected, timeout, journal); err != nil {
		return err
	}
	if f.physicalHook != nil {
		return f.physicalHook(f.physicalChecks)
	}
	return nil
}

func (f *runtimeStageFake) checkHBNRuntimeAccess(context.Context, string, string, string) []PreflightCheck {
	state := "passed"
	if f.denyManagement {
		state = "blocked"
	}
	return []PreflightCheck{{ID: "runtime-management", State: state}}
}
func (f *runtimeStageFake) listHBNRuntimeObjects(_ context.Context, kind, namespace, selector string) ([]unstructured.Unstructured, error) {
	switch kind {
	case "DPUService":
		return []unstructured.Unstructured{*f.service.DeepCopy()}, nil
	case "DPUServiceInterface":
		return deepCopyUnstructuredList(f.interfaces), nil
	}
	return nil, fmt.Errorf("unexpected runtime list")
}
func (f *runtimeStageFake) getHBNRuntimeApplication(context.Context, string, string) (*unstructured.Unstructured, error) {
	return f.app.DeepCopy(), nil
}

type runtimeStageFixture struct {
	activationIntegrationFixture
	activation                             activationStageReceipt
	snapshot                               hostWorkloadLeaseSnapshot
	client                                 *runtimeStageFake
	hosts                                  []HostWorkloadNetworkResult
	pods                                   []corev1.Pod
	ds                                     appsv1.DaemonSet
	image                                  hbnPodImageIdentity
	transports                             hbnRuntimeTransports
	commandReads, trafficReads, leaseReads int
	denyTenant                             bool
	commandHook                            func(int, hbnRuntimeCommand)
	trafficHook                            func(int, *sshprobe.HostTrafficResult)
	leaseHook                              func(int, *hostWorkloadLeaseSnapshot)
	incompleteList                         bool
}

func newRuntimeStageFixture(t *testing.T, serials []string) *runtimeStageFixture {
	t.Helper()
	return newRuntimeStageFixtureFromExecution(t, prepareSoftwareStageExecution(t, serials))
}

func newRuntimeStageFixtureFromExecution(t *testing.T, execution *managementExecution) *runtimeStageFixture {
	t.Helper()
	base, activation, snapshot := hostNetworkStageFixtureFromExecution(t, execution)
	return runtimeStageFixtureFromActivated(t, base, activation, snapshot)
}

func runtimeStageFixtureFromActivated(t *testing.T, base activationIntegrationFixture, activation activationStageReceipt, snapshot hostWorkloadLeaseSnapshot) *runtimeStageFixture {
	t.Helper()
	f := &runtimeStageFixture{activationIntegrationFixture: base, activation: activation, snapshot: snapshot, client: &runtimeStageFake{activationIntegrationFake: base.client}}
	desired := base.prepared.execution.plan.Intent()
	namespace := desired.Namespace
	versionKey := "svc.dpu.nvidia.com/dpuservice-doca-hbn-version"
	parent := namespace + "_" + base.client.deployment.GetName()
	for i := range f.snapshot.TenantNodes {
		labels := f.snapshot.TenantNodes[i].GetLabels()
		labels[versionKey] = "doca-hbn-random"
		labels[activationParentLabel] = parent
		f.snapshot.TenantNodes[i].SetLabels(labels)
	}
	deployment, err := retainedDeploymentObject(base.prepared.execution.verificationInputs())
	if err != nil {
		t.Fatal(err)
	}
	observed, err := observeActivationScope(base.ctx, base.client, base.prepared.execution.verificationInputs(), deployment, activation.DeploymentUID, activation.selected, activation.progress)
	if err != nil {
		t.Fatal(err)
	}
	leases, err := resolveHostWorkloadLeases(base.prepared.execution.plan, base.prepared.execution.reconciler, activation.selected, observed, base.workload.BundleDigest, base.workload.Resources, f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	fence := hostNetworkLeaseFence{snapshot: f.snapshot, leases: leases, activation: observed}
	// Independent pinned-controller fixture: retained template Helm defaults plus
	// retained configuration values, current owner-qualified runtime identities.
	objects, err := retainedWorkloadObjects(base.prepared.execution.verificationInputs())
	if err != nil {
		t.Fatal(err)
	}
	var template, configuration *unstructured.Unstructured
	for _, object := range objects {
		switch object.GetKind() {
		case "DPUServiceTemplate":
			template = object
		case "DPUServiceConfiguration":
			configuration = object
		}
	}
	source, _, _ := unstructured.NestedMap(template.Object, "spec", "helmChart", "source")
	source["path"] = ""
	values, _, _ := unstructured.NestedMap(template.Object, "spec", "helmChart", "values")
	configValues, _, _ := unstructured.NestedMap(configuration.Object, "spec", "serviceConfiguration", "helmChart", "values")
	for key, value := range configValues {
		values[key] = value
	}
	sds, _, _ := unstructured.NestedMap(configuration.Object, "spec", "serviceConfiguration", "serviceDaemonSet")
	sds["labels"] = map[string]any{hbnServiceReferenceLabel: hbnContainerName}
	expressions := []any{map[string]any{"key": versionKey, "operator": "In", "values": []any{"doca-hbn-random"}}, map[string]any{"key": activationParentLabel, "operator": "In", "values": []any{parent}}}
	sds["nodeSelector"] = map[string]any{"nodeSelectorTerms": []any{map[string]any{"matchExpressions": expressions}}}
	var clusterExpressions []any
	keys := make([]string, 0, len(desired.Bootstrap.Tenant.Labels))
	for key := range desired.Bootstrap.Tenant.Labels {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		clusterExpressions = append(clusterExpressions, map[string]any{"key": key, "operator": "In", "values": []any{desired.Bootstrap.Tenant.Labels[key]}})
	}
	clusterSelector := map[string]any{"matchExpressions": clusterExpressions}
	id := base.client.deployment.GetName() + "_doca-hbn_aabbccddee"
	names := []any{}
	controller, notController := true, false
	for i, name := range []string{"p0_if", "p1_if", "pf0hpf_if"} {
		resourceName := fmt.Sprintf("doca-hbn-port-%d-random", i)
		names = append(names, resourceName)
		spec := map[string]any{"dpuClusterSelector": clusterSelector, "template": map[string]any{"metadata": map[string]any{}, "spec": map[string]any{"nodeSelector": map[string]any{"matchExpressions": expressions}, "template": map[string]any{"metadata": map[string]any{"labels": map[string]any{hbnServiceIDLabel: id, "svc.dpu.nvidia.com/interface": name}}, "spec": map[string]any{"interfaceType": "service", "service": map[string]any{"serviceID": id, "network": "mybrhbn", "interfaceName": name}}}}}}
		object := runtimeObject("DPUServiceInterface", resourceName, namespace, spec)
		object.SetUID(types.UID(fmt.Sprintf("interface-%d", i)))
		object.SetResourceVersion("1")
		object.SetGeneration(1)
		object.SetLabels(map[string]string{activationParentLabel: parent, hbnServiceReferenceLabel: hbnContainerName})
		object.SetAnnotations(map[string]string{hbnServiceVersionAnnotation: "aabbccddee", "dpu.nvidia.com/consumed-by": "doca-hbn-random"})
		object.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "svc.dpu.nvidia.com/v1alpha1", Kind: "DPUDeployment", Name: base.client.deployment.GetName(), UID: activation.DeploymentUID, Controller: &controller}, {APIVersion: "svc.dpu.nvidia.com/v1alpha1", Kind: "DPUService", Name: "doca-hbn-random", UID: "service-uid", Controller: &notController}})
		object, err = base.prepared.execution.rules.DefaultedCopy(object)
		if err != nil {
			t.Fatal(err)
		}
		f.client.interfaces = append(f.client.interfaces, *object)
	}
	spec := map[string]any{"helmChart": map[string]any{"source": source, "values": values}, "serviceID": id, "serviceDaemonSet": sds, "interfaces": names, "dpuClusterSelector": clusterSelector, "security": map[string]any{"privileged": true}}
	service := runtimeObject("DPUService", "doca-hbn-random", namespace, spec)
	service.SetUID("service-uid")
	service.SetGeneration(1)
	service.SetResourceVersion("1")
	service.SetCreationTimestamp(metav1.NewTime(time.Now().UTC().Add(-time.Minute)))
	service.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "svc.dpu.nvidia.com/v1alpha1", Kind: "DPUDeployment", Name: base.client.deployment.GetName(), UID: activation.DeploymentUID, Controller: &controller}})
	service.SetLabels(map[string]string{activationParentLabel: parent, hbnServiceReferenceLabel: hbnContainerName})
	service.SetAnnotations(map[string]string{hbnServiceVersionAnnotation: "aabbccddee"})
	service, err = base.prepared.execution.rules.DefaultedCopy(service)
	if err != nil {
		t.Fatal(err)
	}
	f.client.service = service
	f.image = hbnPodImageIdentity{Reference: desired.Generation.ImageRepository + ":" + desired.Generation.ImageTag + "@" + desired.Generation.ImageDigest, ManifestDigest: desired.Generation.ImageDigest, ConfigDigest: "sha256:" + strings.Repeat("b", 64)}
	intent, err := compileHBNRuntimeBinding(base.prepared.execution.verificationInputs(), activation, fence, service, f.client.interfaces, f.image)
	if err != nil {
		t.Fatalf("retained plan factory: %v\nservice spec=%#v", err, service.Object["spec"])
	}
	f.client.app = hbnApplicationFixture(intent, time.Now().UTC())
	f.ds, _ = bindingFixture(intent, "cluster-dpu-hbn-ds")
	f.ds.Status.CurrentNumberScheduled = int32(len(desired.Devices))
	f.ds.Status.DesiredNumberScheduled = int32(len(desired.Devices))
	f.ds.Status.UpdatedNumberScheduled = int32(len(desired.Devices))
	f.ds.Status.NumberReady = int32(len(desired.Devices))
	f.ds.Status.NumberAvailable = int32(len(desired.Devices))
	for _, lease := range leases {
		selectedIntent := intent
		selectedIntent.TenantNodeName = lease.TenantNodeName
		_, pod := bindingFixture(selectedIntent, "cluster-dpu-hbn-ds")
		pod.Name = "hbn-pod-" + strings.ToLower(lease.Serial)
		pod.UID = types.UID("pod-" + lease.Serial)
		f.pods = append(f.pods, pod)
	}
	// Run the real host stage with only its native SSH transport substituted.
	f.hosts, err = runHostWorkloadNetworkStage(base.ctx, f.client, base.prepared, base.controlPlane, base.workload, activation, testJournal(base.prepared.execution), time.Second,
		func(context.Context) (hostWorkloadLeaseSnapshot, error) {
			return cloneHostNetworkStageSnapshot(f.snapshot), nil
		}, func(_ context.Context, _ config.Target, request sshprobe.HostNetworkRequest, _ time.Duration) (sshprobe.HostNetworkReceipt, error) {
			return sshprobe.HostNetworkReceipt{RequestID: hbnBindingDigest(request), State: "complete", RuntimeVerified: true, FileVerified: true}, nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tenant := testTenant(base.controlPlane.tenant.Client)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"gitVersion": "v" + strings.TrimPrefix(tenant.expected.KubernetesVersion, "v")})
		case r.URL.Path == "/api/v1/namespaces/kube-system":
			_ = json.NewEncoder(w).Encode(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "tenant-system"}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}})
		case r.URL.Path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
			if r.Method != "POST" {
				t.Errorf("unexpected permission verb %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(&authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: !f.denyTenant, Denied: f.denyTenant}})
		case r.URL.Path == "/apis/apps/v1/namespaces/"+namespace+"/daemonsets":
			rv := "10"
			continuation := ""
			if f.incompleteList {
				continuation = "repeat"
			}
			_ = json.NewEncoder(w).Encode(&appsv1.DaemonSetList{ListMeta: metav1.ListMeta{ResourceVersion: rv, Continue: continuation}, Items: []appsv1.DaemonSet{*f.ds.DeepCopy()}})
		case r.URL.Path == "/api/v1/namespaces/"+namespace+"/pods":
			_ = json.NewEncoder(w).Encode(&corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}, Items: f.pods})
		case strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/"+namespace+"/pods/"):
			name := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/"+namespace+"/pods/")
			for _, pod := range f.pods {
				if pod.Name == name {
					_ = json.NewEncoder(w).Encode(pod)
					return
				}
			}
			http.NotFound(w, r)
		default:
			t.Errorf("prohibited/unexpected runtime API action %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	tenant.api, err = kubernetes.NewForConfig(&rest.Config{Host: server.URL, Timeout: time.Second, RateLimiter: flowcontrol.NewFakeAlwaysRateLimiter()})
	if err != nil {
		t.Fatal(err)
	}
	f.transports = hbnRuntimeTransports{
		readLeases: func(context.Context) (hostWorkloadLeaseSnapshot, error) {
			f.leaseReads++
			snapshot := cloneHostNetworkStageSnapshot(f.snapshot)
			if f.leaseHook != nil {
				f.leaseHook(f.leaseReads, &snapshot)
			}
			return snapshot, nil
		},
		resolveImage: func(_ context.Context, reference string, _ time.Duration, _ http.RoundTripper) (hbnPodImageIdentity, error) {
			if reference != f.image.Reference {
				t.Fatal("image reference not retained")
			}
			return f.image, nil
		},
		readCommand: func(_ context.Context, namespace, pod string, command hbnRuntimeCommand, prefix netip.Prefix) ([]byte, error) {
			f.commandReads++
			if f.commandHook != nil {
				f.commandHook(f.commandReads, command)
			}
			var serial string
			for _, lease := range leases {
				if "hbn-pod-"+strings.ToLower(lease.Serial) == pod {
					serial = lease.Serial
				}
			}
			if namespace != desired.Namespace || serial == "" {
				t.Fatal("exec broadened outside current selected HBN namespace/Pod")
			}
			if command == hbnReadBGPSummary {
				var asn uint32
				for _, device := range desired.Devices {
					if device.Serial == serial {
						asn = device.ASN
					}
				}
				return runtimeBGPJSON(asn, desired.Networking.FabricASN), nil
			}
			if command != hbnReadRemoteRoute || !prefix.IsValid() {
				t.Fatal("unexpected exec command")
			}
			return runtimeFIBJSON(prefix.String(), "p0_if"), nil
		},
		probeTraffic: func(_ context.Context, target config.Target, request sshprobe.HostTrafficRequest, _ time.Duration) (sshprobe.HostTrafficResult, error) {
			f.trafficReads++
			var selected *activationSelectedDevice
			for i := range activation.selected {
				if activation.selected[i].item.device.Serial == request.Identity.AdapterSerial {
					selected = &activation.selected[i]
				}
			}
			if selected == nil || !reflect.DeepEqual(target.SSH, selected.hostTarget.SSH) || target.InputRef != selected.host.InputRef {
				t.Fatal("traffic changed selected physical/SSH authority")
			}
			result := runtimePassedTraffic(request)
			if f.trafficHook != nil {
				f.trafficHook(f.trafficReads, &result)
			}
			return result, nil
		},
	}
	return f
}

func runtimeBGPJSON(asn, fabric uint32) []byte {
	peer := func() map[string]any {
		return map[string]any{"localAs": asn, "remoteAs": fabric, "state": "Established", "idType": "interface"}
	}
	value := map[string]any{"as": asn, "vrfName": "default", "peerCount": 2, "displayedPeers": 2, "totalPeers": 2, "failedPeers": 0, "peers": map[string]any{"p0_if": peer(), "p1_if": peer()}}
	data, _ := json.Marshal(value)
	return data
}
func runtimeFIBJSON(prefix, port string) []byte {
	parsed := netip.MustParsePrefix(prefix)
	value := map[string]any{prefix: []any{map[string]any{"protocol": "bgp", "prefix": prefix, "prefixLen": parsed.Bits(), "vrfId": 0, "vrfName": "default", "table": 254, "selected": true, "destSelected": true, "installed": true, "nexthops": []any{map[string]any{"interfaceName": port, "active": true, "fib": true, "ip": "fe80::1"}}}}}
	data, _ := json.Marshal(value)
	return data
}
func runtimePassedTraffic(request sshprobe.HostTrafficRequest) sshprobe.HostTrafficResult {
	return sshprobe.HostTrafficResult{State: sshprobe.TrafficProbePassed, RouteVerified: true, ExpectedSource: netip.MustParsePrefix(request.HostAddressPrefix).Addr().String(), ExpectedDestination: request.Destination.String(), ExpectedInterface: request.Identity.InterfaceName, ExpectedGateway: request.Gateway, MTU: request.MTU,
		Ordinary: sshprobe.HostTrafficProbe{State: sshprobe.TrafficProbePassed, PayloadBytes: 56, Counters: &sshprobe.PingCounters{Sent: 3, Received: 3}}, MTUProbe: sshprobe.HostTrafficProbe{State: sshprobe.TrafficProbePassed, PayloadBytes: request.MTU - 28, Counters: &sshprobe.PingCounters{Sent: 3, Received: 3}}}
}
func (f *runtimeStageFixture) run(journal *executionJournal) (HBNRuntimeResult, error) {
	return runHBNRuntimeStage(f.ctx, f.client, newControlPlaneVerificationInputs(f.prepared, f.controlPlane, f.workload), f.controlPlane.tenant.Client, f.activation, f.hosts, journal, time.Second, f.transports)
}

func TestHBNRuntimeStageActualTwoHostFlow(t *testing.T) {
	f := newRuntimeStageFixture(t, []string{"SERIAL-A", "SERIAL-B"})
	journal := testJournal(f.prepared.execution)
	result, err := f.run(journal)
	if err != nil || !result.TrafficValidated || result.State != "passed" || len(result.BGP) != 2 || len(result.Routes) != 2 || len(result.Traffic) != 2 || f.commandReads != 4 || f.trafficReads != 2 {
		t.Fatalf("result=%+v commands=%d traffic=%d err=%v", result, f.commandReads, f.trafficReads, err)
	}
	for _, row := range result.BGP {
		if !row.Fenced || row.State != "passed" {
			t.Fatal("unfenced BGP pass")
		}
	}
	for _, row := range result.Routes {
		if !row.Fenced || row.State != "passed" {
			t.Fatal("unfenced route pass")
		}
	}
	for _, row := range result.Traffic {
		if !row.Fenced {
			t.Fatal("unfenced traffic pass")
		}
	}
	report := journal.file.(*managementMemoryFile).String()
	if !strings.Contains(report, "HBN_RUNTIME_COHORT_VALIDATED") || strings.Contains(report, "SpecDigest") || strings.Contains(report, "containerd://") {
		t.Fatal("unsafe/incomplete report")
	}
}

func TestHBNRuntimeStageRejectsPartialMeasurementsAndDrift(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	for _, scenario := range []string{"wrong-asn", "wrong-route", "ordinary-loss", "df-loss", "service-values", "container-restart", "sidecar-restart", "completed-init-restart", "sidecar-image", "pod-replacement", "interface-replacement", "lease-replaced", "assignment-drift", "tenant-denied", "management-denied", "incomplete-list", "canceled", "cancel-after-read", "later-exec-failure", "host-receipt-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRuntimeStageFixtureFromExecution(t, execution)
			original := f.transports.readCommand
			switch scenario {
			case "wrong-asn":
				f.transports.readCommand = func(ctx context.Context, ns, pod string, command hbnRuntimeCommand, prefix netip.Prefix) ([]byte, error) {
					if command == hbnReadBGPSummary {
						return runtimeBGPJSON(64520, 65000), nil
					}
					return original(ctx, ns, pod, command, prefix)
				}
			case "wrong-route":
				f.transports.readCommand = func(ctx context.Context, ns, pod string, command hbnRuntimeCommand, prefix netip.Prefix) ([]byte, error) {
					if command == hbnReadRemoteRoute {
						return runtimeFIBJSON(prefix.String(), "pf0hpf_if"), nil
					}
					return original(ctx, ns, pod, command, prefix)
				}
			case "ordinary-loss", "df-loss":
				f.trafficHook = func(_ int, result *sshprobe.HostTrafficResult) {
					probe := &result.Ordinary
					if scenario == "df-loss" {
						probe = &result.MTUProbe
					}
					probe.State = sshprobe.TrafficProbeFailed
					probe.Counters.Received = 2
					probe.Counters.LostPct = 33.3333
					result.State = sshprobe.TrafficProbeFailed
				}
			case "service-values":
				f.commandHook = func(count int, _ hbnRuntimeCommand) {
					if count == 1 {
						_ = unstructured.SetNestedField(f.client.service.Object, "unretained-current-startup", "spec", "helmChart", "values", "configuration", "startupYAMLJ2")
					}
				}
			case "container-restart":
				f.commandHook = func(count int, _ hbnRuntimeCommand) {
					if count == 1 {
						f.pods[1].Status.ContainerStatuses[0].RestartCount++
						f.pods[1].Status.ContainerStatuses[0].ContainerID = "containerd://new-instance"
					}
				}
			case "sidecar-restart", "completed-init-restart", "sidecar-image", "pod-replacement":
				f.commandHook = func(count int, _ hbnRuntimeCommand) {
					if count != 1 {
						return
					}
					pod := &f.pods[1] // Drift on the other selected DPU must fence this read too.
					switch scenario {
					case "pod-replacement":
						pod.UID = "replacement-pod"
					case "sidecar-image":
						pod.Status.InitContainerStatuses[1].ImageID = "foreign.example/hbn@sha256:" + strings.Repeat("e", 64)
					case "sidecar-restart":
						pod.Status.InitContainerStatuses[1].RestartCount++
						pod.Status.InitContainerStatuses[1].ContainerID = "containerd://new-sidecar"
					case "completed-init-restart":
						pod.Status.InitContainerStatuses[0].RestartCount++
						pod.Status.InitContainerStatuses[0].ContainerID = "containerd://new-init"
						pod.Status.InitContainerStatuses[0].State.Terminated.ContainerID = "containerd://new-init"
					}
				}
			case "interface-replacement":
				f.commandHook = func(count int, _ hbnRuntimeCommand) {
					if count == 1 {
						f.client.interfaces[0].SetUID("replacement-interface")
					}
				}
			case "lease-replaced":
				f.commandHook = func(count int, _ hbnRuntimeCommand) {
					if count == 1 {
						f.snapshot.CIDRPool.SetUID("replacement-pool")
					}
				}
			case "assignment-drift":
				f.commandHook = func(count int, _ hbnRuntimeCommand) {
					if count == 1 {
						rows, _, _ := unstructured.NestedSlice(f.snapshot.CIDRPool.Object, "status", "allocations")
						rows[0].(map[string]any)["nodeName"] = "foreign-node"
						_ = unstructured.SetNestedSlice(f.snapshot.CIDRPool.Object, rows, "status", "allocations")
					}
				}
			case "tenant-denied":
				f.denyTenant = true
			case "management-denied":
				f.client.denyManagement = true
			case "incomplete-list":
				f.incompleteList = true
			case "canceled":
				canceled, cancel := context.WithCancel(f.ctx)
				cancel()
				f.ctx = canceled
			case "cancel-after-read":
				canceled, cancel := context.WithCancel(f.ctx)
				defer cancel()
				f.ctx = canceled
				f.commandHook = func(count int, _ hbnRuntimeCommand) {
					if count == 1 {
						cancel()
					}
				}
			case "later-exec-failure":
				f.transports.readCommand = func(ctx context.Context, ns, pod string, command hbnRuntimeCommand, prefix netip.Prefix) ([]byte, error) {
					if f.commandReads >= 2 {
						return nil, errors.New("raw-secret-canary")
					}
					return original(ctx, ns, pod, command, prefix)
				}
			case "host-receipt-mismatch":
				f.hosts[1].Configuration.RequestID = "wrong-binding"
			}
			journal := testJournal(f.prepared.execution)
			result, err := f.run(journal)
			if err == nil || result.TrafficValidated || result.State != "failed" {
				t.Fatalf("unsafe success: result=%+v err=%v", result, err)
			}
			if scenario == "service-values" || scenario == "container-restart" || scenario == "sidecar-restart" || scenario == "completed-init-restart" || scenario == "sidecar-image" || scenario == "pod-replacement" || scenario == "interface-replacement" || scenario == "cancel-after-read" || scenario == "lease-replaced" || scenario == "assignment-drift" {
				if len(result.BGP) != 1 || result.BGP[0].Fenced || f.commandReads != 1 || f.trafficReads != 0 {
					t.Fatalf("post-fence drift not retained/stopped: %+v", result)
				}
			}
			if scenario == "later-exec-failure" {
				if len(result.BGP) != 2 || !result.BGP[0].Fenced || len(result.Routes) != 1 || !result.Routes[0].Fenced {
					t.Fatalf("later partial receipts missing: %+v", result)
				}
			}
			if strings.Contains(journal.file.(*managementMemoryFile).String(), "raw-secret-canary") {
				t.Fatal("raw transport output leaked")
			}
		})
	}
}

func TestHBNRuntimeStageSingleHostNeverValidatesTraffic(t *testing.T) {
	f := newRuntimeStageFixture(t, []string{"SERIAL-A"})
	result, err := f.run(testJournal(f.prepared.execution))
	if err != nil || result.TrafficValidated || result.State != "unmeasured" || len(result.BGP) != 1 || f.trafficReads != 0 || len(result.Routes) != 0 {
		t.Fatalf("one-host result=%+v err=%v", result, err)
	}
}

func TestHBNRuntimeStageJournalFailureRetainsPartialResult(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	for _, failAt := range []int{1, 2, 3, 19} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			f := newRuntimeStageFixtureFromExecution(t, execution)
			journal := testJournal(f.prepared.execution)
			journal.file = &hostNetworkStageFailingJournalFile{managementMemoryFile: journal.file.(*managementMemoryFile), failSyncAt: failAt}
			result, err := f.run(journal)
			if err == nil || result.TrafficValidated || journal.failed == nil {
				t.Fatalf("journal failure passed: result=%+v err=%v", result, err)
			}
			if failAt == 1 && (f.commandReads != 0 || len(result.BGP) != 0) {
				t.Fatal("measurement after undurable attempt")
			}
			if failAt == 2 && (f.commandReads != 1 || len(result.BGP) != 1 || result.BGP[0].Fenced) {
				t.Fatal("lost partial result after failed sync")
			}
		})
	}
}

// Keep the fixture clone helper honest: a retained expectation must never be
// an alias of an observed mutable API object.
func TestHBNRuntimeFactoryInputsAreIndependent(t *testing.T) {
	f := newRuntimeStageFixture(t, []string{"SERIAL-A", "SERIAL-B"})
	observer, err := newHostNetworkFenceObserver(f.ctx, f.client, f.prepared, f.controlPlane, f.workload, f.activation, testJournal(f.prepared.execution), time.Second, f.transports.readLeases, hbnRuntimeStage, "HBN_RUNTIME")
	if err != nil {
		t.Fatal(err)
	}
	fence, err := observer()
	if err != nil {
		t.Fatal(err)
	}
	intent, err := compileHBNRuntimeBinding(f.prepared.execution.verificationInputs(), f.activation, fence, f.client.service, f.client.interfaces, f.image)
	if err != nil {
		t.Fatal(err)
	}
	before := runtime.DeepCopyJSON(intent.ExpectedServiceSpec)
	appBefore := runtime.DeepCopyJSON(intent.ApplicationValues)
	_ = unstructured.SetNestedField(f.client.service.Object, "evil", "spec", "helmChart", "values", "configuration", "startupYAMLJ2")
	if !jsonEqual(intent.ExpectedServiceSpec, before) || !jsonEqual(intent.ApplicationValues, appBefore) {
		t.Fatal("factory expectations alias observed API state")
	}
	if _, err = compileHBNRuntimeBinding(f.prepared.execution.verificationInputs(), f.activation, fence, f.client.service, f.client.interfaces, f.image); err == nil {
		t.Fatal("factory accepted unretained values")
	}
}

// A shared prepared execution must not become a cache of mutable API state,
// transports, closures or cleanup ownership from an earlier table case.
func TestRuntimeStagePreparedExecutionKeepsInstancesIndependent(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	snapshotPrepared := func() []byte {
		chart, err := chartFingerprint(execution.operator.chart)
		if err != nil {
			t.Fatal(err)
		}
		crds := make([]any, 0, len(execution.crds))
		for _, crd := range execution.crds {
			crds = append(crds, map[string]any{"name": crd.name, "spec": crd.spec, "versions": crd.versions})
		}
		snapshot := map[string]any{"intent": execution.plan.Intent(), "bundle": execution.retained.Files(), "manager": execution.manager, "config": execution.configObject, "namespaces": execution.namespaces, "crds": crds, "chart": chart, "operatorManifest": execution.operator.renderedManifest, "operatorValues": execution.operator.valuesYAML}
		data, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	before := snapshotPrepared()
	var firstContext context.Context
	const canary = "first-fixture-only"
	t.Run("mutate and release first instance", func(t *testing.T) {
		f := newRuntimeStageFixtureFromExecution(t, execution)
		firstContext = f.ctx
		if firstContext.Err() != nil {
			t.Fatal("first fixture did not start with a live independent context")
		}
		software, err := retainedBlueFieldSoftware(execution.verificationInputs())
		if err != nil {
			t.Fatal(err)
		}
		current := f.client.base.resources[tenantStageKey(software)]
		if current == nil {
			t.Fatal("software fixture is missing")
		}
		if err := unstructured.SetNestedField(current.Object, canary, "spec", "osIso"); err != nil {
			t.Fatal(err)
		}
		if len(execution.crds) == 0 {
			t.Fatal("prepared CRD expectations missing")
		}
		crd := f.client.operator.crds[execution.crds[0].name]
		if crd == nil {
			t.Fatal("observed CRD fixture missing")
		}
		if err := unstructured.SetNestedField(crd.Object, canary, "spec", "group"); err != nil {
			t.Fatal(err)
		}
		if len(f.pods) == 0 || len(f.pods[0].Status.ContainerStatuses) == 0 || f.snapshot.CIDRPool == nil {
			t.Fatal("mutable Pod/lease fixture missing")
		}
		f.pods[0].Status.ContainerStatuses[0].ContainerID = canary
		f.snapshot.CIDRPool.SetUID(types.UID(canary))
		f.commandReads, f.trafficReads, f.leaseReads = 91, 92, 93
		f.denyTenant, f.client.denyManagement = true, true
		f.commandHook = func(int, hbnRuntimeCommand) { f.client.denyManagement = true }
		f.transports.readCommand = func(context.Context, string, string, hbnRuntimeCommand, netip.Prefix) ([]byte, error) {
			return nil, errors.New(canary)
		}
	})
	if firstContext == nil || !errors.Is(firstContext.Err(), context.Canceled) {
		t.Fatal("first child fixture did not cancel its own context during cleanup")
	}
	if !bytes.Equal(before, snapshotPrepared()) {
		t.Fatal("first observed-state mutations changed the retained prepared execution")
	}
	t.Run("fresh second instance runs normally", func(t *testing.T) {
		f := newRuntimeStageFixtureFromExecution(t, execution)
		if f.ctx.Err() != nil || f.commandReads != 0 || f.trafficReads != 0 || f.leaseReads != 0 || f.denyTenant || f.client.denyManagement {
			t.Fatal("second fixture inherited closed context, counters or permission hooks")
		}
		observed, err := json.Marshal(map[string]any{"pods": f.pods, "lease": f.snapshot.CIDRPool, "resources": f.client.base.resources, "crds": f.client.operator.crds})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(observed, []byte(canary)) {
			t.Fatal("second fixture inherited a first-instance deep API mutation")
		}
		result, err := f.run(testJournal(execution))
		if err != nil || !result.TrafficValidated || result.State != "passed" || len(result.BGP) != 2 || len(result.Routes) != 2 || len(result.Traffic) != 2 || f.commandReads != 4 || f.trafficReads != 2 {
			t.Fatalf("fresh second real runtime flow failed: result=%+v commands=%d traffic=%d err=%v", result, f.commandReads, f.trafficReads, err)
		}
	})
	if !bytes.Equal(before, snapshotPrepared()) {
		t.Fatal("second runtime flow mutated the shared prepared input")
	}
}
