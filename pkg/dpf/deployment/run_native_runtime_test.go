// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mitchellh/copystructure"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/flowcontrol"
)

// This fixture serves one fixed completed operation. Only physical collection,
// host inspection, command/traffic reads and metadata resolution are substituted.
type nativeRuntimeLimiter struct {
	flowcontrol.RateLimiter
	waits *atomic.Int64
}

func (l *nativeRuntimeLimiter) Wait(ctx context.Context) error {
	l.waits.Add(1)
	return l.RateLimiter.Wait(ctx)
}

type nativeRuntimeHarness struct {
	limiterWaits            atomic.Int64
	t                       *testing.T
	mu                      sync.Mutex
	api                     *apiFixture
	tenant                  *httptest.Server
	fixture                 *runtimeStageFixture
	operation               *corev1.ConfigMap
	access                  config.Request
	inventory               *inventory.Inventory
	managementGets          map[string]any
	managementLists         map[string]nativeRuntimeList
	tenantGets              map[string]any
	tenantLists             map[string]nativeRuntimeList
	discovery               map[string]*metav1.APIResourceList
	expectedPermissions     map[string]bool
	permissions             map[string]int
	calls                   map[string]int
	commandRows             []string
	trafficRows             []string
	inspections             []string
	imageReads              int
	collections             int
	configureCalls          int
	reboots                 int
	observations            int
	chartReads              int
	mutations               int
	driftAfterTraffic       bool
	badPhysicalAfterTraffic bool
	failTraffic             bool
	drifted                 bool
}

type nativeRuntimeList struct {
	apiVersion, kind string
	items            []unstructured.Unstructured
}

func nativePermissionKey(domain string, a *authorizationv1.ResourceAttributes) string {
	if a == nil {
		return domain + "/<nonresource>"
	}
	return strings.Join([]string{domain, a.Verb, a.Group, a.Version, a.Resource, a.Subresource, a.Namespace, a.Name}, "|")
}

func (h *nativeRuntimeHarness) allow(domain, verb, group, version, resource, subresource, namespace, name string) {
	a := &authorizationv1.ResourceAttributes{Verb: verb, Group: group, Version: version, Resource: resource, Subresource: subresource, Namespace: namespace, Name: name}
	h.expectedPermissions[nativePermissionKey(domain, a)] = true
}

func nativeObject(t *testing.T, value any) *unstructured.Unstructured {
	t.Helper()
	if u, ok := value.(*unstructured.Unstructured); ok {
		return u.DeepCopy()
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err = json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: object}
}

func nativeResourcePath(object *unstructured.Unstructured, plural string) string {
	prefix := "/apis/" + object.GetAPIVersion()
	if object.GetAPIVersion() == "v1" {
		prefix = "/api/v1"
	}
	if object.GetNamespace() != "" {
		prefix += "/namespaces/" + object.GetNamespace()
	}
	return prefix + "/" + plural + "/" + object.GetName()
}

func (h *nativeRuntimeHarness) seedManagement(object *unstructured.Unstructured) {
	plurals := map[string]string{"TenantControlPlane": "tenantcontrolplanes", "Namespace": "namespaces", "Secret": "secrets", "Deployment": "deployments", "DPFOperatorConfig": "dpfoperatorconfigs", "DPUCluster": "dpuclusters", "DPUDiscovery": "dpudiscoveries", "DPUDevice": "dpudevices", "DPUNode": "dpunodes", "DPU": "dpus", "DPUSet": "dpusets", "DPUDeployment": "dpudeployments", "DPUFlavor": "dpuflavors", "BlueFieldSoftware": "bluefieldsoftwares", "DPUServiceTemplate": "dpuservicetemplates", "DPUServiceConfiguration": "dpuserviceconfigurations", "DPUServiceIPAM": "dpuserviceipams", "DPUServiceInterface": "dpuserviceinterfaces", "DPUService": "dpuservices", "DPUServiceChain": "dpuservicechains", "CustomResourceDefinition": "customresourcedefinitions", "Service": "services", "ConfigMap": "configmaps", "DaemonSet": "daemonsets"}
	plural, ok := plurals[object.GetKind()]
	if !ok {
		h.t.Fatalf("unmapped seed kind %s", object.GetKind())
	}
	path := nativeResourcePath(object, plural)
	h.managementGets[path] = object.DeepCopy()

	scoped := strings.TrimSuffix(path, "/"+object.GetName())
	appendList := func(listPath string) {
		list := h.managementLists[listPath]
		list.apiVersion = object.GetAPIVersion()
		list.kind = object.GetKind()
		list.items = append(list.items, *object.DeepCopy())
		h.managementLists[listPath] = list
	}
	switch object.GetKind() {
	case "DPUSet", "DPU", "DPUService", "DPUServiceChain", "DPUServiceInterface":
		appendList(scoped)
	}
	switch object.GetKind() {
	case "DPUCluster", "DPUDiscovery", "DPUDevice", "DPUNode", "DPU", "DPUSet", "DPUDeployment":
		appendList(strings.Replace(scoped, "/namespaces/"+object.GetNamespace(), "", 1))
	}

}

