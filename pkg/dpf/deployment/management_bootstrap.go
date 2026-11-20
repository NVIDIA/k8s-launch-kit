// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// waitManagementBootstrap handles only the known transient configuration
// readiness state after retained configuration checks. It preserves exact
// UID/spec/version during the wait, then verifies the complete bootstrap before
// dependent work. There are no effects inside this wait.
func waitManagementBootstrap(ctx context.Context, management identityResourceReader, execution *managementVerificationInputs,
	receipt managementBootstrapReceipt, journal *executionJournal, interval time.Duration,
) error {
	err := verifyManagementBootstrap(ctx, management, execution, receipt)
	if !bootstrapConfigPending(err) {
		return err
	}
	event := executionEvent{Stage: generation.StageOperatorConfig, Operation: "readiness", Subject: execution.configObject.GetNamespace() + "/" + execution.configObject.GetName(), State: "attempted", ReasonCode: "DPF_CONFIG_READINESS_STARTED", Message: "Wait for the exact retained management bootstrap to become Ready."}
	if writeErr := journal.record(event); writeErr != nil {
		return writeErr
	}
	for {
		if stop := ctx.Err(); stop != nil {
			code := "DPF_CONFIG_READINESS_CANCELED"
			if errors.Is(stop, context.DeadlineExceeded) {
				code = "DPF_CONFIG_READINESS_TIMEOUT"
			}
			return journalFailure(journal, event, false, code, "The retained management bootstrap did not become Ready before cancellation or deadline.", stop)
		}
		if err == nil {
			event.State, event.ReasonCode, event.Message = "ready", "DPF_CONFIG_READY", "The complete retained management bootstrap is Ready for the selected operator version."
			return journal.record(event)
		}
		if !bootstrapConfigPending(err) {
			detail, ok := err.(*ReconciliationError)
			if !ok || detail == nil {
				return err
			}
			return journalFailure(journal, event, false, detail.ReasonCode, "The retained management bootstrap could not be reverified."+bootstrapFailureDiagnostic(err), err)
		}
		detail := err.(*ReconciliationError)
		event.State, event.ReasonCode, event.Message = "waiting", detail.ReasonCode, detail.Message
		if writeErr := journal.record(event); writeErr != nil {
			return writeErr
		}
		ui.FromContext(ctx).Info("[WAITING] Management bootstrap: %s (%s)", event.Message, event.ReasonCode)
		if stop := waitInterval(ctx, interval); stop != nil {
			code := "DPF_CONFIG_READINESS_TIMEOUT"
			if errors.Is(stop, context.Canceled) {
				code = "DPF_CONFIG_READINESS_CANCELED"
			}
			return journalFailure(journal, event, false, code, "The retained management bootstrap did not become Ready before cancellation or deadline.", stop)
		}
		err = verifyManagementBootstrap(ctx, management, execution, receipt)
	}
}

// Keep the first observation's classification. In particular, a terminal or
// malformed status must not disappear when a later readiness read succeeds.
func bootstrapConfigReadinessFailure(readiness controllercontract.Readiness) error {
	code := readiness.ReasonCode
	switch code {
	case "CONFIG_STATUS_STALE", "CONDITION_STATUS_STALE", "RESOURCE_NOT_READY", "RESOURCE_READINESS_UNKNOWN":
		return reconcileError(code, readiness.Message, errBootstrapConfigNotReady)
	case "RESOURCE_TERMINAL_FAILURE", "RESOURCE_DELETING", "RESOURCE_STATUS_INVALID", "RESOURCE_STATUS_AMBIGUOUS", "RESOURCE_METADATA_INVALID", "RESOURCE_API_UNSUPPORTED", "RESOURCE_KIND_UNSUPPORTED", "RESOURCE_GENERATION_MISSING", "CONDITION_GENERATION_MISSING", "READINESS_CONDITION_MISSING":
		return reconcileError(code, "The retained DPFOperatorConfig readiness observation is blocking.", nil)
	default:
		return reconcileError("RESOURCE_STATUS_INVALID", "The retained DPFOperatorConfig readiness classification is unsupported.", nil)
	}
}

// Recognize only our exact pending result, optionally wrapped by the activation
// owner. Traversing arbitrary error trees could hide a joined read/journal error.
func bootstrapConfigPending(err error) bool {
	detail, ok := err.(*ReconciliationError)
	if !ok || detail == nil {
		return false
	}
	if detail.ReasonCode == "ACTIVATION_BOOTSTRAP_CHANGED" {
		return bootstrapConfigPending(detail.cause)
	}
	switch detail.ReasonCode {
	case "CONFIG_STATUS_STALE", "CONDITION_STATUS_STALE", "RESOURCE_NOT_READY", "RESOURCE_READINESS_UNKNOWN":
		return detail.cause == errBootstrapConfigNotReady
	default:
		return false
	}
}

