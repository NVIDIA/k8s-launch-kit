// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestReadinessDoesNotExposeArbitraryPhaseText(t *testing.T) {
	for _, kind := range []string{"DPU", "DPUCluster", "BlueFieldSoftware"} {
		object := dpfObject(kind, map[string]any{
			"phase": "private?token=secret\n" + strings.Repeat("x", 4096), "observedGeneration": int64(2),
		})
		got := EvaluateReadiness(object)
		if got.Ready || strings.Contains(got.Message, "secret") || len(got.Message) > 256 {
			t.Fatalf("untrusted phase escaped readiness classification for %s: %#v", kind, got)
		}
	}
}

func TestEvaluateReadinessForPinnedDPFResources(t *testing.T) {
	tests := []struct {
		name string
		obj  *unstructured.Unstructured
	}{
		{
			name: "operator config",
			obj:  dpfObject("DPFOperatorConfig", map[string]any{"observedGeneration": int64(2), "conditions": []any{conditionMap("Ready", "True", "Success", 2)}}),
		},
		{
			name: "DPU cluster uses phase and Ready condition without invented observed generation",
			obj:  dpfObject("DPUCluster", map[string]any{"phase": "Ready", "conditions": []any{conditionMap("Created", "True", "Success", 0), conditionMap("Ready", "True", "HealthCheckPassed", 0)}}),
		},
		{
			name: "successful singleton DPU discovery scan",
			obj:  dpfObject("DPUDiscovery", map[string]any{"observedGeneration": int64(2), "lastScanTime": "2026-11-14T00:00:00Z", "foundDPUs": int64(1)}),
		},
		{
			name: "DPU device condition is current",
			obj:  dpfObject("DPUDevice", map[string]any{"conditions": []any{conditionMap("Ready", "True", "Successful", 2)}}),
		},
		{
			name: "DPU phase and aggregate observed generation",
			obj:  dpfObject("DPU", map[string]any{"phase": "Ready", "observedGeneration": int64(2), "conditions": []any{conditionMap("Ready", "True", "DPUReady", 0)}}),
		},
		{
			name: "BlueField software phase and current download and readiness conditions",
			obj:  dpfObject("BlueFieldSoftware", map[string]any{"phase": "Ready", "observedGeneration": int64(2), "conditions": []any{conditionMap("Downloaded", "True", "Success", 2), conditionMap("Ready", "True", "Success", 2)}}),
		},
		{
			name: "DPU deployment aggregate condition",
			obj:  dpfObject("DPUDeployment", map[string]any{"observedGeneration": int64(2), "conditions": []any{conditionMap("Ready", "True", "Success", 2)}}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := test.obj.DeepCopy()
			got := EvaluateReadiness(test.obj)
			if !got.Ready || got.ReasonCode != "RESOURCE_READY" {
				t.Fatalf("EvaluateReadiness() = %#v, want ready", got)
			}
			if !reflect.DeepEqual(test.obj, before) {
				t.Fatal("EvaluateReadiness() mutated its input")
			}
		})
	}
}

func TestEvaluateReadinessForKubernetesResources(t *testing.T) {
	deployment := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "controller", "generation": int64(2)},
		"spec":     map[string]any{"replicas": int64(2)},
		"status":   map[string]any{"observedGeneration": int64(2), "updatedReplicas": int64(2), "readyReplicas": int64(2), "availableReplicas": int64(2)},
	}}
	if got := EvaluateReadiness(deployment); !got.Ready {
		t.Fatalf("Deployment readiness = %#v", got)
	}
	node := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": "worker"},
		"status": map[string]any{"conditions": []any{conditionMap("Ready", "True", "KubeletReady", 0)}},
	}}
	if got := EvaluateReadiness(node); !got.Ready {
		t.Fatalf("Node readiness = %#v", got)
	}
}

