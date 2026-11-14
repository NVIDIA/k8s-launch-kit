// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package dpf

import (
	"context"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

type callerPermissionClient interface {
	CheckCallerPermissions(context.Context, deployment.ManagementPreflightRequest) deployment.PreflightResult
}

// hbnPermissionObserver joins the generation and authorization boundaries in
// the target adapter. Discovery neither imports deployment nor maintains a
// second list of permissions or chart-rendering rules.
type hbnPermissionObserver struct {
	fetchChart func(context.Context, string) ([]byte, error)
	newClient  func(context.Context, config.Request, generation.ManagementIdentity, string) (callerPermissionClient, error)
}

func newHBNPermissionObserver() hbnPermissionObserver {
	return hbnPermissionObserver{
		fetchChart: downloadGenerationChart,
		newClient: func(ctx context.Context, request config.Request, identity generation.ManagementIdentity, version string) (callerPermissionClient, error) {
			return deployment.NewAPIClient(ctx, request, identity, version)
		},
	}
}

func (o hbnPermissionObserver) observe(ctx context.Context, request config.Request, current *inventory.Inventory) []inventory.Check {
	unknown := func(code, message string) []inventory.Check {
		return []inventory.Check{{ID: "dpf.hbn.management.caller-rbac.observation", Subject: request.DPFNamespace,
			Vantage: "executor", RequiredPhase: "deploy", State: "unknown", ReasonCode: code, Message: message}}
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
	plan, err := generationInputs(input)
	if err != nil {
		return unknown("CALLER_RBAC_PLAN_UNAVAILABLE", "Complete the provisioning inputs and current selected-device identities before deployment permissions can be derived.")
	}
	progress := ui.FromContext(ctx)
	progress.Info("[RUNNING] HBN-BGP caller permissions: retrieving and validating the selected deployment charts")
	plan.OperatorArchive, err = o.fetchChart(ctx, plan.OperatorChartURL)
	if ctx.Err() != nil {
		return unknown("CALLER_RBAC_CANCELLED", "Caller authorization was cancelled while retrieving the deployment charts.")
	}
	if err != nil {
		return unknown("CALLER_RBAC_OPERATOR_CHART_UNAVAILABLE", "The selected DPF operator chart could not be retrieved to derive deployment permissions.")
	}
	hbn := plan.Workload.Generation
	plan.HBNChartArchive, err = o.fetchChart(ctx, strings.TrimSuffix(hbn.ChartRepository, "/")+"/charts/"+hbn.ChartName+"-"+hbn.ChartVersion+".tgz")
	if ctx.Err() != nil {
		return unknown("CALLER_RBAC_CANCELLED", "Caller authorization was cancelled while retrieving the deployment charts.")
	}
	if err != nil {
		return unknown("CALLER_RBAC_HBN_CHART_UNAVAILABLE", "The selected HBN chart could not be retrieved to validate the deployment plan.")
	}
	retained, err := generation.Generate(plan.Inputs)
	if err != nil {
		return unknown("CALLER_RBAC_PLAN_INVALID", "The selected chart digests, schemas or deployment inputs failed validation; caller permissions remain unverified.")
	}
	if ctx.Err() != nil {
		return unknown("CALLER_RBAC_CANCELLED", "Caller authorization was cancelled while validating the deployment plan.")
	}
	client, err := o.newClient(ctx, request, plan.Management, plan.Operator.KubernetesVersion)
	if err != nil || client == nil {
		return unknown("CALLER_RBAC_MANAGEMENT_UNAVAILABLE", "The configured management credentials could not be bound to this discovery's cluster identity and TLS trust.")
	}
	progress.Info("[RUNNING] HBN-BGP caller permissions: checking scoped management API authorizations")
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
