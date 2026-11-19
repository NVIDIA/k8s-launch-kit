// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/utils/ptr"
)

type tenantPendingManagement struct {
	validationManagementReader
	sources            func() (tenantSources, error)
	hostDNS            *unstructured.Unstructured
	cluster            *unstructured.Unstructured
	missingDeclaration string
}

func (m *tenantPendingManagement) observeTenantSources(context.Context, controllercontract.TenantReadinessExpected) (tenantSources, error) {
	return m.sources()
}
func (*tenantPendingManagement) observeValidationTenantStorage(context.Context, string) managementprobe.TenantStorageResult {
	return managementprobe.TenantStorageResult{State: "passed"}
}
func (m *tenantPendingManagement) currentValidationReads() validationCurrentManagementReader {
	return m
}
func (m *tenantPendingManagement) Get(ctx context.Context, object *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if m.cluster != nil && object.GetKind() == "DPUCluster" && object.GetName() == m.cluster.GetName() && object.GetNamespace() == m.cluster.GetNamespace() {
		return m.cluster.DeepCopy(), nil
	}
	if object.GetKind() == "DPUServiceInterface" && object.GetName() == m.missingDeclaration {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "dpuserviceinterfaces"}, object.GetName())
	}
	if m.hostDNS != nil && object.GetKind() == "Service" && object.GetName() == m.hostDNS.GetName() && object.GetNamespace() == m.hostDNS.GetNamespace() {
		return m.hostDNS.DeepCopy(), nil
	}
	return m.validationManagementReader.Get(ctx, object)
}

type tenantPendingTransport struct {
	next    http.RoundTripper
	objects map[string]any
	reads   map[string]int
}

func (r tenantPendingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	object, ok := r.objects[request.URL.Path]
	if !ok {
		return r.next.RoundTrip(request)
	}
	if request.Method != http.MethodGet {
		return nil, fmt.Errorf("tenant regression attempted an effect")
	}
	r.reads[request.URL.Path]++
	data, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data))), Request: request}, nil
}

// The native source/node, DNS, kube-proxy and complete HBN/SFC resource paths
// share one recorder and run under the real Runner. Unrelated area coverage is
// explicit fixture evidence; no dependent row is filled with a synthetic pass.
type tenantPendingObserver struct {
	fixture     *currentReadHTTPFixture
	management  *tenantPendingManagement
	checks      []validation.CheckSpec
	independent []validation.CheckSpec
	hostDNS     *corev1.Service
	calls       int
	openCalls   int
	first       []validation.Outcome
	blocker     error
	mutate      func(*tenantSources)
}

func (o *tenantPendingObserver) ObserveResources(ctx context.Context, progress validation.Progress) (validation.ResourceSnapshot, error) {
	o.calls++
	active := []validation.CheckSpec{}
	for _, spec := range o.checks {
		if !spec.FullOnly {
			active = append(active, spec)
		}
	}
	r := newValidationRecorder(active, progress)
	for _, spec := range o.independent {
		if !spec.FullOnly {
			r.record(spec.ID, validation.Passed, validation.ReasonNone, "Independent fixture boundary.", "")
		}
	}
	current := o.fixture.sources
	current.observed.TenantControlPlane = current.observed.TenantControlPlane.DeepCopy()
	if o.calls == 1 {
		_ = unstructured.SetNestedField(current.observed.TenantControlPlane.Object, "NotReady", "status", "kubernetesResources", "version", "status")
	}
	if o.mutate != nil {
		o.mutate(&current)
	}
	o.management.sources = func() (tenantSources, error) { return current, o.blocker }
	tenant, err := o.fixture.adapter.native.observeTenantResourcesWithOpener(ctx, o.management, r, func(ctx context.Context, _ validationManagementReader) (*tenantClient, error) {
		o.openCalls++
		return o.fixture.adapter.tenant, o.fixture.adapter.tenant.VerifyIdentity(ctx)
	})
	if validationIdentityChanged(err) {
		return r.snapshot(), err
	}
	if err := o.fixture.adapter.native.observeValidationTenantDNS(ctx, o.management, tenant, o.hostDNS, r); err != nil {
		return r.snapshot(), err
	}
	if err := o.fixture.adapter.native.observeValidationKubeProxy(ctx, o.management, tenant, r); err != nil {
		return r.snapshot(), err
	}
	if err := o.fixture.adapter.native.observeValidationRuntimeResourcesWithImage(ctx, o.management, tenant, r, func(context.Context, string, time.Duration, http.RoundTripper) (hbnPodImageIdentity, error) {
		return o.fixture.adapter.image, nil
	}); err != nil {
		return r.snapshot(), err
	}
	r.blockRemaining()
	if o.calls == 1 {
		o.first = r.snapshot().Checks
	}
	return r.snapshot(), nil
}
func (*tenantPendingObserver) OpenRuntime(context.Context, validation.Progress) (validation.RuntimeSession, error) {
	return nil, errors.New("resource-only regression cannot open runtime")
}

