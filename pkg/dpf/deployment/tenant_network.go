// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementnetwork"
)

type tenantNetworkRunner interface {
	Run(context.Context, config.Request, discovery.ManagementClient, *inventory.Inventory) ([]inventory.ProbeResult, inventory.Cleanup, error)
}

// checkTenantNetwork recollects only the control-plane nodes selected by the
// retained tenant intent. It deliberately accepts no persisted inventory. The
// existing probe owns bounded execution, node identity binding and cleanup.
// These checks establish current interface/routes, not an IPAM reservation.
func (c *APIClient) checkTenantNetwork(ctx context.Context, tenant bootstrap.Tenant, runner tenantNetworkRunner,
	runID string, requestTimeout time.Duration,
) ([]inventory.Check, error) {
	if ctx == nil || c == nil || runner == nil || runID == "" || requestTimeout < time.Second || requestTimeout > 2*time.Minute {
		return nil, fmt.Errorf("tenant network requires a bound client, runner, unique run ID and bounded request timeout")
	}
	end, bounded := ctx.Deadline()
	if !bounded || time.Until(end) < time.Second {
		return nil, fmt.Errorf("tenant network requires a live execution deadline")
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	management, err := c.access.Observe(ctx)
	if err != nil {
		return nil, err
	}
	selected, err := selectTenantNodes(management.Nodes, tenant.NodeSelector)
	if err != nil {
		return nil, err
	}
	// Keep a separate scope snapshot: Run enriches only the temporary inventory.
	management.Nodes = make([]inventory.Node, len(selected))
	for i, node := range selected {
		management.Nodes[i] = node
		management.Nodes[i].Labels = maps.Clone(node.Labels)
	}
	inv := &inventory.Inventory{Management: management, Collection: inventory.Collection{RunID: runID, Mode: "management"}}
	request := config.Request{UseCase: "hbn-bgp", Timeout: min(time.Until(end), time.Hour), RequestTimeout: requestTimeout}
	vip := tenant.VIP
	request.Input.Site.Provisioning = &config.Provisioning{Bootstrap: &config.BootstrapOptions{Tenant: config.TenantOptions{VIP: &vip}}}
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	rows, cleanup, probeErr := runner.Run(ctx, request, c.access, inv)
	inv.Probes = rows
	if cleanup.State != "complete" || len(cleanup.Leftovers) != 0 {
		checks := discovery.ProbeChecks(inv, nil)
		checks = append(checks, inventory.Check{ID: "dpf.hbn.management.tenant-network-cleanup", Subject: "tenant network probe resources", Vantage: "management-cluster", RequiredPhase: "deploy", State: "failed", ReasonCode: "TENANT_PROBE_CLEANUP_INCOMPLETE", Message: "Temporary probe cleanup is incomplete; inspect the probe resource journal before continuing."})
		return checks, fmt.Errorf("tenant network probe cleanup is incomplete; inspect its resource journal before continuing")
	}
	if probeErr != nil {
		return discovery.ProbeChecks(inv, nil), fmt.Errorf("tenant network probes did not complete: %w", probeErr)
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	current, err := c.access.Observe(ctx)
	if err != nil {
		return nil, err
	}
	latest, err := selectTenantNodes(current.Nodes, tenant.NodeSelector)
	if err != nil {
		return nil, err
	}
	if !sameTenantNodeScope(selected, latest) {
		return nil, fmt.Errorf("tenant control-plane selection changed during network probes; recollect before creation")
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	return managementnetwork.CheckNewTenantNetwork(inv.Management.Nodes, tenant), nil
}

func selectTenantNodes(nodes []inventory.Node, selector map[string]string) ([]inventory.Node, error) {
	if len(selector) == 0 {
		return nil, fmt.Errorf("tenant network requires an explicit control-plane node selector")
	}
	selected := []inventory.Node{}
	names, uids := map[string]bool{}, map[string]bool{}
	for _, node := range nodes {
		if !node.ControlPlane {
			continue
		}
		if node.Labels == nil || node.Name == "" || node.UID == "" || names[node.Name] || uids[node.UID] {
			return nil, fmt.Errorf("management control-plane identities or labels are incomplete or ambiguous")
		}
		names[node.Name], uids[node.UID] = true, true
		matches := true
		for key, value := range selector {
			actual, exists := node.Labels[key]
			if !exists || actual != value {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		if !node.Ready || node.Unschedulable || node.OS != "linux" || node.Architecture == "" {
			return nil, fmt.Errorf("selected tenant node %q is not Ready, schedulable Linux with a known architecture", node.Name)
		}
		selected = append(selected, node)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("tenant selector matches no management control-plane nodes")
	}
	return selected, nil
}

func sameTenantNodeScope(before, after []inventory.Node) bool {
	if len(before) != len(after) {
		return false
	}
	byUID := make(map[string]inventory.Node, len(before))
	for _, node := range before {
		byUID[node.UID] = node
	}
	for _, node := range after {
		previous, exists := byUID[node.UID]
		if !exists || previous.Name != node.Name || previous.OS != node.OS || previous.Architecture != node.Architecture ||
			previous.Ready != node.Ready || previous.Unschedulable != node.Unschedulable || previous.ControlPlane != node.ControlPlane || !maps.Equal(previous.Labels, node.Labels) {
			return false
		}
	}
	return true
}
