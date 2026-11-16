// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestBootstrapMaterialReadCausesAreFiniteAndPrivate(t *testing.T) {
	const canary = "private-token-id-secret-name-error-canary"
	for _, tc := range []struct {
		name, want string
		cause      error
	}{
		{name: "missing", want: "not-found", cause: apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, canary)},
		{name: "forbidden", want: "access-denied", cause: apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, canary, errors.New(canary))},
		{name: "unauthorized", want: "access-denied", cause: apierrors.NewUnauthorized(canary)},
		{name: "API timeout", want: "timeout-or-canceled", cause: apierrors.NewTimeoutError(canary, 1)},
		{name: "canceled", want: "timeout-or-canceled", cause: fmt.Errorf("%s: %w", canary, context.Canceled)},
		{name: "deadline", want: "timeout-or-canceled", cause: context.DeadlineExceeded},
		{name: "other", want: "other-api-failure", cause: errors.New(canary)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := &managementMemoryFile{}
			journal := &executionJournal{file: file, digest: "test"}
			cause := bootstrapCredentialReadFailure(journal, "BOOTSTRAP_TOKEN_UNAVAILABLE", tc.cause)
			outer := activationFailure(journal, "EXTERNAL_HOST_REBOOT_UNCERTAIN", "Completion unverified.", cause)
			for _, public := range []string{cause.Error(), outer.Error(), file.String()} {
				if !strings.Contains(public, "Read cause: "+tc.want) || strings.Contains(public, canary) {
					t.Fatalf("unsafe or missing read reason: %s", public)
				}
			}
		})
	}
	if got := bootstrapReadDiagnostic(bootstrapReadCause(canary)); got != "" {
		t.Fatalf("unknown private category escaped: %s", got)
	}
}

func TestBootstrapRealMaterialReadReportsMissingDeniedAndTimeout(t *testing.T) {
	for _, mode := range []string{"missing", "denied", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			m, c, s, dpu := newBootstrapCredentialFixture(t)
			ctx := context.Background()
			want := "not-found"
			switch mode {
			case "missing":
				s.token = nil
			case "denied":
				s.forbidden = "/api/v1/namespaces/kube-system/secrets/bootstrap-token-" + bootstrapTestID
				want = "access-denied"
			case "timeout":
				want = "timeout-or-canceled"
				expected, sources := c.expected, c.source.(*tenantSourceStub).sources
				c, _ = tenantIdentityClientFixtureWithHandler(t, expected, &tenantSourceStub{sources: sources}, func(w http.ResponseWriter, r *http.Request) bool {
					if strings.Contains(r.URL.Path, "/secrets/") {
						<-r.Context().Done()
						return true
					}
					return s.serve(w, r)
				})
			}
			file := &managementMemoryFile{}
			journal := &executionJournal{file: file, digest: "test"}
			err := verifyAbsentRegistrationCredentials(ctx, m, c, dpu, journal)
			if err == nil || !strings.Contains(err.Error(), "Read cause: "+want) || !strings.Contains(file.String(), "Read cause: "+want) {
				t.Fatalf("reader lost finite cause: %v", err)
			}
			if strings.Contains(err.Error()+file.String(), bootstrapTestID) || strings.Contains(err.Error()+file.String(), bootstrapTestSecret) || strings.Contains(err.Error()+file.String(), "private-error-canary") {
				t.Fatal("private material or API message escaped")
			}
			if m.writes != 0 || s.writes != 0 {
				t.Fatal("read failure wrote resources")
			}
		})
	}
}

func TestBootstrapFinalValidMaterialRegistrationReadCancellation(t *testing.T) {
	m, c, s, dpu := newBootstrapCredentialFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.beforeRead = func(path string, n int) {
		if path == "/api/v1/nodes/"+dpu.GetName() && n == 2 {
			cancel()
		}
	}
	file := &managementMemoryFile{}
	journal := &executionJournal{file: file, digest: "test"}
	err := verifyAbsentRegistrationCredentials(ctx, m, c, dpu, journal)
	if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "ACTIVATION_CANCELED") {
		t.Fatalf("final valid-material read lost cancellation: %v", err)
	}
	if strings.Contains(err.Error()+file.String(), "BOOTSTRAP_REGISTRATION_UNMEASURED") || strings.Contains(err.Error()+file.String(), "#recover-from-unavailable-bootstrap-credentials") {
		t.Fatal("cancellation was mislabeled as credential recovery")
	}
	if !strings.Contains(file.String(), "ACTIVATION_CANCELED") || m.writes != 0 || s.writes != 0 {
		t.Fatal("cancellation not safely persisted or caused a mutation")
	}
}
