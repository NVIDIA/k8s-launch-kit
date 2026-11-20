// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/utils/ptr"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestValidationCurrentSetSelectorsUseFreshDeviceAndNodeLabels(t *testing.T) {
	o, reader, selected := runtimeCurrentCorrectionFixture(t)
	current := reader.objects[tenantStageKey(selected[0].dpuSet)].DeepCopy()
	device := reader.objects["DPUDevice/"+current.GetNamespace()+"/"+selected[0].deviceName()]
	deviceLabels := device.GetLabels()
	deviceLabels["current.example/rack"] = "selected"
	device.SetLabels(deviceLabels)
	node := reader.objects["DPUNode/"+current.GetNamespace()+"/"+selected[0].nodeName]
	node.SetLabels(map[string]string{"current.example/host": "selected"})
	expression := func(key, op string, values ...string) map[string]any {
		terms := make([]any, len(values))
		for i := range values {
			terms[i] = values[i]
		}
		return map[string]any{"matchExpressions": []any{map[string]any{"key": key, "operator": op, "values": terms}}}
	}
	for _, test := range []struct {
		name         string
		device, node any
		legacy       map[string]string
		reject       bool
	}{
		{name: "empty selector selects all", device: map[string]any{}, reject: true},
		{name: "omitted selector selects all", reject: true},
		{name: "legacy selected equality", legacy: map[string]string{"current.example/rack": "selected"}, reject: true},
		{name: "legacy disjoint equality", legacy: map[string]string{"current.example/rack": "other"}},
		{name: "current nonretained label expression", device: expression("current.example/rack", "In", "selected"), reject: true},
		{name: "broader subset map", device: map[string]any{"matchLabels": map[string]any{"current.example/rack": "selected"}}, reject: true},
		{name: "exists expression", device: expression("current.example/rack", "Exists"), reject: true},
		{name: "notin expression", device: expression("current.example/rack", "NotIn", "other"), reject: true},
		{name: "doesnotexist expression", device: expression("current.example/absent", "DoesNotExist"), reject: true},
		{name: "matching node expression", device: map[string]any{}, node: expression("current.example/host", "In", "selected"), reject: true},
		{name: "disjoint device", device: expression("current.example/rack", "In", "other")},
		{name: "disjoint node", device: map[string]any{}, node: expression("current.example/host", "In", "other")},
		{name: "malformed relevant expression", device: expression("current.example/rack", "Invalid", "selected"), reject: true},
		{name: "malformed device proven disjoint node", device: expression("current.example/rack", "Invalid", "selected"), node: expression("current.example/host", "In", "other")},
		{name: "malformed node proven disjoint device", device: expression("current.example/rack", "In", "other"), node: expression("current.example/host", "Invalid", "selected")},
	} {
		t.Run(test.name, func(t *testing.T) {
			extra := current.DeepCopy()
			extra.SetName("foreign-set")
			extra.SetUID("foreign-set-uid")
			extra.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "svc.dpu.nvidia.com/v1alpha1", Kind: "DPUDeployment", Name: "foreign", UID: "foreign-dd", Controller: func() *bool { value := true; return &value }()}})
			if test.device == nil {
				unstructured.RemoveNestedField(extra.Object, "spec", "dpuDeviceSelector")
			} else {
				_ = unstructured.SetNestedField(extra.Object, test.device, "spec", "dpuDeviceSelector")
			}
			if test.legacy != nil {
				_ = unstructured.SetNestedStringMap(extra.Object, test.legacy, "spec", "dpuSelector")
			}
			if test.node == nil {
				unstructured.RemoveNestedField(extra.Object, "spec", "dpuNodeSelector")
			} else {
				_ = unstructured.SetNestedField(extra.Object, test.node, "spec", "dpuNodeSelector")
			}
			reader.objects[tenantStageKey(extra)] = extra
			defer delete(reader.objects, tenantStageKey(extra))
			_, err := observeCurrentRuntimeGraph(context.Background(), reader, o.desired, selected)
			if (err != nil) != test.reject {
				t.Fatalf("reject=%v, got %v", test.reject, err)
			}
		})
	}
}

