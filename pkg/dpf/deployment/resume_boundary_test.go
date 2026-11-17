// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestResumePreparationDiagnosticsPreserveFiniteOwnerAndPrivateContext(t *testing.T) {
	const canary = "private-resume-cause-token-canary"
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, canary, errors.New(canary))
	for _, tc := range []struct {
		name    string
		cause   error
		want    string
		context error
		api     bool
	}{
		{name: "namespace read denied", cause: bootstrapVerificationFailure("BOOTSTRAP_NAMESPACE_READ_FAILED", denied), want: "BOOTSTRAP_NAMESPACE_READ_FAILED", api: true},
		{name: "actual intent drift is not API error", cause: bootstrapVerificationFailure("BOOTSTRAP_CONFIG_CHANGED", nil), want: "BOOTSTRAP_CONFIG_CHANGED"},
		{name: "nested pending deadline", cause: reconcileError("OPERATION_RESUME_BOOTSTRAP_UNVERIFIED", canary, errors.Join(reconcileError("DPF_CONFIG_READINESS_TIMEOUT", canary, context.DeadlineExceeded))), want: "DPF_CONFIG_READINESS_TIMEOUT", context: context.DeadlineExceeded},
		{name: "nested pending canceled", cause: reconcileError("OPERATION_RESUME_BOOTSTRAP_UNVERIFIED", canary, reconcileError("DPF_CONFIG_READINESS_CANCELED", canary, context.Canceled)), want: "DPF_CONFIG_READINESS_CANCELED", context: context.Canceled},
		{name: "nested intent drift", cause: reconcileError("OPERATION_RESUME_BOOTSTRAP_UNVERIFIED", canary, reconcileError("DPF_CONFIG_INTENT_CHANGED", canary, nil)), want: "DPF_CONFIG_INTENT_CHANGED"},
		{name: "unknown nonAPI failure", cause: errors.New(canary), want: "OPERATION_RESUME_RESTORE_UNVERIFIED"},
		{name: "unknown typed reason", cause: reconcileError(canary, canary, errors.New(canary)), want: "OPERATION_RESUME_RESTORE_UNVERIFIED"},
		{name: "forged known message", cause: reconcileError("BOOTSTRAP_MANAGER_CHANGED", canary, errors.New(canary)), want: "BOOTSTRAP_MANAGER_CHANGED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var progress bytes.Buffer
			ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&progress))
			check, err := operationResumeCheck(ctx, nil, "OPERATION_RESUME_RESTORE_UNVERIFIED", tc.cause)
			if err == nil || check.ReasonCode != tc.want || check.State != "failed" || check.Subject == "" {
				t.Fatalf("finite owner lost: %v %+v", err, check)
			}
			if !errors.Is(err, tc.cause) {
				t.Fatal("private cause identity lost")
			}
			if tc.context != nil && !errors.Is(err, tc.context) {
				t.Fatal("context identity lost")
			}
			path := filepath.Join(t.TempDir(), "journal.jsonl")
			journal, journalErr := newExecutionJournal(path, "sha256:"+strings.Repeat("a", 64))
			if journalErr != nil {
				t.Fatal(journalErr)
			}
			if err := reportOperationResume(ctx, journal, check); err != nil {
				t.Fatal(err)
			}
			if err := journal.close(); err != nil {
				t.Fatal(err)
			}
			raw, readErr := os.ReadFile(path)
			encoded, encodeErr := json.Marshal(check)
			if readErr != nil || encodeErr != nil {
				t.Fatal("diagnostic serialization failed")
			}
			text := string(raw) + string(encoded) + progress.String() + err.Error()
			if strings.Contains(text, canary) || !strings.Contains(string(raw), tc.want) || !strings.Contains(progress.String(), tc.want) {
				t.Fatal("finite reason missing or raw cause exposed")
			}
			if strings.Contains(check.Message, "Observation: API_") != tc.api {
				t.Fatalf("nonAPI failure mislabeled: %+v", check)
			}
		})
	}
}

func TestJournalFailureKeepsTypedNilCauseAndDurableState(t *testing.T) {
	for _, tc := range []struct {
		code      string
		uncertain bool
	}{
		{code: "DPF_CONFIG_INTENT_CHANGED", uncertain: false},
		{code: "ACTIVATION_ENROLLMENT_SECRET_CHANGED", uncertain: true},
	} {
		t.Run(tc.code, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.jsonl")
			journal, err := newExecutionJournal(path, "sha256:"+strings.Repeat("a", 64))
			if err != nil {
				t.Fatal(err)
			}
			stopped := journalFailure(journal, executionEvent{Stage: "owned-stage", Operation: "check"}, tc.uncertain, tc.code, "Fixed safe diagnostic.", nil)
			var typed *ReconciliationError
			if !errors.As(stopped, &typed) || typed.ReasonCode != tc.code || typed.cause != nil || stopped.Error() != tc.code+": Fixed safe diagnostic." {
				t.Fatalf("nil-cause typing/text changed: %v", stopped)
			}
			if err := journal.close(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			state := "failed"
			if tc.uncertain {
				state = "uncertain"
			}
			if !strings.Contains(string(raw), `"state":"`+state+`"`) || !strings.Contains(string(raw), tc.code) {
				t.Fatal("durable failure disposition changed")
			}
			if tc.uncertain && externalRebootPolicyDiagnostic(stopped) == "" {
				t.Fatal("existing allowlisted policy diagnostic lost its typed owner")
			}
		})
	}
}
