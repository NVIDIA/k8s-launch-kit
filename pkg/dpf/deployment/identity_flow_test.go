// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type identityManagementRunner struct {
	*managementFake
	failAt, calls int
	cause         error
}

func (c *identityManagementRunner) VerifyIdentity(context.Context) error {
	c.calls++
	if c.calls == c.failAt {
		return c.cause
	}
	return nil
}

func TestManagementIdentityJournalAtEveryBootstrapBoundary(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for boundary := 1; boundary <= 7; boundary++ {
		for _, scenario := range []string{"cancel", "deadline", "unavailable", "drift"} {
			t.Run(fmt.Sprintf("%d/%s", boundary, scenario), func(t *testing.T) {
				code, cause := identityFlowCause(scenario)
				client := &identityManagementRunner{managementFake: managementFakeWithReadiness(t, execution.plan.Digest(), execution.manager, execution.crds), failAt: boundary, cause: cause}
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				journal := testJournal(execution)
				_, err := runPreparedManagement(ctx, client, execution, journal, time.Millisecond)
				text := journal.file.(*managementMemoryFile).String()
				if err == nil || !identityFlowCausePreserved(err, cause) || !strings.Contains(text, `"reasonCode":"`+code+`"`) || strings.Contains(text+err.Error(), "private-identity-canary") {
					t.Fatalf("calls=%d err=%v journal=%s", client.calls, err, text)
				}
				if client.calls != boundary {
					t.Fatalf("continued after identity failure: %d", client.calls)
				}
				wantHelm := 0
				if boundary >= 4 {
					wantHelm = 1
				}
				helm := 0
				for _, event := range client.events {
					if event == "helm" {
						helm++
					}
				}
				wantConfig := 0
				if boundary >= 6 {
					wantConfig = 1
				}
				if helm != wantHelm || client.createConfig != wantConfig {
					t.Fatalf("effects crossed boundary: helm=%d config=%d", helm, client.createConfig)
				}
				var events []executionEvent
				for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
					var event executionEvent
					if err := json.Unmarshal([]byte(line), &event); err != nil {
						t.Fatal(err)
					}
					events = append(events, event)
				}
				wantState := "failed"
				if boundary == 2 || boundary == 4 || boundary == 5 || boundary == 6 {
					wantState = "uncertain"
				}
				if events[len(events)-1].State != wantState {
					t.Fatalf("uncertainty changed: %+v", events[len(events)-1])
				}
			})
		}
	}
}

func identityFlowCause(scenario string) (string, error) {
	switch scenario {
	case "cancel":
		return "MANAGEMENT_IDENTITY_CANCELED", context.Canceled
	case "deadline":
		return "MANAGEMENT_IDENTITY_CANCELED", context.DeadlineExceeded
	case "drift":
		return "MANAGEMENT_IDENTITY_CHANGED", errors.Join(errManagementIdentityChanged, errors.New("private-identity-canary"))
	case "not found":
		return "MANAGEMENT_IDENTITY_UNVERIFIED", apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "private-identity-canary")
	case "conflict":
		return "MANAGEMENT_IDENTITY_UNVERIFIED", apierrors.NewConflict(schema.GroupResource{Resource: "namespaces"}, "private-identity-canary", errors.New("private-identity-canary"))
	default:
		return "MANAGEMENT_IDENTITY_UNVERIFIED", apierrors.NewServiceUnavailable("private-identity-canary")
	}
}