// Model the documented Kubernetes daemon/SA/admission transformations on these
// synthetic chart fixtures. Expected controller function is left untouched.
func admitN3cHBNPods(f *runtimeStageFixture) {
	for i := range f.pods {
		pod := &f.pods[i]
		pod.Spec.ServiceAccountName, pod.Spec.DeprecatedServiceAccount = "default", "default"
		pod.Spec.Priority, pod.Spec.PreemptionPolicy, pod.Spec.EnableServiceLinks = ptr.To(int32(0)), ptr.To(corev1.PreemptLowerPriority), ptr.To(true)
		pod.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{{Key: metav1.ObjectNameField, Operator: corev1.NodeSelectorOpIn, Values: []string{pod.Spec.NodeName}}}}}}}}
		for _, key := range []string{corev1.TaintNodeNotReady, corev1.TaintNodeUnreachable} {
			pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute})
		}
		for _, key := range []string{corev1.TaintNodeDiskPressure, corev1.TaintNodeMemoryPressure, corev1.TaintNodePIDPressure, corev1.TaintNodeUnschedulable} {
			pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule})
		}
		const volumeName = "kube-api-access-abc12"
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: volumeName, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptr.To(int32(420)), Sources: []corev1.VolumeProjection{
			{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: ptr.To(int64(3607))}},
			{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
			{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
		}}}})
		for _, containers := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
			for j := range containers {
				containers[j].ImagePullPolicy = corev1.PullAlways
				containers[j].VolumeMounts = append(containers[j].VolumeMounts, corev1.VolumeMount{Name: volumeName, MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true})
			}
		}
	}
}

func n3cReadyWorkload(object *unstructured.Unstructured) {
	generation := object.GetGeneration()
	if generation < 1 {
		generation = 1
		object.SetGeneration(generation)
	}
	names := map[string][]string{
		"DPUService":            {"Ready", "ApplicationsReconciled", "ApplicationsReady"},
		"DPUServiceChain":       {"Ready", "ServiceChainSetReady", "ServiceChainSetReconciled"},
		"DPUServiceInterface":   {"Ready", "ServiceInterfacePreReqsReady", "ServiceInterfaceSetReady", "ServiceInterfaceSetReconciled"},
		"ServiceChainSet":       {"Ready", "ServiceChainsReconciled", "ServiceChainsReady"},
		"ServiceChain":          {"Ready", "ServiceChainReconciled"},
		"ServiceInterfaceSet":   {"Ready", "ServiceInterfacesReconciled", "ServiceInterfacesReady"},
		"NodeServiceInterfaces": {"Ready", "NodeServiceInterfacesReconciled"},
		"DPUServiceTemplate":    {"Ready", "DPUServiceTemplateReconciled"},
		"DPUServiceIPAM":        {"DPUIPAMObjectReconciled"},
	}
	conditions := []any{}
	for _, name := range names[object.GetKind()] {
		conditions = append(conditions, conditionMap(name, "True", "Success", generation))
	}
	status, _, _ := unstructured.NestedMap(object.Object, "status")
	if status == nil {
		status = map[string]any{}
	}
	status["observedGeneration"], status["conditions"], status["numberApplied"] = generation, conditions, int64(1)
	object.Object["status"] = status
}

type n3cRuntimeAdapter struct {
	native     *validationObserver
	management validationManagementReader
	tenant     *tenantClient
	image      hbnPodImageIdentity
	checks     []validation.CheckSpec
	objects    map[string]*unstructured.Unstructured
	pods       []corev1.Pod
	calls      int
	before     func(int)
	snapshots  []validation.ResourceSnapshot
}

func (o *n3cRuntimeAdapter) ObserveResources(ctx context.Context, _ validation.Progress) (validation.ResourceSnapshot, error) {
	o.calls++
	if o.before != nil {
		o.before(o.calls)
	}
	r := newValidationRecorder(o.checks, nil)
	err := o.native.observeValidationRuntimeResourcesWithImage(ctx, o.management, o.tenant, r, func(imageCtx context.Context, ref string, _ time.Duration, _ http.RoundTripper) (hbnPodImageIdentity, error) {
		if _, ok := imageCtx.Deadline(); !ok {
			return hbnPodImageIdentity{}, fmt.Errorf("image observation missing deadline")
		}
		if ref != o.image.Reference {
			return hbnPodImageIdentity{}, fmt.Errorf("image reference changed")
		}
		return o.image, nil
	})
	r.blockRemaining()
	snapshot := r.snapshot()
	o.snapshots = append(o.snapshots, snapshot)
	return snapshot, err
}
func (*n3cRuntimeAdapter) OpenRuntime(context.Context, validation.Progress) (validation.RuntimeSession, error) {
	return nil, fmt.Errorf("resource-only regression cannot open runtime")
}

