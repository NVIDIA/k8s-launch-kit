// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementnetwork"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
)

// runControlPlane executes the management bootstrap and then the independent
// tenant API boundary. Site allocation declarations are required before any
// management effects. Its result does not authorize enrollment or
// establish HBN readiness. It is private until those remaining stages are wired.
func runControlPlane(ctx context.Context, client *APIClient, retained *bundle.Bundle, journal *executionJournal,
	probes probe.Runner, interval, requestTimeout time.Duration,
) (tenantStageReceipt, error) {
	if ctx == nil || client == nil || journal == nil || interval <= 0 || requestTimeout < time.Second || requestTimeout > 2*time.Minute {
		return tenantStageReceipt{}, fmt.Errorf("control-plane execution requires a client, report and bounded polling/request controls")
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return tenantStageReceipt{}, fmt.Errorf("control-plane execution requires a deadline")
	}
	if err := ctx.Err(); err != nil {
		return tenantStageReceipt{}, err
	}
	if err := probes.Assets.Validate(); err != nil {
		return tenantStageReceipt{}, err
	}
	if probes.Journal == nil {
		return tenantStageReceipt{}, fmt.Errorf("control-plane execution requires a durable probe resource journal")
	}
	execution, err := prepareManagementExecution(retained, time.Until(deadline(ctx)))
	if err != nil {
		return tenantStageReceipt{}, err
	}
	if execution.plan.Digest() != journal.digest {
		return tenantStageReceipt{}, fmt.Errorf("control-plane execution report must identify the retained bundle")
	}
	intent := execution.plan.Intent()
	if !client.matchesBinding(intent.Management, intent.KubernetesVersion) {
		return tenantStageReceipt{}, fmt.Errorf("management client binding differs from the retained control-plane plan")
	}
	for _, check := range managementnetwork.CheckTenantAllocation(intent.Bootstrap.Tenant) {
		state := "confirmed"
		if check.State != "passed" {
			state = check.State
		}
		if err := journal.record(executionEvent{Stage: "site-allocation", Operation: "check", Subject: check.Subject,
			State: state, ReasonCode: check.ReasonCode, Message: check.Message}); err != nil {
			return tenantStageReceipt{}, err
		}
		if check.State != "passed" {
			return tenantStageReceipt{}, fmt.Errorf("%s: %s", check.ReasonCode, check.Message)
		}
	}
	management, err := runPreparedManagement(ctx, client, execution, journal, interval)
	if err != nil {
		return tenantStageReceipt{}, err
	}
	runID := "tenant-" + rand.Text()
	observeNetwork := func(ctx context.Context, tenant bootstrap.Tenant) ([]inventory.Check, error) {
		return client.checkTenantNetwork(ctx, tenant, probes, runID, requestTimeout)
	}
	return runTenantStage(ctx, client, execution, management, journal, interval, requestTimeout, observeNetwork)
}
