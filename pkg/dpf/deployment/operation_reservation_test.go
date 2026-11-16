// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
)

type reservationTenantReader struct {
	tenantSourceStub
	err error
}

func (r *reservationTenantReader) observeTenantSources(ctx context.Context, expected TenantReadinessExpected) (tenantSources, error) {
	if r.err != nil {
		return tenantSources{}, r.err
	}
	return r.tenantSourceStub.observeTenantSources(ctx, expected)
}
func reservationWorkload(t *testing.T, execution *managementExecution) (workloadPreparationReceipt, *reservationTenantReader) {
	t.Helper()
	expected, sources := tenantSourceFixture(t)
	reader := &reservationTenantReader{tenantSourceStub: tenantSourceStub{sources: sources}}
	digest := execution.plan.Digest()
	namespace := execution.plan.Intent().Namespace
	tenantUID := types.UID(sources.observed.DPUCluster.GetUID())
	return workloadPreparationReceipt{BundleDigest: digest, ControlPlane: controlPlaneReceipts{
		management: managementBootstrapReceipt{BundleDigest: digest, NamespaceUIDs: map[string]string{namespace: "namespace-one"}},
		tenant:     tenantStageReceipt{ClusterUID: tenantUID, Client: &tenantClient{source: reader, expected: expected, systemUID: "tenant-system-one"}},
		enrollment: enrollmentStageReceipt{BundleDigest: digest, TenantUID: tenantUID, Secret: enrollmentSecretReceipt{UID: "bmc-secret-one"}, Devices: []enrolledDeviceReceipt{{Serial: "SERIAL-A", DeviceUID: "device-one", DPUNodeUID: "node-one"}}},
	}, Software: softwareVerificationReceipt{SoftwareUID: "software-one"}, Resources: []workloadResourceReceipt{{Kind: "DPUFlavor", Namespace: namespace, Name: "flavor-one", UID: "flavor-one", Generation: 1}}}, reader
}