// This joins the complete current selected device graph, current allocation,
// HBN App/DS/Pod and all six realized interface sets/NSI entries over real tenant
// HTTP reads. Only immutable OCI metadata resolution is substituted.
func n3cRuntimeAdapterFixture(t *testing.T, serials ...string) (*n3cRuntimeAdapter, map[string]*unstructured.Unstructured) {
	t.Helper()
	if len(serials) == 0 {
		serials = []string{"SERIAL-A"}
	}
	return n3cRuntimeAdapterFixtureFromPreparation(t, prepareValidationFixture(t, serials))
}

func n3cRuntimeAdapterFixtureFromPreparation(t *testing.T, prepared validationFixturePreparation) (*n3cRuntimeAdapter, map[string]*unstructured.Unstructured) {
	t.Helper()
	f := newRuntimeStageFixtureFromExecution(t, prepared.execution)
	desired := prepared.desired
	admitN3cHBNPods(f)
	for i := range f.snapshot.TenantNodes {
		nodeLabels := f.snapshot.TenantNodes[i].GetLabels()
		nodeLabels["svc.dpu.nvidia.com/dpuservicechain-version"] = f.client.deployment.GetName() + "-current"
		nodeLabels[corev1.LabelHostname] = f.activation.selected[i].dpuName
		f.snapshot.TenantNodes[i].SetLabels(nodeLabels)
	}
	objects := map[string]*unstructured.Unstructured{}
	for key, obj := range f.client.base.resources {
		objects[key] = obj.DeepCopy()
	}
	objects[tenantStageKey(f.client.deployment)] = f.client.deployment.DeepCopy()
	for _, rows := range [][]unstructured.Unstructured{f.client.sets, f.client.dpus, f.client.maint, f.client.versions, f.client.interfaces} {
		for i := range rows {
			obj := rows[i].DeepCopy()
			objects[tenantStageKey(obj)] = obj
		}
	}
	objects[tenantStageKey(f.client.service)] = f.client.service.DeepCopy()
	for _, obj := range objects {
		if strings.HasPrefix(obj.GetKind(), "DPUService") {
			n3cReadyWorkload(obj)
		}
	}
	reader := &currentGraphReadFixture{objects: objects}
	native := &validationObserver{desired: desired, request: f.request, mode: validation.ModeResources, physicalScope: validationPhysicalScope{expected: f.inventory}}
	management := &correctionHBNManagement{validationReadFake: &validationReadFake{get: reader.Get, list: reader.List}, app: f.client.app}
	tenant := testTenant(f.controlPlane.tenant.Client)
	children := runtimeValidationChildren(t, f, desired, objects)
	resources := map[string]string{"ServiceChainSet": "servicechainsets", "ServiceChain": "servicechains", "ServiceInterfaceSet": "serviceinterfacesets", "NodeServiceInterfaces": "nodeserviceinterfaces", "CIDRPool": "cidrpools"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews" && r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(&authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}})
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation %s %s", r.Method, r.URL.Path)
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"gitVersion": "v" + strings.TrimPrefix(tenant.expected.KubernetesVersion, "v")})
			return
		case "/api/v1/namespaces/kube-system":
			_ = json.NewEncoder(w).Encode(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "tenant-system"}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}})
			return
		case "/api/v1/nodes":
			_ = json.NewEncoder(w).Encode(&unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "v1", "kind": "NodeList", "metadata": map[string]any{"resourceVersion": "10"}}, Items: f.snapshot.TenantNodes})
			return
		case "/apis/apps/v1/namespaces/" + desired.plan.Intent().Namespace + "/daemonsets":
			_ = json.NewEncoder(w).Encode(&appsv1.DaemonSetList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}, Items: []appsv1.DaemonSet{f.ds}})
			return
		case "/api/v1/namespaces/" + desired.plan.Intent().Namespace + "/pods":
			_ = json.NewEncoder(w).Encode(&corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}, Items: f.pods})
			return
		case "/apis/svc.dpu.nvidia.com/v1alpha1", "/apis/nv-ipam.nvidia.com/v1alpha1":
			groupVersion := strings.TrimPrefix(r.URL.Path, "/apis/")
			served := []metav1.APIResource{}
			for kind, resource := range resources {
				if (kind == "CIDRPool") == strings.HasPrefix(groupVersion, "nv-ipam") {
					served = append(served, metav1.APIResource{Name: resource, Kind: kind, Namespaced: true})
				}
			}
			_ = json.NewEncoder(w).Encode(&metav1.APIResourceList{GroupVersion: groupVersion, APIResources: served})
			return
		}
		for _, pod := range f.pods {
			if r.URL.Path == "/api/v1/namespaces/"+pod.Namespace+"/pods/"+pod.Name {
				_ = json.NewEncoder(w).Encode(pod)
				return
			}
		}
		for kind, resource := range resources {
			group := "svc.dpu.nvidia.com"
			if kind == "CIDRPool" {
				group = "nv-ipam.nvidia.com"
			}
			for _, namespace := range []string{desired.plan.Intent().Namespace, desired.operatorNamespace} {
				prefix := "/apis/" + group + "/v1alpha1/namespaces/" + namespace + "/" + resource
				if r.URL.Path == prefix {
					selected, _ := labels.Parse(r.URL.Query().Get("labelSelector"))
					rows := []unstructured.Unstructured{}
					for _, obj := range children {
						if obj.GetKind() == kind && obj.GetNamespace() == namespace && selected.Matches(labels.Set(obj.GetLabels())) {
							rows = append(rows, *obj.DeepCopy())
						}
					}
					_ = json.NewEncoder(w).Encode(&unstructured.UnstructuredList{Object: map[string]any{"apiVersion": group + "/v1alpha1", "kind": kind + "List", "metadata": map[string]any{"resourceVersion": "10"}}, Items: rows})
					return
				}
				if strings.HasPrefix(r.URL.Path, prefix+"/") {
					obj := children[kind+"/"+strings.TrimPrefix(r.URL.Path, prefix+"/")]
					if obj != nil {
						_ = json.NewEncoder(w).Encode(obj)
						return
					}
				}
			}
		}
		t.Errorf("unexpected current tenant read %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	api, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, Timeout: time.Second, RateLimiter: flowcontrol.NewFakeAlwaysRateLimiter()})
	if err != nil {
		t.Fatal(err)
	}
	tenant.api = api
	checks, _, err := validationRuntimeChecks(desired.plan)
	if err != nil {
		t.Fatal(err)
	}
	active := []validation.CheckSpec{}
	for _, check := range checks {
		if !check.FullOnly {
			active = append(active, check)
		}
	}
	return &n3cRuntimeAdapter{native: native, management: management, tenant: tenant, image: f.image, objects: objects, pods: f.pods, checks: active, snapshots: []validation.ResourceSnapshot{}}, children
}

