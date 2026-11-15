// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

type hostLeaseObservationHarness struct {
	input           hostLeaseResolverFixture
	workload        workloadPreparationReceipt
	management      *APIClient
	apiFixture      *apiFixture
	tenant          *tenantClient
	managementState *hostLeaseManagementState
	tenantState     *hostLeaseTenantState
	tenantREST      *rest.Config
}

type hostLeaseManagementState struct {
	mu           sync.Mutex
	ipam         *unstructured.Unstructured
	pool         *unstructured.Unstructured
	sources      tenantSources
	ipamReads    int
	poolReads    int
	ipamMutation string
	poolMutation string
}

type hostLeaseTenantState struct {
	mu              sync.Mutex
	nodes           []corev1.Node
	listMode        string
	listCalls       int
	requestCalls    int
	namespaceUID    string
	pageRV          string
	secondPageRV    string
	managementState *hostLeaseManagementState
	onNodeList      func()
	cancelAfterPage context.CancelFunc
}

func newHostLeaseObservationHarness(t *testing.T) *hostLeaseObservationHarness {
	t.Helper()
	state := &hostLeaseManagementState{}
	apiFixture := newAPIFixture(t, state.serveManagement)
	generationInputs := preflightGenerationInputs(t)
	generationInputs.Management = apiFixture.expected
	generationInputs.Operator.KubernetesVersion = "v1.35.6"
	generated, err := generateFixtureBundle(generationInputs)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := generation.LoadPlan(generated)
	if err != nil {
		t.Fatal(err)
	}
	management, err := NewAPIClient(context.Background(), apiFixture.request, apiFixture.expected, plan.Intent().KubernetesVersion)
	if err != nil {
		t.Fatal(err)
	}
	fastREST := rest.CopyConfig(management.access.REST)
	fastREST.RateLimiter = flowcontrol.NewFakeAlwaysRateLimiter()
	management.access.Client, err = kubernetes.NewForConfig(fastREST)
	if err != nil {
		t.Fatal(err)
	}
	management.dynamic, err = dynamic.NewForConfig(fastREST)
	if err != nil {
		t.Fatal(err)
	}
	operatorInputs, archive := plan.Operator()
	operator, err := prepareOperator(OperatorRequest{Archive: archive, Inputs: operatorInputs, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := NewReconciler(operator.renderedManifest, plan.Digest())
	if err != nil {
		t.Fatal(err)
	}
	expected, _, err := tenantPlanInputs(plan)
	if err != nil {
		t.Fatal(err)
	}
	_, sources := tenantSourceFixture(t)
	bindTenantSources(t, expected, &sources)
	const tenantUID = types.UID("dc-uid")
	sources.observed.DPUCluster.SetUID(tenantUID)
	digest, _, _, err := validateTenantSources(expected, string(tenantUID), sources)
	if err != nil {
		t.Fatalf("tenant fixture binding failed: %v; readiness=%+v", err, EvaluateTenantReadiness(expected, sources.observed))
	}
	state.sources = sources

	desiredIPAM, err := retainedWorkloadIPAM(plan)
	if err != nil {
		t.Fatal(err)
	}
	currentIPAM, err := reconciler.markedDesired(desiredIPAM)
	if err != nil {
		t.Fatal(err)
	}
	currentIPAM.SetUID(types.UID("ipam-uid"))
	currentIPAM.SetResourceVersion("ipam-rv-1")
	currentIPAM.SetGeneration(1)
	currentIPAM.Object["status"] = map[string]any{
		"observedGeneration":    int64(1),
		"conditions":            []any{conditionMap("DPUIPAMObjectReconciled", "True", "Success", 1)},
		"dpuClusterAllocations": []any{map[string]any{"dpuCluster": expected.Namespace + "/" + expected.Name, "ipRanges": []any{}}},
	}

	deployment, err := retainedDeploymentForPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	intentDevices := plan.Intent().Devices
	selectedBMCs := make([]enrollmentBMC, len(intentDevices))
	for i := range intentDevices {
		selectedBMCs[i] = enrollmentBMC{device: intentDevices[i]}
	}
	setIntents, err := activationSetIntents(deployment, selectedBMCs)
	if err != nil {
		t.Fatal(err)
	}
	selected := make([]activationSelectedDevice, 0, len(intentDevices))
	activation := activationObservation{sets: map[string]*unstructured.Unstructured{}, dpus: map[string]*unstructured.Unstructured{}, nodes: map[string]*unstructured.Unstructured{}, complete: true}
	for _, item := range selectedBMCs {
		serial := strings.ToLower(item.device.Serial)
		nodeName, deviceName := "dpu-node-"+serial, serial
		set := setIntents[serial].DeepCopy()
		set.SetUID(types.UID("set-uid-" + serial))
		set.SetResourceVersion("set-rv-1")
		set.SetGeneration(1)
		_ = unstructured.SetNestedMap(set.Object, map[string]any{"hold": true}, "spec", "dpuTemplate", "spec", "nodeEffect")
		dpuName := generatedDPUEName(nodeName, deviceName)
		selectedItem := activationSelectedDevice{item: item, receipt: enrolledDeviceReceipt{Serial: item.device.Serial, DeviceUID: types.UID("device-uid-" + serial), DPUNodeUID: types.UID("node-uid-" + serial)},
			nodeName: nodeName, dpuName: dpuName, dpuSet: set}
		selected = append(selected, selectedItem)
		activation.sets[set.GetName()] = set.DeepCopy()
		dpu := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPU",
			"metadata": map[string]any{
				"name": dpuName, "namespace": set.GetNamespace(), "uid": "dpu-uid-" + serial, "resourceVersion": "dpu-rv-1",
				"labels":          map[string]any{activationDPUSetNameLabel: set.GetName(), activationDPUSetNSLabel: set.GetNamespace(), activationDPUDeviceLabel: deviceName},
				"ownerReferences": []any{map[string]any{"apiVersion": set.GetAPIVersion(), "kind": set.GetKind(), "name": set.GetName(), "uid": string(set.GetUID())}},
			},
			"spec": map[string]any{"dpuNodeName": nodeName, "dpuDeviceName": deviceName, "serialNumber": item.device.Serial,
				"cluster": map[string]any{"name": expected.Name, "namespace": expected.Namespace}},
		}}
		activation.dpus[dpuName] = dpu
		dpuNode := enrollmentNodeWithDevice(nodeName, set.GetNamespace(), deviceName)
		dpuNode.SetUID(selectedItem.receipt.DPUNodeUID)
		dpuNode.SetResourceVersion("dpu-node-rv-1")
		_ = unstructured.SetNestedSlice(dpuNode.Object, []any{conditionMap("Ready", "True", "Success", 1)}, "status", "conditions")
		activation.nodes[nodeName] = dpuNode
	}

	poolLabels := map[string]string{}
	if labels, found, labelErr := unstructured.NestedStringMap(desiredIPAM.Object, "spec", "labels"); labelErr != nil {
		t.Fatal(labelErr)
	} else if found {
		for key, value := range labels {
			poolLabels[key] = value
		}
	}
	poolLabels["dpu.nvidia.com/dpuserviceipam-name"] = desiredIPAM.GetName()
	poolLabels["dpu.nvidia.com/dpuserviceipam-namespace"] = desiredIPAM.GetNamespace()
	network, found, err := unstructured.NestedMap(desiredIPAM.Object, "spec", "ipv4Network")
	if err != nil || !found {
		t.Fatalf("retained IPAM network is missing: found=%t err=%v", found, err)
	}
	poolSpec := map[string]any{"cidr": network["network"], "perNodeNetworkPrefix": network["prefixSize"]}
	for _, key := range []string{"gatewayIndex", "excludeRanges", "defaultGateway", "routes"} {
		if value, present := network[key]; present {
			poolKey := key
			if key == "excludeRanges" {
				poolKey = "exclusions"
			}
			poolSpec[poolKey] = runtime.DeepCopyJSONValue(value)
		}
	}
	if selector, present, selectorErr := unstructured.NestedMap(desiredIPAM.Object, "spec", "nodeSelector"); selectorErr != nil {
		t.Fatal(selectorErr)
	} else if present {
		poolSpec["nodeSelector"] = runtime.DeepCopyJSONValue(selector)
	}
	pool := workloadChildObject("nv-ipam.nvidia.com/v1alpha1", "CIDRPool", desiredIPAM.GetName(), desiredIPAM.GetNamespace(), map[string]any{"spec": poolSpec}, poolLabels, nil)
	pool.SetUID(types.UID("pool-uid"))
	pool.SetResourceVersion("pool-rv-1")
	pool.SetGeneration(1)
	_ = unstructured.SetNestedSlice(pool.Object, hostLeaseAllocationRows(t, plan, selected), "status", "allocations")
	state.ipam, state.pool = currentIPAM.DeepCopy(), pool.DeepCopy()

	nodes := make([]corev1.Node, 0, len(selected)+1)
	for i, item := range selected {
		name := "tenant-node-" + serialSuffix(i)
		nodes = append(nodes, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("tenant-node-uid-" + serialSuffix(i)), ResourceVersion: "tenant-node-rv-1",
			Labels: map[string]string{hostLeaseDPUNodeNameLabel: item.nodeName, hostLeaseDPUNodeNamespaceLabel: selected[i].dpuSet.GetNamespace()}},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}})
	}
	nodes = append(nodes, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "unselected-tenant-node", UID: "unselected-node-uid", ResourceVersion: "unselected-node-rv", Labels: map[string]string{"unselected.example/tenant-node": "true"}}})
	tenantState := &hostLeaseTenantState{nodes: nodes, namespaceUID: "tenant-system", pageRV: "tenant-nodes-rv", secondPageRV: "tenant-nodes-rv", managementState: state}
	tenantServer := httptest.NewServer(http.HandlerFunc(tenantState.serveTenant))
	t.Cleanup(tenantServer.Close)
	tenantConfig := &rest.Config{Host: tenantServer.URL, Timeout: 30 * time.Second, RateLimiter: flowcontrol.NewFakeAlwaysRateLimiter()}
	tenantAPI, err := kubernetes.NewForConfig(tenantConfig)
	if err != nil {
		t.Fatal(err)
	}
	tenant := &tenantClient{source: management, expected: expected, dpuClusterUID: string(tenantUID), sourceDigest: digest, systemUID: tenantState.namespaceUID, api: tenantAPI}
	tenantState.onNodeList = func() { apiFixture.uid.Store("replacement-management") }
	workload := workloadPreparationReceipt{BundleDigest: plan.Digest(), ControlPlane: controlPlaneReceipts{
		management: managementBootstrapReceipt{BundleDigest: plan.Digest()},
		tenant:     tenantStageReceipt{ClusterUID: tenantUID, Client: tenant},
	}, Resources: []workloadResourceReceipt{{Kind: "DPUServiceIPAM", Namespace: desiredIPAM.GetNamespace(), Name: desiredIPAM.GetName(),
		UID: currentIPAM.GetUID(), Generation: currentIPAM.GetGeneration(), ChildUID: pool.GetUID()}}}
	input := hostLeaseResolverFixture{plan: plan, reconciler: reconciler, selected: selected, activation: activation, workload: workload,
		snapshot: hostWorkloadLeaseSnapshot{DPUServiceIPAM: currentIPAM.DeepCopy(), CIDRPool: pool.DeepCopy()}}
	return &hostLeaseObservationHarness{input: input, workload: workload, management: management, apiFixture: apiFixture, tenant: tenant, tenantState: tenantState, managementState: state, tenantREST: rest.CopyConfig(tenantConfig)}
}