func TestDeploymentReadinessAllowsOmittedOptionalConditions(t *testing.T) {
	deployment := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "controller", "generation": int64(1)},
		"spec":     map[string]any{"replicas": int64(1)},
		"status":   map[string]any{"observedGeneration": int64(1), "updatedReplicas": int64(1), "readyReplicas": int64(1), "availableReplicas": int64(1)},
	}}
	if got := EvaluateReadiness(deployment); !got.Ready {
		t.Fatalf("Deployment with current complete replica status and omitted conditions = %#v", got)
	}
	deployment.Object["status"].(map[string]any)["conditions"] = []any{conditionMap("Progressing", "False", "ProgressDeadlineExceeded", 0)}
	if got := EvaluateReadiness(deployment); got.Ready || got.ReasonCode != "DEPLOYMENT_ROLLOUT_FAILED" {
		t.Fatalf("Deployment past progress deadline = %#v, want terminal rollout failure", got)
	}
	deployment.Object["status"].(map[string]any)["readyReplicas"] = int64(0)
	deployment.Object["status"].(map[string]any)["availableReplicas"] = int64(0)
	if got := EvaluateReadiness(deployment); got.Ready || got.ReasonCode != "DEPLOYMENT_ROLLOUT_FAILED" {
		t.Fatalf("Failed rollout with missing replicas = %#v, want failure rather than ordinary progression", got)
	}
}

func TestEvaluateReadinessRejectsUnsupportedAPIVersionAndDeletingResources(t *testing.T) {
	wrongVersion := dpfObject("DPUCluster", map[string]any{"phase": "Ready", "conditions": []any{conditionMap("Created", "True", "Success", 0), conditionMap("Ready", "True", "Success", 0)}})
	wrongVersion.SetAPIVersion("provisioning.dpu.nvidia.com/v1beta1")
	deleting := dpfObject("DPU", map[string]any{"phase": "Ready", "observedGeneration": int64(2), "conditions": []any{conditionMap("Ready", "True", "Success", 0)}})
	deleting.Object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-14T00:00:00Z"
	badDeletion := dpfObject("DPU", map[string]any{"phase": "Ready", "observedGeneration": int64(2), "conditions": []any{conditionMap("Ready", "True", "Success", 0)}})
	badDeletion.Object["metadata"].(map[string]any)["deletionTimestamp"] = "soon"
	for _, test := range []struct {
		name string
		obj  *unstructured.Unstructured
		want string
	}{
		{"unsupported API version", wrongVersion, "RESOURCE_API_UNSUPPORTED"},
		{"deletion in progress", deleting, "RESOURCE_DELETING"},
		{"malformed deletion timestamp", badDeletion, "RESOURCE_METADATA_INVALID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := EvaluateReadiness(test.obj)
			if got.Ready || got.ReasonCode != test.want {
				t.Fatalf("EvaluateReadiness() = %#v, want blocked with %s", got, test.want)
			}
		})
	}
}

func TestKubernetesDeploymentScaledToZeroIsNotAnAvailableDependency(t *testing.T) {
	deployment := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "controller", "generation": int64(2)},
		"spec":     map[string]any{"replicas": int64(0)},
		"status":   map[string]any{"observedGeneration": int64(2), "updatedReplicas": int64(0), "readyReplicas": int64(0), "availableReplicas": int64(0)},
	}}
	if got := EvaluateReadiness(deployment); got.Ready || got.ReasonCode != "DEPLOYMENT_SCALED_TO_ZERO" {
		t.Fatalf("EvaluateReadiness() = %#v, want zero-replica deployment blocked", got)
	}
}