func TestValidationN3cNativeRuntimeResourceAdapterHealthyBaseline(t *testing.T) {
	adapter, _ := n3cRuntimeAdapterFixture(t)
	selected, selectErr := validationSelectedHosts(adapter.native.desired, adapter.native.request, adapter.native.physicalScope.expected)
	if selectErr != nil {
		t.Fatal(selectErr)
	}
	graph, graphErr := observeCurrentRuntimeGraph(context.Background(), adapter.management, adapter.native.desired, selected)
	if graphErr != nil {
		t.Fatal("graph: ", graphErr)
	}
	if err := adapter.native.observeRetainedWorkloads(context.Background(), adapter.management, adapter.tenant, graph); err != nil {
		t.Error("retained workloads: ", err)
	}
	allocation, _, allocationErr := observeCurrentAllocation(context.Background(), adapter.management, adapter.tenant, adapter.native.desired, graph)
	if allocationErr != nil {
		t.Fatal("allocation: ", allocationErr)
	}
	if sfcErr := adapter.native.observeTenantSFC(context.Background(), adapter.tenant, graph, allocation, "SERIAL-A"); sfcErr != nil {
		t.Fatal("tenant SFC: ", sfcErr)
	}
	snapshot, err := adapter.ObserveResources(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range snapshot.Checks {
		if row.State != validation.Passed {
			t.Errorf("native current resource row: %+v", row)
		}
	}
}

func TestValidationWorkloadReadinessPreservesOnlyKnownProgress(t *testing.T) {
	rules := testRules(t)
	for _, kind := range []string{"DPUService", "DPUServiceChain", "DPUServiceInterface", "ServiceChainSet", "ServiceChain", "ServiceInterfaceSet", "NodeServiceInterfaces"} {
		t.Run(kind, func(t *testing.T) {
			baseline := runtimeObject(kind, "selected", "dpf", map[string]any{})
			n3cReadyWorkload(baseline)
			for _, test := range []struct {
				name    string
				mutate  func(*unstructured.Unstructured)
				pending bool
			}{
				{name: "known Pending", pending: true, mutate: func(o *unstructured.Unstructured) {
					conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
					conditions[0] = conditionMap("Ready", "False", "Pending", 1)
					_ = unstructured.SetNestedSlice(o.Object, conditions, "status", "conditions")
				}},
				{name: "stale generation", pending: true, mutate: func(o *unstructured.Unstructured) {
					_ = unstructured.SetNestedField(o.Object, int64(0), "status", "observedGeneration")
				}},
				{name: "missing observed generation", pending: true, mutate: func(o *unstructured.Unstructured) {
					unstructured.RemoveNestedField(o.Object, "status", "observedGeneration")
				}},
				{name: "future generation", mutate: func(o *unstructured.Unstructured) {
					_ = unstructured.SetNestedField(o.Object, int64(2), "status", "observedGeneration")
				}},
				{name: "malformed generation", mutate: func(o *unstructured.Unstructured) {
					_ = unstructured.SetNestedField(o.Object, "1", "status", "observedGeneration")
				}},
				{name: "Unknown", mutate: func(o *unstructured.Unstructured) {
					conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
					conditions[0] = conditionMap("Ready", "Unknown", "Pending", 1)
					_ = unstructured.SetNestedSlice(o.Object, conditions, "status", "conditions")
				}},
				{name: "unrecognized False reason", mutate: func(o *unstructured.Unstructured) {
					conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
					conditions[0] = conditionMap("Ready", "False", "Unrecognized", 1)
					_ = unstructured.SetNestedSlice(o.Object, conditions, "status", "conditions")
				}},
				{name: "Pending cannot hide future sibling", mutate: func(o *unstructured.Unstructured) {
					conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
					conditions[0] = conditionMap("Ready", "False", "Pending", 1)
					conditions[1].(map[string]any)["observedGeneration"] = int64(2)
					_ = unstructured.SetNestedSlice(o.Object, conditions, "status", "conditions")
				}},
			} {
				t.Run(test.name, func(t *testing.T) {
					current := baseline.DeepCopy()
					test.mutate(current)
					r := newValidationRecorder([]validation.CheckSpec{{ID: "selected", Area: validation.AreaWorkloads, Subject: "selected", Required: true}}, nil)
					r.recordError("selected", validationRuntimeReadiness(rules.ValidationWorkloadReadiness(current), "Selected workload not current."))
					row := r.snapshot().Checks[0]
					if (row.State == validation.Pending) != test.pending || row.State == validation.Passed {
						t.Fatalf("pending=%v got %+v", test.pending, row)
					}
				})
			}
		})
	}
}

// Runner requires all resource areas. These fixed independent areas isolate the
// runtime adapter under test; no current-device/HBN/SFC outcome is manufactured,
// including the healthy second snapshot. Public whole-observer orchestration is
// covered separately and is not claimed by this narrower adapter regression.
type n3cRunnerBoundary struct {
	*n3cRuntimeAdapter
	independent []validation.Outcome
}

func (o *n3cRunnerBoundary) ObserveResources(ctx context.Context, p validation.Progress) (validation.ResourceSnapshot, error) {
	snapshot, err := o.n3cRuntimeAdapter.ObserveResources(ctx, p)
	snapshot.Checks = append(snapshot.Checks, o.independent...)
	return snapshot, err
}

func TestValidationN3cNativeWorkloadPendingConvergesThroughRunner(t *testing.T) {
	prepared := prepareValidationFixture(t, []string{"SERIAL-A"})
	for _, test := range []struct {
		name, kind           string
		tenant, stale, entry bool
		row                  validation.CheckID
	}{
		{name: "HBN service Pending", kind: "DPUService", row: "hbn/SERIAL-A"},
		{name: "HBN stale known reconciliation", kind: "DPUService", stale: true, row: "hbn/SERIAL-A"},
		{name: "management chain Pending", kind: "DPUServiceChain", row: "workloads/SERIAL-A"},
		{name: "realized tenant chain Pending", kind: "ServiceChain", tenant: true, row: "workloads/SERIAL-A"},
		{name: "selected NSI entry Pending", kind: "NodeServiceInterfaces", tenant: true, entry: true, row: "workloads/SERIAL-A"},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual, children := n3cRuntimeAdapterFixtureFromPreparation(t, prepared)
			var target *unstructured.Unstructured
			candidates := actual.objects
			if test.tenant {
				candidates = children
			}
			for _, object := range candidates {
				if object.GetKind() == test.kind {
					target = object
				}
			}
			if target == nil {
				t.Fatal("missing selected target")
			}
			original := target.DeepCopy()
			actual.before = func(call int) {
				if call > 1 {
					target.Object = original.DeepCopy().Object
					return
				}
				if test.stale {
					_ = unstructured.SetNestedField(target.Object, int64(0), "status", "observedGeneration")
					return
				}
				if test.entry {
					statuses, _, _ := unstructured.NestedSlice(target.Object, "status", "interfaceStatuses")
					statuses[0].(map[string]any)["conditions"] = []any{conditionMap("Ready", "False", "Pending", 1)}
					_ = unstructured.SetNestedSlice(target.Object, statuses, "status", "interfaceStatuses")
					return
				}
				conditions, _, _ := unstructured.NestedSlice(target.Object, "status", "conditions")
				for i, raw := range conditions {
					condition := raw.(map[string]any)
					if condition["type"] == "Ready" || condition["type"] == "ApplicationsReady" {
						conditions[i] = conditionMap(condition["type"].(string), "False", "Pending", 1)
					}
				}
				_ = unstructured.SetNestedSlice(target.Object, conditions, "status", "conditions")
			}
			checks, scope, err := validationRuntimeChecks(actual.native.desired.plan)
			if err != nil {
				t.Fatal(err)
			}
			boundary := &n3cRunnerBoundary{n3cRuntimeAdapter: actual, independent: []validation.Outcome{}}
			for _, area := range []validation.Area{validation.AreaManagement, validation.AreaOperator, validation.AreaDefinitions, validation.AreaSystem, validation.AreaTenant, validation.AreaSoftware, validation.AreaPhysical} {
				spec := validation.CheckSpec{ID: validation.CheckID("fixture/" + string(area)), Area: area, Subject: "Independent " + string(area), Required: true, Coverage: true, FullOnly: area == validation.AreaPhysical}
				checks = append(checks, spec)
				if !spec.FullOnly {
					boundary.independent = append(boundary.independent, validation.Outcome{ID: spec.ID, State: validation.Passed})
				}
			}
			plan, err := validation.NewPlan(validation.PlanInput{Mode: validation.ModeResources, Checks: checks, Runtime: scope})
			if err != nil {
				t.Fatal(err)
			}
			runner, err := validation.NewRunner(plan, boundary, validation.Options{Wait: 5 * time.Second, PollInterval: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			result := runner.Run(context.Background())
			if actual.calls != 2 || result.Verdict != validation.VerdictResourcesPassed {
				t.Fatalf("native wait calls=%d verdict=%s snapshots=%+v", actual.calls, result.Verdict, actual.snapshots)
			}
			firstPending := false
			for _, row := range actual.snapshots[0].Checks {
				if row.ID == test.row {
					firstPending = row.State == validation.Pending && row.Reason == validation.ReasonProgressing
				}
				if row.State != validation.Passed && row.State != validation.Pending {
					t.Fatalf("known progress left blocker %+v", row)
				}
			}
			if !firstPending {
				t.Fatalf("selected native row did not preserve Pending: %+v", actual.snapshots[0])
			}
			for _, row := range actual.snapshots[1].Checks {
				if row.State != validation.Passed {
					t.Fatalf("real second snapshot did not settle: %+v", row)
				}
			}
		})
	}
}

func TestValidationN3cNativeHBNRejectsActualPodOnlyFunctionDrift(t *testing.T) {
	actual, _ := n3cRuntimeAdapterFixture(t)
	actual.pods[0].Spec.Containers[0].Command = []string{"sleep", "infinity"}
	snapshot, err := actual.ObserveResources(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []validation.CheckID{"hbn/coverage", "hbn/SERIAL-A"} {
		failed := false
		for _, row := range snapshot.Checks {
			if row.ID == id {
				failed = row.State == validation.Failed
			}
		}
		if !failed {
			t.Fatalf("actual HBN Pod command drift accepted: %+v", snapshot)
		}
	}
}

func TestValidationN3cNativeHBNMalformedAndUnknownRemainTerminal(t *testing.T) {
	prepared := prepareValidationFixture(t, []string{"SERIAL-A"})
	for _, test := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
	}{
		{name: "future status", mutate: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, int64(2), "status", "observedGeneration")
		}},
		{name: "malformed status", mutate: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "1", "status", "observedGeneration")
		}},
		{name: "stale Unknown", mutate: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, int64(0), "status", "observedGeneration")
			_ = unstructured.SetNestedSlice(o.Object, []any{conditionMap("Ready", "Unknown", "Pending", 0), conditionMap("ApplicationsReconciled", "True", "Success", 1), conditionMap("ApplicationsReady", "True", "Success", 1)}, "status", "conditions")
		}},
		{name: "negative condition generation", mutate: func(o *unstructured.Unstructured) {
			conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
			conditions[0].(map[string]any)["observedGeneration"] = int64(-1)
			_ = unstructured.SetNestedSlice(o.Object, conditions, "status", "conditions")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual, _ := n3cRuntimeAdapterFixtureFromPreparation(t, prepared)
			for _, object := range actual.objects {
				if object.GetKind() == "DPUService" {
					test.mutate(object)
				}
			}
			snapshot, err := actual.ObserveResources(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range snapshot.Checks {
				if (row.ID == "hbn/coverage" || row.ID == "hbn/SERIAL-A") && row.State != validation.Failed {
					t.Fatalf("unsupported progress became retryable/unavailable: %+v", row)
				}
			}
		})
	}
}

