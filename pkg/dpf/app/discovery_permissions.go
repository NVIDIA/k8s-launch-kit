// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/chartfetch"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

type callerPermissionClient interface {
	CheckCallerPermissions(context.Context, deployment.ManagementPreflightRequest) deployment.PreflightResult
}

// HBNPermissionObserver joins the generation and authorization boundaries in
// the DPF application layer. Discovery neither imports deployment nor maintains a
// second list of permissions or chart-rendering rules.
type HBNPermissionObserver struct {
	fetchChart func(context.Context, string) ([]byte, error)
	newClient  func(context.Context, config.Request, generation.ManagementIdentity, string) (callerPermissionClient, error)
}

// NewHBNPermissionObserver composes bounded chart retrieval and identity-bound
// management authorization against the generated or retained deployment bundle.
func NewHBNPermissionObserver() HBNPermissionObserver {
	return HBNPermissionObserver{
		fetchChart: (chartfetch.Downloader{}).Fetch,
		newClient: func(ctx context.Context, request config.Request, identity generation.ManagementIdentity, version string) (callerPermissionClient, error) {
			return deployment.NewAPIClient(ctx, request, identity, version)
		},
	}
}

// Observe derives authorization checks from current observations and a newly
// generated, integrity-checked bundle. Unknown or incomplete evidence stays unknown.
func (o HBNPermissionObserver) Observe(ctx context.Context, request config.Request, current *inventory.Inventory) []inventory.Check {
	unknown := func(code, message string) []inventory.Check {
		return callerRBACUnknown(request, code, message)
	}
	if ctx == nil || current == nil || current.Management == nil || o.fetchChart == nil || o.newClient == nil {
		return unknown("CALLER_RBAC_INPUT_UNAVAILABLE", "Caller authorization requires current management discovery and the deployment-plan checker.")
	}
	if ctx.Err() != nil {
		return unknown("CALLER_RBAC_CANCELLED", "Caller authorization was cancelled before the deployment plan could be inspected.")
	}
	// Site is a value: replacing its inventory must not overwrite the user's
	// prior inventory or reinterpret it as this run's observations.
	input := request.Input
	input.Site.Inventory = current
	plan, err := ResolveGeneration(input, nil)
	if err != nil {
		return unknown("CALLER_RBAC_PLAN_UNAVAILABLE", "Deployment permissions cannot be checked until the selected release has a supported provisioning recipe, site allocations are complete and device identities are verified.")
	}
	progress := ui.FromContext(ctx)
	progress.Info("[RUNNING] HBN-BGP caller permissions: retrieving and validating the selected deployment charts")
	plan.OperatorArchive, err = o.fetchChart(ctx, plan.OperatorChartURL)
	if ctx.Err() != nil {
		return unknown("CALLER_RBAC_CANCELLED", "Caller authorization was cancelled while retrieving the deployment charts.")
	}
	if err != nil {
		code, message := callerChartDownloadFailure("OPERATOR", "DPF operator", err)
		return unknown(code, message)
	}
	hbn := plan.Workload.Generation
	plan.HBNChartArchive, err = o.fetchChart(ctx, strings.TrimSuffix(hbn.ChartRepository, "/")+"/charts/"+hbn.ChartName+"-"+hbn.ChartVersion+".tgz")
	if ctx.Err() != nil {
		return unknown("CALLER_RBAC_CANCELLED", "Caller authorization was cancelled while retrieving the deployment charts.")
	}
	if err != nil {
		code, message := callerChartDownloadFailure("HBN", "HBN", err)
		return unknown(code, message)
	}
	retained, err := generation.Generate(plan.Inputs)
	if err != nil {
		return unknown("CALLER_RBAC_PLAN_INVALID", "The selected chart digests, schemas or deployment inputs failed validation; caller permissions remain unverified.")
	}
	if ctx.Err() != nil {
		return unknown("CALLER_RBAC_CANCELLED", "Caller authorization was cancelled while validating the deployment plan.")
	}
	return o.checkPermissions(ctx, request, plan.Management, plan.Operator.KubernetesVersion, retained)
}