func (h *nativeRuntimeHarness) serve(domain string, w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	path := r.URL.Path
	if domain == "management" {
		path = strings.TrimPrefix(path, "/prefix")
	}
	h.calls[domain+" "+r.Method+" "+path]++
	totalCalls := 0
	for _, n := range h.calls {
		totalCalls += n
	}
	if totalCalls%500 == 0 {
		h.t.Logf("native calls=%d apiCalls=%d physical=%d command=%d traffic=%d", totalCalls, h.api.calls.Load(), h.collections, len(h.commandRows), len(h.trafficRows))
	}
	w.Header().Set("Content-Type", "application/json")
	expectedToken := "Bearer private-test-token"
	if domain == "tenant" {
		expectedToken = "Bearer native-tenant-fixture-token"
	}
	if r.Header.Get("Authorization") != expectedToken {
		h.t.Errorf("wrong %s native auth binding", domain)
	}
	if r.Method == http.MethodPost && path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews" {
		var review authorizationv1.SelfSubjectAccessReview
		data, readErr := io.ReadAll(r.Body)
		if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(data, nil, &review); err != nil || readErr != nil {
			h.t.Errorf("decode %s SSAR: %v", domain, err)
		}
		key := nativePermissionKey(domain, review.Spec.ResourceAttributes)
		if !h.expectedPermissions[key] {
			h.t.Errorf("unexpected SSAR %s", key)
		}
		h.permissions[key]++
		_ = json.NewEncoder(w).Encode(&authorizationv1.SelfSubjectAccessReview{TypeMeta: metav1.TypeMeta{APIVersion: "authorization.k8s.io/v1", Kind: "SelfSubjectAccessReview"}, Status: authorizationv1.SubjectAccessReviewStatus{Allowed: h.expectedPermissions[key]}})
		return
	}
	if r.Method != http.MethodGet {
		h.mutations++
		h.t.Errorf("unexpected native mutation %s %s %s", domain, r.Method, path)
		http.Error(w, "unexpected mutation", http.StatusForbidden)
		return
	}
	if domain == "tenant" {
		switch path {
		case "/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"gitVersion": "v1.35.6"})
			return
		case "/api/v1/namespaces/kube-system":
			_ = json.NewEncoder(w).Encode(&corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "tenant-system", ResourceVersion: "1"}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}})
			return
		case "/apis/nv-ipam.nvidia.com/v1alpha1":
			_ = json.NewEncoder(w).Encode(&metav1.APIResourceList{GroupVersion: "nv-ipam.nvidia.com/v1alpha1", APIResources: []metav1.APIResource{{Kind: "CIDRPool", Name: "cidrpools", Namespaced: true}}})
			return
		case "/apis/svc.dpu.nvidia.com/v1alpha1":
			_ = json.NewEncoder(w).Encode(&metav1.APIResourceList{GroupVersion: "svc.dpu.nvidia.com/v1alpha1", APIResources: []metav1.APIResource{{Kind: "ServiceInterfaceSet", Name: "serviceinterfacesets", Namespaced: true}}})
			return
		}
	}
	gets, lists := h.managementGets, h.managementLists
	if domain == "tenant" {
		gets, lists = h.tenantGets, h.tenantLists
	}
	if domain == "management" {
		if description := h.discovery[path]; description != nil {
			_ = json.NewEncoder(w).Encode(description)
			return
		}
	}
	if value, ok := gets[path]; ok {
		_ = json.NewEncoder(w).Encode(value)
		return
	}
	if list, ok := lists[path]; ok {
		limit := r.URL.Query().Get("limit")
		if limit != "64" && limit != "256" {
			h.t.Errorf("unexpected list bound %s %s limit=%q", domain, path, limit)
		}
		if r.URL.Query().Get("continue") != "" {
			h.t.Errorf("unexpected continuation %s", path)
		}
		selectorText := r.URL.Query().Get("labelSelector")
		expectedSelector := ""
		if domain == "tenant" && (list.kind == "Pod" || list.kind == "DaemonSet") {
			id, _, _ := unstructured.NestedString(h.fixture.client.service.Object, "spec", "serviceID")
			expectedSelector = labels.Set{hbnServiceIDLabel: id}.AsSelector().String()
		}
		if domain == "management" && (list.kind == "DPUServiceInterface" || list.kind == "DPUService" && selectorText != "") {
			expectedSelector = labels.Set{activationParentLabel: h.fixture.prepared.execution.plan.Intent().Namespace + "_" + h.fixture.client.deployment.GetName(), hbnServiceReferenceLabel: hbnContainerName}.AsSelector().String()
		}
		if selectorText != expectedSelector || r.URL.Query().Get("fieldSelector") != "" {
			h.t.Errorf("wrong native selector %s %s got=%q expected=%q", domain, path, selectorText, expectedSelector)
		}
		selector, err := labels.Parse(selectorText)
		if err != nil {
			h.t.Errorf("invalid selector: %v", err)
		}
		result := &unstructured.UnstructuredList{Object: map[string]any{"apiVersion": list.apiVersion, "kind": list.kind + "List", "metadata": map[string]any{"resourceVersion": "10"}}, Items: []unstructured.Unstructured{}}
		for i := range list.items {
			if selector.Matches(labels.Set(list.items[i].GetLabels())) {
				result.Items = append(result.Items, *list.items[i].DeepCopy())
			}
		}
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	h.t.Errorf("unseeded native read %s %s %s", domain, r.Method, path)
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(&metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: 404})
}

func nativeTenantTLS(t *testing.T, handler http.Handler) (*httptest.Server, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "native tenant fixture"}, IPAddresses: []net.IP{net.ParseIP("192.0.2.20")}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(ca, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, ca
}

