// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// Exercise the native Run→API client→saved checkpoint→shared bootstrap wait.
// Once bootstrap is restored, a deliberate tenant-read denial stops the test at
// the next real boundary: it does not invent a passing prerequisite provider.
func TestRunResumeRestoresPendingBootstrapBeforeTenantOrEffects(t *testing.T) {
	const canary = "private-bootstrap-api-token-canary"
	var mu sync.Mutex
	var scenario string
	var operation *corev1.ConfigMap
	var objects map[string]*unstructured.Unstructured
	var configReads, mutations int
	var cancelRead context.CancelFunc
	api := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		deny := func() {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: 403, Message: canary})
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/selfsubjectaccessreviews") {
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview", "status": map[string]any{"allowed": scenario != "permission denied"}})
			return
		}
		if r.Method != http.MethodGet {
			mutations++
			t.Errorf("unexpected persistent effect %s %s", r.Method, r.URL.Path)
			deny()
			return
		}
		if strings.Contains(r.URL.Path, "/configmaps/dpf-op-") {
			if scenario == "operation read denied" {
				deny()
			} else {
				_ = json.NewEncoder(w).Encode(operation)
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/apis/operator.dpu.nvidia.com/v1alpha1") {
			fmt.Fprint(w, `{"kind":"APIResourceList","groupVersion":"operator.dpu.nvidia.com/v1alpha1","resources":[{"name":"dpfoperatorconfigs","kind":"DPFOperatorConfig","namespaced":true}]}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/apis/apps/v1") {
			fmt.Fprint(w, `{"kind":"APIResourceList","groupVersion":"apps/v1","resources":[{"name":"deployments","kind":"Deployment","namespaced":true}]}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/apis/provisioning.dpu.nvidia.com/v1alpha1") {
			fmt.Fprint(w, `{"kind":"APIResourceList","groupVersion":"provisioning.dpu.nvidia.com/v1alpha1","resources":[{"name":"dpuclusters","kind":"DPUCluster","namespaced":true}]}`)
			return
		}
		for _, o := range objects {
			if o.GetKind() == "Namespace" && r.URL.Path != "/prefix/api/v1/namespaces/"+o.GetName() {
				continue
			}
			if !strings.HasSuffix(r.URL.Path, "/"+o.GetName()) {
				continue
			}
			current := o.DeepCopy()
			if configReads >= 4 && (scenario == "manager access revoked" && o.GetKind() == "Deployment" || scenario == "namespace access revoked" && o.GetKind() == "Namespace") {
				deny()
				return
			}
			if o.GetKind() == "DPFOperatorConfig" {
				configReads++
				if scenario != "already ready" && configReads <= 3 {
					setBootstrapTestReadiness(t, current, "False", "Error")
				} else {
					setBootstrapTestReadiness(t, current, "True", "Success")
				}
				switch scenario {
				case "config UID drift":
					if configReads >= 3 {
						current.SetUID("replacement-config")
					}
				case "config spec drift":
					if configReads >= 3 {
						_ = unstructured.SetNestedField(current.Object, int64(1400), "spec", "networking", "controlPlaneMTU")
					}
				case "config version drift":
					if configReads >= 3 {
						_ = unstructured.SetNestedField(current.Object, "v0.0.1", "status", "version")
					}
				case "config access revoked":
					if configReads >= 3 {
						deny()
						return
					}
				case "cancel during wait":
					if configReads == 3 {
						cancelRead()
					}
				}
			}
			if configReads >= 4 && (scenario == "manager UID drift" && o.GetKind() == "Deployment" || scenario == "namespace UID drift" && o.GetKind() == "Namespace") {
				current.SetUID("replacement-bootstrap")
			}
			_ = json.NewEncoder(w).Encode(current)
			return
		}
		// Reaching this selected DPUCluster read proves full bootstrap restoration.
		if strings.Contains(r.URL.Path, "/dpuclusters/") {
			deny()
			return
		}
		t.Errorf("unexpected read %s", r.URL.Path)
		deny()
	})
	retained, err := generateFixtureBundle(operationRunInputs(t, api))
	if err != nil {
		t.Fatal(err)
	}
	execution, err := prepareManagementExecution(retained, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := retainedDeploymentObject(execution.verificationInputs())
	if err != nil {
		t.Fatal(err)
	}
	_, targets := enrollmentBMCInputs(t)
	targets = targets[:1]
	targets[0].Endpoint = "https://192.0.2.20"
	for _, tc := range []struct {
		name, reason, detail string
		waited, tenant       bool
	}{
		{"already ready", "OPERATION_RESUME_TENANT_UNVERIFIED", "", false, true},
		{"pending then ready", "OPERATION_RESUME_TENANT_UNVERIFIED", "", true, true},
		{"client unavailable", "OPERATION_RESUME_MANAGEMENT_UNVERIFIED", "", false, false},
		{"permission denied", "OPERATION_PERMISSION_DENIED", "", false, false},
		{"operation read denied", "OPERATION_RESUME_RECORD_UNAVAILABLE", "API_ACCESS_DENIED", false, false},
		{"BMC access missing", "OPERATION_RESUME_ENROLLMENT_UNVERIFIED", "", false, false},
		{"config UID drift", "DPF_CONFIG_INTENT_CHANGED", "", true, false},
		{"config spec drift", "DPF_CONFIG_INTENT_CHANGED", "", true, false},
		{"config version drift", "DPF_CONFIG_VERSION_MISMATCH", "", true, false},
		{"config access revoked", "DPF_CONFIG_READ_FAILED", "API_ACCESS_DENIED", true, false},
		{"manager UID drift", "BOOTSTRAP_MANAGER_CHANGED", "", true, false},
		{"namespace UID drift", "BOOTSTRAP_NAMESPACE_CHANGED", "", true, false},
		{"cancel during wait", "DPF_CONFIG_READINESS_CANCELED", "", true, false},
		{"manager access revoked", "BOOTSTRAP_MANAGER_READ_FAILED", "API_ACCESS_DENIED", true, false},
		{"namespace access revoked", "BOOTSTRAP_NAMESPACE_READ_FAILED", "API_ACCESS_DENIED", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newTenantStageFake(t, execution)
			receipt := tenantStageBootstrapReceipt(execution, base)
			state := operationState{Binding: operationBinding{Version: 1, ID: "saved-operation", BundleDigest: execution.plan.Digest(), Management: api.expected, NamespaceUID: receipt.NamespaceUIDs[execution.operatorNamespace], DeploymentName: desired.GetName(), CohortDigest: inventory.Digest("dpf-operation-cohort-v1", execution.plan.Intent().Devices)}, Management: receipt, TenantUID: "tenant-one", TenantSystemUID: "tenant-system", TenantSources: map[string]types.UID{"Secret/tenant/source": "source-one"}, Enrollment: enrollmentStageReceipt{BundleDigest: execution.plan.Digest(), TenantUID: "tenant-one", Secret: enrollmentSecretReceipt{UID: "secret-one"}, Devices: []enrolledDeviceReceipt{{Serial: "SERIAL-A", DeviceUID: "device-one", DPUNodeUID: "node-one", BMCIP: "192.0.2.20", BMCPort: 443}}}, Software: softwareVerificationReceipt{SoftwareUID: "software-one"}, Workload: []workloadResourceReceipt{{UID: "workload-one"}}, Children: map[string]activationObjectReceipt{}, HoldIntents: map[string]operationProgress{}, Progress: map[string]operationProgress{}, Resets: map[string]operationReset{}, Hosts: map[string]operationHost{}}
			data, err := encodeOperation(state)
			if err != nil {
				t.Fatal(err)
			}
			op := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: operationName(desired), Namespace: desired.GetNamespace(), UID: activationIntegrationGuardUID, ResourceVersion: "1", Labels: map[string]string{operationLabel: "v1"}}, Data: map[string]string{"checkpoint.json": data}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var progress bytes.Buffer
			ctx = ui.WithOutput(ctx, ui.NewWithWriter(&progress))
			mu.Lock()
			scenario = tc.name
			operation = op.DeepCopy()
			objects = base.resources
			configReads = 0
			mutations = 0
			cancelRead = cancel
			mu.Unlock()
			provider := false
			request := imagePrerequisiteRunRequest(t, retained, func(context.Context, config.Request, *generation.Plan, PrerequisitePhase) (PrerequisiteVerification, error) {
				provider = true
				return PrerequisiteVerification{}, errors.New("must not reach fresh provider")
			})
			request.Access = api.request
			request.Access.Targets = targets
			request.PollInterval = time.Millisecond
			request.Resume = &OperationRef{Namespace: op.Namespace, Name: op.Name, UID: op.UID}
			if tc.name == "client unavailable" {
				request.Access.Kubeconfig = filepath.Join(t.TempDir(), "absent")
			}
			if tc.name == "BMC access missing" {
				request.Access.Targets = nil
			}
			result, runErr := runWithTransports(ctx, request, runTransports{resolveImage: retainedTestImageResolver})
			mu.Lock()
			after, _ := json.Marshal(operation)
			before, _ := json.Marshal(op)
			writes := mutations
			mu.Unlock()
			if runErr == nil || provider || writes != 0 || !bytes.Equal(before, after) || result.Activation != nil || len(result.HostNetworking) != 0 {
				t.Fatalf("resume crossed failure boundary: err=%v provider=%v writes=%d", runErr, provider, writes)
			}
			if tc.name == "cancel during wait" && !errors.Is(runErr, context.Canceled) {
				t.Fatal("cancellation identity lost")
			}
			raw, err := os.ReadFile(request.ReportPath)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, check := range result.Checks {
				if check.ReasonCode == tc.reason {
					found = true
					if check.Subject != op.Namespace+"/"+op.Name || !strings.Contains(check.Message, tc.detail) {
						t.Fatalf("incorrect finite diagnostic: %+v", check)
					}
				}
			}
			if !found || !strings.Contains(string(raw), tc.reason) || !strings.Contains(progress.String(), tc.reason) || !strings.Contains(progress.String(), op.Name) {
				t.Fatalf("missing result/journal/UI reason: %v", runErr)
			}
			if strings.Contains(string(raw)+string(encoded)+progress.String()+runErr.Error(), canary) || strings.Contains(string(raw)+string(encoded)+progress.String(), "credential-canary") {
				t.Fatal("private cause or credential leaked")
			}
			if strings.Contains(string(raw), "DPF_CONFIG_READINESS_STARTED") != tc.waited || strings.Contains(string(raw), "TENANT_READINESS_STARTED") != tc.tenant {
				t.Fatalf("wrong wait/tenant path: %s", raw)
			}
			if tc.tenant && !strings.Contains(string(raw), "DPF_CONFIG_READY") && tc.waited {
				t.Fatal("pending bootstrap was bypassed instead of waited")
			}
		})
	}
}