func newTenantPendingObserver(t *testing.T) (*tenantPendingObserver, validation.Plan, map[string]int) {
	t.Helper()
	f := newCurrentReadHTTPFixture(t, false)
	native := f.adapter.native
	expectedTenant, wantedCluster, err := tenantPlanInputs(native.desired.plan)
	if err != nil {
		t.Fatal(err)
	}
	currentCluster, err := native.desired.verifier.markedDesired(wantedCluster)
	if err != nil {
		t.Fatal(err)
	}
	currentCluster.SetUID(f.sources.observed.DPUCluster.GetUID())
	currentCluster.SetResourceVersion("1")
	currentCluster.SetGeneration(2)
	currentCluster.Object["status"] = runtime.DeepCopyJSONValue(f.sources.observed.DPUCluster.Object["status"])
	f.sources.observed.DPUCluster = currentCluster
	bindTenantSources(t, expectedTenant, &f.sources)
	_ = unstructured.SetNestedMap(f.sources.observed.TenantControlPlane.Object, map[string]any{}, "spec", "addons", "kubeProxy")
	f.adapter.tenant.expected = expectedTenant
	f.adapter.tenant.sourceDigest, _, _, err = validateTenantSources(native.desired.rules, expectedTenant, string(currentCluster.GetUID()), f.sources)
	if err != nil {
		t.Fatal(err)
	}
	if err := native.desired.verifier.Verify(wantedCluster, currentCluster); err != nil {
		t.Fatalf("source fixture retention: %v", err)
	}
	checks, scope, err := validationRuntimeChecks(native.desired.plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range native.validationResourceChecks() {
		id := string(spec.ID)
		if strings.HasPrefix(id, "tenant/") {
			checks = append(checks, spec)
		}
	}
	o := &tenantPendingObserver{fixture: f, checks: checks}
	for _, area := range []validation.Area{validation.AreaManagement, validation.AreaOperator, validation.AreaDefinitions, validation.AreaSystem, validation.AreaSoftware, validation.AreaPhysical} {
		spec := validation.CheckSpec{ID: validation.CheckID("fixture/" + string(area)), Area: area, Subject: "Independent " + string(area), Required: true, Coverage: true, FullOnly: area == validation.AreaPhysical}
		o.independent = append(o.independent, spec)
		o.checks = append(o.checks, spec)
	}
	host, paths := tenantPendingNetworkingFixture(t, f)
	o.hostDNS = host
	hostObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(host)
	if err != nil {
		t.Fatal(err)
	}
	o.management = &tenantPendingManagement{validationManagementReader: f.management, hostDNS: &unstructured.Unstructured{Object: hostObject}, cluster: currentCluster}
	plan, err := validation.NewPlan(validation.PlanInput{Mode: validation.ModeResources, Checks: o.checks, Runtime: scope})
	if err != nil {
		t.Fatal(err)
	}
	return o, plan, paths
}

func TestValidationTenantPendingNativeCompositionConvergesThroughRunner(t *testing.T) {
	o, plan, reads := newTenantPendingObserver(t)
	runner, err := validation.NewRunner(plan, o, validation.Options{Wait: 30 * time.Second, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	result := runner.Run(context.Background())
	if o.calls != 2 || o.openCalls != 1 || result.Verdict != validation.VerdictResourcesPassed {
		for _, row := range result.Checks {
			if row.Outcome.State != validation.Passed && row.Outcome.State != validation.Skipped {
				t.Logf("blocking: %+v", row)
			}
		}
		t.Fatalf("tenant convergence: calls=%d open=%d verdict=%s first=%+v", o.calls, o.openCalls, result.Verdict, o.first)
	}
	for _, row := range o.first {
		if strings.HasPrefix(string(row.ID), "tenant/") && row.ID != "tenant/storage" || strings.HasPrefix(string(row.ID), "hbn/") || strings.HasPrefix(string(row.ID), "workloads/") {
			if row.State != validation.Pending || row.Reason != validation.ReasonProgressing {
				t.Fatalf("unavailable tenant dependent was not pollable: %+v", row)
			}
		}
	}
	for _, path := range []string{"/api/v1/namespaces/kube-system/services/kube-dns", "/apis/discovery.k8s.io/v1/namespaces/kube-system/endpointslices", "/apis/apps/v1/namespaces/kube-system/daemonsets/kube-proxy", "/api/v1/namespaces/kube-system/pods"} {
		if reads[path] == 0 {
			t.Fatalf("healthy snapshot skipped native networking read %s", path)
		}
	}
}

func TestValidationTenantSourceBlockersStopRunner(t *testing.T) {
	for _, scenario := range []string{"denied", "malformed", "conflict", "terminal", "unsupported", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			o, plan, reads := newTenantPendingObserver(t)
			switch scenario {
			case "denied":
				o.blocker = apierrors.NewForbidden(schema.GroupResource{Resource: "tenantcontrolplanes"}, "selected", nil)
			case "unavailable":
				o.blocker = errors.New("unavailable")
			default:
				o.mutate = func(current *tenantSources) {
					switch scenario {
					case "malformed":
						_ = unstructured.SetNestedField(current.observed.TenantControlPlane.Object, int64(7), "status", "kubernetesResources", "version", "status")
					case "unsupported":
						_ = unstructured.SetNestedField(current.observed.TenantControlPlane.Object, "Unsupported", "status", "kubernetesResources", "version", "status")
					case "conflict":
						_ = unstructured.SetNestedField(current.observed.TenantControlPlane.Object, "192.0.2.99", "spec", "networkProfile", "address")
					case "terminal":
						current.observed.DPUCluster = current.observed.DPUCluster.DeepCopy()
						_ = unstructured.SetNestedField(current.observed.DPUCluster.Object, "Error", "status", "phase")
					}
				}
			}
			runner, err := validation.NewRunner(plan, o, validation.Options{Wait: time.Second, PollInterval: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			result := runner.Run(context.Background())
			if o.calls != 1 || o.openCalls != 0 || result.Verdict == validation.VerdictResourcesPassed {
				t.Fatalf("nonprogressing source retried or passed: calls=%d open=%d verdict=%s first=%+v", o.calls, o.openCalls, result.Verdict, o.first)
			}
			for _, row := range o.first {
				if row.ID == "tenant/sources" && row.State == validation.Pending {
					t.Fatalf("blocker became Pending: %+v", row)
				}
			}
			for path, count := range reads {
				if count != 0 {
					t.Fatalf("blocked tenant performed networking reads: %s %d", path, count)
				}
			}
		})
	}
}

func tenantPendingNetworkingFixture(t *testing.T, f *currentReadHTTPFixture) (*corev1.Service, map[string]int) {
	t.Helper()
	intent := f.adapter.native.desired.plan.Intent()
	host := &corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: metav1.ObjectMeta{Name: "host-dns", Namespace: intent.Namespace, UID: "host-dns", ResourceVersion: "1"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromString("dns"), NodePort: 31001}, {Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString("dns-tcp"), NodePort: 31002}}}}
	service := &corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system", UID: "dns", ResourceVersion: "1", Labels: map[string]string{"k8s-app": "kube-dns", "kubernetes.io/name": "CoreDNS"}}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.10", Ports: []corev1.ServicePort{{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt32(53)}, {Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(53)}}}}
	slice := discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system", UID: "dns-endpoints", ResourceVersion: "1", Labels: map[string]string{discoveryv1.LabelServiceName: "kube-dns", discoveryv1.LabelManagedBy: "kamaji-cluster-manager"}}, AddressType: discoveryv1.AddressTypeIPv4, Ports: []discoveryv1.EndpointPort{{Name: ptr.To("dns"), Port: ptr.To(int32(31001)), Protocol: ptr.To(corev1.ProtocolUDP)}, {Name: ptr.To("dns-tcp"), Port: ptr.To(int32(31002)), Protocol: ptr.To(corev1.ProtocolTCP)}}, Endpoints: []discoveryv1.Endpoint{{Addresses: []string{intent.Bootstrap.Tenant.VIP}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}}}
	binding := &rbacv1.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: "kubeadm:node-proxier", UID: "proxy-anchor", ResourceVersion: "1"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:node-proxier"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "kube-proxy", Namespace: "kube-system"}}}
	owner := []metav1.OwnerReference{{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding", Name: binding.Name, UID: binding.UID, Controller: ptr.To(true)}}
	account := &corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: "kube-system", UID: "proxy-account", ResourceVersion: "1", OwnerReferences: owner}}
	endpoint, _, err := f.adapter.tenant.bootstrapCredentialSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	config := &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: "kube-system", UID: "proxy-config", ResourceVersion: "1"}, Data: map[string]string{"config.conf": fmt.Sprintf("apiVersion: kubeproxy.config.k8s.io/v1alpha1\nkind: KubeProxyConfiguration\nbindAddress: 0.0.0.0\nclusterCIDR: %s\nclientConnection:\n  kubeconfig: /var/lib/kube-proxy/kubeconfig.conf\nmode: \"\"\n", intent.Bootstrap.PodCIDR), "kubeconfig.conf": fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: default\n  cluster:\n    server: %s\n    certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt\ncontexts:\n- name: default\n  context: {cluster: default, user: default, namespace: default}\ncurrent-context: default\nusers:\n- name: default\n  user:\n    tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token\n", endpoint)}}
	var set appsv1.DaemonSet
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(validationExpectedKubeProxy(intent.Bootstrap.TenantKubernetesVersion).Object, &set); err != nil {
		t.Fatal(err)
	}
	set.UID, set.ResourceVersion, set.Generation, set.OwnerReferences = "proxy-set", "1", 1, owner
	nodes, err := f.adapter.native.validationSelectedTenantNodes(context.Background(), f.management, f.adapter.tenant)
	if err != nil {
		t.Fatal(err)
	}
	n := int32(len(nodes))
	set.Status = appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: n, CurrentNumberScheduled: n, UpdatedNumberScheduled: n, NumberReady: n, NumberAvailable: n}
	pods := []corev1.Pod{}
	for serial, node := range nodes {
		pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "proxy-" + strings.ToLower(serial), Namespace: "kube-system", UID: types.UID("proxy-" + serial), ResourceVersion: "1", Labels: set.Spec.Template.Labels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: set.Name, UID: set.UID, Controller: ptr.To(true)}}}, Spec: *set.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "kube-proxy", Image: set.Spec.Template.Spec.Containers[0].Image, ImageID: "sha256:proxy", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
		pod.Spec.NodeName = node.Name
		admitted := &runtimeStageFixture{pods: []corev1.Pod{pod}}
		admitN3cHBNPods(admitted)
		pod = admitted.pods[0]
		pod.Spec.ServiceAccountName, pod.Spec.DeprecatedServiceAccount = "kube-proxy", "kube-proxy"
		pod.Spec.Priority = ptr.To(int32(2000001000))
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{Key: corev1.TaintNodeNetworkUnavailable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule})
		pods = append(pods, pod)
	}
	objects := map[string]any{
		"/api/v1/namespaces/kube-system/services/kube-dns":                            service,
		"/apis/discovery.k8s.io/v1/namespaces/kube-system/endpointslices":             &discoveryv1.EndpointSliceList{TypeMeta: metav1.TypeMeta{APIVersion: "discovery.k8s.io/v1", Kind: "EndpointSliceList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []discoveryv1.EndpointSlice{slice}},
		"/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/kubeadm:node-proxier": binding,
		"/api/v1/namespaces/kube-system/serviceaccounts/kube-proxy":                   account,
		"/api/v1/namespaces/kube-system/configmaps/kube-proxy":                        config,
		"/apis/apps/v1/namespaces/kube-system/daemonsets/kube-proxy":                  &set,
		"/apis/apps/v1/namespaces/kube-system/daemonsets":                             &appsv1.DaemonSetList{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSetList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []appsv1.DaemonSet{set}},
		"/api/v1/namespaces/kube-system/pods":                                         &corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: pods},
	}
	reads := map[string]int{}
	base := f.adapter.tenant.api.CoreV1().RESTClient().Get().URL()
	f.adapter.tenant.api, err = kubernetes.NewForConfig(&rest.Config{Host: base.Scheme + "://" + base.Host, Timeout: time.Second, RateLimiter: flowcontrol.NewFakeAlwaysRateLimiter(), WrapTransport: func(next http.RoundTripper) http.RoundTripper {
		return tenantPendingTransport{next: next, objects: objects, reads: reads}
	}})
	if err != nil {
		t.Fatal(err)
	}
	return host, reads
}