func TestDPFContradictoryAndAggregateConditionsBlockReadiness(t *testing.T) {
	tests := []struct {
		name string
		obj  *unstructured.Unstructured
	}{
		{
			name: "DPUDevice Ready with active Error",
			obj: dpfObject("DPUDevice", map[string]any{"conditions": []any{
				conditionMap("Ready", "True", "Successful", 2), conditionMap("Error", "True", "Failure", 2),
			}}),
		},
		{
			name: "DPUCluster ready phase with failed aggregate condition",
			obj: dpfObject("DPUCluster", map[string]any{"phase": "Ready", "conditions": []any{
				conditionMap("Created", "True", "Success", 0), conditionMap("Ready", "True", "HealthCheckPassed", 0), conditionMap("APIServerReady", "False", "Unavailable", 0),
			}}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := EvaluateReadiness(test.obj); got.Ready {
				t.Fatalf("contradictory status accepted: %#v", got)
			}
		})
	}
}

func TestReadinessPreservesSafeControllerReasonsWithoutMessages(t *testing.T) {
	const privateMessage = "download failed at https://artifact.example/image?token=secret"
	tests := []struct {
		name          string
		conditionType string
		status        string
		reason        string
		wantReason    string
	}{
		{name: "known failure reason", conditionType: "Ready", status: "False", reason: "ReconciliationFailed", wantReason: "ReconciliationFailed"},
		{name: "unknown state reason", conditionType: "Ready", status: "Unknown", reason: "WaitingForFirmware", wantReason: "WaitingForFirmware"},
		{name: "malformed reason omitted", conditionType: "Ready", status: "False", reason: "Failure: https://secret", wantReason: ""},
		{name: "oversize reason omitted", conditionType: "Ready", status: "False", reason: strings.Repeat("x", 129), wantReason: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cond := conditionMap(test.conditionType, test.status, test.reason, 2)
			cond["message"] = privateMessage
			obj := dpfObject("DPUDeployment", map[string]any{"observedGeneration": int64(2), "conditions": []any{cond}})
			got := EvaluateReadiness(obj)
			if got.Ready || got.ReasonCode == "" {
				t.Fatalf("EvaluateReadiness() = %#v, want a blocked result", got)
			}
			if strings.Contains(got.Message, privateMessage) || strings.Contains(got.Message, "token=secret") {
				t.Fatalf("EvaluateReadiness() exposed controller message: %q", got.Message)
			}
			if test.wantReason != "" && !strings.Contains(got.Message, "Controller reason: "+test.wantReason+".") {
				t.Fatalf("EvaluateReadiness() message = %q, want controller reason %q", got.Message, test.wantReason)
			}
			if test.wantReason == "" && strings.Contains(got.Message, "Controller reason:") {
				t.Fatalf("EvaluateReadiness() included malformed controller reason: %q", got.Message)
			}
		})
	}
}

func TestReadinessPreservesReasonsOnActiveErrorAndFailurePhase(t *testing.T) {
	device := dpfObject("DPUDevice", map[string]any{"conditions": []any{
		conditionMap("Ready", "False", "BMCAuthenticationFailed", 2),
		conditionMap("Error", "True", "Success", 2),
	}})
	if got := EvaluateReadiness(device); got.Ready || !strings.Contains(got.Message, "Controller reason: BMCAuthenticationFailed.") {
		t.Fatalf("active Error readiness = %#v, want preserved safe reason", got)
	}
	if strings.Contains(EvaluateReadiness(device).Message, "Controller reason: Success.") {
		t.Fatal("active Error readiness mislabeled the successful condition reason as a failure reason")
	}

	software := dpfObject("BlueFieldSoftware", map[string]any{"phase": "Error", "observedGeneration": int64(2), "conditions": []any{
		conditionMap("Ready", "False", "FirmwareInstallFailed", 2),
	}})
	if got := EvaluateReadiness(software); got.Ready || !strings.Contains(got.Message, "Controller reason: FirmwareInstallFailed.") {
		t.Fatalf("failure phase readiness = %#v, want preserved safe reason", got)
	}
}

