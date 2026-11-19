// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package validation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDiagnosticJoinedPriorityAndFirstOrigin(t *testing.T) {
	first := &ObservationError{Category: FatalObservation, Diagnostic: &Diagnostic{Stage: StageRuntimeHBN, Reason: ReasonProgressing, Summary: "First selected HBN condition is pending."}, Cause: errors.New("PRIVATE-CAUSE")}
	second := &ObservationError{Category: FatalObservation, Diagnostic: &Diagnostic{Stage: StageRuntimeSFC, Reason: ReasonHealth, Summary: "Later selected SFC condition failed."}}
	identity := &ObservationError{Category: FatalIdentity, Diagnostic: &Diagnostic{Stage: StageRuntimeIdentity, Reason: ReasonIdentityDrift, Summary: "Selected source identity changed."}}
	for _, test := range []struct {
		name     string
		err      error
		category FatalCategory
		stage    DiagnosticStage
		drift    bool
	}{
		{"first joined origin", errors.Join(first, second), FatalObservation, StageRuntimeHBN, false},
		{"reverse first origin", errors.Join(second, first), FatalObservation, StageRuntimeSFC, false},
		{"higher identity", errors.Join(first, identity), FatalIdentity, StageRuntimeIdentity, true},
		{"input priority keeps identity diagnostic", errors.Join(&ObservationError{Category: FatalInput}, identity, first), FatalInput, StageRuntimeIdentity, true},
		{"canceled beside identity", errors.Join(context.Canceled, identity), FatalIdentity, StageRuntimeIdentity, true},
		{"dynamically unhashable wrapper", valueWrappedError{payload: []int{1}, cause: errors.Join(first, identity)}, FatalIdentity, StageRuntimeIdentity, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := inspectError(test.err)
			if got.primary != test.category || got.identity != test.drift || got.diagnostic == nil || got.diagnostic.Stage != test.stage {
				t.Fatalf("classification=%+v", got)
			}
		})
	}
	var result Result
	setFatal(&result, first)
	setFatal(&result, second)
	setFatal(&result, first)
	if result.Diagnostic == nil || result.Diagnostic.Stage != StageRuntimeHBN {
		t.Fatal("poison replay replaced the first origin")
	}
	first.Diagnostic.Summary = "mutated caller diagnostic"
	if result.Diagnostic.Summary == first.Diagnostic.Summary {
		t.Fatal("result aliases caller diagnostic")
	}
}

type countedDiagnosticCause struct {
	reads *int
	cause error
}

func (e countedDiagnosticCause) Error() string { return "PRIVATE-CAUSE" }
func (e countedDiagnosticCause) Unwrap() error { *e.reads++; return e.cause }

func TestSetFatalDoesNotRewalkAccumulatedCause(t *testing.T) {
	reads := 0
	var result Result
	setFatal(&result, countedDiagnosticCause{reads: &reads, cause: &ObservationError{Category: FatalObservation, Diagnostic: &Diagnostic{Stage: StageRuntimeAllocation, Reason: ReasonIncomplete, Summary: "Allocation read incomplete."}}})
	firstReads := reads
	for range 100 {
		setFatal(&result, errors.New("PRIVATE-LATER-CAUSE"))
	}
	if reads != firstReads {
		t.Fatalf("later errors rewalked accumulated causes: before=%d after=%d", firstReads, reads)
	}
}

func TestDiagnosticProjectionIsBoundedEscapedAndCausePrivate(t *testing.T) {
	diagnostic := &Diagnostic{Stage: StageRuntimeHBN, Reason: ReasonProgressing, Summary: "Selected HBN <condition> is pending.", Action: "Inspect <controller>."}
	result := Result{Mode: ModeFull, Verdict: VerdictIncomplete, Fatal: FatalObservation, Diagnostic: diagnostic, Cause: errors.New("PRIVATE-CAUSE"), Report: ReportDisposition{Status: ReportFailed}}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var html bytes.Buffer
	if err := RenderHTML(&html, result, ReportMetadata{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "runtime-hbn-binding") || !strings.Contains(html.String(), "Selected HBN &lt;condition&gt; is pending.") || !strings.Contains(html.String(), "Inspect &lt;controller&gt;.") {
		t.Fatal("canonical JSON/HTML lost diagnostic")
	}
	if strings.Contains(string(data), "PRIVATE-CAUSE") || strings.Contains(html.String(), "PRIVATE-CAUSE") || strings.Contains(html.String(), "Selected HBN <condition>") {
		t.Fatal("unsafe diagnostic rendering")
	}
	if got := NormalizeDiagnostic(&Diagnostic{Stage: StageRuntimeHBN, Reason: ReasonHealth, Summary: strings.Repeat("x", 900) + "\n", Action: strings.Repeat("a", 900)}); got == nil || len(got.Summary) > 512 || len(got.Action) > 512 {
		t.Fatal("diagnostic was not bounded")
	}
	for _, bad := range []*Diagnostic{{Stage: "PRIVATE-UNKNOWN-STAGE", Reason: ReasonHealth, Summary: "untrusted"}, {Stage: StageRuntimeHBN, Reason: "PRIVATE-UNKNOWN-REASON", Summary: "untrusted"}} {
		if NormalizeDiagnostic(bad) != nil {
			t.Fatal("unknown diagnostic stage/reason accepted")
		}
	}
	if diagnostic, _ := DiagnosticForError(errors.New("PRIVATE-CAUSE")); diagnostic != nil {
		t.Fatal("unknown error became public diagnostic")
	}
}