func TestValidationTenantNativeOpenPreservesSourceClassification(t *testing.T) {
	o, _, _ := newTenantPendingObserver(t)
	for _, test := range []struct {
		name  string
		value any
		state validation.State
	}{
		{name: "progressing", value: "NotReady", state: validation.Pending},
		{name: "malformed", value: int64(7), state: validation.Unverified},
		{name: "unsupported", value: "Unsupported", state: validation.Unverified},
		{name: "conflict", value: "Ready", state: validation.Failed},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := o.fixture.sources
			current.observed.TenantControlPlane = current.observed.TenantControlPlane.DeepCopy()
			_ = unstructured.SetNestedField(current.observed.TenantControlPlane.Object, test.value, "status", "kubernetesResources", "version", "status")
			if test.name == "conflict" {
				_ = unstructured.SetNestedField(current.observed.TenantControlPlane.Object, "192.0.2.99", "spec", "networkProfile", "address")
			}
			o.management.sources = func() (tenantSources, error) { return current, nil }
			tenant, err := o.fixture.adapter.native.openValidationTenant(context.Background(), o.management)
			state, _, _, _ := validationErrorFields(err)
			if tenant != nil || err == nil || state != test.state {
				t.Fatalf("native connection classified %s as %s: tenant=%v err=%v", test.name, state, tenant != nil, err)
			}
		})
	}
}

