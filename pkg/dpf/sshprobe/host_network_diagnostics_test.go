// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"regexp"
	"strings"
	"testing"
)

func TestHostNetworkReadReasonProtocol(t *testing.T) {
	for _, code := range []string{
		"HOST_NETWORK_READ_IDENTITY_DMIDECODE_EXIT_7",
		"HOST_NETWORK_READ_GENERATOR_CONTEXT_BASH_EXIT_7",
		"HOST_NETWORK_READ_GENERATOR_CONTEXT_READLINK_EXIT_127",
		"HOST_NETWORK_READ_GENERATOR_CONTEXT_SHA256SUM_EXIT_1",
		"HOST_NETWORK_READ_MANAGEMENT_ROUTE_IP_EXIT_127",
		"HOST_NETWORK_READ_MANAGEMENT_ROUTE_JQ_EXIT_2",
		"HOST_NETWORK_READ_PCI_BINDING_CHANGED",
		"HOST_NETWORK_READ_NETWORK_FILE_METADATA_INVALID",
	} {
		if got := Code(networkError(code, false)); got != code {
			t.Fatalf("valid code %q rejected as %q", code, got)
		}
		if got := Reason(networkError(code, false)); strings.Contains(got, "requested read failed;") {
			t.Fatalf("missing reason: %s", code)
		}
	}
	for _, code := range []string{
		"HOST_NETWORK_READ_UNKNOWN_IP_EXIT_7",
		"HOST_NETWORK_READ_IDENTITY_UNTRUSTED_EXIT_7",
		"HOST_NETWORK_READ_IDENTITY_DMIDECODE_EXIT_256",
		"HOST_NETWORK_READ_IDENTITY_DMIDECODE_EXIT_-1",
		"HOST_NETWORK_READ_IDENTITY_DMIDECODE_EXIT_007",
		"HOST_NETWORK_READ_IDENTITY_DMIDECODE_EXIT_7_SECRET",
		"HOST_NETWORK_READ_IDENTITY_DMIDECODE_EXIT_secret",
		"HOST_NETWORK_READ_IDENTITY_DMIDECODE_EXIT_7\nsecret",
		"HOST_NETWORK_READ_NETWORK_FILE_UNTRUSTED",
	} {
		if got := Code(networkError(code, false)); got != "HOST_NETWORK_RESPONSE_INVALID" {
			t.Fatalf("invalid code %q accepted as %q", code, got)
		}
		if strings.Contains(Reason(networkError(code, false)), "secret") {
			t.Fatal("unsafe reason reflected")
		}
	}
}

// Every producer/tool pair emitted by the embedded backend must be understood
// by the receiver. This catches accidental lower-case or unrecognized codes.
func TestHostNetworkBackendReadReasonCompatibility(t *testing.T) {
	pairs := regexp.MustCompile(`(?m)^\s*capture_command ([A-Z0-9_]+) ([A-Z0-9_]+) `).FindAllStringSubmatch(hostNetworkBackend, -1)
	if len(pairs) < 30 {
		t.Fatal("capture operations unexpectedly missing")
	}
	for _, pair := range pairs {
		for _, status := range []string{"0", "1", "127", "255"} {
			code := "HOST_NETWORK_READ_" + pair[1] + "_" + pair[2] + "_EXIT_" + status
			if !validHostNetworkReadReason(code) {
				t.Errorf("backend read pair not supported: %s", code)
			}
		}
	}
}

func TestHostNetworkDeadlineReason(t *testing.T) {
	reason := Reason(networkError("HOST_NETWORK_DEADLINE_REQUIRED", false))
	if !strings.Contains(reason, "caller context with a deadline") || !strings.Contains(reason, "no SSH read was started") {
		t.Fatalf("deadline reason: %q", reason)
	}
}

func TestHostNetworkMatchingContextReasons(t *testing.T) {
	for _, code := range []string{"HOST_NETWORK_GENERATOR_CONTEXT_UNSUPPORTED", "HOST_NETWORK_GENERATOR_CONTEXT_CHANGED"} {
		if reason := Reason(networkError(code, false)); !strings.Contains(reason, "interface") {
			t.Fatalf("context failure must retain its safe cause: %s: %s", code, reason)
		}
	}
}

func TestHostNetworkWaitOnlineFailureReasons(t *testing.T) {
	for _, code := range []string{"HOST_NETWORK_WAIT_ONLINE_UNSUPPORTED", "HOST_NETWORK_WAIT_ONLINE_CONTEXT_UNSUPPORTED", "HOST_NETWORK_WAIT_ONLINE_CONTRIBUTION_INVALID"} {
		if reason := Reason(networkError(code, false)); !strings.Contains(reason, "wait-online") {
			t.Fatalf("missing safe wait-online reason: %s", reason)
		}
	}
}

func TestHostNetworkWaitOnlineUnavailableReason(t *testing.T) {
	reason := Reason(networkError("HOST_NETWORK_WAIT_ONLINE_UNSUPPORTED", false))
	for _, cause := range []string{"could not be read safely", "unsafe ancestry", "unsupported bytes or command structure"} {
		if !strings.Contains(reason, cause) {
			t.Fatalf("wait-online diagnostic omits %q: %q", cause, reason)
		}
	}
}

func TestHostNetworkSourceChangedReason(t *testing.T) {
	reason := Reason(networkError("HOST_NETWORK_SOURCE_CHANGED", false))
	for _, phrase := range []string{"Netplan source or bound host-network context changed", "merged configuration", "source and bound context are stable"} {
		if !strings.Contains(reason, phrase) {
			t.Fatalf("source consistency diagnostic omits %q: %q", phrase, reason)
		}
	}
}

func TestHostNetworkOwnershipConflictReason(t *testing.T) {
	reason := Reason(networkError("HOST_NETWORK_OWNERSHIP_CONFLICT", true))
	for _, phrase := range []string{"expected presence", "protected ownership", "exact bytes"} {
		if !strings.Contains(reason, phrase) {
			t.Fatalf("ownership diagnostic omits %q: %q", phrase, reason)
		}
	}
}
