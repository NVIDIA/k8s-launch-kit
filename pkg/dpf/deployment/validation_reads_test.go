// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

type currentReadHTTPFixture struct {
	adapter                  *n3cRuntimeAdapter
	management               *APIClient
	api                      *apiFixture
	sources                  tenantSources
	mu                       sync.Mutex
	paths                    map[string]int
	hook                     func(*http.Request)
	deny                     string
	tenantUID, tenantVersion string
	mutateResponse           func(*http.Request, []byte) []byte
}

func (f *currentReadHTTPFixture) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths[r.URL.Path]++
	if f.hook != nil {
		f.hook(r)
	}
}
func (f *currentReadHTTPFixture) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paths[path]
}

type currentReadTransport struct {
	next    http.RoundTripper
	fixture *currentReadHTTPFixture
}

func (r currentReadTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	r.fixture.record(request)
	response, err := r.next.RoundTrip(request)
	if err != nil {
		return response, err
	}
	r.fixture.mu.Lock()
	uid, version := r.fixture.tenantUID, r.fixture.tenantVersion
	r.fixture.mu.Unlock()
	var replacement string
	if request.URL.Path == "/api/v1/namespaces/kube-system" && uid != "" {
		replacement = `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"` + uid + `"},"status":{"phase":"Active"}}`
	}
	if request.URL.Path == "/version" && version != "" {
		replacement = `{"gitVersion":"` + version + `"}`
	}
	if r.fixture.mutateResponse != nil {
		data, readErr := io.ReadAll(response.Body)
		if readErr != nil {
			_ = response.Body.Close()
			return nil, readErr
		}
		replacement = string(r.fixture.mutateResponse(request, data))
	}
	if replacement != "" {
		_ = response.Body.Close()
		response.Body = io.NopCloser(strings.NewReader(replacement))
		response.ContentLength = int64(len(replacement))
	}
	return response, nil
}

