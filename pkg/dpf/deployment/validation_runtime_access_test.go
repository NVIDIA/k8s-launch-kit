// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// A statically comparable wrapper can contain a dynamically unhashable value.
// Error classification must not use it as an unguarded map[error] key.
type runtimeAccessValueError struct {
	payload any
	cause   error
}

func (runtimeAccessValueError) Error() string   { return "private-runtime-canary" }
func (e runtimeAccessValueError) Unwrap() error { return e.cause }

func runValidationRuntimeEntryFailure(t *testing.T, entry func(context.Context) error) validation.Result {
	t.Helper()
	session, _ := standaloneValidationSession()
	plan := correctionSessionPlan(t, session)
	observer := &runtimeCorrectionObserver{}
	observer.snapshot = func() validation.ResourceSnapshot {
		snapshot := validation.ResourceSnapshot{}
		for _, check := range plan.Input().Checks {
			snapshot.Checks = append(snapshot.Checks, validation.Outcome{ID: check.ID, State: validation.Passed})
		}
		return snapshot
	}
	observer.open = func(ctx context.Context) (validation.RuntimeSession, error) {
		return nil, entry(ctx)
	}
	runner, err := validation.NewRunner(plan, observer, validation.Options{})
	if err != nil {
		t.Fatal(err)
	}
	result := runner.Run(context.Background())
	if observer.calls != 1 {
		t.Fatalf("expected one healthy resource snapshot, got %d", observer.calls)
	}
	return result
}

func TestValidationRuntimeEntryHostAccessRetainsExitCategory(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "SSH authentication", err: &sshprobe.Error{Code: "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED", Stage: "private-runtime-canary"}},
		{name: "SSH permission", err: &sshprobe.Error{Code: "PERMISSION_DENIED", Stage: "private-runtime-canary"}},
		{name: "host observation unavailable", err: &sshprobe.Error{Code: "SSH_CONNECT_FAILED", Stage: "private-runtime-canary"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, _ := standaloneValidationSession()
			hosts := []validationHostRequest{session.hosts["SERIAL-A"]}
			hosts[0].selected.hostTarget.SSH = &config.SSHAccess{}
			reads := 0
			result := runValidationRuntimeEntryFailure(t, func(ctx context.Context) error {
				_, err := openCurrentHostNetworkFence(ctx, hosts, time.Second, func(context.Context, config.Target, sshprobe.HostNetworkRequest, time.Duration) (sshprobe.HostNetworkValidation, error) {
					reads++
					return sshprobe.HostNetworkValidation{}, test.err
				})
				if err == nil {
					t.Fatal("current host access failure was not observed")
				}
				return validationRuntimeError(ctx, err, false)
			})
			if reads != 1 || result.Fatal != validation.FatalAccess || result.ExitCode() != 3 || result.Verdict == validation.VerdictPassed {
				t.Fatalf("host access category lost: reads=%d fatal=%s exit=%d verdict=%s", reads, result.Fatal, result.ExitCode(), result.Verdict)
			}
			assertRuntimeEntrySummarySafe(t, result)
		})
	}
}

