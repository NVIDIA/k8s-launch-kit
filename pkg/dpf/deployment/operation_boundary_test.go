// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func vacantOperationAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	return newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/configmaps/dpf-op-") {
			t.Errorf("unexpected operation preflight request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: 404})
	})
}
func operationRunInputs(t *testing.T, api *apiFixture) generation.Inputs {
	t.Helper()
	inputs := preflightGenerationInputs(t)
	inputs.Management = api.expected
	inputs.Operator.KubernetesVersion = "v1.35.6"
	return inputs
}

func TestRunInitialReservationStopsBeforeFreshCollectionOrEffects(t *testing.T) {
	const canary = "private-operation-token-kubeconfig-canary"
	var mu sync.Mutex
	var status int
	var reason metav1.StatusReason
	var object *corev1.ConfigMap
	var cancelRead context.CancelFunc
	var operationReads int
	api := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/configmaps/dpf-op-") {
			t.Errorf("unexpected effect/API %s %s", r.Method, r.URL.Path)
		}
		operationReads++
		if cancelRead != nil {
			cancelRead()
		}
		if object != nil {
			_ = json.NewEncoder(w).Encode(object)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: reason, Code: int32(status), Message: canary})
	})
	retained, err := generateFixtureBundle(operationRunInputs(t, api))
	if err != nil {
		t.Fatal(err)
	}
	stage, _ := retained.Stage(generation.StageDeployment)
	deployment := stage.Documents()[0].ObjectCopy()
	for _, tc := range []struct {
		name        string
		object      bool
		sameBundle  bool
		foreign     bool
		status      int
		reason      metav1.StatusReason
		cancel      bool
		want        string
		observation string
		provider    bool
	}{
		{name: "same bundle still occupied", object: true, sameBundle: true, want: "OPERATION_RESERVATION_EXISTS"},
		{name: "different bundle occupied", object: true, want: "OPERATION_RESERVATION_EXISTS"},
		{name: "foreign object occupies name", object: true, foreign: true, want: "OPERATION_RESERVATION_EXISTS"},
		{name: "cold namespace and operator APIs absent", status: 404, reason: metav1.StatusReasonNotFound, provider: true},
		{name: "denied inspection", status: 403, reason: metav1.StatusReasonForbidden, want: "OPERATION_SLOT_UNMEASURED", observation: "API_ACCESS_DENIED"},
		{name: "unavailable inspection", status: 503, reason: metav1.StatusReasonServiceUnavailable, want: "OPERATION_SLOT_UNMEASURED", observation: "API_INSPECTION_FAILED"},
		{name: "unknown reason cannot prove absence", status: 404, reason: metav1.StatusReason(canary), want: "OPERATION_SLOT_UNMEASURED", observation: "API_INSPECTION_FAILED"},
		{name: "cancel during inspection", status: 404, reason: metav1.StatusReasonNotFound, cancel: true, want: "OPERATION_CANCELED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			mu.Lock()
			status, reason = tc.status, tc.reason
			object = nil
			operationReads = 0
			cancelRead = nil
			if tc.object {
				digest := "sha256:" + strings.Repeat("a", 64)
				if tc.sameBundle {
					digest = retained.Index().BundleDigest
				}
				object = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: operationName(deployment), Namespace: deployment.GetNamespace(), UID: activationIntegrationGuardUID, ResourceVersion: "5", Labels: map[string]string{operationLabel: "v1"}}, Data: map[string]string{"checkpoint.json": `{"Binding":{"BundleDigest":"` + digest + `"}}`}}
				if tc.foreign {
					object.Labels = nil
				}
			}
			if tc.cancel {
				cancelRead = cancel
			}
			before, _ := json.Marshal(object)
			mu.Unlock()
			called := false
			request := imagePrerequisiteRunRequest(t, retained, func(context.Context, config.Request, *generation.Plan, PrerequisitePhase) (PrerequisiteVerification, error) {
				called = true
				return PrerequisiteVerification{}, errors.New("stop after cold-core vacancy proof")
			})
			request.Access = api.request
			result, runErr := runWithImageResolver(ctx, request, retainedTestImageResolver)
			mu.Lock()
			after, _ := json.Marshal(object)
			reads := operationReads
			mu.Unlock()
			if runErr == nil || called != tc.provider || reads == 0 || api.writes.Load() != 0 || string(before) != string(after) || result.Operation != nil || result.Activation != nil || len(result.HostNetworking) != 0 {
				t.Fatalf("reservation did not stop/retain state: err=%v provider=%v reads=%d writes=%d", runErr, called, reads, api.writes.Load())
			}
			if tc.cancel && !errors.Is(runErr, context.Canceled) {
				t.Fatalf("cancellation lost: %v", runErr)
			}
			want := tc.want
			if tc.provider {
				want = "OPERATION_RESERVATION_AVAILABLE"
			}
			var checkFound bool
			for _, check := range result.Checks {
				if check.ReasonCode == want {
					checkFound = true
					if !strings.Contains(check.Message, tc.observation) {
						t.Fatalf("missing safe observation: %+v", check)
					}
				}
			}
			if !checkFound {
				t.Fatalf("missing structured reason %s: %+v", want, result.Checks)
			}
			report, err := os.ReadFile(request.ReportPath)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(report), want) || strings.Contains(string(report)+string(encoded)+runErr.Error(), canary) || strings.Contains(string(report)+string(encoded), "private-test-token") {
				t.Fatal("missing operation reason or private diagnostics escaped")
			}
			if !tc.provider && strings.Contains(string(report), "FRESH_PREREQUISITES_STARTED") {
				t.Fatal("initial vacancy failure reached physical discovery")
			}
			probes, err := os.ReadFile(request.ProbeJournalPath)
			if err != nil || !strings.Contains(string(probes), `"receipts":[]`) {
				t.Fatalf("probe effects before vacancy: %s %v", probes, err)
			}
		})
	}
}

func TestOperationReservationDiagnosticsPreserveContextAndHideUntrustedText(t *testing.T) {
	const canary = "private-source-kubeconfig-token-canary"
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		err := operationReservationFailure(context.Background(), "OPERATION_SLOT_UNMEASURED", cause)
		check := operationReservationCheck(nil, err)
		if !errors.Is(err, cause) || check.ReasonCode != "OPERATION_CANCELED" || check.State == "passed" {
			t.Fatalf("context identity lost: %v %+v", err, check)
		}
	}
	for _, err := range []error{
		errors.New(canary),
		&ReconciliationError{ReasonCode: canary, Message: canary, cause: errors.New(canary)},
		&ReconciliationError{ReasonCode: "OPERATION_SLOT_UNMEASURED", Message: canary, cause: errors.New(canary)},
	} {
		check := operationReservationCheck(nil, err)
		encoded, marshalErr := json.Marshal(check)
		if marshalErr != nil || strings.Contains(string(encoded), canary) || check.State == "passed" || check.ReasonCode == "" {
			t.Fatalf("unsafe diagnostic: %+v", check)
		}
	}
}

func TestInitialReservationRechecksManagementIdentityBeforeCoreAbsence(t *testing.T) {
	api := vacantOperationAPIFixture(t)
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
	api.uid.Store("replacement-management")
	err = checkInitialOperationVacancy(context.Background(), client, execution)
	if err == nil || operationReservationCheck(execution, err).ReasonCode != "OPERATION_MANAGEMENT_UNMEASURED" || api.writes.Load() != 0 {
		t.Fatalf("identity drift became vacancy: %v", err)
	}
}