func TestEnrollmentIdentityCausesCannotBecomeAbsentCRDsOrPendingNodes(t *testing.T) {
	execution, _, selected := enrollmentScopeFixture(t)
	for _, boundary := range []int{1, 2} {
		for _, scenario := range []string{"cancel", "deadline", "unavailable", "drift", "not found", "conflict"} {
			t.Run(fmt.Sprintf("%d/%s", boundary, scenario), func(t *testing.T) {
				code, cause := identityFlowCause(scenario)
				client := newEnrollmentScopeClient(t, execution)
				client.identityError = cause
				client.failIdentityAt = boundary
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				journal := testJournal(execution)
				_, err := waitSelectedEnrollment(ctx, client, execution, selected, time.Millisecond, journal)
				text := journal.file.(*managementMemoryFile).String()
				if err == nil || !identityFlowCausePreserved(err, cause) || !strings.Contains(text, `"reasonCode":"`+code+`"`) || strings.Contains(text+err.Error(), "private-identity-canary") {
					t.Fatalf("err=%v journal=%s", err, text)
				}
				wantLists := 0
				if boundary == 2 {
					wantLists = 6
				}
				if len(client.listKinds) != wantLists || client.identityCalls != boundary || client.createConfig != 0 {
					t.Fatalf("identity failure retried or authorized effects: lists=%v calls=%d", client.listKinds, client.identityCalls)
				}
				if apierrors.IsNotFound(err) != (scenario == "not found") || apierrors.IsConflict(err) != (scenario == "conflict") {
					t.Fatal("private API cause lost")
				}
			})
		}
	}
}

func TestEnrollmentScopeDiagnosticRejectsUnownedReasonAndMessage(t *testing.T) {
	private := errors.New("private-identity-canary")
	cause := &ReconciliationError{ReasonCode: "ARBITRARY_CODE", Message: "untrusted message", cause: private}
	code, message, forwarded := enrollmentScopeDiagnostic(context.Background(), cause, "ENROLLMENT_SCOPE_CHANGED")
	if code != "ENROLLMENT_SCOPE_CHANGED" || message != cause.Error() || forwarded != nil {
		t.Fatal("nonidentity handling changed")
	}
	for _, scenario := range []string{"cancel", "unavailable", "drift"} {
		want, original := identityFlowCause(scenario)
		typed := managementIdentityDiagnostic(context.Background(), original)
		typed.Message = "private-identity-canary"
		code, message, forwarded = enrollmentScopeDiagnostic(context.Background(), typed, "ENROLLMENT_SCOPE_CHANGED")
		if code != want || strings.Contains(message, "canary") || !errors.Is(forwarded, original) {
			t.Fatalf("unsafe forwarding: %s %s %v", code, message, forwarded)
		}
	}
}

type identityWorkloadClient struct {
	*activationIntegrationFake
	cause error
}

func (c identityWorkloadClient) VerifyIdentity(context.Context) error { return c.cause }

func TestWorkloadReadyFenceIdentityDiagnostics(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, owner := range []string{"management", "tenant"} {
		for _, scenario := range []string{"cancel", "deadline", "unavailable", "drift"} {
			t.Run(owner+"/"+scenario, func(t *testing.T) {
				f := newActivationIntegrationFixtureFromExecution(t, execution, false)
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
				current, err := f.client.Get(f.ctx, target.object)
				if err != nil {
					t.Fatal(err)
				}
				code, cause := identityFlowCause(scenario)
				var client workloadStageClient = f.client
				hit := owner == "management"
				if owner == "management" {
					client = identityWorkloadClient{activationIntegrationFake: f.client, cause: cause}
				} else {
					code = strings.Replace(code, "MANAGEMENT_IDENTITY_", "TENANT_API_IDENTITY_", 1)
					original := f.controlPlane.tenant.Client.source
					f.controlPlane.tenant.Client.source = tenantWaitSourceFunc(func(ctx context.Context, e TenantReadinessExpected) (tenantSources, error) {
						hit = true
						if scenario == "drift" {
							f.controlPlane.tenant.Client.systemUID = "previous-tenant"
							return original.observeTenantSources(ctx, e)
						}
						return tenantSources{}, cause
					})
				}
				journal := testJournal(execution)
				_, err = waitWorkloadResource(f.ctx, client, execution.reconciler, f.controlPlane.tenant.Client, target, current.GetUID(), journal, time.Millisecond)
				text := journal.file.(*managementMemoryFile).String()
				if !hit || err == nil || !strings.Contains(text, `"reasonCode":"`+code+`"`) || strings.Contains(text+err.Error(), "private-identity-canary") {
					t.Fatalf("hit=%v err=%v journal=%s", hit, err, text)
				}
				if !identityFlowCausePreserved(err, cause) {
					t.Fatal("private cause lost")
				}
				if f.client.guardCalls != 0 || f.client.resetSafetyCalls != 0 || len(f.client.patches) != 0 {
					t.Fatal("identity failure performed activation effects")
				}
			})
		}
	}
}