func retainedDeploymentForPlan(plan *generation.Plan) (*unstructured.Unstructured, error) {
	for _, stage := range plan.Stages() {
		if stage.Stage().ID != generation.StageDeployment {
			continue
		}
		documents := stage.Documents()
		if len(documents) == 1 {
			return documents[0].ObjectCopy(), nil
		}
	}
	return nil, fmt.Errorf("fixture retained StageDeployment is missing")
}

func serialSuffix(index int) string {
	return string(rune('a' + index))
}

func (h *hostLeaseObservationHarness) useDefaultClientRateLimits(t *testing.T) {
	t.Helper()
	managementREST := rest.CopyConfig(h.management.access.REST)
	managementREST.Timeout = 30 * time.Second
	managementREST.QPS, managementREST.Burst, managementREST.RateLimiter = 0, 0, nil
	var err error
	h.management.access.Client, err = kubernetes.NewForConfig(managementREST)
	if err != nil {
		t.Fatal(err)
	}
	h.management.dynamic, err = dynamic.NewForConfig(managementREST)
	if err != nil {
		t.Fatal(err)
	}
	tenantREST := rest.CopyConfig(h.tenantREST)
	tenantREST.Timeout = 30 * time.Second
	tenantREST.QPS, tenantREST.Burst, tenantREST.RateLimiter = 0, 0, nil
	h.tenant.api, err = kubernetes.NewForConfig(tenantREST)
	if err != nil {
		t.Fatal(err)
	}
}