// This fixture exercises the actual management source chain and tenant verifier,
// plus every real graph/allocation/HBN/SFC HTTP read for two selected DPUs.
func newCurrentReadHTTPFixture(t *testing.T, paced bool) *currentReadHTTPFixture {
	t.Helper()
	adapter, children := n3cRuntimeAdapterFixture(t, "SERIAL-A", "SERIAL-B")
	// The shared one-node fixture defaults numberApplied to one. This HTTP
	// fixture realizes both selected nodes, including complete set status.
	for _, child := range children {
		if child.GetKind() == "ServiceInterfaceSet" || child.GetKind() == "ServiceChainSet" {
			if err := unstructured.SetNestedField(child.Object, int64(2), "status", "numberApplied"); err != nil {
				t.Fatal(err)
			}
		}
	}
	f := &currentReadHTTPFixture{adapter: adapter, paths: map[string]int{}}
	tenant := adapter.tenant
	var err error
	f.sources, err = tenant.source.observeTenantSources(context.Background(), tenant.expected)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{
		"DPUDeployment": "dpudeployments", "DPUSet": "dpusets", "DPU": "dpus", "DPUNode": "dpunodes", "DPUDevice": "dpudevices", "DPUNodeMaintenance": "dpunodemaintenances",
		"DPUService": "dpuservices", "DPUServiceChain": "dpuservicechains", "DPUServiceInterface": "dpuserviceinterfaces", "DPUServiceIPAM": "dpuserviceipams", "DPUServiceTemplate": "dpuservicetemplates", "DPUServiceConfiguration": "dpuserviceconfigurations", "DPUFlavor": "dpuflavors", "BlueFieldSoftware": "bluefieldsoftwares",
	}
	application := adapter.management.(*correctionHBNManagement).app
	f.api = newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if r.Method != http.MethodGet {
			t.Errorf("management effect: %s %s", r.Method, r.URL.Path)
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == f.deny {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(&metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: 403})
			return
		}
		for _, object := range append([]*unstructured.Unstructured{f.sources.observed.DPUCluster, f.sources.observed.TenantControlPlane, f.sources.observed.Service}, f.sources.observed.Keepalived...) {
			resource := map[string]string{"DPUCluster": "dpuclusters", "TenantControlPlane": "tenantcontrolplanes", "Service": "services", "ConfigMap": "configmaps", "DaemonSet": "daemonsets"}[object.GetKind()]
			gv := object.GroupVersionKind().GroupVersion()
			prefix := "/prefix/apis/" + gv.String()
			if gv.Group == "" {
				prefix = "/prefix/api/" + gv.Version
			}
			if r.URL.Path == prefix+"/namespaces/"+object.GetNamespace()+"/"+resource+"/"+object.GetName() {
				_ = json.NewEncoder(w).Encode(object)
				return
			}
		}
		secret := f.sources.secret
		if r.URL.Path == "/prefix/api/v1/namespaces/"+secret.Namespace+"/secrets/"+secret.Name {
			_ = json.NewEncoder(w).Encode(secret)
			return
		}
		if r.URL.Path == "/prefix/apis/argoproj.io/v1alpha1/namespaces/"+application.GetNamespace()+"/applications/"+application.GetName() {
			_ = json.NewEncoder(w).Encode(application)
			return
		}
		for _, gv := range []string{"provisioning.dpu.nvidia.com/v1alpha1", "svc.dpu.nvidia.com/v1alpha1"} {
			if r.URL.Path == "/prefix/apis/"+gv {
				resources := []metav1.APIResource{}
				for kind, resource := range kinds {
					serviceKind := strings.HasPrefix(kind, "DPUService") || kind == "DPUDeployment"
					if serviceKind == strings.HasPrefix(gv, "svc.") {
						resources = append(resources, metav1.APIResource{Name: resource, Kind: kind, Namespaced: true})
					}
				}
				_ = json.NewEncoder(w).Encode(&metav1.APIResourceList{GroupVersion: gv, APIResources: resources})
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/dpunodemaintenances") {
			_ = json.NewEncoder(w).Encode(&unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUNodeMaintenanceList", "metadata": map[string]any{"resourceVersion": "10"}}, Items: []unstructured.Unstructured{}})
			return
		}
		for kind, resource := range kinds {
			for _, obj := range adapter.objects {
				if obj.GetKind() != kind {
					continue
				}
				path := "/prefix/apis/" + obj.GetAPIVersion() + "/namespaces/" + obj.GetNamespace() + "/" + resource
				if r.URL.Path == path+"/"+obj.GetName() {
					_ = json.NewEncoder(w).Encode(obj)
					return
				}
				if r.URL.Path == path {
					rows := []unstructured.Unstructured{}
					for _, row := range adapter.objects {
						if row.GetKind() == kind && row.GetNamespace() == obj.GetNamespace() {
							rows = append(rows, *row.DeepCopy())
						}
					}
					_ = json.NewEncoder(w).Encode(&unstructured.UnstructuredList{Object: map[string]any{"apiVersion": obj.GetAPIVersion(), "kind": kind + "List", "metadata": map[string]any{"resourceVersion": "10"}}, Items: rows})
					return
				}
			}
		}
		t.Errorf("unexpected management read: %s", r.URL.Path)
		http.NotFound(w, r)
	})
	var limiter flowcontrol.RateLimiter
	if !paced {
		limiter = flowcontrol.NewFakeAlwaysRateLimiter()
	}
	f.management, err = newAPIClient(context.Background(), f.api.request, f.api.expected, "v1.35.6", nil, nil, limiter)
	if err != nil {
		t.Fatal(err)
	}
	tenant.source = f.management
	// The source digest is immutable; only its read port changes from the fixture
	// source to the equivalent HTTP source objects.
	if err := tenant.VerifyIdentity(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := tenant.api.CoreV1().RESTClient().Get().URL()
	cfg := &rest.Config{Host: base.Scheme + "://" + base.Host, Timeout: time.Second, RateLimiter: flowcontrol.NewFakeAlwaysRateLimiter(), WrapTransport: func(next http.RoundTripper) http.RoundTripper { return currentReadTransport{next: next, fixture: f} }}
	tenant.api, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.reset()
	return f
}
func (f *currentReadHTTPFixture) reset() {
	f.mu.Lock()
	f.paths = map[string]int{}
	f.mu.Unlock()
	f.api.calls.Store(0)
}
func (f *currentReadHTTPFixture) capture(ctx context.Context) (validationCurrentCohort, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	selected, err := validationSelectedHosts(f.adapter.native.desired, f.adapter.native.request, f.adapter.native.physicalScope.expected)
	if err != nil {
		return validationCurrentCohort{}, err
	}
	return f.adapter.native.captureValidationCohort(ctx, f.management, f.adapter.tenant, selected, f.adapter.image, true)
}

func TestValidationCurrentReadHTTPBoundedRequestCounts(t *testing.T) {
	f := newCurrentReadHTTPFixture(t, false)
	for range 2 {
		cohort, err := f.capture(context.Background())
		if err != nil || len(cohort.bindings.anchors) != 2 || len(cohort.leases) != 2 {
			t.Fatalf("capture rejected: %v causes=%s anchors=%d leases=%d unavailable=%v", err, currentReadErrorCauses(err), len(cohort.bindings.anchors), len(cohort.leases), cohort.bindings.unavailable)
		}
		// 48 boundary identity/source + 51 semantic data/SSAR + 40 uncached
		// discovery requests. All ten SFC calls and both graph/allocation
		// snapshots remain. Reopening repeats the full boundary sources.
		tenantRequests := 0
		for path, n := range f.paths {
			if !strings.HasPrefix(path, "/prefix/") {
				tenantRequests += n
			}
		}
		t.Logf("management=%d tenant=%d total=%d", f.api.calls.Load(), tenantRequests, int(f.api.calls.Load())+tenantRequests)
		if f.api.calls.Load() != 97 || tenantRequests != 42 {
			t.Fatalf("request decomposition changed: management=%d tenant=%d", f.api.calls.Load(), tenantRequests)
		}
		namespace := f.adapter.native.desired.plan.Intent().Namespace
		for suffix, want := range map[string]int{"/dpudeployments/hbn-bgp": 2, "/dpusets": 2, "/dpus": 2, "/dpunodemaintenances": 2, "/dpuservices": 2, "/dpuservicechains": 2, "/dpuserviceinterfaces": 2, "/dpuserviceipams/hbn-workload": 4, "/cidrpools/hbn-workload": 4, "/nodes": 2, "/daemonsets": 1, "/pods": 1, "/pods/hbn-pod-serial-a": 1, "/pods/hbn-pod-serial-b": 1} {
			got := 0
			for path, n := range f.paths {
				if strings.HasSuffix(path, suffix) && (!strings.HasPrefix(path, "/prefix/") || strings.Contains(path, "/namespaces/"+namespace+"/")) {
					got += n
				}
			}
			if got != want {
				t.Fatalf("semantic current read %s=%d want %d", suffix, got, want)
			}
		}
		if got := f.count("/prefix/apis/provisioning.dpu.nvidia.com/v1alpha1/namespaces/" + f.adapter.tenant.expected.Namespace + "/dpuclusters/" + f.adapter.tenant.expected.Name); got != 4 {
			t.Fatalf("source snapshots=%d, expected 4 boundary snapshots", got)
		}
		if got := f.count("/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"); got != 4 {
			t.Fatalf("named permissions=%d", got)
		}
		if f.api.writes.Load() != 0 {
			t.Fatal("management mutation")
		}
		f.reset()
	}
}

func TestValidationCurrentReadHTTPDriftAndReadFailure(t *testing.T) {
	for _, scenario := range []string{"entry-management", "during-source", "during-cluster", "during-credentials", "during-management", "during-tenant-uid", "during-tenant-version", "denied-and-source", "canceled", "transient-source"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCurrentReadHTTPFixture(t, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "entry-management" {
				f.api.uid.Store("replacement")
			}
			triggered := false
			f.hook = func(r *http.Request) {
				if triggered || !strings.Contains(r.URL.Path, "/dpudeployments/") {
					return
				}
				triggered = true
				switch scenario {
				case "during-source", "denied-and-source":
					f.sources.secret.UID = "replacement"
				case "during-cluster":
					f.sources.observed.DPUCluster.SetUID("replacement")
				case "during-credentials":
					f.sources.secret.Data["super-admin.conf"] = append(f.sources.secret.Data["super-admin.conf"], '\n')
				case "during-management":
					f.api.uid.Store("replacement")
				case "during-tenant-uid":
					f.tenantUID = "replacement"
				case "during-tenant-version":
					f.tenantVersion = "v0.0.1"
				case "canceled":
					cancel()
				case "transient-source":
					f.sources.observed.DPUCluster.SetUID("temporary")
					f.sources.observed.DPUCluster.SetUID(types.UID(f.adapter.tenant.dpuClusterUID))
				}
				if scenario == "denied-and-source" {
					f.deny = r.URL.Path
				}
			}
			cohort, err := f.capture(ctx)
			if scenario == "transient-source" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || cohort.graph.deployment != nil || cohort.bindings.anchors != nil {
				t.Fatalf("partial/failed capture escaped: %v", err)
			}
			if scenario == "canceled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				return
			}
			// Existing deployment verifiers reject these mismatches before a
			// typed identity sentinel; this extraction preserves that behavior.
			if scenario == "during-tenant-version" {
				return
			}
			if !validationIdentityChanged(err) {
				t.Fatalf("lost measured identity cause: %v", err)
			}
			if scenario == "denied-and-source" && !apierrors.IsForbidden(err) {
				t.Fatalf("lost joined denied cause: %v", err)
			}
		})
	}
}

