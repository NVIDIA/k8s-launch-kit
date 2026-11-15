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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
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

// These fixtures model the pinned controller's typed ObjectMeta wire format,
// independently of the comparison being tested. Empty maps are omitted by JSON.
func interfaceWireParent(kind string) *unstructured.Unstructured {
	entry := map[string]any{"interfaceType": kind}
	if kind == "physical" {
		entry["physical"] = map[string]any{"interfaceName": "p0"}
	} else {
		entry["pf"] = map[string]any{"pfID": int64(0)}
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": "DPUServiceInterface", "metadata": map[string]any{"name": "interface-test", "namespace": "workload-system"}, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{"nodeSelector": map[string]any{"matchLabels": map[string]any{"selected": "node"}}, "template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"interface": "p0"}}, "spec": entry}}}}}}
}

func interfaceWireChild(t *testing.T, parent *unstructured.Unstructured, mode string) *unstructured.Unstructured {
	t.Helper()
	labels, _, err := unstructured.NestedStringMap(parent.Object, "spec", "template", "metadata", "labels")
	if err != nil {
		t.Fatal(err)
	}
	annotations, _, err := unstructured.NestedStringMap(parent.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		t.Fatal(err)
	}
	if mode != "" {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["svc.dpu.nvidia.com/interface-mode"] = mode
	}
	spec, found, err := unstructured.NestedMap(parent.Object, "spec", "template", "spec")
	if err != nil || !found {
		t.Fatal("missing fixture template")
	}
	source := struct {
		metav1.TypeMeta   `json:",inline"`
		metav1.ObjectMeta `json:"metadata,omitempty"`
		Spec              map[string]any `json:"spec"`
	}{TypeMeta: metav1.TypeMeta{APIVersion: "svc.dpu.nvidia.com/v1alpha1", Kind: "ServiceInterfaceSet"}, ObjectMeta: metav1.ObjectMeta{Name: parent.GetName(), Namespace: parent.GetNamespace(), UID: types.UID("child-wire-uid"), ResourceVersion: "1", Labels: labels, Annotations: annotations}, Spec: spec}
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	child := &unstructured.Unstructured{}
	if err := child.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}
	return child
}