func bindTenantSources(t *testing.T, expected TenantReadinessExpected, sources *tenantSources) {
	t.Helper()
	dc := sources.observed.DPUCluster
	dc.SetName(expected.Name)
	dc.SetNamespace(expected.Namespace)
	dc.SetLabels(expected.Labels)
	_ = unstructured.SetNestedField(dc.Object, expected.MaxNodes, "spec", "maxNodes")
	_ = unstructured.SetNestedField(dc.Object, expected.Name+"-admin-kubeconfig", "spec", "kubeconfig")
	_ = unstructured.SetNestedField(dc.Object, expected.VIP, "spec", "clusterEndpoint", "keepalived", "vip")
	_ = unstructured.SetNestedField(dc.Object, expected.Interface, "spec", "clusterEndpoint", "keepalived", "interface")
	_ = unstructured.SetNestedField(dc.Object, expected.VirtualRouterID, "spec", "clusterEndpoint", "keepalived", "virtualRouterID")
	selector := map[string]any{}
	for key, value := range expected.NodeSelector {
		selector[key] = value
	}
	_ = unstructured.SetNestedMap(dc.Object, selector, "spec", "clusterEndpoint", "keepalived", "nodeSelector")
	_ = unstructured.SetNestedField(dc.Object, "v"+strings.TrimPrefix(expected.KubernetesVersion, "v"), "status", "version")

	tcp := sources.observed.TenantControlPlane
	tcp.SetName(expected.Name)
	tcp.SetNamespace(expected.Namespace)
	tcp.SetLabels(map[string]string{"dpu.nvidia.com/cluster": expected.Name, "tenant.clastix.io": expected.Name})
	_ = unstructured.SetNestedField(tcp.Object, expected.KubernetesVersion, "spec", "kubernetes", "version")
	_ = unstructured.SetNestedField(tcp.Object, expected.VIP, "spec", "networkProfile", "address")
	_ = unstructured.SetNestedSlice(tcp.Object, []any{expected.PodCIDR}, "spec", "networkProfile", "podCidrs")
	_ = unstructured.SetNestedSlice(tcp.Object, []any{expected.ServiceCIDR}, "spec", "networkProfile", "serviceCidrs")
	_ = unstructured.SetNestedField(tcp.Object, expected.KubernetesVersion, "status", "kubernetesResources", "version", "version")
	setOwnerReference(t, tcp, expected.Name, dc.GetUID())

	svc := sources.observed.Service
	svc.SetName(expected.Name)
	svc.SetNamespace(expected.Namespace)
	_ = unstructured.SetNestedField(svc.Object, expected.Name, "spec", "selector", "kamaji.clastix.io/name")
	for _, object := range sources.observed.Keepalived {
		object.SetName(expected.Name + "-keepalived")
		object.SetNamespace(expected.Namespace)
		setOwnerReference(t, object, expected.Name, dc.GetUID())
		if object.GetKind() == "DaemonSet" {
			_ = unstructured.SetNestedStringMap(object.Object, map[string]string{"app.kubernetes.io/name": expected.Name + "-keepalived"}, "spec", "selector", "matchLabels")
			_ = unstructured.SetNestedStringMap(object.Object, expected.NodeSelector, "spec", "template", "spec", "nodeSelector")
			labels := object.GetLabels()
			if labels == nil {
				labels = make(map[string]string)
			}
			labels["app.kubernetes.io/name"] = expected.Name + "-keepalived"
			object.SetLabels(labels)
			_ = unstructured.SetNestedStringMap(object.Object, labels, "spec", "template", "metadata", "labels")
			volumes, _, _ := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "volumes")
			for _, raw := range volumes {
				volume, ok := raw.(map[string]any)
				if !ok {
					t.Fatal("keepalived fixture volume is malformed")
				}
				if volume["name"] == "conf-template" {
					_ = unstructured.SetNestedField(volume, expected.Name+"-keepalived", "configMap", "name")
				}
			}
			_ = unstructured.SetNestedSlice(object.Object, volumes, "spec", "template", "spec", "volumes")
			env, _, _ := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "initContainers")
			if len(env) > 0 {
				container, ok := env[0].(map[string]any)
				if !ok {
					t.Fatal("keepalived fixture init container is malformed")
				}
				variables, _, _ := unstructured.NestedSlice(container, "env")
				for _, raw := range variables {
					variable, ok := raw.(map[string]any)
					if !ok {
						t.Fatal("keepalived fixture endpoint variable is malformed")
					}
					switch variable["name"] {
					case "INTERFACE":
						variable["value"] = expected.Interface
					case "VIRTUAL_ROUTER_ID":
						variable["value"] = fmt.Sprint(expected.VirtualRouterID)
					case "VIRTUAL_IP":
						variable["value"] = expected.VIP
					}
				}
				_ = unstructured.SetNestedSlice(container, variables, "env")
				_ = unstructured.SetNestedSlice(object.Object, env, "spec", "template", "spec", "initContainers")
			}
		}
	}
	if sources.secret != nil {
		sources.secret.SetName(expected.Name + "-admin-kubeconfig")
		sources.secret.SetNamespace(expected.Namespace)
	}
}