func TestValidationCurrentReadDefaultPacedCapture(t *testing.T) {
	f := newCurrentReadHTTPFixture(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err := f.capture(ctx)
	if err != nil {
		t.Fatalf("default paced capture: %v", err)
	}
	t.Logf("default 5QPS management capture completed in %s with %d management HTTP reads", time.Since(start), f.api.calls.Load())
}

func TestValidationBufferedCompletionsRejectUnacceptedRegion(t *testing.T) {
	for _, cause := range []error{errors.New("unavailable"), context.Canceled, errManagementIdentityChanged} {
		completed := []validation.Outcome{}
		r := newValidationRecorder([]validation.CheckSpec{{ID: "independent"}, {ID: "dependent"}}, func(e validation.ProgressEvent) {
			if e.Phase == validation.Completed {
				completed = append(completed, e.Outcome)
			}
		})
		r.record("independent", validation.Passed, validation.ReasonNone, "accepted", "")
		buffer := r.bufferCompletions()
		buffer.run("dependent", func() error { return nil })
		if len(completed) != 1 {
			t.Fatal("provisional completion emitted")
		}
		r.acceptBuffered(buffer, cause)
		if r.outcomes["independent"].State != validation.Passed || r.outcomes["dependent"].State != validation.Unverified || len(completed) != 2 || completed[1].State != validation.Unverified {
			t.Fatalf("acceptance failed: %+v", r.outcomes)
		}
	}
}

func currentReadErrorCauses(err error) string {
	if err == nil {
		return ""
	}
	result := err.Error()
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range e.Unwrap() {
			result += "; " + currentReadErrorCauses(child)
		}
	case interface{ Unwrap() error }:
		result += "; " + currentReadErrorCauses(e.Unwrap())
	}
	return result
}