func identityFlowCausePreserved(got, cause error) bool {
	if errors.Is(cause, context.Canceled) {
		return errors.Is(got, context.Canceled)
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return errors.Is(got, context.DeadlineExceeded)
	}
	var status apierrors.APIStatus
	if errors.As(cause, &status) {
		return apierrors.ReasonForError(got) == status.Status().Reason
	}
	return true // The finite positive-drift reason is projected before journal sanitization.
}

func TestIdentityDiagnosticsStayWithinResumeAndRebootReasonContracts(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, scenario := range []string{"cancel", "deadline", "unavailable", "drift"} {
		t.Run(scenario, func(t *testing.T) {
			code, cause := identityFlowCause(scenario)
			diagnostic := managementIdentityDiagnostic(context.Background(), cause)
			// The activation owner normalizes cancellation before the BeforeSubmit
			// allowlist, while preserving its existing conservative outer stop.
			boundary := activationBoundaryFailure(context.Background(), testJournal(execution), code, diagnostic.Message, diagnostic)
			expected := code
			if scenario == "cancel" || scenario == "deadline" {
				expected = "ACTIVATION_CANCELED"
			}
			safe := externalRebootPolicyDiagnostic(boundary)
			if !strings.Contains(safe, expected) || strings.Contains(safe, "private-identity-canary") {
				t.Fatalf("reboot projection=%s", safe)
			}
			// Resume deliberately does not turn arbitrary nested identity reasons or
			// messages into its public preparation-reason namespace.
			check, err := operationResumeCheck(context.Background(), execution, "OPERATION_RESUME_TENANT_UNVERIFIED", diagnostic)
			want := "OPERATION_RESUME_TENANT_UNVERIFIED"
			if scenario == "cancel" || scenario == "deadline" {
				want = "OPERATION_RESUME_CANCELED"
			}
			if check.ReasonCode != want || !identityFlowCausePreserved(err, cause) || strings.Contains(check.Message+err.Error(), "private-identity-canary") {
				t.Fatalf("check=%+v err=%v", check, err)
			}
		})
	}
}

func TestControlPlanePreparationKeepsScopeIdentityStopAndNoEffects(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, scenario := range []string{"cancel", "unavailable", "drift", "not found", "conflict"} {
		t.Run(scenario, func(t *testing.T) {
			code, cause := identityFlowCause(scenario)
			client := newEnrollmentScopeClient(t, execution)
			client.binding = true
			client.identityError = cause
			client.failIdentityAt = 1
			request := controlPlaneTestRequest(t)
			for i := range request.Targets {
				if request.Targets[i].Endpoint == "https://192.0.2.20" {
					request.Targets[i].Endpoint = "https://" + execution.plan.Intent().Devices[0].BMCIP
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			journal := testJournal(execution)
			_, err := prepareControlPlaneWithExecution(ctx, client, execution, journal, probe.Runner{}, request, time.Millisecond, time.Second)
			text := journal.file.(*managementMemoryFile).String()
			if err == nil || !identityFlowCausePreserved(err, cause) || !strings.Contains(text, `"reasonCode":"`+code+`"`) || strings.Contains(text+err.Error(), "private-identity-canary") {
				t.Fatalf("err=%v journal=%s", err, text)
			}
			if client.identityCalls != 1 || len(client.listKinds) != 0 || len(client.namespaces) != 0 || client.createConfig != 0 {
				t.Fatal("scope identity failure continued preparation")
			}
			for _, event := range client.events {
				if strings.HasPrefix(event, "create:") || event == "helm" {
					t.Fatalf("effect after failed scope identity: %s", event)
				}
			}
		})
	}
}