func setOwnerReference(t *testing.T, object *unstructured.Unstructured, name string, uid types.UID) {
	t.Helper()
	owners, found, err := unstructured.NestedSlice(object.Object, "metadata", "ownerReferences")
	if err != nil || !found || len(owners) != 1 {
		t.Fatalf("fixture owner reference is malformed: found=%t err=%v", found, err)
	}
	owner, ok := owners[0].(map[string]any)
	if !ok {
		t.Fatal("fixture owner reference is not an object")
	}
	owner["name"], owner["uid"] = name, string(uid)
	if err := unstructured.SetNestedSlice(object.Object, owners, "metadata", "ownerReferences"); err != nil {
		t.Fatal(err)
	}
}

func (s *hostLeaseManagementState) serveManagement(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	for _, object := range s.sources.observed.Keepalived {
		if object == nil {
			continue
		}
		resource := map[string]string{"ConfigMap": "configmaps", "DaemonSet": "daemonsets"}[object.GetKind()]
		if resource != "" && strings.Contains(path, "/"+resource+"/") && strings.HasSuffix(path, "/"+object.GetName()) {
			writeObservationJSON(w, object)
			return
		}
	}
	switch {
	case path == "/prefix/apis/svc.dpu.nvidia.com/v1alpha1":
		writeObservationJSON(w, &metav1.APIResourceList{GroupVersion: "svc.dpu.nvidia.com/v1alpha1", APIResources: []metav1.APIResource{{Name: "dpuserviceipams", Kind: "DPUServiceIPAM", Namespaced: true}}})
	case strings.HasSuffix(path, "/dpuserviceipams/hbn-workload"):
		s.ipamReads++
		object := s.ipam.DeepCopy()
		if s.ipamMutation == "spec" && s.ipamReads > 1 {
			_ = unstructured.SetNestedField(object.Object, "192.0.2.0/24", "spec", "ipv4Network", "network")
		}
		writeObservationJSON(w, object)
	case strings.HasSuffix(path, "/dpuclusters/"+s.sources.observed.DPUCluster.GetName()):
		writeObservationJSON(w, s.sources.observed.DPUCluster)
	case strings.HasSuffix(path, "/tenantcontrolplanes/"+s.sources.observed.TenantControlPlane.GetName()):
		writeObservationJSON(w, s.sources.observed.TenantControlPlane)
	case strings.HasSuffix(path, "/services/"+s.sources.observed.Service.GetName()):
		writeObservationJSON(w, s.sources.observed.Service)
	case strings.HasSuffix(path, "/secrets/"+s.sources.secret.GetName()):
		writeObservationJSON(w, s.sources.secret)
	case strings.HasPrefix(path, "/prefix/apis/"):
		writeObservationJSON(w, &metav1.APIResourceList{GroupVersion: strings.TrimPrefix(path, "/prefix/apis/")})
	default:
		http.NotFound(w, r)
	}
}