func TestValidationN3cNSIEntryMalformedStatusCannotHideBehindPending(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
	}{
		{name: "malformed collection", mutate: func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedMap(o.Object, map[string]any{}, "status", "interfaceStatuses")
		}},
		{name: "duplicate Pending selected entry", mutate: func(o *unstructured.Unstructured) {
			statuses, _, _ := unstructured.NestedSlice(o.Object, "status", "interfaceStatuses")
			statuses[0].(map[string]any)["conditions"] = []any{conditionMap("Ready", "False", "Pending", 4)}
			statuses = append(statuses, statuses[0])
			_ = unstructured.SetNestedSlice(o.Object, statuses, "status", "interfaceStatuses")
		}},
		{name: "malformed unselected entry", mutate: func(o *unstructured.Unstructured) {
			statuses, _, _ := unstructured.NestedSlice(o.Object, "status", "interfaceStatuses")
			statuses = append(statuses, map[string]any{"name": int64(1)})
			_ = unstructured.SetNestedSlice(o.Object, statuses, "status", "interfaceStatuses")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			set, shard := selectedNSIFixture()
			test.mutate(shard)
			r := newValidationRecorder([]validation.CheckSpec{{ID: "selected", Area: validation.AreaWorkloads, Subject: "selected", Required: true}}, nil)
			r.recordError("selected", verifySelectedNSIEntry(testRules(t), set, shard))
			if row := r.snapshot().Checks[0]; row.State != validation.Failed {
				t.Fatalf("malformed NSI status became retryable/unavailable: %+v", row)
			}
		})
	}
}

