// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestVerifyCIDRPoolExclusionsMatchesPinnedSingleClusterMapping(t *testing.T) {
	network := map[string]any{
		"network": "192.0.2.0/24", "prefixSize": int64(30),
		"excludeRanges": []any{
			map[string]any{"startIP": "192.0.2.5", "endIP": "192.0.2.5"},
			map[string]any{"startIP": "192.0.2.9", "endIP": "192.0.2.9"},
		},
	}
	pool := map[string]any{"exclusions": []any{
		map[string]any{"startIP": "192.0.2.9", "endIP": "192.0.2.9"},
		map[string]any{"startIP": "192.0.2.5", "endIP": "192.0.2.5"},
	}}
	if err := verifyCIDRPoolExclusions(network, pool); err != nil {
		t.Fatalf("rendered host exclusions did not match pinned single-cluster mapping: %v", err)
	}
	if err := verifyCIDRPoolExclusions(map[string]any{"network": "192.0.2.0/24"}, map[string]any{}); err != nil {
		t.Fatalf("empty exclusions should remain omitted: %v", err)
	}
}

func TestVerifyCIDRPoolChildRejectsMalformedDefaultGateway(t *testing.T) {
	parent := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": "DPUServiceIPAM",
		"metadata": map[string]any{"name": "pool", "namespace": "tenant"},
		"spec": map[string]any{"ipv4Network": map[string]any{
			"network": "192.0.2.0/24", "prefixSize": int64(30), "defaultGateway": false,
		}},
	}}
	child := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "nv-ipam.nvidia.com/v1alpha1", "kind": "CIDRPool",
		"metadata": map[string]any{"name": "pool", "namespace": "tenant", "labels": map[string]any{
			"dpu.nvidia.com/dpuserviceipam-name": "pool", "dpu.nvidia.com/dpuserviceipam-namespace": "tenant",
		}},
		"spec": map[string]any{"cidr": "192.0.2.0/24", "perNodeNetworkPrefix": int64(30), "defaultGateway": "false"},
	}}
	if err := verifyCIDRPoolChild(parent, child); err == nil || !strings.Contains(err.Error(), "default-gateway setting is malformed") {
		t.Fatalf("string defaultGateway was treated as false: %v", err)
	}

	delete(child.Object["spec"].(map[string]any), "defaultGateway")
	if err := verifyCIDRPoolChild(parent, child); err != nil {
		t.Fatalf("omitted defaultGateway did not retain the false schema default: %v", err)
	}
}

func TestVerifyCIDRPoolExclusionsRejectsDriftAndUnsupportedMappings(t *testing.T) {
	base := map[string]any{
		"network": "192.0.2.0/24", "prefixSize": int64(30),
		"excludeRanges": []any{map[string]any{"startIP": "192.0.2.5", "endIP": "192.0.2.5"}},
	}
	tests := []struct {
		name    string
		network map[string]any
		pool    map[string]any
	}{
		{name: "missing exclusion", network: base, pool: map[string]any{}},
		{name: "changed address", network: base, pool: map[string]any{"exclusions": []any{map[string]any{"startIP": "192.0.2.6", "endIP": "192.0.2.6"}}}},
		{name: "malformed range", network: base, pool: map[string]any{"exclusions": []any{map[string]any{"startIP": "not-an-ip", "endIP": "not-an-ip"}}}},
		{name: "non-singleton range", network: base, pool: map[string]any{"exclusions": []any{map[string]any{"startIP": "192.0.2.5", "endIP": "192.0.2.6"}}}},
		{name: "per-cluster quota", network: map[string]any{"subnetsPerDPUCluster": int64(4)}, pool: map[string]any{}},
		{name: "static allocations", network: map[string]any{"allocations": map[string]any{"node-a": "192.0.2.0/30"}}, pool: map[string]any{}},
		{name: "legacy exclusions", network: map[string]any{"exclusions": []any{"192.0.2.5"}}, pool: map[string]any{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := verifyCIDRPoolExclusions(test.network, test.pool); err == nil {
				t.Fatal("verifyCIDRPoolExclusions() accepted drift or an unsupported transform")
			}
		})
	}
}

func TestWorkloadResourceReadinessUsesOnlyPreNodeControllerConditions(t *testing.T) {
	for _, test := range []struct {
		kind      string
		mode      workloadCheckMode
		condition string
	}{
		{kind: "DPUServiceTemplate", mode: workloadTemplateReadiness, condition: "DPUServiceTemplateReconciled"},
		{kind: "DPUServiceIPAM", mode: workloadIPAMReadiness, condition: "DPUIPAMObjectReconciled"},
		{kind: "DPUServiceInterface", mode: workloadInterfaceReadiness, condition: "ServiceInterfaceSetReconciled"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			conditions := []any{conditionMap(test.condition, "True", "Success", 3)}
			if test.mode == workloadTemplateReadiness {
				conditions = append(conditions, conditionMap("Ready", "True", "Success", 3))
			}
			object := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "test.dpu.nvidia.com/v1alpha1", "kind": test.kind,
				"metadata": map[string]any{"generation": int64(3)},
				"status":   map[string]any{"observedGeneration": int64(3), "conditions": conditions},
			}}
			if got := workloadResourceReadiness(object, test.mode); !got.Ready {
				t.Fatalf("current pre-node condition did not pass: %+v", got)
			}
			object.Object["status"].(map[string]any)["observedGeneration"] = int64(2)
			if got := workloadResourceReadiness(object, test.mode); got.Ready || got.ReasonCode != "WORKLOAD_STATUS_STALE" {
				t.Fatalf("stale status passed: %+v", got)
			}
		})
	}
}

