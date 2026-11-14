// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestValidateEnrolledDeviceBindsSelectedIdentityAndResetPolicy(t *testing.T) {
	selected := enrollmentTestBMC()
	for _, tc := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
		wantOK bool
	}{
		{name: "exact spec before controller status", wantOK: true},
		{name: "matching optional status", mutate: func(object *unstructured.Unstructured) { setDeviceStatus(object) }, wantOK: true},
		{name: "wrong serial", mutate: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, "SERIAL-B", "spec", "serialNumber")
		}},
		{name: "wrong endpoint", mutate: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, "192.0.2.99", "spec", "bmcIp")
		}},
		{name: "reset policy omitted", mutate: func(object *unstructured.Unstructured) {
			unstructured.RemoveNestedField(object.Object, "spec", "bmcFactoryResetPolicy")
		}},
		{name: "factory reset enabled", mutate: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, "OnInitialization", "spec", "bmcFactoryResetPolicy")
		}},
		{name: "per device credential", mutate: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, "device-secret", "spec", "bmcCredentialSecretName")
		}},
		{name: "skip hardware provisioning", mutate: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, "true", "metadata", "labels", "provisioning.dpu.nvidia.com/skip-hw-provisioning")
		}},
		{name: "status serial conflict", mutate: func(object *unstructured.Unstructured) {
			setDeviceStatus(object)
			_ = unstructured.SetNestedField(object.Object, "SERIAL-B", "status", "serialNumber")
		}},
		{name: "status PSID conflict", mutate: func(object *unstructured.Unstructured) {
			setDeviceStatus(object)
			_ = unstructured.SetNestedField(object.Object, "OTHER-PSID", "status", "psid")
		}},
		{name: "wrong known DPU type", mutate: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, "BlueField3", "status", "dpuType")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := enrolledDeviceFixture()
			if tc.mutate != nil {
				tc.mutate(object)
			}
			err := validateEnrolledDevice(object, selected, "operator", false)
			if (err == nil) != tc.wantOK {
				t.Fatalf("validateEnrolledDevice() error=%v, wantOK=%t", err, tc.wantOK)
			}
		})
	}
	object := enrolledDeviceFixture()
	if err := validateEnrolledDevice(object, selected, "operator", true); err == nil {
		t.Fatal("requireStatus accepted missing controller identity")
	}
	setDeviceStatus(object)
	if err := validateEnrolledDevice(object, selected, "operator", true); err != nil {
		t.Fatalf("requireStatus rejected exact controller identity: %v", err)
	}
}

func TestEnrolledDeviceReadinessRequiresCurrentDPF26ConditionsAndBF4Status(t *testing.T) {
	ready := readyEnrolledDeviceFixture()
	if got := enrolledDeviceReadiness(ready); !got.Ready {
		t.Fatalf("exact DPF 26.8 readiness fixture rejected: %#v", got)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
	}{
		{name: "factory reset reason", mutate: func(object *unstructured.Unstructured) {
			setConditionStatus(object, "BMCFactoryResetReady", "True", "FactoryResetCompleted", 1)
		}},
		{name: "stale credentials", mutate: func(object *unstructured.Unstructured) {
			setConditionStatus(object, "BMCCredentialsReady", "True", "Ready", 0)
		}},
		{name: "missing server certificate", mutate: func(object *unstructured.Unstructured) { removeCondition(object, "BMCServerCertificateReady") }},
		{name: "false CA trust", mutate: func(object *unstructured.Unstructured) {
			setConditionStatus(object, "CATrustBundleReady", "False", "Pending", 1)
		}},
		{name: "missing PSID", mutate: func(object *unstructured.Unstructured) {
			unstructured.RemoveNestedField(object.Object, "status", "psid")
		}},
		{name: "wrong BF type", mutate: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, "BlueField3", "status", "dpuType")
		}},
		{name: "active error", mutate: func(object *unstructured.Unstructured) {
			appendCondition(object, conditionMap("Error", "True", "InitializationFailed", 1))
		}},
		{name: "skip hardware provisioning", mutate: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(object.Object, "true", "metadata", "labels", "provisioning.dpu.nvidia.com/skip-hw-provisioning")
		}},
		{name: "duplicate readiness", mutate: func(object *unstructured.Unstructured) {
			appendCondition(object, conditionMap("Ready", "True", "Ready", 1))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := readyEnrolledDeviceFixture()
			tc.mutate(object)
			if got := enrolledDeviceReadiness(object); got.Ready {
				t.Fatalf("invalid readiness accepted: %#v", got)
			}
		})
	}
}

