// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
)

// checkKamajiCompatibility recollects the external producer using the trusted
// catalog rule. Discovery and retained bundle metadata cannot replace this gate.
func (c *APIClient) checkKamajiCompatibility(ctx context.Context, namespace, releaseVersion string) PreflightCheck {
	if c == nil {
		return blockedPreflight("external.kamaji.compatibility", "MANAGEMENT_CLIENT_UNAVAILABLE", "The bound management API client is unavailable.")
	}
	result := c.observeKamajiCompatibility(ctx, namespace, releaseVersion)
	if result.State != "passed" {
		return blockedPreflight("external.kamaji.compatibility", result.ReasonCode, result.Message)
	}
	return PreflightCheck{ID: "external.kamaji.compatibility", State: result.State, ReasonCode: result.ReasonCode, Message: result.Message}
}

func (c *APIClient) observeKamajiCompatibility(ctx context.Context, namespace, releaseVersion string) managementprobe.KamajiCompatibilityResult {
	if c == nil {
		return managementprobe.KamajiCompatibilityResult{State: "unknown", ReasonCode: "MANAGEMENT_CLIENT_UNAVAILABLE", Message: "The bound management API client is unavailable.", Namespace: namespace, Release: releaseVersion}
	}
	return (managementprobe.Collector{Kubernetes: c.access.Client}).CollectKamajiCompatibility(ctx, namespace, releaseVersion)
}