func bootstrapFailureDiagnostic(err error) string {
	detail, ok := err.(*ReconciliationError)
	if !ok || detail == nil {
		return ""
	}
	if detail.ReasonCode == "ACTIVATION_BOOTSTRAP_CHANGED" {
		return bootstrapFailureDiagnostic(detail.cause)
	}
	message := bootstrapVerificationMessage(detail.ReasonCode)
	if message == "" {
		message = bootstrapWaitFailureMessage(detail.ReasonCode)
	}
	if message == "" {
		switch detail.ReasonCode {
		case "CONFIG_STATUS_STALE", "CONDITION_STATUS_STALE", "RESOURCE_NOT_READY", "RESOURCE_READINESS_UNKNOWN":
			message = "The retained configuration is waiting for current readiness."
		default:
			return ""
		}
	}
	return " Bootstrap cause " + detail.ReasonCode + ": " + message
}

func bootstrapVerificationMessage(code string) string {
	switch code {
	case "BOOTSTRAP_PLAN_INVALID":
		return "Prepared management execution is incomplete."
	case "BOOTSTRAP_RECEIPT_CHANGED":
		return "Bootstrap receipt manager identity differs from the retained operator plan."
	case "BOOTSTRAP_NAMESPACE_READ_FAILED":
		return "A retained management namespace could not be read or verified through the management API."
	case "BOOTSTRAP_CONFIG_READ_FAILED":
		return "The retained DPFOperatorConfig could not be read or verified through the management API."
	case "BOOTSTRAP_MANAGER_READ_FAILED":
		return "The retained DPF manager Deployment could not be read or verified through the management API."
	case "BOOTSTRAP_CONFIG_IDENTITY_CHANGED":
		return "The operator configuration no longer has the bootstrap receipt's UID."
	case "BOOTSTRAP_NAMESPACE_CHANGED":
		return "A management namespace no longer matches the bootstrap receipt."
	case "BOOTSTRAP_CONFIG_CHANGED":
		return "DPFOperatorConfig no longer matches the bootstrap receipt and retained intent."
	case "BOOTSTRAP_CONFIG_VERSION_CHANGED":
		return "DPFOperatorConfig no longer reports the retained operator version."
	case "BOOTSTRAP_MANAGER_CHANGED":
		return "DPF manager Deployment no longer matches the bootstrap receipt."
	case "BOOTSTRAP_MANAGER_NOT_READY":
		return "DPF manager Deployment is no longer ready."
	case "BOOTSTRAP_MANAGEMENT_UNVERIFIED":
		return "Management identity could not be verified for the retained bootstrap."
	default:
		return ""
	}
}

func bootstrapVerificationFailure(code string, cause error) error {
	return reconcileError(code, bootstrapVerificationMessage(code), cause)
}

// Reconstruct the existing wait owner's finite stop reasons without copying
// controller messages or arbitrary error text into the resume result.
func bootstrapWaitFailureMessage(code string) string {
	switch code {
	case "DPF_CONFIG_READINESS_TIMEOUT":
		return "The retained DPFOperatorConfig did not become Ready before the execution deadline."
	case "DPF_CONFIG_READINESS_CANCELED":
		return "The retained DPFOperatorConfig readiness wait was canceled."
	case "DPF_CONFIG_INTENT_CHANGED":
		return "The DPFOperatorConfig identity or retained intent changed while waiting."
	case "DPF_CONFIG_VERSION_MISMATCH":
		return "DPFOperatorConfig reports a different, invalid or unreported operator version."
	case "DPF_CONFIG_READ_FAILED":
		return "The retained DPFOperatorConfig could not be read while waiting."
	case "RESOURCE_TERMINAL_FAILURE":
		return "The retained DPFOperatorConfig reports a terminal failure."
	case "RESOURCE_DELETING":
		return "The retained DPFOperatorConfig is being deleted."
	case "RESOURCE_STATUS_INVALID":
		return "The retained DPFOperatorConfig status is invalid."
	case "RESOURCE_STATUS_AMBIGUOUS":
		return "The retained DPFOperatorConfig readiness status is ambiguous."
	case "RESOURCE_METADATA_INVALID":
		return "The retained DPFOperatorConfig metadata is invalid."
	case "RESOURCE_GENERATION_MISSING", "CONDITION_GENERATION_MISSING", "READINESS_CONDITION_MISSING":
		return "The retained DPFOperatorConfig readiness metadata is incomplete."
	case "RESOURCE_API_UNSUPPORTED", "RESOURCE_KIND_UNSUPPORTED":
		return "The retained DPFOperatorConfig readiness API or kind is unsupported."
	default:
		return ""
	}
}