func setInterfaceWireField(t *testing.T, object *unstructured.Unstructured, value any, path ...string) {
	t.Helper()
	if err := unstructured.SetNestedField(object.Object, value, path...); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyServiceInterfaceSetWireMetadata(t *testing.T) {
	for _, kind := range []string{"physical", "pf"} {
		for _, mode := range []string{"", "nsi"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				parent := interfaceWireParent(kind)
				child := interfaceWireChild(t, parent, mode)
				if _, found, _ := unstructured.NestedFieldNoCopy(child.Object, "metadata", "labels"); found {
					t.Fatal("typed ObjectMeta did not omit empty labels")
				}
				if mode == "" {
					if _, found, _ := unstructured.NestedFieldNoCopy(child.Object, "metadata", "annotations"); found {
						t.Fatal("typed ObjectMeta did not omit empty annotations")
					}
				}
				beforeParent, beforeChild := parent.DeepCopy(), child.DeepCopy()
				if err := verifyWorkloadChild(parent, child, TenantReadinessExpected{}); err != nil {
					t.Fatalf("pinned wire representation rejected: %v", err)
				}
				if !reflect.DeepEqual(beforeParent.Object, parent.Object) || !reflect.DeepEqual(beforeChild.Object, child.Object) {
					t.Fatal("verification mutated source objects")
				}
			})
		}
	}
	for _, mode := range []string{"", "nsi"} {
		t.Run("preserve-template-metadata/"+mode, func(t *testing.T) {
			parent := interfaceWireParent("physical")
			setInterfaceWireField(t, parent, map[string]any{"team": "network"}, "spec", "template", "metadata", "labels")
			setInterfaceWireField(t, parent, map[string]any{"owner": "fixture"}, "spec", "template", "metadata", "annotations")
			if err := verifyWorkloadChild(parent, interfaceWireChild(t, parent, mode), TenantReadinessExpected{}); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("explicit-parent-nsi", func(t *testing.T) {
		parent := interfaceWireParent("physical")
		setInterfaceWireField(t, parent, map[string]any{"svc.dpu.nvidia.com/interface-mode": "nsi"}, "spec", "template", "metadata", "annotations")
		if err := verifyWorkloadChild(parent, interfaceWireChild(t, parent, "nsi"), TenantReadinessExpected{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("explicit empty outer maps", func(t *testing.T) {
		p := interfaceWireParent("physical")
		c := interfaceWireChild(t, p, "")
		c.SetLabels(map[string]string{})
		c.SetAnnotations(map[string]string{})
		if err := verifyWorkloadChild(p, c, TenantReadinessExpected{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("PF empty virtual network", func(t *testing.T) {
		p := interfaceWireParent("pf")
		setInterfaceWireField(t, p, "", "spec", "template", "spec", "template", "spec", "pf", "virtualNetwork")
		if err := verifyWorkloadChild(p, interfaceWireChild(t, p, "nsi"), TenantReadinessExpected{}); err != nil {
			t.Fatal(err)
		}
	})

}

func TestVerifyServiceInterfaceSetRejectsMetadataAndModeDrift(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, *unstructured.Unstructured, *unstructured.Unstructured)
	}{
		{"extra label", func(t *testing.T, _, c *unstructured.Unstructured) {
			setInterfaceWireField(t, c, map[string]any{"unexpected": "private-value-canary"}, "metadata", "labels")
		}},
		{"extra annotation", func(t *testing.T, _, c *unstructured.Unstructured) {
			setInterfaceWireField(t, c, "private-value-canary", "metadata", "annotations", "unexpected")
		}},
		{"missing expected label", func(t *testing.T, p, _ *unstructured.Unstructured) {
			setInterfaceWireField(t, p, map[string]any{"expected": "value"}, "spec", "template", "metadata", "labels")
		}},
		{"missing expected annotation", func(t *testing.T, p, _ *unstructured.Unstructured) {
			setInterfaceWireField(t, p, "value", "spec", "template", "metadata", "annotations", "expected")
		}},
		{"changed label", func(t *testing.T, p, c *unstructured.Unstructured) {
			setInterfaceWireField(t, p, map[string]any{"expected": "original"}, "spec", "template", "metadata", "labels")
			setInterfaceWireField(t, c, map[string]any{"expected": "changed"}, "metadata", "labels")
		}},
		{"changed annotation", func(t *testing.T, p, c *unstructured.Unstructured) {
			setInterfaceWireField(t, p, "original", "spec", "template", "metadata", "annotations", "expected")
			setInterfaceWireField(t, c, "changed", "metadata", "annotations", "expected")
		}},
		{"legacy mode", func(t *testing.T, _, c *unstructured.Unstructured) {
			setInterfaceWireField(t, c, "legacy", "metadata", "annotations", "svc.dpu.nvidia.com/interface-mode")
		}},
		{"unknown mode", func(t *testing.T, _, c *unstructured.Unstructured) {
			setInterfaceWireField(t, c, "private-value-canary", "metadata", "annotations", "svc.dpu.nvidia.com/interface-mode")
		}},
		{"reserved parent conflict", func(t *testing.T, p, c *unstructured.Unstructured) {
			setInterfaceWireField(t, p, "legacy", "spec", "template", "metadata", "annotations", "svc.dpu.nvidia.com/interface-mode")
			setInterfaceWireField(t, c, "legacy", "metadata", "annotations", "svc.dpu.nvidia.com/interface-mode")
		}},
		{"reserved parent malformed", func(t *testing.T, p, _ *unstructured.Unstructured) {
			setInterfaceWireField(t, p, false, "spec", "template", "metadata", "annotations", "svc.dpu.nvidia.com/interface-mode")
		}},
		{"parent requires stamp", func(t *testing.T, p, c *unstructured.Unstructured) {
			setInterfaceWireField(t, p, "nsi", "spec", "template", "metadata", "annotations", "svc.dpu.nvidia.com/interface-mode")
			unstructured.RemoveNestedField(c.Object, "metadata", "annotations")
		}},
		{"VPC PF", func(t *testing.T, p, c *unstructured.Unstructured) {
			for _, o := range []*unstructured.Unstructured{p, c} {
				prefix := []string{"spec"}
				if o == p {
					prefix = append(prefix, "template", "spec")
				}
				unstructured.RemoveNestedField(o.Object, append(prefix, "template", "spec", "physical")...)
				setInterfaceWireField(t, o, "pf", append(prefix, "template", "spec", "interfaceType")...)
				setInterfaceWireField(t, o, map[string]any{"pfID": int64(0), "virtualNetwork": "vpc-current"}, append(prefix, "template", "spec", "pf")...)
			}
		}},
		{"unsupported interface mode path", func(t *testing.T, p, c *unstructured.Unstructured) {
			for _, o := range []*unstructured.Unstructured{p, c} {
				prefix := []string{"spec"}
				if o == p {
					prefix = append(prefix, "template", "spec")
				}
				unstructured.RemoveNestedField(o.Object, append(prefix, "template", "spec", "physical")...)
				setInterfaceWireField(t, o, "vf", append(prefix, "template", "spec", "interfaceType")...)
				setInterfaceWireField(t, o, map[string]any{"vfID": int64(0), "pfID": int64(0)}, append(prefix, "template", "spec", "vf")...)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := interfaceWireParent("physical")
			c := interfaceWireChild(t, p, "nsi")
			tc.mutate(t, p, c)
			err := verifyWorkloadChild(p, c, TenantReadinessExpected{})
			if err == nil || strings.Contains(err.Error(), "private-value-canary") {
				t.Fatalf("drift accepted or raw value exposed: %v", err)
			}
		})
	}
	for _, side := range []string{"parent", "child"} {
		for _, field := range []string{"labels", "annotations"} {
			for _, bad := range []struct {
				name  string
				value any
			}{{"null", nil}, {"scalar", int64(7)}, {"list", []any{}}, {"nonstring value", map[string]any{"key": false}}} {
				t.Run(side+"/"+field+"/"+bad.name, func(t *testing.T) {
					p := interfaceWireParent("physical")
					c := interfaceWireChild(t, p, "nsi")
					o, path := c, []string{"metadata", field}
					if side == "parent" {
						o, path = p, []string{"spec", "template", "metadata", field}
					}
					setInterfaceWireField(t, o, bad.value, path...)
					if err := verifyWorkloadChild(p, c, TenantReadinessExpected{}); err == nil {
						t.Fatal("malformed metadata accepted")
					}
				})
			}
		}
	}
}

func TestVerifyServiceInterfaceSetRejectsSpecAndIdentityDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *unstructured.Unstructured)
	}{
		{"name", func(_ *testing.T, c *unstructured.Unstructured) { c.SetName("other") }},
		{"namespace", func(_ *testing.T, c *unstructured.Unstructured) { c.SetNamespace("other") }},
		{"UID", func(_ *testing.T, c *unstructured.Unstructured) { c.SetUID("") }},
		{"resource version", func(_ *testing.T, c *unstructured.Unstructured) { c.SetResourceVersion("") }},
		{"GVK", func(_ *testing.T, c *unstructured.Unstructured) { c.SetKind("OtherKind") }},
		{"deleting", func(_ *testing.T, c *unstructured.Unstructured) { now := metav1.Now(); c.SetDeletionTimestamp(&now) }},
		{"selector", func(t *testing.T, c *unstructured.Unstructured) {
			setInterfaceWireField(t, c, "foreign", "spec", "nodeSelector", "matchLabels", "selected")
		}},
		{"inner label", func(t *testing.T, c *unstructured.Unstructured) {
			setInterfaceWireField(t, c, "foreign", "spec", "template", "metadata", "labels", "interface")
		}},
		{"physical name", func(t *testing.T, c *unstructured.Unstructured) {
			setInterfaceWireField(t, c, "p1", "spec", "template", "spec", "physical", "interfaceName")
		}},
		{"extra spec field", func(t *testing.T, c *unstructured.Unstructured) {
			setInterfaceWireField(t, c, false, "spec", "unexpected")
		}},
		{"missing spec", func(_ *testing.T, c *unstructured.Unstructured) { delete(c.Object, "spec") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := interfaceWireParent("physical")
			c := interfaceWireChild(t, p, "nsi")
			tc.mutate(t, c)
			if err := verifyWorkloadChild(p, c, TenantReadinessExpected{}); err == nil {
				t.Fatal("spec or identity drift accepted")
			}
		})
	}
	t.Run("PF ID", func(t *testing.T) {
		p := interfaceWireParent("pf")
		c := interfaceWireChild(t, p, "nsi")
		setInterfaceWireField(t, c, int64(1), "spec", "template", "spec", "pf", "pfID")
		if err := verifyWorkloadChild(p, c, TenantReadinessExpected{}); err == nil {
			t.Fatal("PF ID drift accepted")
		}
	})

}

func TestWorkloadChildVerificationDiagnosticIsFiniteAndPrivate(t *testing.T) {
	const canary = "private-error-canary"
	var typedNil *ReconciliationError
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""}, {"typed nil", typedNil, ""}, {"ordinary", errors.New(canary), ""},
		{"unknown code", &ReconciliationError{ReasonCode: canary, Message: canary, cause: errors.New(canary)}, ""},
		{"known wrapped", fmt.Errorf("%s: %w", canary, &ReconciliationError{ReasonCode: "WORKLOAD_INTERFACE_CHILD_MODE_MISMATCH", Message: canary, cause: errors.New(canary)}), "WORKLOAD_INTERFACE_CHILD_MODE_MISMATCH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := workloadChildVerificationMessage("bounded stage message", tc.err)
			if strings.Contains(got, canary) || workloadChildVerificationReason(tc.err) != tc.want {
				t.Fatalf("unsafe diagnostic %q", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatal("finite category missing")
			}
		})
	}
	for _, reason := range []string{"WORKLOAD_INTERFACE_CHILD_METADATA_INVALID", "WORKLOAD_INTERFACE_CHILD_METADATA_MISMATCH", "WORKLOAD_INTERFACE_CHILD_SPEC_MISMATCH", "WORKLOAD_INTERFACE_CHILD_MODE_MISMATCH"} {
		if workloadChildVerificationReason(&ReconciliationError{ReasonCode: reason, Message: canary}) != reason {
			t.Fatal("finite interface category rejected")
		}
	}
}

type workloadChildDiagnosticClient struct {
	*activationIntegrationFake
	point             string
	targetReads       int
	lastInterfaceSeen bool
}

func (c *workloadChildDiagnosticClient) getTenantWorkloadChild(ctx context.Context, tenant *tenantClient, parent *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	child, err := c.softwarePreparationFake.getTenantWorkloadChild(ctx, tenant, parent)
	if err != nil {
		return nil, err
	}
	if parent.GetKind() != "DPUServiceInterface" {
		return child, nil
	}
	if parent.GetName() == "pf0hpf" {
		c.lastInterfaceSeen = true
	}
	if parent.GetName() != "p0" {
		return child, nil
	}
	c.targetReads++
	corrupt := c.point == "initial" || c.point == "final" && c.targetReads >= 2 || c.point == "receipt" && c.lastInterfaceSeen
	if corrupt {
		a := child.GetAnnotations()
		a["unexpected"] = "private-value-canary"
		child.SetAnnotations(a)
	}
	return child, nil
}

func TestWorkloadChildDiagnosticsReachInitialFinalAndReceiptFailures(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, point := range []string{"initial", "final", "receipt"} {
		t.Run(point, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			c := &workloadChildDiagnosticClient{activationIntegrationFake: f.client, point: point}
			f.controlPlane.tenant.Client.source = c
			objects, err := retainedWorkloadObjects(execution)
			if err != nil {
				t.Fatal(err)
			}
			specs, err := orderWorkloadObjects(objects)
			if err != nil {
				t.Fatal(err)
			}
			var target workloadResourceSpec
			for _, spec := range specs {
				if spec.object.GetKind() == "DPUServiceInterface" && spec.object.GetName() == "p0" {
					target = spec
				}
			}
			if target.object == nil {
				t.Fatal("missing retained interface")
			}
			current, err := c.Get(f.ctx, target.object)
			if err != nil {
				t.Fatal(err)
			}
			journal := testJournal(execution)
			ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
			defer cancel()
			want := "WORKLOAD_TENANT_CHILD_MISMATCH"
			if point == "receipt" {
				want = "WORKLOAD_RESOURCE_SET_CHANGED"
				_, err = runWorkloadStage(ctx, c, execution, f.controlPlane, f.workload.Software, f.prepared.selected, f.prepared.password, journal, time.Millisecond, time.Second, objects)
			} else {
				if point == "final" {
					want = "WORKLOAD_TENANT_CHILD_CHANGED"
				}
				_, err = waitWorkloadResource(ctx, c, execution.reconciler, f.controlPlane.tenant.Client, target, current.GetUID(), journal, time.Millisecond)
			}
			text := journal.file.(*managementMemoryFile).String()
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(text, want) || !strings.Contains(text, "WORKLOAD_INTERFACE_CHILD_METADATA_MISMATCH") || strings.Contains(text+err.Error(), "private-value-canary") {
				t.Fatalf("point=%s err=%v journal=%s", point, err, text)
			}
			if c.guardCalls != 0 || c.resetSafetyCalls != 0 || len(c.patches) != 0 {
				t.Fatal("workload mismatch reached activation effects")
			}
		})
	}
}