func TestValidationTenantPendingFullDependenciesPreserveIndependentFailures(t *testing.T) {
	for _, declarationFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("declaration failure %t", declarationFailure), func(t *testing.T) {
			o, _, reads := newTenantPendingObserver(t)
			native := o.fixture.adapter.native
			native.mode = validation.ModeFull
			checks := append([]validation.CheckSpec(nil), o.checks...)
			independentID := validation.CheckID("physical/independent")
			checks = append(checks, validation.CheckSpec{ID: independentID, Area: validation.AreaPhysical, Subject: "Independent measured physical failure", Required: true, FullOnly: true})
			r := newValidationRecorder(checks, nil)
			r.record("tenant/sources", validation.Pending, validation.ReasonProgressing, "Controller progressing.", "")
			r.record(independentID, validation.Failed, validation.ReasonHealth, "Measured independent physical failure.", "")
			if declarationFailure {
				objects, err := retainedPlanWorkloadObjects(native.desired.plan)
				if err != nil {
					t.Fatal(err)
				}
				// This declaration follows the IPAM roots in retained ordering. A
				// missing tenant must not hide an independently missing declaration.
				for _, object := range objects {
					if object.GetKind() == "DPUServiceInterface" {
						o.management.missingDeclaration = object.GetName()
						break
					}
				}
			}
			if err := native.observeValidationRuntimeResourcesWithImage(context.Background(), o.management, nil, r, func(context.Context, string, time.Duration, http.RoundTripper) (hbnPodImageIdentity, error) {
				t.Fatal("missing tenant attempted image resolution")
				return hbnPodImageIdentity{}, errors.New("unexpected image resolution")
			}); err != nil {
				t.Fatal(err)
			}
			r.blockRemaining()
			if got := r.outcomes[independentID]; got.State != validation.Failed || got.Reason != validation.ReasonHealth {
				t.Fatalf("independent failure overwritten: %+v", got)
			}
			if got := r.outcomes["devices/coverage"]; got.State != validation.Passed {
				t.Fatalf("independent device collection was lost: %+v", got)
			}
			if got := r.outcomes["workloads/coverage"]; declarationFailure && (got.State != validation.Failed || got.Reason != validation.ReasonMissing) {
				t.Fatalf("measured declaration failure erased by Pending: %+v", got)
			}
			for _, device := range native.desired.plan.Intent().Devices {
				for _, prefix := range []string{"physical/host-network/", "hbn/", "workloads/"} {
					if got := r.outcomes[validation.CheckID(prefix+device.Serial)]; got.State != validation.Pending || got.Reason != validation.ReasonProgressing {
						t.Fatalf("full-mode tenant dependent blocked wait: %+v", got)
					}
				}
			}
			for path, count := range reads {
				if count != 0 {
					t.Fatalf("missing tenant attempted networking read: %s %d", path, count)
				}
			}
		})
	}
}

