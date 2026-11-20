// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"
)

func TestManagementIdentityDiagnosticKeepsPrivateCauses(t *testing.T) {
	const canary = "private-management-token-error-canary"
	for _, tc := range []struct {
		cause error
		code  string
	}{
		{cause: errors.New("management identity changed: " + canary), code: "MANAGEMENT_IDENTITY_UNVERIFIED"},
		{cause: errors.Join(errManagementIdentityChanged, errors.New(canary)), code: "MANAGEMENT_IDENTITY_CHANGED"},
		{cause: errors.Join(context.Canceled, errors.New(canary)), code: "MANAGEMENT_IDENTITY_CANCELED"},
		{cause: context.DeadlineExceeded, code: "MANAGEMENT_IDENTITY_CANCELED"},
	} {
		got := managementIdentityDiagnostic(context.Background(), tc.cause)
		encoded, err := json.Marshal(got)
		if err != nil || got.ReasonCode != tc.code || !errors.Is(got, tc.cause) || strings.Contains(got.Error()+string(encoded), canary) {
			t.Fatalf("unsafe diagnostic: %v %s", got, encoded)
		}
	}
}

func TestManagementIdentityMarksOnlyObservedDifferences(t *testing.T) {
	for _, field := range []string{"UID", "endpoint", "trust", "version", "missing baseline", "missing measured UID", "unavailable", "canceled"} {
		t.Run(field, func(t *testing.T) {
			fixture := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
			client, err := NewAPIClient(context.Background(), fixture.request, fixture.expected, "v1.35.6")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			changed := true
			switch field {
			case "UID":
				fixture.uid.Store("different-management")
			case "endpoint":
				client.expected.APIEndpoint += "/different"
			case "trust":
				client.expected.TrustDigest = "different-trust"
			case "version":
				client.version = "v1.35.5"
			case "missing baseline":
				client.expected.SystemNamespaceUID, changed = "", false
			case "missing measured UID":
				fixture.uid.Store("")
				changed = false
			case "unavailable":
				client.access.Client, changed = fake.NewClientset(), false
			case "canceled":
				cancel()
				changed = false
			}
			err = client.VerifyIdentity(ctx)
			if err == nil || errors.Is(err, errManagementIdentityChanged) != changed || fixture.writes.Load() != 0 {
				t.Fatalf("changed=%v err=%v writes=%d", changed, err, fixture.writes.Load())
			}
			got := managementIdentityDiagnostic(ctx, err)
			want := "MANAGEMENT_IDENTITY_UNVERIFIED"
			if changed {
				want = "MANAGEMENT_IDENTITY_CHANGED"
			} else if field == "canceled" {
				want = "MANAGEMENT_IDENTITY_CANCELED"
				if !errors.Is(got, context.Canceled) {
					t.Fatal("cancellation lost")
				}
			}
			if got.ReasonCode != want {
				t.Fatalf("diagnostic=%v want=%s", got, want)
			}
		})
	}
}

type identityRuntimeClient struct {
	activationBoundaryReader
	identity func(context.Context) error
}

func (c identityRuntimeClient) VerifyIdentity(ctx context.Context) error { return c.identity(ctx) }

func TestHBNRuntimeManagementIdentityFailureKeepsReasonAndCause(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	for _, phase := range []string{"entry", "exit"} {
		for _, scenario := range []string{"cancel", "deadline", "unavailable", "UID drift"} {
			t.Run(phase+"/"+scenario, func(t *testing.T) {
				f := newRuntimeStageFixtureFromExecution(t, execution)
				fixture := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
				management, err := NewAPIClient(context.Background(), fixture.request, fixture.expected, "v1.35.6")
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(f.ctx)
				defer cancel()
				journal := testJournal(execution)
				file := journal.file.(*managementMemoryFile)
				injected := false
				closing := phase == "entry"
				client := identityRuntimeClient{activationBoundaryReader: f.client, identity: func(ctx context.Context) error {
					if closing {
						injected = true
						switch scenario {
						case "cancel":
							cancel()
						case "deadline":
							return context.DeadlineExceeded
						case "unavailable":
							management.access.Client = fake.NewClientset()
						case "UID drift":
							fixture.uid.Store("different-management")
						}
						return management.VerifyIdentity(ctx)
					}
					return f.client.VerifyIdentity(ctx)
				}}
				patches, claims := len(f.activationIntegrationFixture.client.patches), f.activationIntegrationFixture.client.guardCalls
				fences := fixtureAcceptanceFences(t, f, client, journal)
				if phase == "exit" {
					if err := fences.physical(ctx); err != nil {
						t.Fatal(err)
					}
					closing = true
				}
				err = fences.physical(ctx)
				want := "MANAGEMENT_IDENTITY_CHANGED"
				switch scenario {
				case "cancel", "deadline":
					want = "ACTIVATION_CANCELED"
				case "unavailable":
					want = "MANAGEMENT_IDENTITY_UNVERIFIED"
				}
				if !injected || err == nil || !strings.Contains(file.String(), `"reasonCode":"`+want+`"`) {
					t.Fatalf("injected=%v err=%v journal=%s", injected, err, file.String())
				}
				if scenario == "cancel" && !errors.Is(err, context.Canceled) || scenario == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("runtime lost management cancellation")
				}
				if f.commandReads != 0 || f.trafficReads != 0 {
					t.Fatal("receipt boundary measured runtime")
				}
				if fixture.writes.Load() != 0 || len(f.activationIntegrationFixture.client.patches) != patches || f.activationIntegrationFixture.client.guardCalls != claims {
					t.Fatal("failed observation introduced an effect")
				}
			})
		}
	}
}

type canceledEnrollmentFenceClient struct {
	activationBoundaryReader
	cancel context.CancelFunc
	hit    *bool
}

func (c canceledEnrollmentFenceClient) Get(ctx context.Context, wanted *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if wanted.GetKind() == "Secret" && wanted.GetName() == enrollmentSecretName {
		*c.hit = true
		c.cancel()
		return nil, ctx.Err()
	}
	return c.activationBoundaryReader.Get(ctx, wanted)
}

func TestHBNRuntimeLateControlCancellationDoesNotAssertCredentialDrift(t *testing.T) {
	f := newRuntimeStageFixture(t, []string{"SERIAL-A", "SERIAL-B"})
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	hit := false
	client := canceledEnrollmentFenceClient{activationBoundaryReader: f.client, cancel: cancel, hit: &hit}
	journal := testJournal(f.prepared.execution)
	fences := fixtureAcceptanceFences(t, f, client, journal)
	err := fences.physical(ctx)
	text := journal.file.(*managementMemoryFile).String()
	if !hit || !errors.Is(err, context.Canceled) ||
		!strings.Contains(text, "ACTIVATION_CANCELED") || strings.Contains(text, "ACTIVATION_ENROLLMENT_SECRET_CHANGED") || f.commandReads != 0 || f.trafficReads != 0 {
		t.Fatalf("hit=%v err=%v journal=%s", hit, err, text)
	}
}
