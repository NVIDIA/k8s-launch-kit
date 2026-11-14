// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

const (
	hbnCallerRBACPrefix = "dpf.hbn.management.caller-rbac."
)

// HBNPermissionObserver measures the current Launch Kit caller's management
// permissions for the resolved HBN workflow. It must not report controller
// service-account permissions through this check family.
type HBNPermissionObserver func(context.Context, config.Request, *inventory.Inventory) []inventory.Check

func hbnPermissionChecks(ctx context.Context, request config.Request, current *inventory.Inventory, observe HBNPermissionObserver) []inventory.Check {
	subject := strings.TrimSpace(request.DPFNamespace)
	if subject == "" {
		subject = "management"
	}
	unknown := func(reason, message string) []inventory.Check {
		return []inventory.Check{{
			ID: hbnCallerRBACPrefix + "observation", Subject: subject,
			Vantage: "executor", RequiredPhase: "deploy", State: "unknown",
			ReasonCode: reason, Message: message,
		}}
	}
	if observe == nil {
		return unknown("CALLER_RBAC_OBSERVER_UNAVAILABLE", "The current Launch Kit caller's management permissions were not inspected.")
	}
	if ctx == nil || ctx.Err() != nil {
		return unknown("CALLER_RBAC_INSPECTION_CANCELLED", "Caller permission inspection was cancelled before a complete result was available.")
	}
	if request.DPFNamespace == "" || request.DPFNamespace != strings.TrimSpace(request.DPFNamespace) {
		return unknown("CALLER_RBAC_SCOPE_UNAVAILABLE", "The resolved DPF namespace is required to inspect caller permissions.")
	}
	if current == nil || current.Management == nil || current.Management.APIEndpoint == "" || current.Management.SystemNamespaceUID == "" || current.Management.TrustDigest == "" {
		return unknown("MANAGEMENT_IDENTITY_UNAVAILABLE", "Current management API endpoint, cluster identity, and TLS trust identity are required to inspect caller permissions.")
	}
	checks := observe(ctx, request, current)
	if ctx.Err() != nil {
		return unknown("CALLER_RBAC_INSPECTION_CANCELLED", "Caller permission inspection did not complete before discovery was cancelled or timed out.")
	}
	if len(checks) == 0 {
		return unknown("CALLER_RBAC_RESULT_EMPTY", "Caller permission inspection returned no required authorization checks.")
	}
	seen := make(map[string]struct{}, len(checks))
	for _, check := range checks {
		if !strings.HasPrefix(check.ID, hbnCallerRBACPrefix) || len(check.ID) == len(hbnCallerRBACPrefix) || strings.TrimSpace(check.Subject) == "" || check.Vantage != "executor" || check.RequiredPhase != "deploy" || (check.State != "passed" && check.State != "failed" && check.State != "unknown") || strings.TrimSpace(check.ReasonCode) == "" || strings.TrimSpace(check.Message) == "" {
			return unknown("CALLER_RBAC_RESULT_INVALID", "Caller permission inspection returned an incomplete or unsupported authorization check.")
		}
		if _, exists := seen[check.ID]; exists {
			return unknown("CALLER_RBAC_RESULT_INVALID", "Caller permission inspection returned duplicate authorization checks.")
		}
		seen[check.ID] = struct{}{}
	}
	return checks
}