type currentTenantSourceFake struct {
	verify func(context.Context) error
	reads  validationCurrentTenantReader
}

func (f currentTenantSourceFake) VerifyIdentity(ctx context.Context) error { return f.verify(ctx) }
func (f currentTenantSourceFake) currentValidationReads() validationCurrentTenantReader {
	return f.reads
}

func TestValidationCurrentReadScopeJoinsBothCloseFailures(t *testing.T) {
	for _, cancelAtRead := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		order := []string{}
		managementCalls, tenantCalls := 0, 0
		management := &validationReadFake{identity: func(ctx context.Context) error {
			managementCalls++
			order = append(order, "management")
			if managementCalls == 2 {
				return errors.Join(errManagementIdentityChanged, ctx.Err())
			}
			return nil
		}}
		tenant := currentTenantSourceFake{verify: func(ctx context.Context) error {
			tenantCalls++
			order = append(order, "tenant")
			if tenantCalls == 2 {
				return errors.Join(errTenantIdentityChanged, ctx.Err())
			}
			return nil
		}}
		readErr := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "selected", errors.New("private"))
		err := withValidationCurrentReads(ctx, management, tenant, func(validationCurrentManagementReader, validationCurrentTenantReader) error {
			order = append(order, "read")
			if cancelAtRead {
				cancel()
			}
			return readErr
		})
		cancel()
		if !errors.Is(err, errManagementIdentityChanged) || !errors.Is(err, errTenantIdentityChanged) || !apierrors.IsForbidden(err) || managementCalls != 2 || tenantCalls != 2 {
			t.Fatalf("close did not retain independent causes: %v order=%v", err, order)
		}
		if cancelAtRead && !errors.Is(err, context.Canceled) {
			t.Fatalf("lost cancellation: %v", err)
		}
		if strings.Join(order, ",") != "management,tenant,read,management,tenant" {
			t.Fatalf("scope order=%v", order)
		}
	}
}

