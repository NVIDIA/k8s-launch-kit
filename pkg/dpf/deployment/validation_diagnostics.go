// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
)

// validationRuntimeStageError projects an already-failed runtime read. Resource
// readiness callers retain their existing Pending/health result semantics.
// Raw cause text is deliberately never used, including for unknown failures.
func validationRuntimeStageError(ctx context.Context, stage validation.DiagnosticStage, err error) error {
	if err == nil {
		return nil
	}
	wrapped := validationRuntimeError(ctx, err, false).(*validation.ObservationError)
	wrapped.Category = validation.ErrorCategory(wrapped)
	if existing, category := validation.DiagnosticForError(err); existing != nil && category == wrapped.Category {
		return err
	}
	diagnostic := validation.Diagnostic{Stage: stage, Reason: validation.ReasonIncomplete,
		Summary: "The runtime observation could not complete at this stage.",
		Action:  "Inspect the selected source for this stage and repeat validation."}
	switch stage {
	case validation.StageRuntimePhysical:
		diagnostic.Summary = "Current selected physical identity and host prerequisite inspection could not complete."
	case validation.StageRuntimeHostNetwork:
		diagnostic.Summary = "Current selected-host network qualification could not complete."
	case validation.StageRuntimeCurrentGraph:
		diagnostic.Summary = "The current selected DPU controller graph could not be verified."
	case validation.StageRuntimeAllocation:
		diagnostic.Summary = "The current selected workload allocation could not be verified."
	case validation.StageRuntimeHBN:
		diagnostic.Summary = "The current selected HBN workload binding could not be verified."
	case validation.StageRuntimeSFC:
		diagnostic.Summary = "The current selected service-chain and interface evidence could not be verified."
	case validation.StageRuntimeIdentity:
		diagnostic.Summary = "Current management or tenant source identity inspection could not complete."
	case validation.StageRuntimeCohort:
		diagnostic.Summary = "The current runtime cohort fence could not complete."
	}
	switch wrapped.Category {
	case validation.FatalInput:
		diagnostic.Reason = validation.ReasonInvalidInput
	case validation.FatalAccess:
		diagnostic.Reason = validation.ReasonUnavailable
	case validation.FatalIdentity:
		diagnostic.Reason = validation.ReasonIdentityDrift
	case validation.FatalCanceled:
		diagnostic.Reason = validation.ReasonCanceled
	case validation.FatalTimeout:
		diagnostic.Reason = validation.ReasonTimedOut
	}
	if check := validationDiagnosticCheck(err, wrapped.Category); check != nil {
		// These fields are the native recorder's trusted finite projection, not
		// the generic Error string or remote Kubernetes/SSH response text.
		diagnostic.Reason = check.reason
		// SSH reasons use fixed prose followed by a parenthesized backend code.
		// Keep the prose; opaque backend codes are not public diagnostic text.
		diagnostic.Summary, _, _ = strings.Cut(check.summary, " (")
	}
	switch diagnostic.Reason {
	case validation.ReasonProgressing:
		diagnostic.Action = "Allow the selected controller to reconcile, inspect its current readiness, and repeat validation."
	case validation.ReasonHealth, validation.ReasonMissing:
		diagnostic.Action = "Inspect the selected controller or workload reported here, resolve its current condition, and repeat validation."
	case validation.ReasonDenied, validation.ReasonUnavailable:
		diagnostic.Action = "Restore read access to the selected observation source and repeat validation."
	case validation.ReasonIdentityDrift:
		diagnostic.Action = "Inspect the changed selected identity; repeat discovery and generation if the replacement is intended."
	case validation.ReasonCanceled, validation.ReasonTimedOut:
		diagnostic.Action = "Check source responsiveness and the observation budget, then repeat validation."
	}
	wrapped.Diagnostic = validation.NormalizeDiagnostic(&diagnostic)
	return wrapped
}

func validationDiagnosticCheck(err error, category validation.FatalCategory) *validationCheckError {
	if check, ok := err.(*validationCheckError); ok && check != nil && validation.ErrorCategory(validationRuntimeError(context.Background(), check, false)) == category {
		return check
	}
	switch cause := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range cause.Unwrap() {
			if check := validationDiagnosticCheck(child, category); check != nil {
				return check
			}
		}
	case interface{ Unwrap() error }:
		return validationDiagnosticCheck(cause.Unwrap(), category)
	}
	return nil
}
