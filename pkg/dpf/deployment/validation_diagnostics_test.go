// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func TestValidationDiagnosticPreservesInnerStageAndPrivateUnknownCause(t *testing.T) {
	cause := errors.New("PRIVATE-CAUSE signed-url?token=secret")
	inner := validationRuntimeStageError(context.Background(), validation.StageRuntimeHBN, cause)
	outer := validationRuntimeStageError(context.Background(), validation.StageRuntimeIdentity, inner)
	if outer != inner || !errors.Is(outer, cause) {
		t.Fatal("outer scope replaced existing error diagnostic or lost cause")
	}
	diagnostic, _ := validation.DiagnosticForError(outer)
	if diagnostic == nil || diagnostic.Stage != validation.StageRuntimeHBN || diagnostic.Reason != validation.ReasonIncomplete || strings.Contains(diagnostic.Summary, "PRIVATE-CAUSE") {
		t.Fatalf("unsafe unknown projection: %+v", diagnostic)
	}
	if validationRuntimeStageError(context.Background(), validation.StageRuntimeHBN, nil) != nil {
		t.Fatal("successful read became a failure")
	}
}

func TestValidationDiagnosticJoinedNativeCausePriority(t *testing.T) {
	health := validationFailure(validation.Failed, validation.ReasonHealth, "Lower-priority health summary.")
	access := validationFailure(validation.Unverified, validation.ReasonDenied, "Selected source read access is denied.")
	inner := validationRuntimeStageError(context.Background(), validation.StageRuntimeHBN, health)
	for _, test := range []struct {
		name     string
		err      error
		category validation.FatalCategory
		reason   validation.Reason
	}{
		{"identity after health", errors.Join(health, errValidationResourceChanged), validation.FatalIdentity, validation.ReasonIdentityDrift},
		{"health after identity", errors.Join(errValidationResourceChanged, health), validation.FatalIdentity, validation.ReasonIdentityDrift},
		{"access after health", errors.Join(health, access), validation.FatalAccess, validation.ReasonDenied},
		{"health after access", errors.Join(access, health), validation.FatalAccess, validation.ReasonDenied},
		{"existing health diagnostic beside native identity", errors.Join(inner, errValidationResourceChanged), validation.FatalIdentity, validation.ReasonIdentityDrift},
		{"existing health diagnostic beside native API denial", errors.Join(inner, apierrors.NewUnauthorized("PRIVATE-CAUSE")), validation.FatalAccess, validation.ReasonUnavailable},
		{"typed input beside health", errors.Join(health, &validation.ObservationError{Category: validation.FatalInput}), validation.FatalInput, validation.ReasonInvalidInput},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := validationRuntimeStageError(ctx, validation.StageRuntimeIdentity, test.err)
			diagnostic, category := validation.DiagnosticForError(err)
			if validation.ErrorCategory(err) != test.category || category != test.category || diagnostic == nil || diagnostic.Reason != test.reason {
				t.Fatalf("category=%s diagcategory=%s diagnostic=%+v", validation.ErrorCategory(err), category, diagnostic)
			}
			if diagnostic.Reason != validation.ReasonHealth && diagnostic.Summary == "Lower-priority health summary." {
				t.Fatal("lower health cause overwrote primary diagnostic")
			}
		})
	}
	codeCause := validationHostNetworkError(&sshprobe.Error{Code: "PRIVATE-SSH-CODE (PRIVATE-CAUSE"})
	diagnostic, _ := validation.DiagnosticForError(validationRuntimeStageError(context.Background(), validation.StageRuntimeHostNetwork, codeCause))
	if diagnostic == nil || strings.Contains(diagnostic.Summary, "PRIVATE-") {
		t.Fatal("opaque SSH code entered public diagnostic")
	}
}