func TestReadinessRejectsUnsafeConditionTypesWithoutEchoingThem(t *testing.T) {
	for _, typeName := range []string{"private?token=secret", strings.Repeat("x", 317)} {
		t.Run(typeName[:min(len(typeName), 12)], func(t *testing.T) {
			cluster := dpfObject("DPUCluster", map[string]any{"phase": "Ready", "conditions": []any{
				conditionMap("Created", "True", "Success", 0),
				conditionMap("Ready", "True", "Success", 0),
				conditionMap(typeName, "False", "ControllerFailed", 0),
			}})
			got := EvaluateReadiness(cluster)
			if got.Ready || got.ReasonCode != "RESOURCE_STATUS_INVALID" {
				t.Fatalf("unsafe condition type result = %#v, want invalid status", got)
			}
			if strings.Contains(got.Message, typeName) || strings.Contains(got.Message, "secret") {
				t.Fatalf("unsafe condition type was echoed: %q", got.Message)
			}
		})
	}
}

func TestReadinessAcceptsQualifiedConditionType(t *testing.T) {
	const conditionType = "networking.example.com/ControllerReady"
	cluster := dpfObject("DPUCluster", map[string]any{"phase": "Ready", "conditions": []any{
		conditionMap("Created", "True", "Success", 0),
		conditionMap("Ready", "True", "Success", 0),
		conditionMap(conditionType, "False", "ControllerFailed", 0),
	}})
	got := EvaluateReadiness(cluster)
	if got.Ready || !strings.Contains(got.Message, "DPUCluster "+conditionType) {
		t.Fatalf("qualified condition type result = %#v, want bounded actionable condition name", got)
	}
}

func TestEvaluateReadinessRejectsStaleAndIncompleteStatus(t *testing.T) {
	tests := []struct {
		name       string
		obj        *unstructured.Unstructured
		reasonCode string
	}{
		{
			name:       "stale operator condition",
			obj:        dpfObject("DPFOperatorConfig", map[string]any{"observedGeneration": int64(2), "conditions": []any{conditionMap("Ready", "True", "Success", 1)}}),
			reasonCode: "CONDITION_STATUS_STALE",
		},
		{
			name:       "stale DPU device Ready condition",
			obj:        dpfObject("DPUDevice", map[string]any{"conditions": []any{conditionMap("Ready", "True", "Successful", 1)}}),
			reasonCode: "DEVICE_STATUS_STALE",
		},
		{
			name:       "stale DPU phase",
			obj:        dpfObject("DPU", map[string]any{"phase": "Ready", "observedGeneration": int64(1), "conditions": []any{conditionMap("Ready", "True", "DPUReady", 0)}}),
			reasonCode: "DPU_STATUS_STALE",
		},
		{
			name:       "discovery scan does not match current generation",
			obj:        dpfObject("DPUDiscovery", map[string]any{"observedGeneration": int64(1), "lastScanTime": "2026-11-14T00:00:00Z", "foundDPUs": int64(1)}),
			reasonCode: "DISCOVERY_SCAN_STALE",
		},
		{
			name:       "current scan found no DPU",
			obj:        dpfObject("DPUDiscovery", map[string]any{"observedGeneration": int64(2), "lastScanTime": "2026-11-14T00:00:00Z"}),
			reasonCode: "DISCOVERY_NO_DPUS_FOUND",
		},
		{
			name:       "terminal DPU error phase",
			obj:        dpfObject("DPU", map[string]any{"phase": "Error", "observedGeneration": int64(2), "conditions": []any{conditionMap("Ready", "False", "Failure", 0)}}),
			reasonCode: "RESOURCE_TERMINAL_FAILURE",
		},
		{
			name:       "DPU deployment progress",
			obj:        dpfObject("DPUDeployment", map[string]any{"observedGeneration": int64(2), "conditions": []any{conditionMap("Ready", "Unknown", "Pending", 2)}}),
			reasonCode: "RESOURCE_READINESS_UNKNOWN",
		},
		{
			name: "Kubernetes Deployment rollout incomplete",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"generation": int64(2)},
				"spec": map[string]any{"replicas": int64(2)}, "status": map[string]any{"observedGeneration": int64(2), "updatedReplicas": int64(2), "readyReplicas": int64(1), "availableReplicas": int64(1)},
			}},
			reasonCode: "DEPLOYMENT_ROLLOUT_INCOMPLETE",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := EvaluateReadiness(test.obj)
			if got.Ready || got.ReasonCode != test.reasonCode {
				t.Fatalf("EvaluateReadiness() = %#v, want blocked with %s", got, test.reasonCode)
			}
		})
	}
}