// runtimeValidationChildren builds the current tenant descendants for an already
// activated retained workload. It owns no observer, transport or lifecycle.
func runtimeValidationChildren(t *testing.T, f *runtimeStageFixture, desired *managementDesired, objects map[string]*unstructured.Unstructured) map[string]*unstructured.Unstructured {
	t.Helper()
	tenant := testTenant(f.controlPlane.tenant.Client)
	children := map[string]*unstructured.Unstructured{}
	var chain *unstructured.Unstructured
	for _, obj := range objects {
		if obj.GetKind() == "DPUServiceChain" {
			chain = obj
		}
	}
	if chain == nil {
		t.Fatal("fixture lacks current chain")
	}
	chainSpec, _, _ := unstructured.NestedMap(chain.Object, "spec", "template", "spec")
	chainSet := workloadChildObject("svc.dpu.nvidia.com/v1alpha1", "ServiceChainSet", chain.GetName(), chain.GetNamespace(), map[string]any{"spec": chainSpec}, nil, nil)
	n3cReadyWorkload(chainSet)
	children["ServiceChainSet/"+chainSet.GetName()] = chainSet
	switches, _, _ := unstructured.NestedSlice(chainSet.Object, "spec", "template", "spec", "switches")
	controller := true
	nodeName := f.snapshot.TenantNodes[0].GetName()
	child := workloadChildObject("svc.dpu.nvidia.com/v1alpha1", "ServiceChain", "selected-chain", chainSet.GetNamespace(), map[string]any{"spec": map[string]any{"node": nodeName, "switches": switches}}, map[string]string{"svc.dpu.nvidia.com/servicechainset-name": chainSet.GetName(), "svc.dpu.nvidia.com/servicechainset-namespace": chainSet.GetNamespace()}, nil)
	child.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: chainSet.GetAPIVersion(), Kind: chainSet.GetKind(), Name: chainSet.GetName(), UID: chainSet.GetUID(), Controller: &controller}})
	n3cReadyWorkload(child)
	children["ServiceChain/"+child.GetName()] = child
	shard := workloadChildObject("svc.dpu.nvidia.com/v1alpha1", "NodeServiceInterfaces", "selected-node-sfc", desired.operatorNamespace, map[string]any{"spec": map[string]any{"node": nodeName, "type": "sfc", "interfaces": []any{}}}, map[string]string{"svc.dpu.nvidia.com/node": nodeName, "svc.dpu.nvidia.com/nsi-type": "sfc"}, nil)
	n3cReadyWorkload(shard)
	entries, statuses := []any{}, []any{}
	for _, parent := range objects {
		if parent.GetKind() == "DPUServiceIPAM" {
			pool, poolErr := f.client.workloadChild(f.ctx, tenant, parent)
			if poolErr != nil {
				t.Fatal(poolErr)
			}
			if parent.GetName() == f.snapshot.DPUServiceIPAM.GetName() {
				pool = f.snapshot.CIDRPool.DeepCopy()
			}
			children["CIDRPool/"+pool.GetName()] = pool
		}
		if parent.GetKind() != "DPUServiceInterface" {
			continue
		}
		set, setErr := f.client.workloadChild(f.ctx, tenant, parent)
		if setErr != nil {
			t.Fatal(setErr)
		}
		n3cReadyWorkload(set)
		children["ServiceInterfaceSet/"+set.GetName()] = set
		entry, _, _ := unstructured.NestedMap(set.Object, "spec", "template", "spec")
		name := set.GetNamespace() + "_" + set.GetName()
		entry["name"] = name
		entryLabels, _, _ := unstructured.NestedStringMap(set.Object, "spec", "template", "metadata", "labels")
		if entryLabels == nil {
			entryLabels = map[string]string{}
		}
		entryLabels["svc.dpu.nvidia.com/serviceinterfaceset-name"] = set.GetName()
		entryLabels["svc.dpu.nvidia.com/serviceinterfaceset-namespace"] = set.GetNamespace()
		if entry["interfaceType"] == "service" {
			value, _, _ := unstructured.NestedString(entry, "service", "serviceID")
			entryLabels["svc.dpu.nvidia.com/service-id"] = value
		}
		rawLabels := map[string]any{}
		for k, v := range entryLabels {
			rawLabels[k] = v
		}
		entry["labels"] = rawLabels
		annotations, found, _ := unstructured.NestedMap(set.Object, "spec", "template", "metadata", "annotations")
		if found {
			entry["annotations"] = annotations
		}
		entries = append(entries, entry)
		statuses = append(statuses, map[string]any{"name": name, "conditions": []any{conditionMap("Ready", "True", "Success", 1)}})
	}
	_ = unstructured.SetNestedSlice(shard.Object, entries, "spec", "interfaces")
	_ = unstructured.SetNestedSlice(shard.Object, statuses, "status", "interfaceStatuses")
	children["NodeServiceInterfaces/"+shard.GetName()] = shard
	for i := 1; i < len(f.snapshot.TenantNodes); i++ {
		node := f.snapshot.TenantNodes[i].GetName()
		chainChild := child.DeepCopy()
		chainChild.SetName(fmt.Sprintf("selected-chain-%d", i))
		chainChild.SetUID(types.UID(chainChild.GetName()))
		_ = unstructured.SetNestedField(chainChild.Object, node, "spec", "node")
		children["ServiceChain/"+chainChild.GetName()] = chainChild
		nsi := shard.DeepCopy()
		nsi.SetName(fmt.Sprintf("selected-node-sfc-%d", i))
		nsi.SetUID(types.UID(nsi.GetName()))
		nsi.SetLabels(map[string]string{"svc.dpu.nvidia.com/node": node, "svc.dpu.nvidia.com/nsi-type": "sfc"})
		_ = unstructured.SetNestedField(nsi.Object, node, "spec", "node")
		children["NodeServiceInterfaces/"+nsi.GetName()] = nsi
	}
	return children
}