func newNativeRuntimeHarness(t *testing.T) *nativeRuntimeHarness {
	t.Helper()
	h := &nativeRuntimeHarness{t: t, managementGets: map[string]any{}, managementLists: map[string]nativeRuntimeList{}, tenantGets: map[string]any{}, tenantLists: map[string]nativeRuntimeList{}, discovery: map[string]*metav1.APIResourceList{}, expectedPermissions: map[string]bool{}, permissions: map[string]int{}, calls: map[string]int{}}
	h.api = newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) { h.serve("management", w, r) }, metav1.APIResource{Name: "secrets", Kind: "Secret", Namespaced: true})
	input := softwareStageGenerationInputs(t, []string{"SERIAL-A", "SERIAL-B"})
	input.Management = h.api.expected
	input.Operator.KubernetesVersion = "v1.35.6"
	retained, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := prepareStageFixtureExecution(retained)
	if err != nil {
		t.Fatal(err)
	}
	base := newActivationIntegrationFixtureFromExecution(t, execution, false)
	enrichHostNetworkFixtureIdentities(t, &base)
	// One source DPUCluster satisfies both retained ownership and controller readiness.
	expected, wantedCluster, err := tenantPlanInputs(execution.plan)
	if err != nil {
		t.Fatal(err)
	}
	sources := base.client.sources.sources
	sources.observed.DPUCluster = base.client.base.resources[tenantStageKey(wantedCluster)].DeepCopy()
	bindTenantSources(t, expected, &sources)
	// Tenant is dialed through the production TLS/auth construction using its saved VIP.
	tenantServer, ca := nativeTenantTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.serve("tenant", w, r) }))
	h.tenant = tenantServer
	tenantConfig := clientapi.Config{CurrentContext: "selected", Clusters: map[string]*clientapi.Cluster{"tenant": {Server: "https://192.0.2.20:30538", CertificateAuthorityData: ca}}, AuthInfos: map[string]*clientapi.AuthInfo{"reader": {Token: "native-tenant-fixture-token"}}, Contexts: map[string]*clientapi.Context{"selected": {Cluster: "tenant", AuthInfo: "reader"}}}
	kubeconfig, err := clientcmd.Write(tenantConfig)
	if err != nil {
		t.Fatal(err)
	}
	sources.secret = sources.secret.DeepCopy()
	sources.secret.Data = map[string][]byte{"super-admin.conf": kubeconfig}
	base.client.sources.sources = sources
	tenant := testTenant(base.controlPlane.tenant.Client)
	digest, _, _, err := validateTenantSources(expected, "dc-uid", sources)
	if err != nil {
		t.Fatal(err)
	}
	tenant.sourceDigest = digest
	client := recoveryActivationClient{base.client}
	tenant.source = client
	op, store := checkpointFixture(t, base)
	op.state.Binding.NamespaceUID = base.controlPlane.management.NamespaceUIDs[execution.plan.Intent().Namespace]
	op.state.TenantSources, err = base.controlPlane.tenant.Client.operationSources(base.ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, err := encodeOperation(op.state)
	if err != nil {
		t.Fatal(err)
	}
	op.object.Data["checkpoint.json"] = data
	store.object = op.object.DeepCopy()
	base.prepared.operation = op
	reboot := &recoveryRebooter{fixture: &base}
	activation, err := runActivationStage(base.ctx, client, base.prepared, base.controlPlane, base.workload, base.inventory, base.request, base.journal, time.Millisecond, time.Second, reboot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := hostNetworkFixtureLeaseSnapshot(t, base, activation)
	snapshot.CIDRPool.SetGeneration(1)
	f := runtimeStageFixtureFromActivated(t, base, activation, snapshot)
	h.fixture = f
	if len(op.state.Children) != 6 || len(op.state.Progress) != 2 || len(op.state.Resets) != 2 || len(op.state.Hosts) != 2 {
		t.Fatal("native fixture checkpoint lacks completed lifecycle")
	}
	for _, r := range op.state.Resets {
		if !r.Acknowledged || !r.ACKIntent || len(r.ArmProofs) == 0 {
			t.Fatal("incomplete reset history")
		}
	}
	for _, host := range op.state.Hosts {
		if host.Result == nil || !host.Result.LeaseReverified {
			t.Fatal("incomplete host result")
		}
	}
	h.operation = store.object.DeepCopy()
	namespace := execution.plan.Intent().Namespace
	// Seed native management discovery and one current named/list object per identity.
	for gv, resources := range base.client.operator.resources {
		h.discovery["/apis/"+gv] = &metav1.APIResourceList{GroupVersion: gv, APIResources: resources}
	}
	h.discovery["/apis/apps/v1"] = &metav1.APIResourceList{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{{Name: "deployments", Kind: "Deployment", Namespaced: true}, {Name: "daemonsets", Kind: "DaemonSet", Namespaced: true}}}
	for _, object := range base.client.operator.crds {
		h.seedManagement(object)
	}
	for _, object := range base.client.base.resources {
		switch object.GetKind() {
		case "DPU", "DPUSet", "DPUCluster", "DPUService", "DPUServiceChain":
			continue
		}
		h.seedManagement(object)
	}
	h.seedManagement(sources.observed.DPUCluster)
	h.seedManagement(sources.observed.TenantControlPlane)
	h.seedManagement(sources.observed.Service)
	for _, object := range sources.observed.Keepalived {
		h.seedManagement(object)
	}
	for _, objects := range [][]unstructured.Unstructured{base.client.sets, base.client.dpus} {
		for i := range objects {
			h.seedManagement(&objects[i])
		}
	}
	h.seedManagement(f.client.service)
	for _, object := range base.client.versions {
		if object.GetKind() == "DPUServiceChain" {
			h.seedManagement(&object)
		}
	}
	for i := range f.client.interfaces {
		h.seedManagement(&f.client.interfaces[i])
	}
	h.managementLists["/apis/provisioning.dpu.nvidia.com/v1alpha1/namespaces/"+namespace+"/dpunodemaintenances"] = nativeRuntimeList{apiVersion: "provisioning.dpu.nvidia.com/v1alpha1", kind: "DPUNodeMaintenance", items: []unstructured.Unstructured{}}
	h.managementGets["/api/v1/namespaces/"+namespace+"/configmaps/"+h.operation.Name] = h.operation.DeepCopy()
	h.managementGets[nativeResourcePath(f.client.app, "applications")] = f.client.app.DeepCopy()
	// Use the existing native registry data; only the retained release's manager image changes.
	registry := softwareRegistryPodFixture()
	manager := softwareManagerPodFixture()
	manager.Spec.Containers[0].Image = base.client.managerImage
	manager.Status.ContainerStatuses[0].Image = base.client.managerImage
	lease := softwareLeaseFixture()
	duration := int32(3600)
	lease.Spec.LeaseDurationSeconds = &duration
	for path, value := range map[string]any{
		"/api/v1/namespaces/" + namespace + "/pods/" + softwareRegistryPodName:                        registry,
		"/api/v1/namespaces/" + namespace + "/pods/manager-0":                                         manager,
		"/apis/coordination.k8s.io/v1/namespaces/" + namespace + "/leases/" + softwareLeaderLeaseName: lease,
		"/api/v1/namespaces/" + namespace + "/configmaps/" + softwareCABundleName:                     &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: softwareCABundleName, Namespace: namespace, UID: "ca-uid", ResourceVersion: "1"}, Data: map[string]string{softwareCABundleKey: string(softwareTestCAPEM(t))}},
		"/api/v1/namespaces/" + namespace + "/configmaps/" + softwareRegistryConfigName:               &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: softwareRegistryConfigName, Namespace: namespace, UID: "nginx-config-uid", ResourceVersion: "1"}, Data: map[string]string{softwareRegistryConfigKey: softwarePinnedNginxConfig}},
		"/api/v1/namespaces/" + namespace + "/persistentvolumeclaims/bfb-pvc":                         &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "bfb-pvc", Namespace: namespace, UID: "claim-uid", ResourceVersion: "1"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv-bfb"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}},
	} {
		h.managementGets[path] = value
	}
	h.managementGets["/api/v1/namespaces/"+expected.Namespace+"/secrets/"+sources.secret.Name] = sources.secret.DeepCopy()
	objects, err := retainedWorkloadObjects(execution.verificationInputs())
	if err != nil {
		t.Fatal(err)
	}
	for _, desired := range objects {
		if desired.GetKind() != "DPUServiceIPAM" && desired.GetKind() != "DPUServiceInterface" {
			continue
		}
		parent, err := base.client.Get(base.ctx, desired)
		if err != nil {
			t.Fatal(err)
		}
		child, err := base.client.workloadChild(base.ctx, tenant, parent)
		if err != nil {
			t.Fatal(err)
		}
		plural := "serviceinterfacesets"
		if child.GetKind() == "CIDRPool" {
			plural = "cidrpools"
			child.SetGeneration(1)
			if child.GetName() == "hbn-workload" {
				child = f.snapshot.CIDRPool.DeepCopy()
			} else {
				_ = unstructured.SetNestedSlice(child.Object, []any{map[string]any{"nodeName": "tenant-node-a", "prefix": "10.0.124.0/32"}, map[string]any{"nodeName": "tenant-node-b", "prefix": "10.0.124.1/32"}}, "status", "allocations")
			}
		}
		h.tenantGets[nativeResourcePath(child, plural)] = child
	}
	h.tenantLists["/api/v1/nodes"] = nativeRuntimeList{apiVersion: "v1", kind: "Node", items: deepCopyUnstructuredList(f.snapshot.TenantNodes)}
	ds := nativeObject(t, f.ds)
	ds.SetAPIVersion("apps/v1")
	ds.SetKind("DaemonSet")
	h.tenantLists["/apis/apps/v1/namespaces/"+namespace+"/daemonsets"] = nativeRuntimeList{apiVersion: "apps/v1", kind: "DaemonSet", items: []unstructured.Unstructured{*ds}}
	podList := nativeRuntimeList{apiVersion: "v1", kind: "Pod", items: []unstructured.Unstructured{}}
	for i := range f.pods {
		pod := nativeObject(t, &f.pods[i])
		pod.SetAPIVersion("v1")
		pod.SetKind("Pod")
		podList.items = append(podList.items, *pod)
		h.tenantGets[nativeResourcePath(pod, "pods")] = pod.DeepCopy()
	}
	h.tenantLists["/api/v1/namespaces/"+namespace+"/pods"] = podList
	// Full native prerequisite inventory uses observed facts and independent management roots.
	h.inventory = activatedCurrentInventory(t, base)
	h.inventory.Management = &inventory.Management{APIEndpoint: h.api.expected.APIEndpoint, SystemNamespaceUID: h.api.expected.SystemNamespaceUID, TrustDigest: h.api.expected.TrustDigest, KubernetesVersion: execution.plan.Intent().KubernetesVersion}
	for i := range h.inventory.Hosts {
		host := &h.inventory.Hosts[i]
		ip := fmt.Sprintf("192.0.2.%d", 10+i)
		host.Runtime.VirtualMachine = new(bool)
		host.Runtime.SSHServerAddress = ip
		host.Runtime.Network.Links = append(host.Runtime.Network.Links, inventory.NetworkLink{Name: "mgmt0", PCIAddress: "0000:35:00.0", PhysicalPFRoots: []string{"0000:35:00.0"}})
		host.Runtime.Interfaces = []inventory.Interface{{Name: "mgmt0", Index: 2, Addresses: []string{ip + "/24"}}}
		host.Runtime.Routes = []inventory.Route{{Destination: "0.0.0.0/0", InterfaceIndex: 2}, {Destination: "::/0", InterfaceIndex: 2}}
		host.Runtime.Observations = map[string]inventory.Observation{"interfaces": {State: "observed"}, "routes": {State: "observed"}, "host-management-roots": {State: "observed"}, "network-links": {State: "observed"}, "network-link-details": {State: "observed"}}
		for j := range host.Adapters[0].PCIFunctions {
			pf := &host.Adapters[0].PCIFunctions[j]
			group := 12 + i*2 + j
			pf.IOMMUGroup = &group
			for _, suffix := range []string{"driver", "netdevs", "iommu-group"} {
				observation := inventory.Observation{State: "observed"}
				if suffix == "iommu-group" {
					observation.Values = []string{fmt.Sprintf("group=%d", group)}
				}
				host.Runtime.Observations["host-pf:"+pf.Address+":"+suffix] = observation
			}
			addresses := []string{}
			if j == 0 {
				addresses = []string{op.state.Hosts[host.Adapters[0].SerialNumber].Request.HostAddressPrefix}
			}
			host.Runtime.Interfaces = append(host.Runtime.Interfaces, inventory.Interface{Name: pf.Netdevs[0], Index: 10 + j, MTU: 9000, Addresses: addresses})
		}

	}
	h.access = base.request
	h.access.Kubeconfig = h.api.request.Kubeconfig
	h.access.Context = h.api.request.Context
	h.access.RequestTimeout = 2 * time.Minute
	h.access.Input.Site.Inventory = h.inventory
	// Explicit finite permission contract. No persistent effect is authorized by fixture routing.
	for _, verb := range []string{"get", "create", "update"} {
		name := h.operation.Name
		if verb == "create" {
			name = ""
		}
		h.allow("management", verb, "", "v1", "configmaps", "", namespace, name)
	}
	for _, resource := range []string{"dpuservices", "dpuservicechains"} {
		h.allow("management", "list", "svc.dpu.nvidia.com", "v1alpha1", resource, "", namespace, "")
	}
	for _, verb := range []string{"get", "create"} {
		h.allow("management", verb, "svc.dpu.nvidia.com", "v1alpha1", "dpudeployments", "", namespace, "")
	}
	for _, resource := range []string{"dpunodes", "dpus"} {
		for _, verb := range []string{"get", "list"} {
			h.allow("management", verb, "provisioning.dpu.nvidia.com", "v1alpha1", resource, "", namespace, "")
		}
	}
	h.allow("management", "patch", "provisioning.dpu.nvidia.com", "v1alpha1", "dpunodes", "", namespace, "")
	h.allow("management", "list", "provisioning.dpu.nvidia.com", "v1alpha1", "dpusets", "", namespace, "")
	for _, verb := range []string{"get", "list", "patch"} {
		h.allow("management", verb, "provisioning.dpu.nvidia.com", "v1alpha1", "dpunodemaintenances", "", namespace, "")
	}
	for _, verb := range []string{"get", "create"} {
		h.allow("management", verb, "", "v1", "configmaps", "", namespace, "")
	}
	h.allow("management", "list", "svc.dpu.nvidia.com", "v1alpha1", "dpuserviceinterfaces", "", namespace, "")
	h.allow("management", "get", "argoproj.io", "v1alpha1", "applications", "", execution.operatorNamespace, f.client.app.GetName())
	h.allow("tenant", "list", "apps", "v1", "daemonsets", "", namespace, "")
	h.allow("tenant", "list", "", "v1", "pods", "", namespace, "")
	for _, pod := range f.pods {
		h.allow("tenant", "get", "", "v1", "pods", "", namespace, pod.Name)
		h.allow("tenant", "create", "", "v1", "pods", "exec", namespace, pod.Name)
	}
	return h
}

