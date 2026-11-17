// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"fmt"
	"strings"
	"testing"
)

func TestHostNetworkReasonsExplainUnavailableAndConflictingPrerequisites(t *testing.T) {
	for _, test := range []struct {
		code string
		want string
	}{
		{code: "HOST_NETWORK_TOOL_MISSING_JQ", want: "not installed"},
		{code: "HOST_NETWORK_TOOL_MISSING", want: "not available"},
		{code: "HOST_NETWORK_BACKEND_UNSUPPORTED", want: "outside the supported"},
		{code: "HOST_NETWORK_NETPLAN_UNSUPPORTED", want: "Netplan definition"},
		{code: "HOST_NETWORK_MTU_CAPABILITY_UNAVAILABLE", want: "minimum and maximum MTUs"},
		{code: "HOST_NETWORK_MTU_UNSUPPORTED", want: "at least 1280 bytes"},
		{code: "HOST_NETWORK_BACKEND_OWNERSHIP_UNAVAILABLE", want: "matching generated Netplan"},
		{code: "HOST_NETWORK_MANAGEMENT_PATH_CONFLICT", want: "separate management path"},
		{code: "HOST_NETWORK_IDENTITY_CHANGED", want: "selected identity"},
		{code: "HOST_NETWORK_ROOT_REQUIRED", want: "root access"},
		{code: "HOST_NETWORK_ROUTING_POLICY_UNSUPPORTED", want: "policy-routing"},
		{code: "HOST_NETWORK_SOURCE_INVALID", want: "Netplan source"},
		{code: "HOST_NETWORK_ACCOUNT_UNAVAILABLE", want: "systemd-network"},
		{code: "HOST_NETWORK_SHADOW_UNAVAILABLE", want: "isolated preview"},
		{code: "HOST_NETWORK_GENERATED_METADATA_UNSUPPORTED", want: "ownership or permissions"},
		{code: "HOST_NETWORK_GENERATED_OUTPUT_CONFLICT", want: "outside the selected workload interface"},
	} {
		err := fmt.Errorf("private diagnostic canary: %w", &HostNetworkError{ReasonCode: test.code, Blocking: false})
		if got := Reason(err); !strings.Contains(got, test.want) || strings.Contains(got, "canary") || Code(err) != test.code {
			t.Fatalf("reason for %s must preserve its cause without remote/private text: %q", test.code, got)
		}
	}
}