func TestValidationRuntimeErrorPreservesTypedSourceAndContextPrecedence(t *testing.T) {
	denied := apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "daemonsets"}, "selected", errors.New("private-runtime-canary"))
	unauthorized := apierrors.NewUnauthorized("private-runtime-canary")
	health := validationFailure(validation.Failed, validation.ReasonHealth, "Measured host configuration differs.")
	unavailable := validationFailure(validation.Unverified, validation.ReasonUnavailable, "Selected current source is unavailable.")
	unknown := errors.New("private-runtime-canary")
	for _, test := range []struct {
		name     string
		err      error
		canceled bool
		drift    bool
		category validation.FatalCategory
		exit     int
	}{
		{name: "management Forbidden", err: denied, category: validation.FatalAccess, exit: 3},
		{name: "tenant Unauthorized", err: unauthorized, category: validation.FatalAccess, exit: 3},
		{name: "classified denied", err: validationFailure(validation.Unverified, validation.ReasonDenied, "Selected access denied."), category: validation.FatalAccess, exit: 3},
		{name: "classified unavailable", err: unavailable, category: validation.FatalAccess, exit: 3},
		{name: "health remains observation", err: health, category: validation.FatalObservation, exit: 4},
		{name: "unknown remains observation", err: unknown, category: validation.FatalObservation, exit: 4},
		{name: "incomplete remains observation", err: validationFailure(validation.Unverified, validation.ReasonIncomplete, "Incomplete cohort."), category: validation.FatalObservation, exit: 4},
		{name: "unknown API reason is not access", err: &apierrors.StatusError{ErrStatus: metav1.Status{Reason: "Unrecognized", Code: http.StatusForbidden, Message: "private-runtime-canary"}}, category: validation.FatalObservation, exit: 4},
		{name: "canceled unknown", err: unknown, canceled: true, category: validation.FatalCanceled, exit: 4},
		{name: "deadline", err: context.DeadlineExceeded, category: validation.FatalTimeout, exit: 4},
		{name: "cancellation precedes deadline", err: errors.Join(context.DeadlineExceeded, context.Canceled), category: validation.FatalCanceled, exit: 4},
		{name: "access precedes canceled context", err: denied, canceled: true, category: validation.FatalAccess, exit: 3},
		{name: "access beside health", err: errors.Join(health, unavailable), category: validation.FatalAccess, exit: 3},
		{name: "health beside access", err: errors.Join(unavailable, health), category: validation.FatalAccess, exit: 3},
		{name: "access after unknown API branch", err: errors.Join(&apierrors.StatusError{ErrStatus: metav1.Status{Reason: "Unrecognized", Code: http.StatusInternalServerError}}, denied), category: validation.FatalAccess, exit: 3},
		{name: "dynamically unhashable wrapper", err: runtimeAccessValueError{payload: []string{"private-runtime-canary"}, cause: errors.Join(health, denied)}, category: validation.FatalAccess, exit: 3},
		{name: "native drift precedes canceled context", err: errValidationResourceChanged, canceled: true, category: validation.FatalIdentity, exit: 3},
		{name: "access joined with native drift", err: errors.Join(denied, errValidationResourceChanged), canceled: true, category: validation.FatalIdentity, exit: 3},
		{name: "native drift joined with access", err: errors.Join(errValidationResourceChanged, denied), category: validation.FatalIdentity, exit: 3},
		{name: "explicit drift precedes access", err: unavailable, drift: true, category: validation.FatalIdentity, exit: 3},
		{name: "typed input remains highest with drift and access", err: errors.Join(denied, errValidationResourceChanged, &validation.ObservationError{Category: validation.FatalInput}), canceled: true, category: validation.FatalInput, exit: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := runValidationRuntimeEntryFailure(t, func(ctx context.Context) error {
				if test.canceled {
					child, cancel := context.WithCancel(ctx)
					cancel()
					ctx = child
				}
				source := fmt.Errorf("private-runtime-canary: %w", test.err)
				wrapped := validationRuntimeError(ctx, source, test.drift)
				if !errors.Is(wrapped, source) {
					t.Fatal("runtime wrapper lost its original cause")
				}
				return wrapped
			})
			if result.Fatal != test.category || result.ExitCode() != test.exit {
				t.Fatalf("wanted %s/%d, got %s/%d", test.category, test.exit, result.Fatal, result.ExitCode())
			}
			wantDrift := test.drift || validationIdentityChanged(test.err)
			for _, row := range result.Checks {
				if row.ID == "management/coverage" && row.Outcome.State != validation.Passed {
					t.Fatal("runtime entry erased independent healthy management evidence")
				}
				if row.CohortBound && !strings.HasPrefix(string(row.ID), "runtime/") {
					if wantDrift && (row.Outcome.Reason != validation.ReasonIdentityDrift || row.PriorOutcome == nil || row.PriorOutcome.State != validation.Passed) {
						t.Fatalf("known drift did not revoke the prior cohort pass: %+v", row)
					}
					if !wantDrift && row.Outcome.Reason == validation.ReasonIdentityDrift {
						t.Fatal("access/observation failure invented identity drift")
					}
				}
			}
			assertRuntimeEntrySummarySafe(t, result)
		})
	}
}

func assertRuntimeEntrySummarySafe(t *testing.T, result validation.Result) {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-runtime-canary") || strings.Contains(fmt.Sprint(result), "private-runtime-canary") {
		t.Fatal("runtime error exposed private cause text")
	}
}