func TestValidationCurrentReadResourceCloseInvalidatesOnlyDependentRows(t *testing.T) {
	for _, scenario := range []string{"source-drift", "close-denied", "nil-tenant", "pod-drift"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCurrentReadHTTPFixture(t, false)
			completed := []validation.Outcome{}
			checks := f.adapter.checks
			hostStarts := 0
			if scenario == "pod-drift" {
				f.adapter.native.mode = validation.ModeFull
				var err error
				checks, _, err = validationRuntimeChecks(f.adapter.native.desired.plan)
				if err != nil {
					t.Fatal(err)
				}
				f.mutateResponse = func(request *http.Request, data []byte) []byte {
					if !strings.Contains(request.URL.Path, "/pods/hbn-pod-") {
						return data
					}
					var pod corev1.Pod
					if err := json.Unmarshal(data, &pod); err != nil {
						t.Error(err)
						return data
					}
					pod.UID = "replacement"
					encoded, err := json.Marshal(pod)
					if err != nil {
						t.Error(err)
						return data
					}
					return encoded
				}
			}
			r := newValidationRecorder(checks, func(event validation.ProgressEvent) {
				if event.Phase == validation.Running && strings.HasPrefix(string(event.Check.ID), "physical/host-network/") {
					hostStarts++
				}
				if event.Phase == validation.Completed {
					completed = append(completed, event.Outcome)
				}
			})
			deployments := 0
			f.hook = func(request *http.Request) {
				if !strings.Contains(request.URL.Path, "/dpudeployments/") {
					return
				}
				deployments++
				// Two per-device graphs form the independently accepted region.
				if deployments != 3 {
					return
				}
				switch scenario {
				case "source-drift":
					f.sources.secret.UID = "replacement"
				case "close-denied":
					f.deny = "/prefix/api/v1/namespaces/" + f.sources.secret.Namespace + "/secrets/" + f.sources.secret.Name
				}
			}
			tenant := f.adapter.tenant
			if scenario == "nil-tenant" {
				tenant = nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			imageReads := 0
			err := f.adapter.native.observeValidationRuntimeResourcesWithImage(ctx, f.management, tenant, r, func(context.Context, string, time.Duration, http.RoundTripper) (hbnPodImageIdentity, error) {
				imageReads++
				return f.adapter.image, nil
			})
			if scenario == "nil-tenant" && imageReads != 0 {
				t.Fatal("management-only collection contacted image registry")
			}
			if scenario != "nil-tenant" && err == nil {
				t.Fatal("failed close accepted")
			}
			if scenario == "nil-tenant" && err != nil {
				t.Fatal(err)
			}
			for _, id := range []validation.CheckID{"devices/coverage", "devices/SERIAL-A", "devices/SERIAL-B"} {
				if r.outcomes[id].State != validation.Passed {
					t.Fatalf("independent device evidence erased: %s %+v", id, r.outcomes[id])
				}
			}
			for _, out := range completed {
				if (strings.HasPrefix(string(out.ID), "hbn/") || strings.HasPrefix(string(out.ID), "workloads/")) && out.State == validation.Passed {
					t.Fatalf("provisional accepted progress: %+v", out)
				}
			}
			if scenario == "pod-drift" && hostStarts != 0 {
				t.Fatalf("host qualification ran after observed Pod drift: %d", hostStarts)
			}
			if scenario == "close-denied" && !validationRuntimeAccessFailure(err) {
				t.Fatalf("access category lost: %v", err)
			}
		})
	}
}