func (s *hostLeaseTenantState) serveTenant(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestCalls++
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v1/namespaces/kube-system":
		writeObservationJSON(w, &corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(s.namespaceUID)}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}})
	case "/version":
		writeObservationJSON(w, map[string]string{"gitVersion": "v1.35.6"})
	case "/api/v1/nodes":
		s.listCalls++
		if s.listMode == "forbidden" {
			w.WriteHeader(http.StatusForbidden)
			writeObservationJSON(w, &metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden})
			return
		}
		writeNodeListPage(w, s, r)
		if s.listMode == "cancel-after-first" && s.listCalls == 1 && s.cancelAfterPage != nil {
			s.cancelAfterPage()
		}
		if s.listCalls == 2 {
			switch s.listMode {
			case "replace-management":
				s.onNodeList()
			case "replace-tenant":
				s.namespaceUID = "replacement-tenant"
			}
		}
	case "/apis/nv-ipam.nvidia.com/v1alpha1":
		writeObservationJSON(w, &metav1.APIResourceList{GroupVersion: "nv-ipam.nvidia.com/v1alpha1", APIResources: []metav1.APIResource{{Name: "cidrpools", Kind: "CIDRPool", Namespaced: true}}})
	default:
		if strings.Contains(r.URL.Path, "/cidrpools/") {
			s.managementState.mu.Lock()
			defer s.managementState.mu.Unlock()
			s.managementState.poolReads++
			object := s.managementState.pool.DeepCopy()
			if s.managementState.poolMutation == "replace" && s.managementState.poolReads > 1 {
				object.SetUID("replacement-pool")
				object.SetResourceVersion("13")
			}
			if s.managementState.poolMutation == "allocations" && s.managementState.poolReads > 1 {
				allocations, _, _ := unstructured.NestedSlice(object.Object, "status", "allocations")
				if len(allocations) > 0 {
					allocations[0].(map[string]any)["gateway"] = "192.0.2.1"
					_ = unstructured.SetNestedSlice(object.Object, allocations, "status", "allocations")
				}
			}
			writeObservationJSON(w, object)
			return
		}
		http.NotFound(w, r)
	}
}

