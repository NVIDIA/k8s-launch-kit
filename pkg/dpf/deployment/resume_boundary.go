// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// These are preparation-owner diagnostics, never arbitrary server/error text.
func operationResumeMessage(code string) string {
	switch code {
	case "OPERATION_RESUME_MANAGEMENT_UNVERIFIED":
		return "Resume could not verify the retained management identity or access. Correct access without changing the saved operation, configuration or bundle."
	case "OPERATION_PERMISSION_DENIED":
		return "The caller is denied a required operation-record ConfigMap permission. Inspect get, create and update authorization before continuing."
	case "OPERATION_PERMISSION_UNVERIFIED":
		return "Required operation-record ConfigMap authorization could not be measured. Inspect management access before continuing."
	case "OPERATION_RESUME_PERMISSION_UNVERIFIED":
		return "Resume could not verify required operation-record permissions. Inspect management access before continuing."
	case "OPERATION_RESUME_RECORD_UNVERIFIED":
		return "Resume could not verify the exact saved operation, namespace, bundle or selected cohort. Inspect the reported record; do not adopt or replace it."
	case "OPERATION_RESUME_REFERENCE_INVALID":
		return "Resume reference differs from the retained deployment's operation record. Use only the original reported namespace/name/UID."
	case "OPERATION_RESUME_RECORD_UNAVAILABLE":
		return "The original operation record is missing, replaced or unreadable. Preserve current state; do not recreate or adopt a record."
	case "OPERATION_RESUME_CHECKPOINT_INVALID":
		return "The saved operation checkpoint is incomplete or outside its supported schema. Preserve the exact record and report for diagnosis."
	case "OPERATION_RESUME_BINDING_CHANGED":
		return "The saved operation's cluster, bundle or selected cohort differs from retained intent. Resume only with its original configuration and bundle."
	case "OPERATION_RESUME_NAMESPACE_UNVERIFIED":
		return "The original operation namespace could not be read or verified through the management API."
	case "OPERATION_RESUME_NAMESPACE_CHANGED":
		return "The original operation namespace is missing, replaced, deleting or unreadable. Preserve current state and inspect management identity."
	case "OPERATION_RESUME_ENROLLMENT_CHANGED":
		return "The selected enrollment differs from the saved operation. Preserve its original device identities and inputs."
	case "OPERATION_RESUME_ENROLLMENT_UNVERIFIED":
		return "Resume could not bind selected BMC access to the saved enrollment. Inspect the original selected identities and protected access."
	case "OPERATION_RESUME_BOOTSTRAP_UNVERIFIED":
		return "Resume could not restore the retained management bootstrap. Inspect its namespaces, operator configuration and manager readiness."
	case "OPERATION_RESUME_TENANT_UNVERIFIED":
		return "Resume could not restore the same tenant identity or credential sources. Inspect the original tenant and access; do not recreate or adopt it."
	case "OPERATION_RESUME_PLAN_INVALID":
		return "Resume could not recover the retained workload plan. Preserve the original bundle and report for diagnosis."
	case "OPERATION_RESUME_SOFTWARE_UNVERIFIED":
		return "Resume could not verify retained software resources or registry identity. Inspect current software preparation without reimaging devices."
	case "OPERATION_RESUME_WORKLOAD_UNVERIFIED":
		return "Resume could not verify retained workload resources and ownership. Inspect the original selected workload; do not replace it."
	case "OPERATION_RESUME_RESTORE_UNVERIFIED":
		return "Resume could not restore the saved preparation receipts. Preserve current state and inspect the operation before continuing."
	case "OPERATION_RESUME_CANCELED":
		return "Resume preparation was canceled or its deadline expired. Preserve the operation and current state before deciding whether to resume again."
	default:
		return ""
	}
}

func operationResumeCheck(ctx context.Context, execution *managementVerificationInputs, code string, cause error) (inventory.Check, error) {
	message := operationResumeMessage(code)
	if message == "" {
		code = "OPERATION_RESUME_RESTORE_UNVERIFIED"
		message = operationResumeMessage(code)
	}
	// Wrappers identify the preparation stage. Preserve a more specific finite
	// producer reason when present; never publish its arbitrary Message field.
	current := cause
	for range 8 {
		var failure *ReconciliationError
		if !errors.As(current, &failure) || failure == nil {
			break
		}
		if safe := operationResumeMessage(failure.ReasonCode); safe != "" {
			code, message = failure.ReasonCode, safe
		} else if safe := bootstrapVerificationMessage(failure.ReasonCode); safe != "" {
			code, message = failure.ReasonCode, safe
		} else if safe := bootstrapWaitFailureMessage(failure.ReasonCode); safe != "" {
			code, message = failure.ReasonCode, safe
		}
		current = failure.cause
	}
	if ctx.Err() != nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		if code != "DPF_CONFIG_READINESS_TIMEOUT" && code != "DPF_CONFIG_READINESS_CANCELED" {
			code = "OPERATION_RESUME_CANCELED"
			message = operationResumeMessage(code)
		}
		cause = errors.Join(cause, ctx.Err())
	} else {
		var status apierrors.APIStatus
		if errors.As(cause, &status) {
			message += " Observation: " + apiFailureCode(safeAPIError("resume preparation", cause)) + "."
		}
	}
	message += " Preserve the operation and current state; inspect this prerequisite before an explicit resume."
	subject := "retained deployment operation"
	if desired, err := retainedDeploymentObject(execution); err == nil {
		subject = desired.GetNamespace() + "/" + operationName(desired)
	}
	check := inventory.Check{ID: "dpf.hbn.deployment.resume-preparation", Subject: subject, Vantage: "management-cluster", RequiredPhase: "deploy", State: "failed", ReasonCode: code, Message: message}
	return check, reconcileError(code, message, cause)
}

func reportOperationResume(ctx context.Context, journal *executionJournal, check inventory.Check) error {
	if err := journal.record(executionEvent{Stage: "resume-preparation", Operation: "restore", Subject: check.Subject, State: check.State, ReasonCode: check.ReasonCode, Message: check.Message}); err != nil {
		return err
	}
	ui.FromContext(ctx).Warning("[BLOCKED] Resume operation %q: %s (%s)", check.Subject, check.Message, check.ReasonCode)
	return nil
}