func TestValidationTenantPendingSystemRoleDependencyBoundary(t *testing.T) {
	o := validationResourceFixture(t)
	role := systemArtifactRole{Component: "flannel", Management: false}
	prefix := systemRoleID(role)
	service := validationObject("svc.dpu.nvidia.com/v1alpha1", "DPUService", o.desired.operatorNamespace, "flannel")
	app := validationObject("argoproj.io/v1alpha1", "Application", o.desired.operatorNamespace, "flannel")
	_ = unstructured.SetNestedSlice(app.Object, []any{map[string]any{"group": "apps", "version": "v1", "kind": "DaemonSet", "namespace": service.GetNamespace(), "name": "flannel", "status": "Synced"}}, "status", "resources")
	for _, sourceState := range []validation.State{validation.Pending, validation.Unverified, validation.Failed} {
		r := newValidationRecorder(o.validationResourceChecks(), nil)
		reason := validation.ReasonProgressing
		if sourceState != validation.Pending {
			reason = validation.ReasonDenied
		}
		r.record("tenant/sources", sourceState, reason, "Observed source.", "")
		err := r.run(validation.CheckID(prefix+"/workload"), func() error {
			return o.observeValidationSystemWorkload(context.Background(), &validationReadFake{}, nil, role, service, app, systemArtifactExpectation{WorkloadKind: "DaemonSet"}, nil, r)
		})
		r.pendingDependents(prefix+"/device/", validation.CheckID(prefix+"/workload"))
		r.pendingDependents("definitions/tenant/", "tenant/sources")
		r.blockRemaining()
		state, _, _, _ := validationErrorFields(err)
		want := validation.Unverified
		if sourceState == validation.Pending {
			want = validation.Pending
		}
		if state != want {
			t.Fatalf("system role did not retain tenant state %s: %s", sourceState, state)
		}
		for _, spec := range o.systemValidationChecks() {
			if strings.HasPrefix(string(spec.ID), prefix+"/device/") || strings.HasPrefix(string(spec.ID), "definitions/tenant/") {
				if got := r.outcomes[spec.ID]; got.State != want {
					t.Fatalf("system/definition dependent had wrong state: %+v want %s", got, want)
				}
			}
		}
	}
}