func writeNodeListPage(w http.ResponseWriter, state *hostLeaseTenantState, request *http.Request) {
	mode := state.listMode
	cursor := request.URL.Query().Get("continue")
	page := &corev1.NodeList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"}, ListMeta: metav1.ListMeta{ResourceVersion: state.pageRV}}
	if mode == "missing-resource-version" {
		page.ResourceVersion = ""
	}
	if mode == "long-chain" {
		page.Continue = fmt.Sprintf("cursor-%d", state.listCalls)
		writeObservationJSON(w, page)
		return
	}
	if cursor == "" {
		if len(state.nodes) == 0 {
			writeObservationJSON(w, page)
			return
		}
		first := state.nodes[0]
		if mode == "wrong-gvk" {
			first.Kind = "Pod"
		}
		page.Items = append(page.Items, first)
		if len(state.nodes) > 1 || mode == "repeat-cursor" {
			page.Continue = "page-2"
		}
		writeObservationJSON(w, page)
		return
	}
	if mode == "repeat-cursor" {
		page.Continue = cursor
		writeObservationJSON(w, page)
		return
	}
	if mode == "resource-version-drift" {
		page.ResourceVersion = state.secondPageRV
	}
	if mode == "duplicate-node" || mode == "duplicate-uid" {
		duplicate := state.nodes[0].DeepCopy()
		if mode == "duplicate-node" && len(state.nodes) > 1 {
			duplicate.Name = state.nodes[1].Name
		}
		if mode == "duplicate-uid" && len(state.nodes) > 1 {
			duplicate.UID = state.nodes[1].UID
		}
		page.Items = append(page.Items, *duplicate)
	} else {
		page.Items = append(page.Items, state.nodes[1:]...)
	}
	writeObservationJSON(w, page)
}

func writeObservationJSON(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, "fixture encoding failed", http.StatusInternalServerError)
	}
}

func (h *hostLeaseObservationHarness) observe(t *testing.T) (hostWorkloadLeaseSnapshot, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return h.observeContext(ctx)
}

func (h *hostLeaseObservationHarness) observeContext(ctx context.Context) (hostWorkloadLeaseSnapshot, error) {
	return observeHostWorkloadLeaseSnapshot(ctx, h.management, h.tenant, h.input.plan, h.input.reconciler, h.workload, 15*time.Second)
}