func TestInterfaceNodeSelectorIsExplicitAndBounded(t *testing.T) {
	valid := func(values ...any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "uplink"}, "spec": map[string]any{
			"template": map[string]any{"spec": map[string]any{"nodeSelector": map[string]any{"matchExpressions": []any{map[string]any{
				"key": "kubernetes.io/hostname", "operator": "In", "values": values,
			}}}}},
		}}}
	}
	if err := validateInterfaceNodeSelector(valid("node-a", "node-b"), 2); err != nil {
		t.Fatalf("complete finite selector rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		object *unstructured.Unstructured
		count  int
	}{
		{name: "missing values", object: valid(), count: 1},
		{name: "wrong count", object: valid("node-a"), count: 2},
		{name: "duplicate values", object: valid("node-a", "node-a"), count: 2},
		{name: "invalid value", object: valid("bad/value"), count: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateInterfaceNodeSelector(test.object, test.count); err == nil {
				t.Fatal("unbounded or incomplete selector was accepted")
			}
		})
	}
}

func TestGetTenantWorkloadChildRejectsMissingOrWrongObservedGVK(t *testing.T) {
	for _, test := range []struct {
		name              string
		apiVersion        string
		kind              string
		emptyResourceList bool
	}{
		{name: "wrong kind", apiVersion: "nv-ipam.nvidia.com/v1alpha1", kind: "IPPool"},
		{name: "missing type metadata"},
		{name: "empty resource description", emptyResourceList: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			expected, observed := tenantSourceFixture(t)
			source := &tenantSourceStub{sources: observed}
			tenant, _ := tenantIdentityClientFixture(t, expected, source)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/namespaces/kube-system":
					_ = json.NewEncoder(w).Encode(&corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "tenant-system"}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}})
				case "/version":
					_ = json.NewEncoder(w).Encode(map[string]string{"gitVersion": "v" + strings.TrimPrefix(expected.KubernetesVersion, "v")})
				case "/apis/nv-ipam.nvidia.com/v1alpha1":
					if test.emptyResourceList {
						_, _ = w.Write([]byte("null"))
						return
					}
					_ = json.NewEncoder(w).Encode(&metav1.APIResourceList{GroupVersion: "nv-ipam.nvidia.com/v1alpha1", APIResources: []metav1.APIResource{{Name: "cidrpools", Kind: "CIDRPool", Namespaced: true}}})
				case "/apis/nv-ipam.nvidia.com/v1alpha1/namespaces/" + expected.Namespace + "/cidrpools/pool":
					if test.emptyResourceList {
						t.Error("child read continued after empty tenant API resource description")
						return
					}
					_ = json.NewEncoder(w).Encode(&unstructured.Unstructured{Object: map[string]any{
						"apiVersion": test.apiVersion, "kind": test.kind,
						"metadata": map[string]any{"name": "pool", "namespace": expected.Namespace, "uid": "child-uid", "resourceVersion": "1"},
					}})
				default:
					t.Errorf("unexpected tenant API path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			api, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			tenant.api = api
			parent := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": "DPUServiceIPAM",
				"metadata": map[string]any{"name": "pool", "namespace": expected.Namespace},
				"spec":     map[string]any{"ipv4Network": map[string]any{"network": "192.0.2.0/24", "prefixSize": int64(30)}},
			}}
			child, err := (&APIClient{}).getTenantWorkloadChild(context.Background(), tenant, parent)
			if err == nil || child != nil {
				t.Fatalf("tenant child with untrusted GVK was accepted: child=%#v err=%v", child, err)
			}
		})
	}
}

func TestGetTenantWorkloadChildDiscoveryHonorsContext(t *testing.T) {
	expected, observed := tenantSourceFixture(t)
	source := &tenantSourceStub{sources: observed}
	tenant, _ := tenantIdentityClientFixture(t, expected, source)
	discoveryStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/kube-system":
			_ = json.NewEncoder(w).Encode(&corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "tenant-system"}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}})
		case "/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"gitVersion": "v" + strings.TrimPrefix(expected.KubernetesVersion, "v")})
		case "/apis/nv-ipam.nvidia.com/v1alpha1":
			close(discoveryStarted)
			<-r.Context().Done()
		default:
			t.Errorf("unexpected tenant API path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	api, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	tenant.api = api
	parent := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": "DPUServiceIPAM",
		"metadata": map[string]any{"name": "pool", "namespace": expected.Namespace},
		"spec":     map[string]any{"ipv4Network": map[string]any{"network": "192.0.2.0/24", "prefixSize": int64(30)}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := (&APIClient{}).getTenantWorkloadChild(ctx, tenant, parent)
		result <- err
	}()
	select {
	case <-discoveryStarted:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("tenant API discovery did not start")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("tenant API discovery did not return cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tenant API discovery ignored cancellation")
	}
}
