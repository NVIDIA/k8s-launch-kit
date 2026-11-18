// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// checkInitialOperationVacancy observes only a core ConfigMap. Neither a cold
// operator API nor an absent operator namespace requires installation first.
// This early observation never replaces the final create-only reservation.
func checkInitialOperationVacancy(ctx context.Context, client *APIClient, execution *managementExecution) error {
	deployment, err := retainedDeploymentObject(execution.verificationInputs())
	if err != nil {
		return operationReservationFailure(ctx, "OPERATION_PLAN_INVALID", err)
	}
	if err := ctx.Err(); err != nil {
		return operationReservationFailure(ctx, "OPERATION_CANCELED", err)
	}
	// Keep identity failures separate from the requested object's NotFound.
	// A failed kube-system read cannot prove the operation record is absent.
	if err := client.VerifyIdentity(ctx); err != nil {
		return operationReservationFailure(ctx, "OPERATION_MANAGEMENT_UNMEASURED", err)
	}
	object, err := client.access.Client.CoreV1().ConfigMaps(deployment.GetNamespace()).Get(ctx, operationName(deployment), metav1.GetOptions{})
	if ctx.Err() != nil {
		return operationReservationFailure(ctx, "OPERATION_CANCELED", err)
	}
	if err != nil && apierrors.IsNotFound(safeAPIError("read operation record", err)) {
		return nil
	}
	if err != nil || object == nil {
		return operationReservationFailure(ctx, "OPERATION_SLOT_UNMEASURED", err)
	}
	// Any object at the reserved name blocks initial deployment. Matching bundle
	// bytes, labels or completed progress do not authorize reusing that record.
	return operationReservationFailure(ctx, "OPERATION_RESERVATION_EXISTS", nil)
}

func operationReservationMessage(code string) string {
	switch code {
	case "OPERATION_MANAGEMENT_UNMEASURED":
		return "Management identity or access could not be verified for the operation check; correct access and repeat the inspection before deployment."
	case "OPERATION_PLAN_INVALID":
		return "The retained deployment cannot identify its operation record; correct and regenerate the bundle before deployment."
	case "OPERATION_SLOT_UNMEASURED":
		return "The operation record could not be inspected; verify management access and inspect current state before another initial attempt."
	case "OPERATION_RESERVATION_EXISTS":
		return "An existing object occupies the operation record. Inspect the operation-reservation check's namespace/name subject and preserve the current record. Resume only the original operation and inputs, or have the deployment owner arrange controlled retirement; Launch Kit has no retirement command. Follow https://nvidia.github.io/k8s-launch-kit/user/dpf-deployment/#retire-a-previous-deployment-record."
	case "OPERATION_PARENT_EXISTS":
		return "A DPUDeployment already exists at the selected name; inspect its recorded operation instead of starting another initial deployment."
	case "OPERATION_PARENT_UNMEASURED":
		return "DPUDeployment absence could not be verified; inspect management access and current deployment state before continuing."
	case "OPERATION_TENANT_SOURCES_UNMEASURED":
		return "The tenant source identities could not be retained for recovery; inspect the current tenant and access before continuing."
	case "OPERATION_CHECKPOINT_INVALID":
		return "The initial operation checkpoint is incomplete or outside its supported bounds; preserve the prepared resources and report for diagnosis."
	case "OPERATION_CANCELED":
		return "Operation inspection or reservation was canceled; inspect current records and earlier effects before deciding how to recover."
	case "OPERATION_RESERVATION_UNCERTAIN":
		return "The operation reservation outcome is uncertain; preserve current state and inspect its live record before any further deployment attempt."
	default:
		return ""
	}
}

func operationReservationDiagnostic(code string, cause error) (string, string) {
	message := operationReservationMessage(code)
	if message == "" {
		code = "OPERATION_RESERVATION_UNCERTAIN"
		message = operationReservationMessage(code)
	}
	if cause != nil && (code == "OPERATION_MANAGEMENT_UNMEASURED" || code == "OPERATION_SLOT_UNMEASURED" || code == "OPERATION_PARENT_UNMEASURED" || code == "OPERATION_TENANT_SOURCES_UNMEASURED" || code == "OPERATION_RESERVATION_UNCERTAIN") {
		message += " Observation: " + apiFailureCode(safeAPIError("operation boundary", cause)) + "."
	}
	return code, message
}

func operationReservationFailure(ctx context.Context, code string, cause error) error {
	if ctx.Err() != nil || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		code, cause = "OPERATION_CANCELED", errors.Join(cause, ctx.Err())
	}
	code, message := operationReservationDiagnostic(code, cause)
	return reconcileError(code, message, cause)
}

// operationReservationCheck projects only this owner's finite diagnostics.
// Rebuild the message instead of publishing arbitrary error or cause text.
func operationReservationCheck(execution *managementVerificationInputs, err error) inventory.Check {
	subject := "retained DPUDeployment"
	if deployment, planErr := retainedDeploymentObject(execution); planErr == nil {
		subject = deployment.GetNamespace() + "/" + operationName(deployment)
	}
	check := inventory.Check{ID: "dpf.hbn.deployment.operation-reservation", Subject: subject, Vantage: "management-cluster", RequiredPhase: "deploy",
		State: "passed", ReasonCode: "OPERATION_RESERVATION_AVAILABLE", Message: "No existing operation record blocks initial deployment; the record must still be reserved before DPUDeployment creation."}
	if err == nil {
		return check
	}
	code, cause := "OPERATION_RESERVATION_UNCERTAIN", error(nil)
	var failure *ReconciliationError
	if errors.As(err, &failure) && failure != nil && operationReservationMessage(failure.ReasonCode) != "" {
		code, cause = failure.ReasonCode, failure.cause
	}
	check.State = "failed"
	check.ReasonCode, check.Message = operationReservationDiagnostic(code, cause)
	return check
}

func reportOperationReservation(ctx context.Context, journal *executionJournal, check inventory.Check) error {
	if err := journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "operation-reservation", Subject: check.Subject,
		State: check.State, ReasonCode: check.ReasonCode, Message: check.Message}); err != nil {
		return err
	}
	if check.State != "passed" {
		ui.FromContext(ctx).Warning("[BLOCKED] Deployment operation %q: %s (%s)", check.Subject, check.Message, check.ReasonCode)
	}
	return nil
}
