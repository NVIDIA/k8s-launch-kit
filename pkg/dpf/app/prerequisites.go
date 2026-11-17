// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifacts"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
)

// DeploymentPrerequisites refreshes physical observations and authorization for
// the exact retained plan. Resume and post-activation defer lifecycle eligibility
// to deployment; only post-activation omits origin artifact byte reads.
func DeploymentPrerequisites(retained *bundle.Bundle, runDir string, resume bool) deployment.PrerequisiteVerifier {
	return deploymentPrerequisitesWithTransports(retained, runDir, resume, nil)
}

// The private transport seam exercises the native collection/publication/plan
// comparison with bounded test transports while retaining production policies.
func deploymentPrerequisitesWithTransports(retained *bundle.Bundle, runDir string, resume bool, configure func(*discovery.Dependencies)) deployment.PrerequisiteVerifier {
	return func(ctx context.Context, request config.Request, plan *generation.Plan, phase deployment.PrerequisitePhase) (deployment.PrerequisiteVerification, error) {
		if retained == nil || plan == nil || plan.Digest() != retained.Index().BundleDigest {
			return deployment.PrerequisiteVerification{}, fmt.Errorf("fresh prerequisites require the validated retained plan")
		}
		if phase != deployment.BeforeActivation && phase != deployment.AfterActivation {
			return deployment.PrerequisiteVerification{}, fmt.Errorf("unsupported prerequisite phase")
		}
		store, err := artifacts.New(request.Input, filepath.Join(runDir, string(phase)+"-discovered.yaml"), true)
		if err != nil {
			return deployment.PrerequisiteVerification{}, err
		}
		deps := retainedCollectors(store.RunDir, retained, plan, resume, phase)
		if configure != nil {
			configure(&deps)
		}
		report := discovery.Collect(ctx, request, store.RunID(), deps)
		publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err = store.Publish(publishCtx, &report)
		cancel()
		if err != nil {
			return deployment.PrerequisiteVerification{}, fmt.Errorf("save fresh deployment discovery: %w", err)
		}
		if ctx.Err() != nil {
			return deployment.PrerequisiteVerification{}, ctx.Err()
		}
		if report.Execution != "completed" || report.Discovery.Outcome != "complete" || len(report.Errors) > 0 || report.Readiness == nil {
			return deployment.PrerequisiteVerification{}, fmt.Errorf("fresh deployment discovery is incomplete; inspect %s", store.RunDir)
		}

		if !resume && phase == deployment.BeforeActivation && report.Readiness.State != "ready" {
			return deployment.PrerequisiteVerification{Inventory: report.Inventory, Checks: report.Readiness.Checks}, fmt.Errorf("fresh HBN-BGP prerequisites are blocked; inspect %s", store.RunDir)
		}
		current := request.Input
		current.Site.Inventory = report.Inventory
		if err := VerifyDeploymentPlan(current, retained, plan); err != nil {
			return deployment.PrerequisiteVerification{}, err
		}
		return deployment.PrerequisiteVerification{Inventory: report.Inventory, Checks: report.Readiness.Checks}, nil
	}
}
