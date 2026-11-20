// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
)

func TestValidationResourceReadPreservesDeadlineAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name     string
		cancel   bool
		identity bool
		category validation.FatalCategory
		reason   validation.Reason
		exit     int
	}{
		{"resource deadline", false, false, validation.FatalTimeout, validation.ReasonTimedOut, 4},
		{"caller cancellation", true, false, validation.FatalCanceled, validation.ReasonCanceled, 4},
		{"measured identity outranks deadline", false, true, validation.FatalIdentity, validation.ReasonIdentityDrift, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, _ := standaloneValidationSession()
			input := correctionSessionPlan(t, session).Input()
			input.Mode = validation.ModeResources
			input.Checks = append(input.Checks, validation.CheckSpec{ID: "management/identity", Area: validation.AreaManagement, Subject: "Management identity", Required: true})
			plan, err := validation.NewPlan(input)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			observer := &validationObserver{mode: validation.ModeResources, checks: input.Checks,
				newManagement: func(readCtx context.Context) (validationManagementReader, error) {
					reads++
					if test.cancel {
						cancel()
					}
					<-readCtx.Done()
					if test.identity {
						return nil, errors.Join(readCtx.Err(), errManagementIdentityChanged)
					}
					return nil, readCtx.Err()
				},
			}
			runner, err := validation.NewRunner(plan, observer, validation.Options{Wait: 20 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			result := runner.Run(ctx)
			if reads != 1 || result.Fatal != test.category || result.ExitCode() != test.exit {
				t.Fatalf("initial read classification: reads=%d fatal=%s exit=%d", reads, result.Fatal, result.ExitCode())
			}
			for _, row := range result.Checks {
				if row.ID == "management/identity" && row.Outcome.Reason != test.reason {
					t.Fatalf("management read reason: %+v", row.Outcome)
				}
			}
			encoded, err := json.Marshal(result)
			if err != nil || !bytes.Contains(encoded, []byte(`"fatal":"`+string(test.category)+`"`)) {
				t.Fatalf("terminal JSON classification: %s %v", encoded, err)
			}
			var html bytes.Buffer
			if err := validation.RenderHTML(&html, result, validation.ReportMetadata{}); err != nil {
				t.Fatal(err)
			}
			if test.category == validation.FatalTimeout && strings.Contains(html.String(), "Validation was canceled.") {
				t.Fatal("resource deadline presented as caller cancellation")
			}
		})
	}
}