type nativeNoReboot struct{ h *nativeRuntimeHarness }

func (r nativeNoReboot) RebootExternal(context.Context, ActivationRequest) (ActivationObservation, error) {
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	r.h.reboots++
	return ActivationObservation{}, fmt.Errorf("unexpected reboot")
}
func (r nativeNoReboot) ObserveExternal(context.Context, ActivationRequest, string) (ActivationObservation, error) {
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	r.h.observations++
	return ActivationObservation{}, fmt.Errorf("unexpected reboot observation")
}

func (h *nativeRuntimeHarness) run() (RunResult, []byte, string, error) {
	h.t.Helper()
	dir := h.t.TempDir()
	request := RunRequest{Resume: &OperationRef{Namespace: h.operation.Namespace, Name: h.operation.Name, UID: h.operation.UID}, Bundle: h.fixture.prepared.execution.retained, Access: h.access, ReportPath: filepath.Join(dir, "report.jsonl"), ProbeJournalPath: filepath.Join(dir, "probes.json"), PollInterval: time.Millisecond, RequestTimeout: 2 * time.Minute, MaxSoftwareBytes: 1 << 20, Rebooter: nativeNoReboot{h: h}}
	request.VerifyChartSource = func(context.Context, *generation.Plan) (PreflightCheck, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.chartReads++
		return PreflightCheck{}, fmt.Errorf("unexpected chart-source reexecution")
	}
	request.VerifyPrerequisites = func(_ context.Context, _ config.Request, plan *generation.Plan, phase PrerequisitePhase) (PrerequisiteVerification, error) {
		h.t.Logf("native prerequisite phase=%s apiCalls=%d", phase, h.api.calls.Load())
		copied, err := copystructure.Copy(h.inventory)
		if err != nil {
			return PrerequisiteVerification{}, err
		}
		inv := copied.(*inventory.Inventory)
		// The existing provider boundary supplies an observed backend row for each
		// fixed host. Pass those observations as inputs; the real host check logic
		// still evaluates physical geometry, IOMMU and management separation.
		backends := map[string]inventory.Check{}
		for serial, host := range map[string]string{"SERIAL-A": "host-a.example.test", "SERIAL-B": "host-b.example.test"} {
			backends[serial] = inventory.Check{ID: "dpf.hbn.host-network-backend", Subject: serial + " @ " + host, Vantage: "executor", RequiredPhase: "deploy", State: "passed", ReasonCode: "HOST_NETWORK_BACKEND_SUPPORTED", Message: "Observed Netplan 1.1.2, MTU 9000 within 1280-9216, separate mgmt0; exact transaction is inspected by owned resume."}
		}
		checks := discovery.HostHBNChecks(inv, backends)

		checks = append(checks, inventory.Check{ID: "dpf.hbn.management.enrollments", Subject: "selected DPUs", Vantage: "executor", RequiredPhase: "deploy", State: "failed", ReasonCode: "DPU_ALREADY_ENROLLED", Message: "Current enrolled objects require owned resume proof."}, inventory.Check{ID: "dpf.hbn.management.tenant-network", Subject: "tenant " + plan.Intent().Bootstrap.Tenant.Name + " network selection", Vantage: "management-cluster", RequiredPhase: "deploy", State: "unknown", ReasonCode: "TENANT_EXISTING_NETWORK_UNVERIFIED", Message: "Current endpoint requires owned resume proof."})
		return PrerequisiteVerification{Inventory: inv, Checks: checks}, nil
	}
	transports := runTransports{
		requestLimiter: &nativeRuntimeLimiter{RateLimiter: flowcontrol.NewFakeAlwaysRateLimiter(), waits: &h.limiterWaits},
		resolveImage: func(_ context.Context, reference string, timeout time.Duration, _ http.RoundTripper) (hbnPodImageIdentity, error) {
			if reference != h.fixture.image.Reference || timeout <= 0 {
				return hbnPodImageIdentity{}, fmt.Errorf("changed retained image identity")
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			h.imageReads++
			return h.fixture.image, nil
		},
		tenantDial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != "192.0.2.20:30538" {
				return nil, fmt.Errorf("unexpected tenant dial")
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", h.tenant.Listener.Addr().String())
		},
		enrollmentCollector: enrollmentCollectorFunc(func(_ context.Context, target config.Target, _ time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.collections++
			if h.collections%2 == 1 {
				h.t.Logf("native physical cohort collection=%d apiCalls=%d commands=%d traffic=%d", h.collections, h.api.calls.Load(), len(h.commandRows), len(h.trafficRows))
			}
			var selected *enrollmentBMC
			for i := range h.fixture.prepared.selected {
				if h.fixture.prepared.selected[i].target.Endpoint == target.Endpoint {
					selected = &h.fixture.prepared.selected[i]
				}
			}
			if selected == nil {
				return inventory.Device{}, nil, fmt.Errorf("unselected physical endpoint")
			}
			if len(h.trafficRows) >= 2 && !h.drifted && (h.driftAfterTraffic || h.badPhysicalAfterTraffic) {
				h.drifted = true
				if h.driftAfterTraffic {
					key := "/api/v1/namespaces/" + h.fixture.prepared.execution.plan.Intent().Namespace + "/pods/hbn-pod-serial-b"
					pod := h.tenantGets[key].(*unstructured.Unstructured).DeepCopy()
					pod.SetUID("changed-after-physical-exit")
					h.tenantGets[key] = pod
					listPath := strings.TrimSuffix(key, "/hbn-pod-serial-b")
					list := h.tenantLists[listPath]
					for i := range list.items {
						if list.items[i].GetName() == pod.GetName() {
							list.items[i] = *pod.DeepCopy()
						}
					}
					h.tenantLists[listPath] = list
				}
			}
			observed := inventory.Device{Model: "B4240", ObservationState: "observed", ID: "physical-" + selected.device.Serial, SerialNumber: selected.device.Serial, PSID: selected.device.PSID, Observations: map[string]inventory.Observation{"bmc-administrator": {State: "observed", ReasonCode: "BMC_ADMIN_VERIFIED"}, "host-reset-support": {State: "observed", Tool: "redfish", ReasonCode: "RESET_SUPPORT_VERIFIED", Values: []string{selected.release, "bf4-reset-safety-v1", "B4240", "BF4-26.10-0001"}}, "psid": {State: "observed", ReasonCode: "PSID_OBSERVED", Values: []string{selected.device.PSID}}}}
			if h.badPhysicalAfterTraffic && h.drifted {
				observed.SerialNumber = "REPLACEMENT"
			}
			return observed, nil, nil
		}),
		configureHostNetwork: func(context.Context, config.Target, sshprobe.HostNetworkRequest, time.Duration) (sshprobe.HostNetworkReceipt, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.configureCalls++
			return sshprobe.HostNetworkReceipt{}, fmt.Errorf("unexpected host configure")
		},
		inspectHostNetwork: func(_ context.Context, target config.Target, request sshprobe.HostNetworkRequest, _ time.Duration) (sshprobe.HostNetworkReceipt, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			serial := request.Identity.AdapterSerial
			saved, known := h.fixture.prepared.operation.state.Hosts[serial]
			if !known || !sameOperationRequest(saved.Request, request) {
				return sshprobe.HostNetworkReceipt{}, fmt.Errorf("inspect changed original host transaction")
			}
			h.inspections = append(h.inspections, serial)
			var original *activationSelectedDevice
			for i := range h.fixture.activation.selected {
				if h.fixture.activation.selected[i].item.device.Serial == serial {
					original = &h.fixture.activation.selected[i]
				}
			}
			if original == nil || target.Endpoint != original.hostTarget.Endpoint || !reflect.DeepEqual(target.SSH, original.hostTarget.SSH) || target.InputRef != saved.Result.HostInputRef {
				h.t.Error("inspector changed host authority")
			}
			return sshprobe.HostNetworkReceipt{RequestID: saved.Result.Configuration.RequestID, State: "complete", RuntimeVerified: true, FileVerified: true, VerifiedRepeat: true}, nil
		},
		readRuntimeCommand: func(_ context.Context, namespace, pod string, command hbnRuntimeCommand, prefix netip.Prefix) ([]byte, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			serial := ""
			var asn uint32
			for _, device := range h.fixture.prepared.execution.plan.Intent().Devices {
				if "hbn-pod-"+strings.ToLower(device.Serial) == pod {
					serial = device.Serial
					asn = device.ASN
				}
			}
			if serial == "" || namespace != h.fixture.prepared.execution.plan.Intent().Namespace {
				return nil, fmt.Errorf("unselected runtime command")
			}
			h.t.Logf("native command=%d pod=%s prefix=%s apiCalls=%d", command, pod, prefix, h.api.calls.Load())
			h.commandRows = append(h.commandRows, fmt.Sprintf("%s/%s/%d/%s", namespace, pod, command, prefix))
			if command == hbnReadBGPSummary {
				return runtimeBGPJSON(asn, h.fixture.prepared.execution.plan.Intent().Networking.FabricASN), nil
			}
			expectedPrefix := "10.0.120.8/29"
			if serial == "SERIAL-B" {
				expectedPrefix = "10.0.120.0/29"
			}
			if command != hbnReadRemoteRoute || prefix.String() != expectedPrefix {
				return nil, fmt.Errorf("unexpected runtime command")
			}
			return runtimeFIBJSON(prefix.String(), "p0_if"), nil
		},
		probeTraffic: func(_ context.Context, target config.Target, request sshprobe.HostTrafficRequest, _ time.Duration) (sshprobe.HostTrafficResult, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.t.Logf("native traffic=%s to=%s apiCalls=%d", request.Identity.AdapterSerial, request.Destination, h.api.calls.Load())
			h.trafficRows = append(h.trafficRows, request.Identity.AdapterSerial+"/"+request.HostAddressPrefix+"/"+request.Destination.String())
			var selected *activationSelectedDevice
			for i := range h.fixture.activation.selected {
				if h.fixture.activation.selected[i].item.device.Serial == request.Identity.AdapterSerial {
					selected = &h.fixture.activation.selected[i]
				}
			}
			if selected == nil || target.InputRef != selected.host.InputRef || !reflect.DeepEqual(target.SSH, selected.hostTarget.SSH) {
				return sshprobe.HostTrafficResult{}, fmt.Errorf("traffic changed SSH authority")
			}
			result := runtimePassedTraffic(request)
			if h.failTraffic {
				result.State = sshprobe.TrafficProbeFailed
				result.Ordinary.State = sshprobe.TrafficProbeFailed
				result.Ordinary.Counters.Received = 2
				result.Ordinary.Counters.LostPct = 33.3333
			}
			return result, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	var output bytes.Buffer
	ctx = ui.WithOutput(ctx, ui.NewWithWriter(&output))
	result, err := runWithTransports(ctx, request, transports)
	report, readErr := os.ReadFile(request.ReportPath)
	if readErr != nil {
		h.t.Fatal(readErr)
	}
	return result, report, output.String(), err
}

func (h *nativeRuntimeHarness) assertNoReplay() {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	managementReviews := 0
	for key, count := range h.permissions {
		if strings.HasPrefix(key, "management|") {
			managementReviews += count
		}
	}
	// The API identity fixture serves a few core routes before this harness's
	// dispatcher; include those requests in the persistent-effect canary.
	if int(h.api.writes.Load()) != managementReviews {
		h.t.Errorf("management non-read requests=%d, expected only %d SSAR reviews", h.api.writes.Load(), managementReviews)
	}
	tenantCalls := 0
	for key, count := range h.calls {
		if strings.HasPrefix(key, "tenant ") {
			tenantCalls += count
		}
	}
	if h.limiterWaits.Load() != int64(h.api.calls.Load())+int64(tenantCalls) {
		h.t.Errorf("native limiter wiring waits=%d HTTP=%d", h.limiterWaits.Load(), int(h.api.calls.Load())+tenantCalls)
	}
	h.t.Logf("native request totals management=%d tenant=%d total=%d physical=%d command=%d traffic=%d", h.api.calls.Load(), tenantCalls, int(h.api.calls.Load())+tenantCalls, h.collections, len(h.commandRows), len(h.trafficRows))
	if h.mutations != 0 || h.configureCalls != 0 || h.reboots != 0 || h.observations != 0 || h.chartReads != 0 {
		h.t.Fatalf("native replay mutations/configure/reboots/observe/chart=%d/%d/%d/%d/%d", h.mutations, h.configureCalls, h.reboots, h.observations, h.chartReads)
	}
	for key := range h.expectedPermissions {
		if h.permissions[key] == 0 {
			h.t.Errorf("required native permission not observed %s", key)
		}
	}
}

func (h *nativeRuntimeHarness) assertFencedMatrix(result *HBNRuntimeResult) {
	h.t.Helper()
	if result == nil || len(result.BGP) != 2 || len(result.Routes) != 2 || len(result.Traffic) != 2 {
		h.t.Fatal("incomplete native matrix")
	}
	bgp := map[string]bool{}
	for _, r := range result.BGP {
		if r.State != "passed" || !r.Fenced || (r.DPUSerial == "SERIAL-A" && r.HostInputRef != "host-input-a") || (r.DPUSerial == "SERIAL-B" && r.HostInputRef != "host-input-b") || r.ObservedASN != r.LocalASN || bgp[r.DPUSerial] {
			h.t.Fatalf("invalid fenced BGP row %+v", r)
		}
		bgp[r.DPUSerial] = true
	}
	if !bgp["SERIAL-A"] || !bgp["SERIAL-B"] {
		h.t.Fatal("changed BGP cohort")
	}
	routes := map[string]string{}
	for _, r := range result.Routes {
		if r.State != "passed" || !r.Fenced {
			h.t.Fatalf("invalid fenced route row %+v", r)
		}
		routes[r.DPUSerial+"/"+r.RemoteDPUSerial] = r.Prefix
	}
	if !reflect.DeepEqual(routes, map[string]string{"SERIAL-A/SERIAL-B": "10.0.120.8/29", "SERIAL-B/SERIAL-A": "10.0.120.0/29"}) {
		h.t.Fatalf("changed route cohort %v", routes)
	}
	traffic := map[string]string{}
	for _, r := range result.Traffic {
		if !r.Fenced || r.Measurement.State != sshprobe.TrafficProbePassed {
			h.t.Fatalf("invalid fenced traffic row %+v", r)
		}
		traffic[r.SourceDPUSerial+"/"+r.DestinationDPUSerial] = r.SourceHost + "/" + r.DestinationHost
	}
	if !reflect.DeepEqual(traffic, map[string]string{"SERIAL-A/SERIAL-B": "host-input-a/host-input-b", "SERIAL-B/SERIAL-A": "host-input-b/host-input-a"}) {
		h.t.Fatalf("changed traffic cohort %v", traffic)
	}
}

func TestRunNativeCompletedTwoHostRuntime(t *testing.T) {
	h := newNativeRuntimeHarness(t)
	result, report, output, err := h.run()
	if err != nil {
		t.Fatalf("native Run: %v\n%s", err, report)
	}
	if !result.TrafficValidated || result.State != RunStateValidated || result.LastCompletedStage != hbnRuntimeStage || result.Operation == nil || result.Operation.UID != h.operation.UID || result.HBNRuntime == nil || len(result.HBNRuntime.BGP) != 2 || len(result.HBNRuntime.Routes) != 2 || len(result.HBNRuntime.Traffic) != 2 {
		t.Fatalf("native result=%+v", result)
	}
	if len(h.commandRows) != 4 || len(h.trafficRows) != 2 || len(h.inspections) != 6 || h.imageReads != 2 {
		t.Fatalf("native rows commands=%v traffic=%v inspections=%v image=%d", h.commandRows, h.trafficRows, h.inspections, h.imageReads)
	}
	h.assertFencedMatrix(result.HBNRuntime)
	lines := bytes.Split(bytes.TrimSpace(report), []byte("\n"))
	var final executionEvent
	if len(lines) == 0 || json.Unmarshal(lines[len(lines)-1], &final) != nil || final.Stage != "run" || final.Operation != "complete" || final.State != "passed" || final.ReasonCode != "HBN_RUNTIME_COHORT_VALIDATED" {
		t.Fatal("missing final durable validated Run completion")
	}
	if !bytes.Contains(report, []byte("HBN_RUNTIME_COHORT_VALIDATED")) || !strings.Contains(output, "[PASS] HBN runtime: 2 DPUs, 2 remote routes and all 2 directed host pairs validated") {
		t.Fatalf("final durable/UI evidence missing: %s", report)
	}
	h.assertNoReplay()
}

func TestRunNativeRuntimeFinalPhysicalExitFences(t *testing.T) {
	for _, scenario := range []string{"pod-after-exit", "physical-identity"} {
		t.Run(scenario, func(t *testing.T) {
			h := newNativeRuntimeHarness(t)
			h.driftAfterTraffic = scenario == "pod-after-exit"
			h.badPhysicalAfterTraffic = scenario == "physical-identity"
			result, report, _, err := h.run()
			want := "HBN_RUNTIME_FINAL_FENCE_FAILED"
			if scenario == "physical-identity" {
				want = "HBN_RUNTIME_PHYSICAL_EXIT_FAILED"
			}
			if scenario == "physical-identity" && !bytes.Contains(report, []byte("ENROLLMENT_BMC_IDENTITY_CHANGED")) {
				t.Errorf("missing real BMC identity diagnostic")
			}
			if bytes.Contains(report, []byte("HBN_RUNTIME_COHORT_VALIDATED")) {
				t.Error("failed final fence retained cohort validation")
			}
			if err == nil || !strings.Contains(string(report), want) || result.TrafficValidated || result.HBNRuntime == nil || len(result.HBNRuntime.BGP) != 2 || len(result.HBNRuntime.Routes) != 2 || len(result.HBNRuntime.Traffic) != 2 || !h.drifted {
				t.Fatalf("native final fence result=%+v err=%v report=%s", result, err, report)
			}
			h.assertFencedMatrix(result.HBNRuntime)
			h.assertNoReplay()
		})
	}
}

func TestRunNativeRuntimeFailedMeasurementRetriesWholeMatrix(t *testing.T) {
	h := newNativeRuntimeHarness(t)
	h.failTraffic = true
	first, report, _, err := h.run()
	if err == nil || first.TrafficValidated {
		t.Fatalf("first measurement unexpectedly passed: %v %s", err, report)
	}
	h.mu.Lock()
	beforeCommands, beforeTraffic := len(h.commandRows), len(h.trafficRows)
	h.failTraffic = false
	h.mu.Unlock()
	second, report, _, err := h.run()
	if err != nil || !second.TrafficValidated {
		t.Fatalf("retry native Run: %v %s", err, report)
	}
	h.assertFencedMatrix(second.HBNRuntime)
	commands, traffic := h.commandRows[beforeCommands:], h.trafficRows[beforeTraffic:]
	if len(commands) != 4 || len(traffic) != 2 {
		t.Fatalf("retry skipped complete matrix commands=%v traffic=%v", commands, traffic)
	}
	sort.Strings(traffic)
	if !strings.HasPrefix(traffic[0], "SERIAL-A/10.0.120.2/29/10.0.120.10") || !strings.HasPrefix(traffic[1], "SERIAL-B/10.0.120.10/29/10.0.120.2") {
		t.Fatalf("retry directed identities=%v", traffic)
	}
	if first.ReportPath == second.ReportPath {
		t.Fatal("retry reused exclusive report path")
	}
	h.assertNoReplay()
}
