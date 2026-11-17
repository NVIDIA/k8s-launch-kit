// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func TestTenantKeepalivedRejectsBrokenNetworkConfiguration(t *testing.T) {
	for name, mutate := range map[string]func(*unstructured.Unstructured, *unstructured.Unstructured){
		"placeholder-only template": func(cm, _ *unstructured.Unstructured) {
			cm.Object["data"] = map[string]any{"keepalived.conf.tmpl": "# interface INTERFACE\n# virtual_router_id VIRTUAL_ROUTER_ID\n# VIRTUAL_IP"}
		},
		"missing port": func(_, ds *unstructured.Unstructured) {
			init := keepalivedInit(ds)
			env := init["env"].([]any)
			init["env"] = append(env[:3], env[4:]...)
		},
		"wrong port": func(_, ds *unstructured.Unstructured) {
			keepalivedInit(ds)["env"].([]any)[3].(map[string]any)["value"] = "6443"
		},
		"static node IP": func(_, ds *unstructured.Unstructured) {
			env := keepalivedInit(ds)["env"].([]any)[0].(map[string]any)
			delete(env, "valueFrom")
			env["value"] = "192.0.2.99"
		},
		"missing tolerations": func(_, ds *unstructured.Unstructured) {
			unstructured.RemoveNestedField(ds.Object, "spec", "template", "spec", "tolerations")
		},
		"missing affinity": func(_, ds *unstructured.Unstructured) {
			unstructured.RemoveNestedField(ds.Object, "spec", "template", "spec", "affinity")
		},
		"missing privileges": func(_, ds *unstructured.Unstructured) {
			containers, _, _ := unstructured.NestedSlice(ds.Object, "spec", "template", "spec", "containers")
			delete(containers[0].(map[string]any), "securityContext")
			_ = unstructured.SetNestedSlice(ds.Object, containers, "spec", "template", "spec", "containers")
		},
		"non-root pod": func(_, ds *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(ds.Object, int64(1000), "spec", "template", "spec", "securityContext", "runAsUser")
		},
		"pod network": func(_, ds *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(ds.Object, false, "spec", "template", "spec", "hostNetwork")
		},
		"renderer command": func(_, ds *unstructured.Unstructured) { keepalivedInit(ds)["command"] = []any{"/bin/true"} },
		"renderer mount": func(_, ds *unstructured.Unstructured) {
			keepalivedInit(ds)["volumeMounts"].([]any)[0].(map[string]any)["name"] = "other"
		},
		"wrong configmap": func(_, ds *unstructured.Unstructured) {
			volumes, _, _ := unstructured.NestedSlice(ds.Object, "spec", "template", "spec", "volumes")
			volumes[1].(map[string]any)["configMap"].(map[string]any)["name"] = "other"
			_ = unstructured.SetNestedSlice(ds.Object, volumes, "spec", "template", "spec", "volumes")
		},
	} {
		t.Run(name, func(t *testing.T) {
			expected, observed := readyTenantFixture(t)
			mutate(observed.Keepalived[0], observed.Keepalived[1])
			got := EvaluateTenantReadiness(expected, observed)
			if got.Ready || got.State != TenantReadinessConflict || got.ReasonCode != "TENANT_KEEPALIVED_SPEC_MISMATCH" {
				t.Fatalf("broken configuration accepted or misclassified: %#v", got)
			}
		})
	}
}

func keepalivedInit(ds *unstructured.Unstructured) map[string]any {
	return ds.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["initContainers"].([]any)[0].(map[string]any)
}

func TestTenantReadinessWaitsForObservedPlacementAndGeneration(t *testing.T) {
	for _, scenario := range []string{"missing generation", "zero generation", "misscheduled", "unknown placement"} {
		t.Run(scenario, func(t *testing.T) {
			expected, observed := readyTenantFixture(t)
			switch scenario {
			case "missing generation":
				unstructured.RemoveNestedField(observed.TenantControlPlane.Object, "status", "observedGeneration")
			case "zero generation":
				_ = unstructured.SetNestedField(observed.TenantControlPlane.Object, int64(0), "status", "observedGeneration")
			case "misscheduled":
				_ = unstructured.SetNestedField(observed.Keepalived[1].Object, int64(1), "status", "numberMisscheduled")
			case "unknown placement":
				unstructured.RemoveNestedField(observed.Keepalived[1].Object, "status", "numberMisscheduled")
			}
			got := EvaluateTenantReadiness(expected, observed)
			if got.Ready || got.State != TenantReadinessPending {
				t.Fatalf("ordinary convergence not classified pending: %#v", got)
			}
		})
	}
}
