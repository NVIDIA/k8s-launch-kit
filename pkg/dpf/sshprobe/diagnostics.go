// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package sshprobe

import (
	"errors"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"golang.org/x/crypto/ssh"
)

// commandFailure classifies bounded output, but never returns it. Tool diagnostics
// can include paths and credentials; only static explanations reach artifacts.
func commandFailure(err error, stage, command, output string) *Error {
	code := "SSH_COMMAND_FAILED"
	var status *ssh.ExitError
	if errors.As(err, &status) {
		switch status.ExitStatus() {
		case 126:
			code = "TOOL_NOT_EXECUTABLE"
		case 127:
			code = "TOOL_NOT_AVAILABLE"
		case 66:
			if stage == "dnsmasq" {
				code = "PROPERTY_NOT_EXPOSED"
			}
		case 77:
			if stage == "dnsmasq" {
				code = "PERMISSION_DENIED"
			}
		}
	}
	lower := strings.ToLower(output)
	switch {
	case strings.Contains(lower, "command not found"), strings.Contains(lower, "not found in path"):
		code = "TOOL_NOT_AVAILABLE"
	case strings.Contains(lower, "permission denied"), strings.Contains(lower, "access denied"), strings.Contains(lower, "operation not permitted"), strings.Contains(lower, "a password is required"), strings.Contains(lower, "not allowed to execute"):
		code = "PERMISSION_DENIED"
	case strings.Contains(lower, "not supported"), strings.Contains(lower, "unsupported"), strings.Contains(lower, "unknown parameter"), strings.Contains(lower, "unrecognized option"):
		code = "OPERATION_UNSUPPORTED"
	}
	tool := commandTool(command)
	if strings.HasPrefix(command, "sudo -n ") && (strings.Contains(lower, "sudo: not found") || strings.Contains(lower, "sudo: command not found")) {
		tool = "sudo"
	}
	return &Error{Code: code, Stage: stage, Tool: tool}
}

func commandTool(command string) string {
	f := strings.Fields(command)
	if len(f) >= 3 && f[0] == "sudo" && f[1] == "-n" {
		f = f[2:]
	}
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// Reason returns a safe explanation without library or remote error text.
func Reason(err error) string {
	switch Code(err) {
	case "TOOL_NOT_AVAILABLE":
		return "Required tool is not available in the remote command environment."
	case "TOOL_NOT_EXECUTABLE":
		return "The remote tool exists but could not be executed."
	case "PERMISSION_DENIED":
		return "The configured account or noninteractive sudo policy does not permit this read."
	case "OPERATION_UNSUPPORTED":
		return "The installed tool or device does not support this read operation."
	case "SSH_TIMEOUT_OR_CANCELLED":
		return "The observation timed out or was cancelled."
	case "SSH_OUTPUT_LIMIT":
		return "The remote response exceeded the bounded output limit."
	case "SSH_RUNTIME_INVALID":
		return "The command returned missing, malformed or ambiguous observation data."
	case "PROPERTY_NOT_EXPOSED":
		return "The requested property is not exposed in the command response."
	case "NOT_APPLICABLE":
		return "This observation does not apply to this device or collection vantage."
	case "SSH_HOST_KEY_REJECTED":
		return "The remote SSH host key did not match the configured trust."
	case "SSH_CONNECT_FAILED":
		return "The configured SSH endpoint could not be reached."
	case "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED":
		return "SSH authentication or session negotiation failed."
	default:
		return "The requested read failed; no reliable observation was obtained."
	}
}

func unavailable(err error, tool string) inventory.Observation {
	var e *Error
	if errors.As(err, &e) && e.Tool != "" {
		tool = e.Tool
	}
	return inventory.Observation{State: "unavailable", Values: []string{}, ReasonCode: Code(err), Message: Reason(err), Tool: tool}
}

func missing(tool string) inventory.Observation {
	return unavailable(&Error{Code: "PROPERTY_NOT_EXPOSED"}, tool)
}

func observed(tool string, values ...string) inventory.Observation {
	return inventory.Observation{State: "observed", Values: values, Tool: tool}
}