func TestValidationDiagnosticExplainsPassedRoutePostFenceFailure(t *testing.T) {
	for _, test := range []struct {
		name     string
		cause    error
		reason   validation.Reason
		category validation.FatalCategory
	}{
		{"known Pending", validationFailure(validation.Pending, validation.ReasonProgressing, "Selected HBN service has not reconciled its current generation."), validation.ReasonProgressing, validation.FatalObservation},
		{"known health", validationFailure(validation.Failed, validation.ReasonHealth, "Selected HBN workload is unhealthy."), validation.ReasonHealth, validation.FatalObservation},
		{"access unavailable", validationFailure(validation.Unverified, validation.ReasonUnavailable, "Selected HBN source is unavailable."), validation.ReasonUnavailable, validation.FatalAccess},
		{"unknown private cause", errors.New("PRIVATE-CAUSE signed-url?token=secret"), validation.ReasonIncomplete, validation.FatalObservation},
		{"known identity", errValidationResourceChanged, validation.ReasonIdentityDrift, validation.FatalIdentity},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, _ := standaloneValidationSession()
			plan := correctionSessionPlan(t, session)
			afterLastRoute := false
			routes, traffic, finalPhysical := 0, 0, 0
			session.read = func(_ context.Context, _ string, pod string, command hbnRuntimeCommand, prefix netip.Prefix) ([]byte, error) {
				if command == hbnReadBGPSummary {
					asn := uint32(65101)
					if pod == "hbn-b" {
						asn = 65201
					}
					return runtimeBGPJSON(asn, 65000), nil
				}
				routes++
				if routes == len(session.scope.Routes) {
					afterLastRoute = true
				}
				return runtimeFIBJSON(prefix.String(), "p0_if"), nil
			}
			session.observe = func(ctx context.Context) (validationCurrentCohort, error) {
				if afterLastRoute {
					return validationCurrentCohort{}, validationRuntimeStageError(ctx, validation.StageRuntimeHBN, test.cause)
				}
				return session.anchor, nil
			}
			session.verifyPhysical = func(context.Context) error { finalPhysical++; return nil }
			session.probe = func(context.Context, config.Target, sshprobe.HostTrafficRequest, time.Duration) (sshprobe.HostTrafficResult, error) {
				traffic++
				return sshprobe.HostTrafficResult{}, errors.New("unexpected probe")
			}
			observer := &runtimeCorrectionObserver{open: func(context.Context) (validation.RuntimeSession, error) { return session, nil }}
			observer.snapshot = func() validation.ResourceSnapshot {
				snapshot := validation.ResourceSnapshot{}
				for _, check := range plan.Input().Checks {
					snapshot.Checks = append(snapshot.Checks, validation.Outcome{ID: check.ID, State: validation.Passed})
				}
				return snapshot
			}
			runner, err := validation.NewRunner(plan, observer, validation.Options{})
			if err != nil {
				t.Fatal(err)
			}
			result := runner.Run(context.Background())
			if result.Fatal != test.category || result.Diagnostic == nil || result.Diagnostic.Stage != validation.StageRuntimeHBN || result.Diagnostic.Reason != test.reason {
				t.Fatalf("lost native post-fence diagnostic: fatal=%s diagnostic=%+v", result.Fatal, result.Diagnostic)
			}
			if routes != 2 || traffic != 0 || finalPhysical != 0 || session.poisoned == nil {
				t.Fatalf("poison path routes=%d traffic=%d physical=%d", routes, traffic, finalPhysical)
			}
			lastRoute := validation.RouteCheckID(session.scope.Routes[1])
			found := false
			for _, row := range result.Checks {
				if row.ID == lastRoute {
					found = true
					if row.Outcome.State != validation.Unverified || row.PriorOutcome == nil || row.PriorOutcome.State != validation.Passed || row.PriorOutcome.Route == nil || !row.PriorOutcome.Route.Installed {
						t.Fatalf("route facts lost: %+v", row)
					}
				}
				if row.CohortBound && test.reason != validation.ReasonIdentityDrift && strings.Contains(row.Outcome.Action, "Restore the selected identities") {
					t.Fatal("ordinary observation failure invented identity remediation")
				}
				if row.ID == "management/coverage" && row.Outcome.State != validation.Passed {
					t.Fatal("independent management evidence was erased")
				}
			}
			if !found {
				t.Fatal("last route result absent")
			}
			var html bytes.Buffer
			if err := validation.RenderHTML(&html, result, validation.ReportMetadata{}); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), result.Diagnostic.Summary) || !strings.Contains(html.String(), result.Diagnostic.Summary) || strings.Contains(string(data), "PRIVATE-CAUSE") || strings.Contains(html.String(), "PRIVATE-CAUSE") {
				t.Fatal("diagnostic lost or raw cause published")
			}
		})
	}
}