func TestValidateEnrolledNodeUsesExactAssociationWithoutInventedGeneration(t *testing.T) {
	selected := enrollmentTestBMC()
	object := enrolledNodeFixture()
	if err := validateEnrolledNode(object, selected, "operator"); err != nil {
		t.Fatalf("exact DPUNode rejected: %v", err)
	}
	// The pinned DPUNode condition writer omits observedGeneration. A current
	// Ready=True plus exact object UID/spec is the available source contract.
	if got := enrolledNodeReadiness(object); !got.Ready {
		t.Fatalf("Ready without observedGeneration was rejected: %#v", got)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
	}{
		{name: "wrong reference", mutate: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(object.Object, []any{map[string]any{"name": "serial-b"}}, "spec", "dpus")
		}},
		{name: "multiple devices", mutate: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(object.Object, []any{map[string]any{"name": "serial-a"}, map[string]any{"name": "serial-b"}}, "spec", "dpus")
		}},
		{name: "false Ready", mutate: func(object *unstructured.Unstructured) { setNodeCondition(object, "Ready", "False") }},
		{name: "invalid details active", mutate: func(object *unstructured.Unstructured) { appendNodeCondition(object, "InvalidDPUDetails", "True") }},
		{name: "node effect active", mutate: func(object *unstructured.Unstructured) {
			appendNodeCondition(object, "DPUNodeNodeEffectInProgress", "True")
		}},
		{name: "unknown node effect", mutate: func(object *unstructured.Unstructured) {
			appendNodeCondition(object, "DPUNodeNodeEffectInProgress", "Unknown")
		}},
		{name: "script reboot method", mutate: func(object *unstructured.Unstructured) {
			_ = unstructured.SetNestedMap(object.Object, map[string]any{"script": map[string]any{"name": "reboot"}}, "spec", "nodeRebootMethod")
		}},
		{name: "implicit reboot method default", mutate: func(object *unstructured.Unstructured) {
			unstructured.RemoveNestedField(object.Object, "spec", "nodeRebootMethod")
		}},
		{name: "duplicate Ready", mutate: func(object *unstructured.Unstructured) { appendNodeCondition(object, "Ready", "True") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := enrolledNodeFixture()
			tc.mutate(mutated)
			if tc.name == "wrong reference" || tc.name == "multiple devices" || tc.name == "script reboot method" || tc.name == "implicit reboot method default" {
				if err := validateEnrolledNode(mutated, selected, "operator"); err == nil {
					t.Fatal("invalid DPUNode association accepted")
				}
				return
			}
			if got := enrolledNodeReadiness(mutated); got.Ready {
				t.Fatalf("invalid DPUNode readiness accepted: %#v", got)
			}
		})
	}
}

func TestEnrolledNodeInvalidDetailsIsTerminalBeforeReady(t *testing.T) {
	for _, tc := range []struct {
		name       string
		readyState string
		invalid    string
		wantCode   string
	}{
		{name: "missing Ready", invalid: "True", wantCode: "RESOURCE_TERMINAL_FAILURE"},
		{name: "False Ready", readyState: "False", invalid: "True", wantCode: "RESOURCE_TERMINAL_FAILURE"},
		{name: "Unknown invalid details", readyState: "True", invalid: "Unknown", wantCode: "RESOURCE_READINESS_UNKNOWN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := enrolledNodeFixture()
			if tc.readyState == "" {
				unstructured.RemoveNestedField(object.Object, "status", "conditions")
			} else {
				setNodeCondition(object, "Ready", tc.readyState)
			}
			conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
			conditions = append(conditions, map[string]any{
				"type": "InvalidDPUDetails", "status": tc.invalid,
				"reason": "InvalidDPUDetails", "message": "remote text must not be exposed",
			})
			_ = unstructured.SetNestedSlice(object.Object, conditions, "status", "conditions")

			got := enrolledNodeReadiness(object)
			if got.Ready || got.ReasonCode != tc.wantCode {
				t.Fatalf("enrolledNodeReadiness() = %#v, want blocked reason %q", got, tc.wantCode)
			}
			if tc.invalid == "True" && !strings.Contains(got.Message, "Controller reason: InvalidDPUDetails.") {
				t.Fatalf("terminal failure did not retain safe controller reason: %q", got.Message)
			}
			if strings.Contains(got.Message, "remote text") {
				t.Fatalf("controller message leaked into readiness: %q", got.Message)
			}
		})
	}
}