func TestObserveHostWorkloadLeaseSnapshotReadsCompleteBoundSources(t *testing.T) {
	h := newHostLeaseObservationHarness(t)
	beforeIPAM := h.managementState.ipam.DeepCopy()
	beforePool := h.managementState.pool.DeepCopy()
	beforeExpected := h.tenant.expected
	snapshot, err := h.observe(t)
	if err != nil {
		t.Fatalf("observeHostWorkloadLeaseSnapshot(): %v", err)
	}
	if len(snapshot.TenantNodes) != len(h.tenantState.nodes) || h.tenantState.listCalls != 2 || h.managementState.poolReads != 2 || h.managementState.ipamReads != 2 {
		t.Fatalf("did not read the complete bounded source set: nodes=%d listCalls=%d poolReads=%d ipamReads=%d", len(snapshot.TenantNodes), h.tenantState.listCalls, h.managementState.poolReads, h.managementState.ipamReads)
	}
	for _, node := range snapshot.TenantNodes {
		if node.GetAPIVersion() != "v1" || node.GetKind() != "Node" || node.GetUID() == "" {
			t.Fatalf("typed API omitted a required Node identity: %#v", node.Object)
		}
	}
	leases, err := resolveHostWorkloadLeases(h.input.plan, h.input.reconciler, h.input.selected, h.input.activation, h.workload, snapshot)
	if err != nil || len(leases) != len(h.input.selected) {
		t.Fatalf("fresh snapshot did not feed the pure lease resolver: leases=%d err=%v", len(leases), err)
	}
	if !reflect.DeepEqual(beforeIPAM, h.managementState.ipam) || !reflect.DeepEqual(beforePool, h.managementState.pool) || !reflect.DeepEqual(beforeExpected, h.tenant.expected) {
		t.Fatal("observer mutated caller-owned API or tenant inputs")
	}
}

func TestObserveHostWorkloadLeaseSnapshotFitsDefaultClientRequestBudget(t *testing.T) {
	h := newHostLeaseObservationHarness(t)
	h.useDefaultClientRateLimits(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	managementRequestsBefore := h.apiFixture.calls.Load()
	tenantRequestsBefore := h.tenantState.requestCalls
	started := time.Now()
	snapshot, err := observeHostWorkloadLeaseSnapshot(ctx, h.management, h.tenant, h.input.plan, h.input.reconciler, h.workload, 30*time.Second)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("production-default client rate limits did not finish within the supported request budget (%s): %v", elapsed, err)
	}
	if elapsed >= 30*time.Second || len(snapshot.TenantNodes) != len(h.tenantState.nodes) {
		t.Fatalf("default-client request budget was not met: elapsed=%s nodes=%d want=%d", elapsed, len(snapshot.TenantNodes), len(h.tenantState.nodes))
	}
	if h.tenantState.listCalls != 2 || h.managementState.poolReads != 2 || h.managementState.ipamReads != 2 {
		t.Fatalf("default-client request coverage was incomplete: nodePages=%d poolReads=%d ipamReads=%d", h.tenantState.listCalls, h.managementState.poolReads, h.managementState.ipamReads)
	}
	t.Logf("default client-go limits: elapsed=%s managementRequests=%d tenantRequests=%d nodePages=%d poolReads=%d ipamReads=%d", elapsed,
		h.apiFixture.calls.Load()-managementRequestsBefore, h.tenantState.requestCalls-tenantRequestsBefore, h.tenantState.listCalls, h.managementState.poolReads, h.managementState.ipamReads)
}

func TestObserveHostWorkloadLeaseSnapshotRejectsIncompleteOrUnstableSources(t *testing.T) {
	tests := []struct {
		name               string
		configure          func(*hostLeaseObservationHarness)
		serviceUnavailable bool
		wantError          string
		wantListCalls      int
	}{
		{name: "forbidden Node list", configure: func(h *hostLeaseObservationHarness) { h.tenantState.listMode = "forbidden" }, wantError: "list tenant Nodes failed (Forbidden)"},
		{name: "repeated continuation", configure: func(h *hostLeaseObservationHarness) { h.tenantState.listMode = "repeat-cursor" }, wantError: "repeated a continuation token"},
		{name: "duplicate Node name", configure: func(h *hostLeaseObservationHarness) { h.tenantState.listMode = "duplicate-node" }, wantError: "incomplete or duplicate identity"},
		{name: "duplicate Node UID", configure: func(h *hostLeaseObservationHarness) { h.tenantState.listMode = "duplicate-uid" }, wantError: "incomplete or duplicate identity"},
		{name: "missing ListMeta resourceVersion", configure: func(h *hostLeaseObservationHarness) { h.tenantState.listMode = "missing-resource-version" }, wantError: "page has no resourceVersion"},
		{name: "resourceVersion changes between pages", configure: func(h *hostLeaseObservationHarness) {
			h.tenantState.listMode, h.tenantState.secondPageRV = "resource-version-drift", "other-rv"
		}, serviceUnavailable: true, wantError: "changed resourceVersion between pages"},
		{name: "tenant pool replacement", configure: func(h *hostLeaseObservationHarness) { h.managementState.poolMutation = "replace" }, serviceUnavailable: true, wantError: "tenant CIDRPool is missing, replaced, deleting or lacks fresh metadata"},
		{name: "tenant pool allocation drift", configure: func(h *hostLeaseObservationHarness) { h.managementState.poolMutation = "allocations" }, serviceUnavailable: true, wantError: "changed during the complete Node listing"},
		{name: "management IPAM spec drift", configure: func(h *hostLeaseObservationHarness) { h.managementState.ipamMutation = "spec" }, serviceUnavailable: true, wantError: "retained DPUServiceIPAM is missing, replaced, stale or changed"},
		{name: "finite page bound", configure: func(h *hostLeaseObservationHarness) { h.tenantState.listMode = "long-chain" }, wantError: "complete page bound", wantListCalls: hostLeaseNodeMaxPages},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHostLeaseObservationHarness(t)
			test.configure(h)
			_, err := h.observe(t)
			if err == nil {
				t.Fatal("observer accepted an incomplete or unstable source set")
			}
			if test.serviceUnavailable && !apierrors.IsServiceUnavailable(err) {
				t.Fatalf("source drift should remain retryable unavailable, got %T: %v", err, err)
			}
			if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("unexpected failure reason: want substring %q, got %v", test.wantError, err)
			}
			if test.wantListCalls != 0 && h.tenantState.listCalls != test.wantListCalls {
				t.Fatalf("page bound failure used %d calls, want exactly %d", h.tenantState.listCalls, test.wantListCalls)
			}
		})
	}
}