func TestValidationCurrentReadHTTPRejectsMalformedAndReplacedData(t *testing.T) {
	for _, scenario := range []string{"repeated-page", "truncated-page", "discovery-conflict", "named-pod", "allocation"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCurrentReadHTTPFixture(t, false)
			pools := 0
			f.mutateResponse = func(request *http.Request, data []byte) []byte {
				path := request.URL.Path
				var object map[string]any
				if json.Unmarshal(data, &object) != nil {
					return data
				}
				switch {
				case scenario == "repeated-page" && strings.HasSuffix(path, "/servicechains"):
					object["metadata"].(map[string]any)["continue"] = "repeat"
				case scenario == "truncated-page" && strings.HasSuffix(path, "/servicechains"):
					return []byte(`{"kind":"ServiceChainList","items":[`)
				case scenario == "discovery-conflict" && path == "/apis/svc.dpu.nvidia.com/v1alpha1":
					resources := object["resources"].([]any)
					object["resources"] = append(resources, resources...)
				case scenario == "named-pod" && strings.Contains(path, "/pods/hbn-pod-"):
					object["metadata"].(map[string]any)["uid"] = "replacement"
				case scenario == "allocation" && strings.Contains(path, "/cidrpools/hbn-workload"):
					pools++
					if pools > 1 {
						object["metadata"].(map[string]any)["uid"] = "replacement"
					}
				default:
					return data
				}
				encoded, err := json.Marshal(object)
				if err != nil {
					t.Error(err)
					return data
				}
				return encoded
			}
			cohort, err := f.capture(context.Background())
			if err == nil || cohort.graph.deployment != nil {
				t.Fatalf("malformed/replaced data accepted: %v", err)
			}
			// Four source snapshots prove errors did not bypass close.
			path := "/prefix/apis/provisioning.dpu.nvidia.com/v1alpha1/namespaces/" + f.adapter.tenant.expected.Namespace + "/dpuclusters/" + f.adapter.tenant.expected.Name
			if f.count(path) != 4 {
				t.Fatalf("error skipped full source close: snapshots=%d", f.count(path))
			}
		})
	}
}

