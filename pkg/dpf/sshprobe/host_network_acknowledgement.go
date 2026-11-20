// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sshprobe

import (
	"context"
	"errors"
)

func hostNetworkAcknowledgementFailure(ctx context.Context, cause error) error {
	kind := "TRANSPORT_FAILED"
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(cause, context.Canceled) {
		kind = "CANCELED"
	} else if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(cause, context.DeadlineExceeded) {
		kind = "DEADLINE"
	} else if Code(cause) == "SSH_TIMEOUT_OR_CANCELLED" {
		kind = "COMMAND_TIMEOUT"
	}
	return &HostNetworkError{ReasonCode: "HOST_NETWORK_ACKNOWLEDGEMENT_UNKNOWN", Blocking: true, ackCause: kind, cause: errors.Join(cause, ctx.Err())}
}

// HostNetworkAcknowledgementDiagnostic projects only an owned finite cause.
// It never exposes remote output, payloads or an arbitrary wrapped error message.
func HostNetworkAcknowledgementDiagnostic(err error) string {
	var failure *HostNetworkError
	if !errors.As(err, &failure) || failure == nil || failure.ReasonCode != "HOST_NETWORK_ACKNOWLEDGEMENT_UNKNOWN" {
		return ""
	}
	var message string
	switch failure.ackCause {
	case "CANCELED":
		message = "The caller canceled while awaiting the host transaction acknowledgement."
	case "DEADLINE":
		message = "The caller's operation deadline expired while awaiting the host transaction acknowledgement."
	case "COMMAND_TIMEOUT":
		message = "The SSH command deadline expired while awaiting the host transaction acknowledgement."
	case "TRANSPORT_FAILED":
		message = "The SSH transport did not return a successful host transaction acknowledgement."
	case "RESPONSE_INVALID":
		message = "The host transaction response did not contain one valid protocol receipt."
	case "REQUEST_ID_MISMATCH":
		message = "The host transaction response did not match the exact requested transaction."
	default:
		return ""
	}
	return " HOST_NETWORK_ACK_CAUSE_" + failure.ackCause + ": " + message
}
