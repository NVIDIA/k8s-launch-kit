// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"errors"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
)

// externalRebootSSHDiagnostic persists finite transport facts, never the error
// string, remote output, endpoint, credential paths or arbitrary callback text.
// Unknown categories are private; unknown stages receive a fixed label.
func externalRebootSSHDiagnostic(err error) string {
	var diagnostic *sshprobe.Error
	if !errors.As(err, &diagnostic) || diagnostic == nil {
		return ""
	}
	var reason string
	switch diagnostic.Code {
	case "SSH_CONFIGURATION_INVALID":
		reason = "The configured SSH connection parameters could not be used."
	case "SSH_TRUST_INVALID":
		reason = "The configured SSH trust snapshot could not be used."
	case "SSH_IDENTITY_INVALID":
		reason = "The selected host UUID, boot ID or adapter identity could not be verified."
	case "PERMISSION_DENIED":
		reason = "The configured account or noninteractive sudo policy did not permit the command."
	case "SSH_REBOOT_NOT_STARTED":
		reason = "The SSH transport established that the reboot command was not dispatched."
	case "SSH_CONNECT_FAILED", "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED", "SSH_HOST_KEY_REJECTED", "SSH_COMMAND_FAILED", "SSH_TIMEOUT_OR_CANCELLED", "SSH_OUTPUT_LIMIT", "SSH_RUNTIME_INVALID", "TOOL_NOT_AVAILABLE", "TOOL_NOT_EXECUTABLE", "DEVICE_ACCESS_FAILED", "OPERATION_UNSUPPORTED":
		reason = sshprobe.Reason(&sshprobe.Error{Code: diagnostic.Code})
	default:
		return ""
	}
	stage := diagnostic.Stage
	switch stage {
	case "endpoint", "credential", "private-key", "session", "known-hosts", "known-host-algorithms", "dial", "deadline", "handshake", "external-session", "external-identity", "external-reboot", "external-host-uuid", "external-host-boot-id", "external-host-adapter-vpd", "external-reboot-system-level":
	default:
		stage = "unclassified"
	}
	return " SSH cause " + diagnostic.Code + " at " + stage + ": " + reason
}

// externalRebootPolicyDiagnostic retains an allowlisted policy stop from the
// pre-submission callback. Its arbitrary error/message text stays private.
func externalRebootPolicyDiagnostic(err error) string {
	var diagnostic *ReconciliationError
	if !errors.As(err, &diagnostic) || diagnostic == nil {
		return ""
	}
	switch diagnostic.ReasonCode {
	case "MANAGEMENT_IDENTITY_CHANGED", "MANAGEMENT_IDENTITY_UNVERIFIED", "TENANT_API_IDENTITY_CHANGED", "TENANT_API_IDENTITY_UNVERIFIED",
		"ACTIVATION_BOOTSTRAP_CHANGED", "ACTIVATION_WORKLOAD_TARGET_CHANGED",
		"ACTIVATION_WORKLOAD_RECEIPT_CHANGED", "ACTIVATION_SOFTWARE_RECEIPT_CHANGED",
		"ACTIVATION_ENROLLMENT_SCOPE_CHANGED", "ACTIVATION_ENROLLMENT_SECRET_CHANGED",
		"ACTIVATION_CANCELED", "ACTIVATION_REBOOT_REQUEST_CHANGED", "ACTIVATION_REBOOT_SCOPE_CHANGED":
		return " Pre-submission policy stopped: " + diagnostic.ReasonCode + "; preserve the claim and inspect the recorded policy failure before continuing."
	default:
		return ""
	}
}
