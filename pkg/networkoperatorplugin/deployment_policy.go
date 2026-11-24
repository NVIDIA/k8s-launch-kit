// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	pkgerrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/hostplatform"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/preflight"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// deploymentPolicy owns the differing Host deployment phases. Applying desired
// objects, refreshing observations and polling remain in the common coordinator.
// Readiness is selected separately through resourceReadPolicy.
type deploymentPolicy interface {
	prepare(*bundle.Bundle) (deploymentObjects, error)
	checkPrerequisites(context.Context, client.Client, *bundle.Bundle, *config.LaunchKitConfig) error
	bootstrap(context.Context, client.Client, *bundle.Bundle, DeployOptions, helmValuesInstaller) error
	preflight(context.Context, client.Client, *bundle.Bundle, DeployOptions) error
	configureAfterPreflight(context.Context, client.Client, *config.LaunchKitConfig, deploymentObjects, bool) error
	canRetryDependentAPI(*unstructured.Unstructured, *unstructured.Unstructured) bool
}

type kubernetesDeploymentPolicy struct{}
type openShiftDeploymentPolicy struct{}

func deploymentPolicyFor(platform hostplatform.Selection, cfg *config.LaunchKitConfig) (deploymentPolicy, error) {
	if err := platform.Validate(); err != nil {
		return nil, err
	}
	if platform == hostplatform.OpenShift {
		if cfg == nil {
			return nil, fmt.Errorf("OpenShift deployment requires a cluster config")
		}
		return openShiftDeploymentPolicy{}, nil
	}
	return kubernetesDeploymentPolicy{}, nil
}

func (kubernetesDeploymentPolicy) prepare(artifacts *bundle.Bundle) (deploymentObjects, error) {
	return prepareDeploymentObjects(artifacts, false)
}

func (openShiftDeploymentPolicy) prepare(artifacts *bundle.Bundle) (deploymentObjects, error) {
	return prepareDeploymentObjects(artifacts, true)
}

func (kubernetesDeploymentPolicy) checkPrerequisites(context.Context, client.Client, *bundle.Bundle, *config.LaunchKitConfig) error {
	return nil
}

func (openShiftDeploymentPolicy) checkPrerequisites(ctx context.Context, c client.Client, artifacts *bundle.Bundle, cfg *config.LaunchKitConfig) error {
	if _, present := artifacts.ValuesContent(); present {
		return pkgerrors.NewValidationError("OpenShift deployment contains values.yaml; regenerate with --flavor ocp and remove stale Helm values", nil, "Remove stale values.yaml")
	}
	return operatorReadinessError(CheckOCPOperators(ctx, c, cfg, true), cfg)
}

func (kubernetesDeploymentPolicy) bootstrap(ctx context.Context, c client.Client, artifacts *bundle.Bundle, opts DeployOptions, install helmValuesInstaller) error {
	// Ownership inspection precedes both Helm and overwrite remediation.
	if opts.OverwriteExisting {
		in, err := buildPreflightInputs(c, artifacts, opts)
		if err != nil {
			return err
		}
		if err := preflight.CheckSecondaryNetworkRemovals(ctx, in); err != nil {
			return fmt.Errorf("secondary-network ownership check before overwrite: %w", err)
		}
	}
	return runHelmInstallPhaseWithInstaller(ctx, artifacts, opts, ui.FromContext(ctx), install)
}

// OpenShift operators are installed externally through OLM.
func (openShiftDeploymentPolicy) bootstrap(context.Context, client.Client, *bundle.Bundle, DeployOptions, helmValuesInstaller) error {
	log.Log.V(1).Info("Network Operator Helm management disabled; skipping operator install")
	return nil
}

func (kubernetesDeploymentPolicy) preflight(ctx context.Context, c client.Client, artifacts *bundle.Bundle, opts DeployOptions) error {
	in, err := buildPreflightInputs(c, artifacts, opts)
	if err != nil {
		return err
	}
	return evaluatePreflightResults(ctx, in, preflight.RunAll(ctx, in), opts, ui.FromContext(ctx))
}

func (openShiftDeploymentPolicy) preflight(ctx context.Context, c client.Client, artifacts *bundle.Bundle, opts DeployOptions) error {
	// This local copy adapts external operator ownership without changing the
	// coordinator's options or making serialized config a selection authority.
	opts.SkipHelmChart = true
	in, err := buildPreflightInputs(c, artifacts, opts)
	if err != nil {
		return err
	}
	// Run every check before filtering: scans, diagnostics and cancellation
	// retain their existing behavior even for externally owned stray CRs.
	results := preflight.RunAll(ctx, in)
	filtered := results[:0]
	for _, result := range results {
		if result.Code != preflight.CodeStrayCRs {
			filtered = append(filtered, result)
		}
	}
	return evaluatePreflightResults(ctx, in, filtered, opts, ui.FromContext(ctx))
}

func (kubernetesDeploymentPolicy) configureAfterPreflight(context.Context, client.Client, *config.LaunchKitConfig, deploymentObjects, bool) error {
	return nil
}

func (openShiftDeploymentPolicy) configureAfterPreflight(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, selected deploymentObjects, dryRun bool) error {
	if selected.ocpOwner == "" {
		return applyOCPOperatorConfiguration(ctx, c, cfg, selected.operatorConfigs, dryRun)
	}
	if !dryRun {
		namespace := ocpCleanNamespaces(cfg).operator
		if err := invalidateOCPCleanReceipt(ctx, c, namespace, selected.ocpOwner); err != nil {
			return err
		}
	}
	return applyOCPOperatorConfigurationForCohort(ctx, c, cfg, selected.operatorConfigs, dryRun, selected.ocpOwner)
}

func (kubernetesDeploymentPolicy) canRetryDependentAPI(*unstructured.Unstructured, *unstructured.Unstructured) bool {
	return false
}

func (openShiftDeploymentPolicy) canRetryDependentAPI(obj, ncp *unstructured.Unstructured) bool {
	return obj.GetAPIVersion() == "nv-ipam.nvidia.com/v1alpha1" && obj.GetKind() == "IPPool" && ncp != nil
}
