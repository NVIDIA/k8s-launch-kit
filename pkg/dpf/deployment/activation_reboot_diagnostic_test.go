// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
)

func TestUncertainRebootPersistsFiniteSSHReasonWithoutPrivateText(t *testing.T) {
	const canary = "private-password-and-remote-output-canary"
	var typedNil *sshprobe.Error
	for _, test := range []struct {
		name        string
		cause       error
		code, stage string
	}{
		{"nil", nil, "", ""}, {"typed nil", typedNil, "", ""}, {"plain error", errors.New(canary), "", ""},
		{"unknown code", &sshprobe.Error{Code: canary, Stage: canary}, "", ""},
		{"handshake", fmt.Errorf("%s: %w", canary, &sshprobe.Error{Code: "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED", Stage: "handshake", Tool: canary}), "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED", "handshake"},
		{"connect", &sshprobe.Error{Code: "SSH_CONNECT_FAILED", Stage: "dial"}, "SSH_CONNECT_FAILED", "dial"},
		{"known trust failure", &sshprobe.Error{Code: "SSH_HOST_KEY_REJECTED", Stage: "handshake"}, "SSH_HOST_KEY_REJECTED", "handshake"},
		{"unknown stage", &sshprobe.Error{Code: "SSH_COMMAND_FAILED", Stage: canary, Tool: canary}, "SSH_COMMAND_FAILED", "unclassified"},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := &managementMemoryFile{}
			journal := &executionJournal{file: file, digest: "test"}
			result := activationFailure(journal, "EXTERNAL_HOST_REBOOT_UNCERTAIN", "The reboot completion is unverified.", test.cause)
			if result == nil || strings.Contains(result.Error()+file.String(), canary) {
				t.Fatalf("private cause escaped: %v", result)
			}
			if test.code != "" {
				wanted := "SSH cause " + test.code + " at " + test.stage + ":"
				if !strings.Contains(result.Error(), wanted) || !strings.Contains(file.String(), wanted) {
					t.Fatalf("safe cause absent: %v journal=%s", result, file.String())
				}
			}
			if test.code == "" && strings.Contains(file.String(), "SSH cause") {
				t.Fatal("unclassified raw cause was persisted")
			}
			if test.cause != nil && !errors.Is(result, test.cause) {
				t.Fatal("private causal chain lost")
			}
		})
	}
}

func TestRebootSSHDiagnosticUsesOnlySupportedStaticCategories(t *testing.T) {
	for _, code := range []string{"SSH_CONFIGURATION_INVALID", "SSH_TRUST_INVALID", "SSH_IDENTITY_INVALID", "SSH_REBOOT_NOT_STARTED", "SSH_CONNECT_FAILED", "SSH_AUTHENTICATION_OR_HANDSHAKE_FAILED", "SSH_HOST_KEY_REJECTED", "SSH_COMMAND_FAILED", "SSH_TIMEOUT_OR_CANCELLED", "SSH_OUTPUT_LIMIT", "SSH_RUNTIME_INVALID", "TOOL_NOT_AVAILABLE", "TOOL_NOT_EXECUTABLE", "PERMISSION_DENIED", "DEVICE_ACCESS_FAILED", "OPERATION_UNSUPPORTED"} {
		if got := externalRebootSSHDiagnostic(&sshprobe.Error{Code: code, Stage: "external-reboot-system-level"}); !strings.Contains(got, code) || !strings.Contains(got, "external-reboot-system-level") {
			t.Fatalf("missing bounded reason for %s", code)
		}
	}
	file := &managementMemoryFile{}
	journal := &executionJournal{file: file, digest: "test"}
	_ = activationFailure(journal, "ACTIVATION_SCOPE_CHANGED", "Current scope differs.", &sshprobe.Error{Code: "SSH_CONNECT_FAILED", Stage: "dial"})
	if strings.Contains(file.String(), "SSH cause") {
		t.Fatal("reboot-specific diagnostic changed another failure boundary")
	}
}