func TestEvaluateReadinessRejectsMalformedAndUnsupportedResources(t *testing.T) {
	badCondition := dpfObject("DPUDevice", map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "Yes"}}})
	duplicateReady := dpfObject("DPUDevice", map[string]any{"conditions": []any{conditionMap("Ready", "True", "Successful", 2), conditionMap("Ready", "True", "Successful", 2)}})
	badTimestamp := dpfObject("DPUDiscovery", map[string]any{"observedGeneration": int64(2), "lastScanTime": "yesterday", "foundDPUs": int64(1)})
	unknown := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "example/v1", "kind": "Unknown"}}
	tests := []struct {
		obj        *unstructured.Unstructured
		reasonCode string
	}{
		{nil, "RESOURCE_UNAVAILABLE"},
		{badCondition, "RESOURCE_STATUS_INVALID"},
		{duplicateReady, "RESOURCE_STATUS_AMBIGUOUS"},
		{badTimestamp, "RESOURCE_STATUS_INVALID"},
		{unknown, "RESOURCE_KIND_UNSUPPORTED"},
	}
	for _, test := range tests {
		got := EvaluateReadiness(test.obj)
		if got.Ready || got.ReasonCode != test.reasonCode {
			t.Errorf("EvaluateReadiness(%v) = %#v, want blocked with %s", test.obj, got, test.reasonCode)
		}
	}
}

func dpfObject(kind string, status map[string]any) *unstructured.Unstructured {
	apiVersion := "provisioning.dpu.nvidia.com/v1alpha1"
	if kind == "DPFOperatorConfig" {
		apiVersion = "operator.dpu.nvidia.com/v1alpha1"
	}
	if kind == "DPUDeployment" {
		apiVersion = "svc.dpu.nvidia.com/v1alpha1"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": "test-resource", "generation": int64(2)},
		"status":   status,
	}}
}

func conditionMap(conditionType, status, reason string, observedGeneration int64) map[string]any {
	condition := map[string]any{"type": conditionType, "status": status, "reason": reason}
	if observedGeneration > 0 {
		condition["observedGeneration"] = observedGeneration
	}
	return condition
}

func TestSoftwareReadinessRequiresCurrentDownloadAndReadyConditions(t *testing.T) {
	for _, conditionType := range []string{"Downloaded", "Ready"} {
		for _, scenario := range []string{"missing", "stale", "false", "future", "duplicate"} {
			t.Run(conditionType+"/"+scenario, func(t *testing.T) {
				conditions := []any{}
				for _, kind := range []string{"Downloaded", "Ready"} {
					if kind == conditionType && scenario == "missing" {
						continue
					}
					value := conditionMap(kind, "True", "Success", 2)
					if kind == conditionType {
						switch scenario {
						case "stale":
							value["observedGeneration"] = int64(1)
						case "future":
							value["observedGeneration"] = int64(3)
						case "false":
							value["status"] = "False"
							value["reason"] = "Reconciling"
						case "duplicate":
							conditions = append(conditions, conditionMap(kind, "True", "Success", 2))
						}
					}
					conditions = append(conditions, value)
				}
				object := dpfObject("BlueFieldSoftware", map[string]any{"phase": "Ready", "observedGeneration": int64(2), "conditions": conditions})
				got := EvaluateReadiness(object)
				if got.Ready || softwareReadiness(object) == nil {
					t.Fatalf("incomplete software accepted: %#v", got)
				}
			})
		}
	}
}