func TestValidationCurrentReadHTTPPoisonStopsMeasurements(t *testing.T) {
	f := newCurrentReadHTTPFixture(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	anchor, err := f.capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	session, _ := standaloneValidationSession()
	session.anchor, session.observe = anchor, f.capture
	measurements := 0
	session.read = func(context.Context, string, string, hbnRuntimeCommand, netip.Prefix) ([]byte, error) {
		measurements++
		return nil, nil
	}
	f.hook = func(request *http.Request) {
		if strings.Contains(request.URL.Path, "/dpudeployments/") {
			f.sources.secret.UID = "replacement"
		}
	}
	if _, err := session.ObserveBGP(ctx, "SERIAL-A"); !validationIdentityChanged(err) {
		t.Fatalf("missing pre-measurement identity failure: %v", err)
	}
	calls := f.api.calls.Load()
	if _, err := session.ObserveBGP(ctx, "SERIAL-B"); err == nil {
		t.Fatal("poisoned probe resumed")
	}
	if err := session.VerifyCohort(ctx); err == nil {
		t.Fatal("poisoned final verification passed")
	}
	if measurements != 0 || f.api.calls.Load() != calls {
		t.Fatalf("poisoned runtime read again: measurements=%d requests=%d/%d", measurements, calls, f.api.calls.Load())
	}
}

func TestValidationCurrentReadExtractionKeepsDeploymentWrapperFences(t *testing.T) {
	f := newCurrentReadHTTPFixture(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	parent, err := retainedWorkloadIPAM(f.adapter.native.desired.plan)
	if err != nil {
		t.Fatal(err)
	}
	app := f.adapter.management.(*correctionHBNManagement).app
	var chain *unstructured.Unstructured
	for _, object := range f.adapter.objects {
		if object.GetKind() == "DPUServiceChain" {
			chain = object
			break
		}
	}
	if chain == nil {
		t.Fatal("fixture chain missing")
	}
	for _, test := range []struct {
		name     string
		requests int
		read     func() error
	}{
		{name: "management Get", requests: 6, read: func() error { _, err := f.management.Get(ctx, parent); return err }},
		{name: "management List", requests: 6, read: func() error {
			_, err := f.management.List(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": "DPUService", "metadata": map[string]any{"namespace": parent.GetNamespace()}}}, "")
			return err
		}},
		{name: "Application", requests: 5, read: func() error {
			_, err := f.management.getHBNRuntimeApplication(ctx, app.GetNamespace(), app.GetName())
			return err
		}},
		{name: "tenant child", requests: 46, read: func() error { _, err := f.adapter.tenant.getWorkloadChild(ctx, parent); return err }},
		{name: "tenant SFC", requests: 46, read: func() error {
			_, err := f.adapter.tenant.listValidationSFCObjects(ctx, "ServiceChainSet", chain.GetNamespace(), chain.GetName(), "")
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.reset()
			if err := test.read(); err != nil {
				t.Fatal(err)
			}
			requests := int(f.api.calls.Load())
			for path, n := range f.paths {
				if !strings.HasPrefix(path, "/prefix/") {
					requests += n
				}
			}
			if requests != test.requests {
				t.Fatalf("deployment per-call guard count changed: %d want %d", requests, test.requests)
			}
		})
	}
	f.reset()
	f.hook = func(request *http.Request) {
		if strings.Contains(request.URL.Path, "/dpuserviceipams/") {
			f.api.uid.Store("replacement")
		}
	}
	if object, err := f.management.Get(ctx, parent); !errors.Is(err, errManagementIdentityChanged) || object != nil {
		t.Fatalf("deployment post-read drift fence bypassed: %v", err)
	}
}

// Lose the allocation after retained prerequisites passed, while changing the
// selected deployment. No after-side graph is available to accept those passes.
func TestValidationCurrentReadIncompleteAllocationDiscardsProvisionalPasses(t *testing.T) {
	for _, scenario := range []string{"denied", "pending"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCurrentReadHTTPFixture(t, false)
			ipamReads := 0
			f.hook = func(request *http.Request) {
				if !strings.Contains(request.URL.Path, "/dpuserviceipams/") {
					return
				}
				ipamReads++
				if ipamReads != 3 {
					return
				}
				for _, object := range f.adapter.objects {
					if object.GetKind() == "DPUDeployment" {
						object.SetUID("replacement")
					}
					if object.GetKind() == "DPUServiceIPAM" && strings.HasSuffix(request.URL.Path, "/"+object.GetName()) && scenario == "pending" {
						_ = unstructured.SetNestedSlice(object.Object, []any{conditionMap("Ready", "False", "Pending", object.GetGeneration())}, "status", "conditions")
					}
				}
				if scenario == "denied" {
					f.deny = request.URL.Path
				}
			}
			completed := []validation.Outcome{}
			r := newValidationRecorder(f.adapter.checks, func(event validation.ProgressEvent) {
				if event.Phase == validation.Completed {
					completed = append(completed, event.Outcome)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			err := f.adapter.native.observeValidationRuntimeResourcesWithImage(ctx, f.management, f.adapter.tenant, r, func(context.Context, string, time.Duration, http.RoundTripper) (hbnPodImageIdentity, error) {
				return f.adapter.image, nil
			})
			if err != nil {
				t.Fatalf("incomplete allocation became fatal: %v", err)
			}
			if ipamReads < 3 {
				t.Fatal("allocation failure was not reached after retained prerequisites")
			}
			r.blockRemaining()
			for _, out := range completed {
				if (strings.HasPrefix(string(out.ID), "hbn/") || strings.HasPrefix(string(out.ID), "workloads/")) && out.State == validation.Passed {
					t.Fatalf("incomplete semantic collection emitted pass: %+v", out)
				}
			}
			for _, id := range []validation.CheckID{"devices/coverage", "devices/SERIAL-A", "devices/SERIAL-B"} {
				if r.outcomes[id].State != validation.Passed {
					t.Fatalf("independent device evidence lost: %+v", r.outcomes[id])
				}
			}
			if scenario == "pending" {
				for id, out := range r.outcomes {
					if strings.HasPrefix(string(id), "hbn/") || strings.HasPrefix(string(id), "workloads/") {
						if out.State != validation.Pending || out.Reason != validation.ReasonProgressing {
							t.Fatalf("progressing allocation left retry blocker: %+v", out)
						}
					}
				}
			} else {
				if r.outcomes["hbn/coverage"].Reason != validation.ReasonDenied || r.outcomes["workloads/coverage"].State != validation.Unverified || r.outcomes["workloads/coverage"].Reason != validation.ReasonDependency {
					t.Fatalf("denial/dependency evidence changed: %+v", r.outcomes)
				}
			}
		})
	}
}

func TestValidationCurrentReadIncompleteAllocationConvergesThroughRunner(t *testing.T) {
	f := newCurrentReadHTTPFixture(t, false)
	f.adapter.management = f.management
	original := map[string]*unstructured.Unstructured{}
	for key, object := range f.adapter.objects {
		original[key] = object.DeepCopy()
	}
	ipamReads := 0
	f.adapter.before = func(call int) {
		ipamReads = 0
		if call > 1 {
			for key, object := range f.adapter.objects {
				object.Object = original[key].DeepCopy().Object
			}
		}
	}
	f.hook = func(request *http.Request) {
		if f.adapter.calls != 1 || !strings.Contains(request.URL.Path, "/dpuserviceipams/") {
			return
		}
		ipamReads++
		if ipamReads != 3 {
			return
		}
		for _, object := range f.adapter.objects {
			if object.GetKind() == "DPUDeployment" {
				object.SetUID("replacement")
			}
			if object.GetKind() == "DPUServiceIPAM" && strings.HasSuffix(request.URL.Path, "/"+object.GetName()) {
				_ = unstructured.SetNestedSlice(object.Object, []any{conditionMap("Ready", "False", "Pending", object.GetGeneration())}, "status", "conditions")
			}
		}
	}
	checks, scope, err := validationRuntimeChecks(f.adapter.native.desired.plan)
	if err != nil {
		t.Fatal(err)
	}
	boundary := &n3cRunnerBoundary{n3cRuntimeAdapter: f.adapter, independent: []validation.Outcome{}}
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
	runner, err := validation.NewRunner(plan, boundary, validation.Options{Wait: 30 * time.Second, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	result := runner.Run(context.Background())
	if f.adapter.calls != 2 || result.Verdict != validation.VerdictResourcesPassed {
		current, captureErr := f.capture(context.Background())
		if captureErr == nil {
			for _, item := range current.graph.selected {
				t.Logf("selected SFC %s: %v", item.retained.Serial, f.adapter.native.observeTenantSFC(context.Background(), f.adapter.tenant, current.graph, current.snapshot, item.retained.Serial))
			}
		}
		t.Fatalf("native allocation convergence failed: calls=%d verdict=%s snapshots=%+v", f.adapter.calls, result.Verdict, f.adapter.snapshots)
	}
	for _, out := range f.adapter.snapshots[0].Checks {
		if strings.HasPrefix(string(out.ID), "hbn/") || strings.HasPrefix(string(out.ID), "workloads/") {
			if out.State != validation.Pending {
				t.Fatalf("first incomplete collection was accepted or blocked retry: %+v", out)
			}
		}
	}
	for _, out := range f.adapter.snapshots[1].Checks {
		if out.State != validation.Passed {
			t.Fatalf("settled collection did not pass: %+v", out)
		}
	}
}