func TestReservationFailuresRetainSafeReasonWithoutAdoption(t *testing.T) {
	const canary = "private-reservation-secret-error-canary"
	var mu sync.Mutex
	var scenario string
	var creates int
	var saved *corev1.ConfigMap
	api := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fail := func(code int, reason metav1.StatusReason) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Code: int32(code), Reason: reason, Message: canary})
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/prefix/apis/svc.dpu.nvidia.com/v1alpha1":
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "APIResourceList", "groupVersion": "svc.dpu.nvidia.com/v1alpha1", "resources": []any{map[string]any{"name": "dpudeployments", "kind": "DPUDeployment", "namespaced": true}}})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/dpudeployments/"):
			switch scenario {
			case "parent exists":
				_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": "DPUDeployment", "metadata": map[string]any{"name": "existing"}})
			case "parent denied":
				fail(403, metav1.StatusReasonForbidden)
			case "parent unknown 404":
				fail(404, metav1.StatusReason(canary))
			default:
				fail(404, metav1.StatusReasonNotFound)
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/configmaps"):
			creates++
			var desired corev1.ConfigMap
			data, readErr := io.ReadAll(r.Body)
			if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(data, nil, &desired); err != nil || readErr != nil {
				t.Errorf("decode create: %v", err)
			}
			desired.UID = activationIntegrationGuardUID
			desired.ResourceVersion = "1"
			switch scenario {
			case "create denied":
				fail(403, metav1.StatusReasonForbidden)
			case "unknown create outcome":
				fail(500, metav1.StatusReasonInternalError)
			case "lost create response":
				saved = desired.DeepCopy()
				fail(500, metav1.StatusReasonInternalError)
			case "late collision":
				saved = desired.DeepCopy()
				saved.Data = map[string]string{"checkpoint.json": "older checkpoint"}
				fail(409, metav1.StatusReasonAlreadyExists)
			case "invalid create response":
				desired.UID = "invalid"
				saved = desired.DeepCopy()
				_ = json.NewEncoder(w).Encode(&desired)
			default:
				saved = desired.DeepCopy()
				_ = json.NewEncoder(w).Encode(&desired)
			}
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/configmaps/"):
			if saved != nil {
				_ = json.NewEncoder(w).Encode(saved)
			} else {
				fail(404, metav1.StatusReasonNotFound)
			}
		default:
			t.Errorf("unexpected reservation effect/read %s %s", r.Method, r.URL.Path)
			fail(500, metav1.StatusReasonInternalError)
		}
	})
	retained, err := generateFixtureBundle(operationRunInputs(t, api))
	if err != nil {
		t.Fatal(err)
	}
	execution, err := prepareManagementExecution(retained, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewAPIClient(context.Background(), api.request, api.expected, "v1.35.6")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, want, observation string
		creates                 int
	}{
		{name: "parent exists", want: "OPERATION_PARENT_EXISTS"},
		{name: "parent denied", want: "OPERATION_PARENT_UNMEASURED", observation: "API_ACCESS_DENIED"},
		{name: "parent unknown 404", want: "OPERATION_PARENT_UNMEASURED", observation: "API_INSPECTION_FAILED"},
		{name: "plan invalid", want: "OPERATION_PLAN_INVALID"},
		{name: "tenant sources missing", want: "OPERATION_TENANT_SOURCES_UNMEASURED", observation: "API_INSPECTION_FAILED"},
		{name: "tenant source UID missing", want: "OPERATION_TENANT_SOURCES_UNMEASURED"},
		{name: "checkpoint incomplete", want: "OPERATION_CHECKPOINT_INVALID"},
		{name: "checkpoint oversized", want: "OPERATION_CHECKPOINT_INVALID"},
		{name: "canceled", want: "OPERATION_CANCELED"},
		{name: "create denied", want: "OPERATION_RESERVATION_UNCERTAIN", observation: "API_ACCESS_DENIED", creates: 1},
		{name: "unknown create outcome", want: "OPERATION_RESERVATION_UNCERTAIN", observation: "API_INSPECTION_FAILED", creates: 1},
		{name: "lost create response", want: "OPERATION_RESERVATION_EXISTS", creates: 1},
		{name: "late collision", want: "OPERATION_RESERVATION_EXISTS", creates: 1},
		{name: "invalid create response", want: "OPERATION_RESERVATION_UNCERTAIN", creates: 1},
		{name: "successful reservation", creates: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			scenario = tc.name
			creates = 0
			saved = nil
			mu.Unlock()
			workload, reader := reservationWorkload(t, execution)
			prepared := &preparedControlPlaneRun{execution: execution}
			if tc.name == "plan invalid" {
				prepared.execution = nil
			}
			marker := errors.New(canary)
			switch tc.name {
			case "tenant sources missing":
				reader.err = marker
			case "tenant source UID missing":
				reader.sources.observed.Service.SetUID("")
			case "checkpoint incomplete":
				workload.ControlPlane.enrollment.Secret.UID = ""
			case "checkpoint oversized":
				workload.Resources[0].Name = strings.Repeat("x", maxOperationBytes)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if tc.name == "canceled" {
				cancel()
			}
			if tc.name == "late collision" {
				if err := checkInitialOperationVacancy(ctx, client, execution); err != nil {
					t.Fatalf("initial slot was not vacant: %v", err)
				}
			}
			op, runErr := reserveDeploymentOperation(ctx, client, prepared, workload)
			mu.Lock()
			count := creates
			current := saved.DeepCopy()
			mu.Unlock()
			if count != tc.creates {
				t.Fatalf("creates=%d want%d", count, tc.creates)
			}
			if tc.want == "" {
				if runErr != nil || op == nil || op.object.UID != current.UID {
					t.Fatalf("reservation not returned: %v", runErr)
				}
				if _, err := decodeOperation(current.Data["checkpoint.json"]); err != nil {
					t.Fatal(err)
				}
				return
			}
			if runErr == nil || op != nil || !strings.Contains(runErr.Error(), tc.want) {
				t.Fatalf("wrong reservation result: op=%v err=%v", op, runErr)
			}
			if tc.name == "tenant sources missing" && !errors.Is(runErr, marker) {
				t.Fatal("private cause chain lost")
			}
			if tc.name == "canceled" && !errors.Is(runErr, context.Canceled) {
				t.Fatal("cancellation lost")
			}
			check := operationReservationCheck(execution, runErr)
			if check.ReasonCode != tc.want || !strings.Contains(check.Message, tc.observation) {
				t.Fatalf("safe check=%+v", check)
			}
			reportPath := filepath.Join(t.TempDir(), "operation.jsonl")
			journal, err := newExecutionJournal(reportPath, execution.plan.Digest())
			if err != nil {
				t.Fatal(err)
			}
			if err := reportOperationReservation(ctx, journal, check); err != nil {
				t.Fatal(err)
			}
			result := RunResult{Checks: []inventory.Check{check}}
			returned := finishRunReport(&result, runErr, func() error { return nil }, journal, execution.plan.Intent().Namespace)
			data, err := os.ReadFile(reportPath)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if returned == nil || !strings.Contains(string(data), tc.want) || !strings.Contains(string(encoded), tc.want) || strings.Contains(string(data)+string(encoded)+runErr.Error(), canary) {
				t.Fatal("finite reason missing or cause exposed")
			}
			mu.Lock()
			after, _ := json.Marshal(saved)
			mu.Unlock()
			before, _ := json.Marshal(current)
			if string(before) != string(after) {
				t.Fatal("reporting changed reservation")
			}
		})
	}
}