func TestObserveHostWorkloadLeaseSnapshotRejectsWrongBindingAndIdentityChanges(t *testing.T) {
	t.Run("wrong tenant namespace", func(t *testing.T) {
		h := newHostLeaseObservationHarness(t)
		h.tenant.expected.Namespace = "other-tenant"
		if _, err := h.observe(t); err == nil || h.tenantState.listCalls != 0 {
			t.Fatalf("mismatched tenant namespace reached the tenant API: err=%v listCalls=%d", err, h.tenantState.listCalls)
		}
	})
	t.Run("management identity changes during Node list", func(t *testing.T) {
		h := newHostLeaseObservationHarness(t)
		h.tenantState.listMode = "replace-management"
		// The management identity is replaced after the second Node page.
		if _, err := h.observe(t); err == nil || !strings.Contains(err.Error(), "read tenant CIDRPool failed") || h.tenantState.listCalls != 2 || h.managementState.poolReads != 1 {
			t.Fatalf("replacement management cluster did not produce the expected identity failure: %v", err)
		}
	})
	t.Run("tenant identity changes during Node list", func(t *testing.T) {
		h := newHostLeaseObservationHarness(t)
		h.tenantState.listMode = "replace-tenant"
		if _, err := h.observe(t); err == nil || !strings.Contains(err.Error(), "read tenant CIDRPool failed") || h.tenantState.listCalls != 2 || h.managementState.poolReads != 1 {
			t.Fatalf("replacement tenant cluster did not produce the expected identity failure: %v", err)
		}
	})
}

func TestObserveHostWorkloadLeaseSnapshotHonorsCancellationAndDeadline(t *testing.T) {
	t.Run("canceled after first Node page", func(t *testing.T) {
		h := newHostLeaseObservationHarness(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		h.tenantState.listMode = "cancel-after-first"
		h.tenantState.cancelAfterPage = cancel
		if _, err := h.observeContext(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation after the first page, got %v", err)
		}
		if h.tenantState.listCalls != 1 {
			t.Fatalf("canceled listing continued to %d pages", h.tenantState.listCalls)
		}
	})
	t.Run("already-expired execution deadline", func(t *testing.T) {
		h := newHostLeaseObservationHarness(t)
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		if _, err := h.observeContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected expired execution deadline, got %v", err)
		}
		if h.tenantState.listCalls != 0 {
			t.Fatalf("expired execution deadline reached Node list %d times", h.tenantState.listCalls)
		}
	})
}

func TestListCompleteTenantNodesRejectsMalformedGVK(t *testing.T) {
	h := newHostLeaseObservationHarness(t)
	h.tenantState.listMode = "wrong-gvk"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := listCompleteTenantNodes(ctx, h.tenant.api.CoreV1()); err == nil || !strings.Contains(err.Error(), "wrong API identity") {
		t.Fatalf("explicitly wrong tenant Node GVK did not produce the expected identity failure: %v", err)
	}
}