func enrollmentTestBMC() enrollmentBMC {
	return enrollmentBMC{device: generation.ResolvedDevice{Serial: "SERIAL-A", BMCIP: "192.0.2.20", BMCPort: 443, PSID: "MT_0000001774"}}
}

func enrolledDeviceFixture() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": enrollmentDeviceAPIVersion, "kind": "DPUDevice",
		"metadata": map[string]any{"name": "serial-a", "namespace": "operator", "uid": "device-uid", "resourceVersion": "1", "generation": int64(1)},
		"spec":     map[string]any{"serialNumber": "SERIAL-A", "bmcIp": "192.0.2.20", "bmcPort": int64(443), "bmcFactoryResetPolicy": "Never"},
	}}
}

func readyEnrolledDeviceFixture() *unstructured.Unstructured {
	object := enrolledDeviceFixture()
	setDeviceStatus(object)
	conditions := []any{}
	for _, kind := range []string{"Initialized", "Discovered", "Ready", "BMCCredentialsReady", "BMCServerCertificateReady", "CATrustBundleReady"} {
		conditions = append(conditions, conditionMap(kind, "True", "Ready", 1))
	}
	conditions = append(conditions, conditionMap("BMCFactoryResetReady", "True", "FactoryResetSkipped", 1))
	_ = unstructured.SetNestedSlice(object.Object, conditions, "status", "conditions")
	return object
}

func setDeviceStatus(object *unstructured.Unstructured) {
	_ = unstructured.SetNestedField(object.Object, "SERIAL-A", "status", "serialNumber")
	_ = unstructured.SetNestedField(object.Object, "192.0.2.20", "status", "bmcIp")
	_ = unstructured.SetNestedField(object.Object, int64(443), "status", "bmcPort")
	_ = unstructured.SetNestedField(object.Object, "MT_0000001774", "status", "psid")
	_ = unstructured.SetNestedField(object.Object, "BlueField4", "status", "dpuType")
}

func setConditionStatus(object *unstructured.Unstructured, kind, status, reason string, observed int64) {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for i, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if ok && condition["type"] == kind {
			conditions[i] = conditionMap(kind, status, reason, observed)
		}
	}
	_ = unstructured.SetNestedSlice(object.Object, conditions, "status", "conditions")
}

func removeCondition(object *unstructured.Unstructured, kind string) {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	filtered := make([]any, 0, len(conditions))
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] != kind {
			filtered = append(filtered, raw)
		}
	}
	_ = unstructured.SetNestedSlice(object.Object, filtered, "status", "conditions")
}

func appendCondition(object *unstructured.Unstructured, condition map[string]any) {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	conditions = append(conditions, condition)
	_ = unstructured.SetNestedSlice(object.Object, conditions, "status", "conditions")
}

func enrolledNodeFixture() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": enrollmentDeviceAPIVersion, "kind": "DPUNode",
		"metadata": map[string]any{"name": "dpu-node-serial-a", "namespace": "operator", "uid": "node-uid", "resourceVersion": "1", "generation": int64(4)},
		"spec":     map[string]any{"dpus": []any{map[string]any{"name": "serial-a"}}, "nodeRebootMethod": map[string]any{"external": map[string]any{}}},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "Ready"}}},
	}}
}

func setNodeCondition(object *unstructured.Unstructured, kind, status string) {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for i, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if ok && condition["type"] == kind {
			conditions[i] = map[string]any{"type": kind, "status": status, "reason": status}
		}
	}
	_ = unstructured.SetNestedSlice(object.Object, conditions, "status", "conditions")
}

func appendNodeCondition(object *unstructured.Unstructured, kind, status string) {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	conditions = append(conditions, map[string]any{"type": kind, "status": status, "reason": status})
	_ = unstructured.SetNestedSlice(object.Object, conditions, "status", "conditions")
}

func TestEnrolledIdentityFailuresStayBounded(t *testing.T) {
	selected := enrollmentTestBMC()
	selected.device.BMCIP = "not-an-ip"
	if err := validateEnrolledDevice(enrolledDeviceFixture(), selected, "operator", false); err == nil {
		t.Fatal("invalid selected endpoint accepted")
	}
	for _, object := range []*unstructured.Unstructured{nil, {Object: map[string]any{"metadata": map[string]any{"name": "dpu-device"}}}} {
		if got := enrolledDeviceReadiness(object); got.Ready {
			t.Fatalf("malformed identity accepted: %#v", got)
		}
	}
	if got := enrolledNodeReadiness(nil); got.Ready {
		t.Fatal("missing DPUNode accepted")
	}
}
