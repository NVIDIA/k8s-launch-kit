// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
)

// Only a measured difference from a nonempty retained identity may use this
// marker. A failed read or missing baseline cannot establish replacement.
var errTenantIdentityChanged = errors.New("tenant API or source identity changed")

var errManagementIdentityChanged = errors.New("management API identity changed")

func identityInspectionCause(ctx context.Context, cause error) error {
	if ctx != nil && ctx.Err() != nil {
		return errors.Join(cause, ctx.Err())
	}
	return cause
}

// tenantIdentityDiagnostic projects verification failures without exposing API
// errors or credentials. The original cause remains private for errors.Is/As.
func tenantIdentityDiagnostic(ctx context.Context, cause error) *ReconciliationError {
	cause = identityInspectionCause(ctx, cause)
	code, message := "TENANT_API_IDENTITY_UNVERIFIED", "Tenant API or source identity could not be verified; dependent work remains stopped."
	switch {
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		code, message = "TENANT_API_IDENTITY_CANCELED", "Tenant API identity inspection was canceled or its deadline expired; preserve the operation and current state."
	case errors.Is(cause, errTenantIdentityChanged):
		code, message = "TENANT_API_IDENTITY_CHANGED", "A current tenant API or source identity differs from its retained identity; dependent work remains stopped."
	}
	return &ReconciliationError{ReasonCode: code, Message: message, cause: cause}
}

func managementIdentityDiagnostic(ctx context.Context, cause error) *ReconciliationError {
	cause = identityInspectionCause(ctx, cause)
	code, message := "MANAGEMENT_IDENTITY_UNVERIFIED", "Management API identity could not be verified; dependent work remains stopped."
	switch {
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		code, message = "MANAGEMENT_IDENTITY_CANCELED", "Management API identity inspection was canceled or its deadline expired; preserve the operation and current state."
	case errors.Is(cause, errManagementIdentityChanged):
		code, message = "MANAGEMENT_IDENTITY_CHANGED", "A current management API identity differs from its retained identity; dependent work remains stopped."
	}
	return &ReconciliationError{ReasonCode: code, Message: message, cause: cause}
}