func TestValidationTenantPendingKeepsMeasuredSystemFailures(t *testing.T) {
	o := validationResourceFixture(t)
	o.fetchSystemChart = func(context.Context, string) ([]byte, error) { return nil, errors.New("offline artifact") }
	r := newValidationRecorder(o.validationResourceChecks(), nil)
	r.record("tenant/sources", validation.Pending, validation.ReasonProgressing, "Controller progressing.", "")
	r.pendingDependents("tenant/api", "tenant/sources")
	if err := o.observeSystemResources(context.Background(), &validationReadFake{}, nil, r); err != nil {
		t.Fatal(err)
	}
	r.blockRemaining()
	for _, role := range systemArtifactRoles() {
		id := validation.CheckID(systemRoleID(role) + "/service")
		if got := r.outcomes[id]; got.State != validation.Failed || got.Reason != validation.ReasonMissing {
			t.Fatalf("tenant progress erased authoritative missing system role: %+v", got)
		}
	}
	if got := r.outcomes["system/artifact"]; got.State != validation.Unverified || got.Reason != validation.ReasonUnavailable {
		t.Fatalf("independent artifact error was erased: %+v", got)
	}
	for _, id := range []validation.CheckID{"tenant/dns/service", "tenant/dns/endpoints", "tenant/kube-proxy"} {
		if got := r.outcomes[id]; got.State != validation.Pending {
			t.Fatalf("unperformed tenant row lost progress: %+v", got)
		}
	}
}

func TestValidationPendingCoreDNSRoleRetainsSeparateDNSObligation(t *testing.T) {
	o := validationResourceFixture(t)
	role := systemArtifactRole{Component: "coredns", Management: true}
	for _, parentSuffix := range []string{"service", "application"} {
		r := newValidationRecorder(o.validationResourceChecks(), nil)
		parent := validation.CheckID(systemRoleID(role) + "/" + parentSuffix)
		r.record(parent, validation.Pending, validation.ReasonProgressing, "CoreDNS role progressing.", "")
		r.pendingSystemRoleDependents(role, parent)
		r.pendingDependents("tenant/dns/endpoints", "system/dns/management-service")
		for _, id := range []validation.CheckID{"system/dns/management-service", "tenant/dns/endpoints"} {
			if got := r.outcomes[id]; got.State != validation.Pending || got.Reason != validation.ReasonProgressing {
				t.Fatalf("%s lost CoreDNS role progress: %+v", parent, got)
			}
		}
	}
}