// ObserveRetained checks current caller authorization against the exact bundle
// being deployed, after binding current observations and supported desired
// settings. It never regenerates a new recipe from the current release catalog.
func (o HBNPermissionObserver) ObserveRetained(retained *bundle.Bundle, plan *generation.Plan) discovery.HBNPermissionObserver {
	return func(ctx context.Context, request config.Request, current *inventory.Inventory) []inventory.Check {
		if ctx == nil || current == nil || current.Management == nil || plan == nil || retained == nil || o.newClient == nil {
			return callerRBACUnknown(request, "CALLER_RBAC_INPUT_UNAVAILABLE", "Caller authorization requires a validated retained plan and current management discovery.")
		}
		if ctx.Err() != nil {
			return callerRBACUnknown(request, "CALLER_RBAC_CANCELLED", "Caller authorization was cancelled before the retained plan could be inspected.")
		}
		input := request.Input
		input.Site.Inventory = current
		if err := VerifyDeploymentPlan(input, retained, plan); err != nil {
			return callerRBACUnknown(request, "CALLER_RBAC_PLAN_UNAVAILABLE", "Current selected identities or supported settings do not match the retained deployment plan; caller permissions remain unverified.")
		}
		intent := plan.Intent()
		return o.checkPermissions(ctx, request, intent.Management, intent.KubernetesVersion, retained)
	}
}

func callerRBACUnknown(request config.Request, code, message string) []inventory.Check {
	return []inventory.Check{{ID: "dpf.hbn.management.caller-rbac.observation", Subject: request.DPFNamespace,
		Vantage: "executor", RequiredPhase: "deploy", State: "unknown", ReasonCode: code, Message: message}}
}

func (o HBNPermissionObserver) checkPermissions(ctx context.Context, request config.Request, identity generation.ManagementIdentity, kubernetesVersion string, retained *bundle.Bundle) []inventory.Check {
	unknown := func(code, message string) []inventory.Check {
		return callerRBACUnknown(request, code, message)
	}
	client, err := o.newClient(ctx, request, identity, kubernetesVersion)
	if err != nil || client == nil {
		return unknown("CALLER_RBAC_MANAGEMENT_UNAVAILABLE", "The configured management credentials could not be bound to this discovery's cluster identity and TLS trust.")
	}
	ui.FromContext(ctx).Info("[RUNNING] HBN-BGP caller permissions: checking scoped management API authorizations")
	result := client.CheckCallerPermissions(ctx, deployment.ManagementPreflightRequest{Bundle: retained})
	if ctx.Err() != nil {
		return unknown("CALLER_RBAC_CANCELLED", "Caller authorization was cancelled; repeat discovery to obtain a complete observation.")
	}
	if len(result.Checks) == 0 {
		return unknown("CALLER_RBAC_RESULT_INCOMPLETE", "The deployment-plan checker returned no authorization decisions.")
	}
	checks := make([]inventory.Check, 0, len(result.Checks))
	allPassed := true
	for _, check := range result.Checks {
		state, reason := check.State, check.ReasonCode
		if state == "blocked" {
			state = "unknown"
			if reason == "CALLER_PERMISSION_MISSING" {
				state = "failed"
			}
		}
		if state == "passed" && reason == "" {
			reason = "CALLER_PERMISSION_VERIFIED"
		}
		if check.State != "passed" {
			allPassed = false
		}
		checks = append(checks, inventory.Check{ID: "dpf.hbn.management.caller-rbac." + check.ID,
			Subject: request.DPFNamespace, Vantage: "executor", RequiredPhase: "deploy",
			State: state, ReasonCode: reason, Message: check.Message})
	}
	if result.Ready != allPassed {
		return unknown("CALLER_RBAC_RESULT_INCOMPLETE", "The deployment-plan authorization summary contradicts its individual decisions.")
	}
	return checks
}

// callerChartDownloadFailure exposes only the downloader's safe diagnostics.
// Arbitrary injected errors may contain credentials, so retain a generic reason.
func callerChartDownloadFailure(chartCode, chartName string, err error) (string, string) {
	prefix := "CALLER_RBAC_" + chartCode + "_CHART_"
	var failure *chartfetch.Error
	if errors.As(err, &failure) {
		return prefix + failure.Code(), "The selected " + chartName + " chart could not be retrieved: " + failure.Error() + "."
	}
	return prefix + "UNAVAILABLE", "The selected " + chartName + " chart could not be retrieved; transport details were omitted."
}
