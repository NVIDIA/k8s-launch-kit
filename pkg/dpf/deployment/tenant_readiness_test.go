// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestEvaluateTenantReadinessConvergedResources(t *testing.T) {
	expected, observed := readyTenantFixture(t)
	before := cloneTenantObserved(observed)
	got := EvaluateTenantReadiness(expected, observed)
	if !got.Ready || got.State != "ready" || got.ReasonCode != "TENANT_RESOURCES_CONVERGED" {
		t.Fatalf("EvaluateTenantReadiness() = %#v, want converged", got)
	}
	if !reflect.DeepEqual(observed, before) {
		t.Fatal("tenant readiness mutated the observed resources")
	}
}

func TestEvaluateTenantReadinessBlocksStaleAndProgressingStatus(t *testing.T) {
	t.Run("Kamaji observed generation lags", func(t *testing.T) {
		expected, observed := readyTenantFixture(t)
		observed.TenantControlPlane.Object["status"].(map[string]any)["observedGeneration"] = int64(1)
		got := EvaluateTenantReadiness(expected, observed)
		if got.Ready || got.State != "pending" || got.ReasonCode != "TENANT_CONTROL_PLANE_STATUS_STALE" {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("Kamaji upgrading", func(t *testing.T) {
		expected, observed := readyTenantFixture(t)
		observed.TenantControlPlane.Object["status"].(map[string]any)["kubernetesResources"].(map[string]any)["version"].(map[string]any)["status"] = "Upgrading"
		got := EvaluateTenantReadiness(expected, observed)
		if got.Ready || got.State != "pending" || got.ReasonCode != "TENANT_CONTROL_PLANE_PROGRESSING" {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("malformed generation", func(t *testing.T) {
		expected, observed := readyTenantFixture(t)
		observed.TenantControlPlane.Object["status"].(map[string]any)["observedGeneration"] = "two"
		got := EvaluateTenantReadiness(expected, observed)
		if got.Ready || got.State != "invalid" || got.ReasonCode != "TENANT_CONTROL_PLANE_STATUS_INVALID" {
			t.Fatalf("got %#v", got)
		}
	})
}

func TestEvaluateTenantReadinessRejectsDeletingWrongOwnerAndSpecDrift(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*TenantReadinessExpected, *TenantReadinessObserved)
		wantReason string
	}{
		{name: "deleting cluster", mutate: func(_ *TenantReadinessExpected, o *TenantReadinessObserved) {
			o.DPUCluster.Object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-14T00:00:00Z"
		}, wantReason: "TENANT_RESOURCE_DELETING"},
		{name: "wrong owner uid", mutate: func(_ *TenantReadinessExpected, o *TenantReadinessObserved) {
			refs := o.TenantControlPlane.Object["metadata"].(map[string]any)["ownerReferences"].([]any)
			refs[0].(map[string]any)["uid"] = "another-cluster-uid"
		}, wantReason: "TENANT_CONTROL_PLANE_OWNERSHIP_MISMATCH"},
		{name: "VIP changed", mutate: func(_ *TenantReadinessExpected, o *TenantReadinessObserved) {
			o.DPUCluster.Object["spec"].(map[string]any)["clusterEndpoint"].(map[string]any)["keepalived"].(map[string]any)["vip"] = "192.0.2.99"
		}, wantReason: "TENANT_SPEC_MISMATCH"},
		{name: "node selector changed", mutate: func(_ *TenantReadinessExpected, o *TenantReadinessObserved) {
			o.Keepalived[1].Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["nodeSelector"].(map[string]any)["node-role.kubernetes.io/control-plane"] = "false"
		}, wantReason: "TENANT_KEEPALIVED_SPEC_MISMATCH"},
		{name: "service drift", mutate: func(_ *TenantReadinessExpected, o *TenantReadinessObserved) {
			o.Service.Object["spec"].(map[string]any)["selector"].(map[string]any)["kamaji.clastix.io/name"] = "other"
		}, wantReason: "TENANT_SERVICE_SPEC_MISMATCH"},
		{name: "service target port drift", mutate: func(_ *TenantReadinessExpected, o *TenantReadinessObserved) {
			o.Service.Object["spec"].(map[string]any)["ports"].([]any)[0].(map[string]any)["targetPort"] = int64(6443)
		}, wantReason: "TENANT_SERVICE_SPEC_MISMATCH"},
		{name: "running version drift", mutate: func(_ *TenantReadinessExpected, o *TenantReadinessObserved) {
			o.TenantControlPlane.Object["status"].(map[string]any)["kubernetesResources"].(map[string]any)["version"].(map[string]any)["version"] = "v1.34.0"
		}, wantReason: "TENANT_CONTROL_PLANE_VERSION_MISMATCH"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expected, observed := readyTenantFixture(t)
			test.mutate(&expected, &observed)
			got := EvaluateTenantReadiness(expected, observed)
			if got.Ready || got.ReasonCode != test.wantReason {
				t.Fatalf("got %#v, want reason %s", got, test.wantReason)
			}
		})
	}
}

func TestEvaluateTenantReadinessDoesNotClaimTenantAPIOrVIPReachability(t *testing.T) {
	expected, observed := readyTenantFixture(t)
	got := EvaluateTenantReadiness(expected, observed)
	if !got.Ready || got.ReasonCode != "TENANT_RESOURCES_CONVERGED" {
		t.Fatalf("got %#v", got)
	}
	if got.Message == "" || !containsAll(got.Message, "tenant API access", "VIP operation remain unverified") {
		t.Fatalf("readiness message overclaims proof: %q", got.Message)
	}
}

func TestEvaluateTenantReadinessUsesKamajiPluralCIDRFields(t *testing.T) {
	expected, observed := readyTenantFixture(t)
	// Kamaji 26.8 documents plural fields as authoritative over legacy singular fields.
	observed.TenantControlPlane.Object["spec"].(map[string]any)["networkProfile"].(map[string]any)["podCidr"] = "10.244.0.0/16"
	got := EvaluateTenantReadiness(expected, observed)
	if !got.Ready {
		t.Fatalf("plural podCidrs matches retained intent; deprecated podCidr must not override it: %#v", got)
	}
}

func readyTenantFixture(t *testing.T) (TenantReadinessExpected, TenantReadinessObserved) {
	expected := TenantReadinessExpected{
		Name: "hbn-tenant", Namespace: "hbn-tenant",
		Labels: map[string]string{"l8k.example/tenant": "hbn"}, MaxNodes: 2,
		KubernetesVersion: "v1.35.6", VIP: "192.0.2.20", Interface: "eth0",
		PodCIDR: "10.244.0.0/14", ServiceCIDR: "10.96.0.0/16", VirtualRouterID: 126,
		NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""},
	}
	dc := tenantObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUCluster", "hbn-tenant", "hbn-tenant", map[string]any{
		"uid": "dc-uid", "labels": map[string]any{"l8k.example/tenant": "hbn"},
	}, map[string]any{
		"type": "kamaji", "maxNodes": int64(2), "kubeconfig": "hbn-tenant-admin-kubeconfig",
		"clusterEndpoint": map[string]any{"keepalived": map[string]any{
			"vip": expected.VIP, "interface": expected.Interface, "virtualRouterID": int64(expected.VirtualRouterID),
			"nodeSelector": map[string]any{"node-role.kubernetes.io/control-plane": ""},
		}},
	}, map[string]any{
		"phase": "Ready", "conditions": []any{
			tenantCondition("Ready", "True"), tenantCondition("Created", "True"), tenantCondition("KeepalivedReady", "True"),
		},
	})
	tcp := tenantObject("kamaji.clastix.io/v1alpha1", "TenantControlPlane", expected.Name, expected.Namespace, map[string]any{
		"uid": "tcp-uid", "labels": map[string]any{"dpu.nvidia.com/cluster": expected.Name, "tenant.clastix.io": expected.Name},
		"ownerReferences": []any{map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUCluster", "name": expected.Name, "uid": "dc-uid"}},
	}, map[string]any{
		"kubernetes":     map[string]any{"version": expected.KubernetesVersion},
		"networkProfile": map[string]any{"address": expected.VIP, "port": int64(30538), "podCidrs": []any{expected.PodCIDR}, "serviceCidrs": []any{expected.ServiceCIDR}},
	}, map[string]any{
		"observedGeneration":  int64(2),
		"kubernetesResources": map[string]any{"version": map[string]any{"version": expected.KubernetesVersion, "status": "Ready"}},
	})
	service := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": expected.Name, "namespace": expected.Namespace, "uid": "svc-uid"},
		"spec": map[string]any{"type": "NodePort", "selector": map[string]any{"kamaji.clastix.io/name": expected.Name}, "ports": []any{map[string]any{
			"name": "kube-apiserver", "protocol": "TCP", "port": int64(30538), "targetPort": int64(30538), "nodePort": int64(30538),
		}}},
	}}
	owner := []any{map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUCluster", "name": expected.Name, "uid": "dc-uid"}}
	data, err := os.ReadFile("testdata/tenant-keepalived-v26.8.0.yaml")
	if err != nil {
		t.Fatal(err)
	}
	objects, err := decodeOperatorManifest(data)
	if err != nil || len(objects) != 2 {
		t.Fatalf("invalid upstream keepalived fixture: %v", err)
	}
	var cm, ds *unstructured.Unstructured
	for _, object := range objects {
		object.SetNamespace(expected.Namespace)
		if err := unstructured.SetNestedSlice(object.Object, owner, "metadata", "ownerReferences"); err != nil {
			t.Fatal(err)
		}
		switch object.GetKind() {
		case "ConfigMap":
			cm = object
			object.SetUID("cm-uid")
		case "DaemonSet":
			ds = object
			object.SetUID("ds-uid")
		}
	}
	if cm == nil || ds == nil {
		t.Fatal("missing upstream keepalived fixture kinds")
	}
	ds.SetGeneration(2)
	// DPF's handler adds this required affinity after rendering the template.
	affinity := map[string]any{"nodeAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{"nodeSelectorTerms": []any{
		map[string]any{"matchExpressions": []any{map[string]any{"key": "node-role.kubernetes.io/master", "operator": "Exists"}}},
		map[string]any{"matchExpressions": []any{map[string]any{"key": "node-role.kubernetes.io/control-plane", "operator": "Exists"}}},
	}}}}
	if err := unstructured.SetNestedMap(ds.Object, affinity, "spec", "template", "spec", "affinity"); err != nil {
		t.Fatal(err)
	}
	tolerations := []any{
		map[string]any{"key": "node-role.kubernetes.io/master", "operator": "Exists", "effect": "NoSchedule"},
		map[string]any{"key": "node-role.kubernetes.io/control-plane", "operator": "Exists", "effect": "NoSchedule"},
	}
	if err := unstructured.SetNestedSlice(ds.Object, tolerations, "spec", "template", "spec", "tolerations"); err != nil {
		t.Fatal(err)
	}
	ds.Object["status"] = map[string]any{"observedGeneration": int64(2), "desiredNumberScheduled": int64(3), "numberReady": int64(3), "updatedNumberScheduled": int64(3), "numberMisscheduled": int64(0)}

	return expected, TenantReadinessObserved{DPUCluster: dc, TenantControlPlane: tcp, Service: service, Keepalived: []*unstructured.Unstructured{cm, ds}}
}

func tenantObject(apiVersion, kind, name, namespace string, metadata, spec, status map[string]any) *unstructured.Unstructured {
	metadata["name"], metadata["namespace"], metadata["generation"] = name, namespace, int64(2)
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": metadata, "spec": spec, "status": status}}
}

func tenantCondition(kind, status string) map[string]any {
	return map[string]any{"type": kind, "status": status, "reason": "Ready"}
}

func cloneTenantObserved(in TenantReadinessObserved) TenantReadinessObserved {
	out := in
	if in.DPUCluster != nil {
		out.DPUCluster = in.DPUCluster.DeepCopy()
	}
	if in.TenantControlPlane != nil {
		out.TenantControlPlane = in.TenantControlPlane.DeepCopy()
	}
	if in.Service != nil {
		out.Service = in.Service.DeepCopy()
	}
	out.Keepalived = make([]*unstructured.Unstructured, len(in.Keepalived))
	for i, object := range in.Keepalived {
		if object != nil {
			out.Keepalived[i] = object.DeepCopy()
		}
	}
	return out
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
