// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// waitManagementBootstrap handles only the known transient configuration
// readiness state after retained configuration checks. It preserves exact
// UID/spec/version during the wait, then verifies the complete bootstrap before
// dependent work. There are no effects inside this wait.
func waitManagementBootstrap(ctx context.Context, management identityResourceReader, execution *managementVerificationInputs,
	receipt managementBootstrapReceipt, journal *executionJournal, interval time.Duration,
) error {
	err := verifyManagementBootstrap(ctx, management, execution, receipt)
	if !errors.Is(err, errBootstrapConfigNotReady) {
		return err
	}
	initial, err := management.Get(ctx, execution.configObject.DeepCopy())
	event := executionEvent{Stage: "management-bootstrap", Operation: "bootstrap-recheck", Subject: execution.configObject.GetNamespace() + "/" + execution.configObject.GetName()}
	if err != nil {
		return journalFailure(journal, event, false, "BOOTSTRAP_CONFIG_READ_FAILED", "The operator configuration could not be reread before its readiness wait; dependent work remains stopped.", err)
	}
	if initial == nil || initial.GetUID() == "" || initial.GetUID() != types.UID(receipt.OperatorConfigUID) {
		return journalFailure(journal, event, false, "BOOTSTRAP_CONFIG_IDENTITY_CHANGED", "The operator configuration no longer has the bootstrap receipt's UID; dependent work remains stopped.", nil)
	}
	if _, err := waitOperatorConfigReady(ctx, management, execution.reconciler, execution.configObject, initial, execution.operatorVersion, journal, interval); err != nil {
		return err
	}
	return verifyManagementBootstrap(ctx, management, execution, receipt)
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
	case "RESOURCE_API_UNSUPPORTED", "RESOURCE_KIND_UNSUPPORTED":
		return "The retained DPFOperatorConfig readiness API or kind is unsupported."
	default:
		return ""
	}
}
