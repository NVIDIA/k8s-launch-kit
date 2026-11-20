// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func activationBootstrapFailure(ctx context.Context, journal *executionJournal, cause error) error {
	message := "Management bootstrap changed at the activation boundary." + bootstrapFailureDiagnostic(cause)
	if !bootstrapConfigPending(cause) || ctx.Err() != nil {
		return activationBoundaryFailure(ctx, journal, "ACTIVATION_BOOTSTRAP_CHANGED", message, cause)
	}
	message = "The retained management bootstrap is not Ready; dependent effects remain stopped." + bootstrapFailureDiagnostic(cause)
	const code = "ACTIVATION_BOOTSTRAP_NOT_READY"
	if err := journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "activation", State: "waiting", ReasonCode: code, Message: message}); err != nil {
		return err
	}
	ui.FromContext(ctx).Info("[WAITING] %s (%s)", message, code)
	return reconcileError("ACTIVATION_BOOTSTRAP_CHANGED", message, cause)
}

// A nil return means readiness recovered, not that an old request/proof is
// usable. Its pre-claim caller must return to the outer observation loop.
func deferActivationBootstrap(ctx context.Context, client identityResourceReader, verification *controlPlaneVerificationInputs, journal *executionJournal, interval time.Duration, cause error) error {
	if !bootstrapConfigPending(cause) {
		return cause
	}
	if err := waitManagementBootstrap(ctx, client, verification.execution, verification.management, journal, interval); err != nil {
		return activationBootstrapFailure(ctx, journal, err)
	}
	return nil
}

// Only no-effect stage entry/completion boundaries use this owner. After any
// readiness wait it repeats the complete control, physical and enrollment fence.
// Submission and acknowledgement continue to use the immediate strict fence.
func waitActivationBoundary(ctx context.Context, client activationBoundaryReader, verification *controlPlaneVerificationInputs, tenant tenantVerificationReader, selected []activationSelectedDevice, deploymentUID types.UID, objects []*unstructured.Unstructured, journal *executionJournal, interval, timeout time.Duration) error {
	for {
		err := verifyActivationBoundary(ctx, client, verification, tenant, selected, deploymentUID, objects, journal, timeout)
		if !bootstrapConfigPending(err) {
			return err
		}
		if err := deferActivationBootstrap(ctx, client, verification, journal, interval, err); err != nil {
			return err
		}
	}
}
