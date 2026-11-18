// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/artifacts"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/catalog"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementaccess"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// DiscoveryRequest selects public inputs and the local discovery publication.
type DiscoveryRequest struct {
	Inventory      string
	UserConfig     string
	UseCase        *string
	OutputPath     string
	ExplicitOutput bool
}

// DiscoveryResult retains the input needed to label the final native report.
type DiscoveryResult struct {
	Report inventory.Report
	Input  config.Input
}

// Discover validates access, collects observations and publishes available evidence.
// Publication has its own bounded cancellation-independent cleanup budget.
func Discover(ctx context.Context, r DiscoveryRequest) DiscoveryResult {
	progress := ui.FromContext(ctx)
	logger := log.FromContext(ctx)
	fail := func(input config.Input, code int, message string) DiscoveryResult {
		report := discovery.NewReport()
		report.Execution = "failed"
		report.Errors = append(report.Errors, inventory.Problem{Code: "INVALID_CONFIGURATION", Message: message, ExitCode: code})
		progress.Info("[FAIL] Discovery preparation: %s", ui.DisplayText(message))
		return DiscoveryResult{Report: report, Input: input}
	}
	progress.Info("[RUNNING] Configuration: validating inventory, optional overrides and access settings")
	input, err := config.LoadInputs(r.Inventory, r.UserConfig)
	if r.Inventory == "" {
		err = fmt.Errorf("DPF discovery requires --inventory inventory.yaml; run l8k discover --target dpf --use-case hbn-bgp --inventory inventory.yaml --save-cluster-config dpf-discovered.yaml; --user-config overrides.yaml is optional")
	}
	if err == nil {
		input, err = config.WithUseCase(input, r.UseCase)
	}
	if err != nil {
		return fail(input, 2, err.Error())
	}
	resolved, err := config.Resolve(input)
	if err != nil {
		return fail(input, 2, err.Error())
	}
	if err := validateManagement(resolved); err != nil {
		return fail(input, 2, err.Error())
	}
	// Unqualified image assets reject management mode before any remote operation.
	if resolved.Mode == "management" && catalog.ProbeAssets().Validate() != nil {
		return fail(input, 2, "management network probes require diagnostic image assets that have passed Kubernetes runtime qualification; inventory mode remains available without those probes")
	}
	store, err := artifacts.New(input, r.OutputPath, r.ExplicitOutput)
	if err != nil {
		return fail(input, 1, err.Error())
	}
	progress.Info("[PASS] Configuration: %d hosts, %d DPUs; %s mode", len(input.Site.Discovery.Hosts), len(input.Site.Discovery.DPUs), resolved.Mode)
	logger.V(1).Info("Discovery configuration ready", "mode", resolved.Mode, "sources", len(resolved.Targets), "timeout", resolved.Timeout, "requestTimeout", resolved.RequestTimeout, "concurrency", resolved.MaxConcurrentBMCs)
	for _, endpoint := range resolved.Targets {
		if endpoint.Access.InsecureSkipVerify() {
			progress.Info("Warning: BMC TLS certificate verification disabled for %s", endpoint.Endpoint)
		}
	}
	deps := StandaloneCollectors(resolved, store.RunDir)
	if resolved.Input.Site.Inventory == nil {
		resolved.Input.Site.Inventory = store.PriorInventory
	}
	report := discovery.Collect(ctx, resolved, store.RunID(), deps)
	progress.Info("[RUNNING] Publication: saving inventory and detailed report")
	publishStarted := time.Now()
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	err = store.Publish(publishCtx, &report)
	cancel()
	if err != nil {
		logger.Error(err, "Discovery publication failed")
		progress.Info("[FAIL] Publication incomplete; see the result for details")
		report.Execution = "failed"
		report.Errors = append(report.Errors, inventory.Problem{Code: "PUBLICATION_FAILED", Message: err.Error(), ExitCode: 1})
		_ = store.Report(&report)
	} else {
		progress.Info("[PASS] Publication: %s discovery artifacts saved", report.Discovery.Outcome)
	}
	logger.V(1).Info("Discovery publication finished", "outcome", report.Discovery.Outcome, "success", err == nil, "duration", time.Since(publishStarted))

	return DiscoveryResult{Report: report, Input: input}
}

func validateManagement(r config.Request) error {
	if r.Kubeconfig == "" {
		return nil
	}
	_, err := managementaccess.Open(r, nil)
	return err
}
