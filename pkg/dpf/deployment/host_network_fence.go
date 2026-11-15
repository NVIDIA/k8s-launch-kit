// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
)

// newHostNetworkFenceObserver is shared by host configuration and runtime
// validation. Lease IO precedes the fresh complete readiness/metadata check;
// neither stage may weaken this boundary to a subset of hosts.
func newHostNetworkFenceObserver(ctx context.Context, client activationStageClient, prepared *preparedControlPlaneRun,
	controlPlane controlPlaneReceipts, workload workloadPreparationReceipt, activation activationStageReceipt,
	journal *executionJournal, requestTimeout time.Duration, readLeases hostLeaseSnapshotReader, stage, prefix string,
) (func() (hostNetworkLeaseFence, error), error) {
	if ctx == nil || client == nil || prepared == nil || prepared.execution == nil || prepared.execution.plan == nil || prepared.execution.reconciler == nil || journal == nil || readLeases == nil || controlPlane.tenant.Client == nil || requestTimeout < time.Second || requestTimeout > 2*time.Minute {
		return nil, fmt.Errorf("host network fence requires complete retained inputs")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("host network fence requires a deadline")
	}
	if err := validateHostNetworkCohort(activation); err != nil {
		return nil, err
	}
	execution := prepared.execution
	objects, err := retainedWorkloadObjects(execution)
	if err != nil {
		return nil, err
	}
	deployment, err := retainedDeploymentObject(execution)
	if err != nil {
		return nil, err
	}
	failActivationFence := func(code, message string, cause error) (hostNetworkLeaseFence, error) {
		err := fmt.Errorf("%s: %s", code, message)
		return hostNetworkLeaseFence{}, errors.Join(err, cause, journal.record(executionEvent{Stage: stage, Operation: "activation-readiness",
			Subject: execution.operatorNamespace, State: "failed", ReasonCode: code, Message: message}))
	}
	return func() (hostNetworkLeaseFence, error) {
		if err := verifyActivationBoundary(ctx, client, prepared, controlPlane, workload, activation.selected, activation.DeploymentUID, objects, journal, requestTimeout); err != nil {
			return hostNetworkLeaseFence{}, err
		}
		snapshot, err := readLeases(ctx)
		if err != nil {
			return hostNetworkLeaseFence{}, err
		}
		// Check settled controller state after the lease reads. Metadata holds
		// and reboot requests can change without changing object generation.
		observed, err := observeActivationScope(ctx, client, execution, deployment, activation.DeploymentUID, activation.selected, activation.progress)
		if err != nil || !observed.complete {
			return failActivationFence(prefix+"_ACTIVATION_SCOPE_UNAVAILABLE", "The complete selected activation scope could not be verified.", err)
		}
		if observed.deployment == nil || !EvaluateReadiness(observed.deployment).Ready || !activationDPUsReady(observed, activation.selected) {
			return failActivationFence(prefix+"_ACTIVATION_NOT_READY", "The selected DPUDeployment or DPUs no longer report current readiness.", nil)
		}
		effectsReady, err := selectedNodeEffectsReady(observed)
		if err != nil || !effectsReady {
			return failActivationFence(prefix+"_NODE_EFFECTS_NOT_SETTLED", "Selected maintenance holds must remain released and node effects must report applied and ready.", err)
		}
		for _, indexes := range activation.groups {
			pending, _, err := selectedHostRebootPending(observed, activation.selected, indexes)
			if err != nil || pending {
				return failActivationFence(prefix+"_REBOOT_PENDING", "A selected host has a pending or invalid external reboot request.", err)
			}
		}
		sets, dpus, nodes, maintenance := activationReceipts(observed)
		if !reflect.DeepEqual(sets, activation.DPUSets) || !reflect.DeepEqual(dpus, activation.DPUs) ||
			!reflect.DeepEqual(nodes, activation.Nodes) || !reflect.DeepEqual(maintenance, activation.Maintenance) {
			return failActivationFence(prefix+"_ACTIVATION_CHANGED", "A selected activation object's identity or generation changed.", nil)
		}
		leases, err := resolveHostWorkloadLeases(execution.plan, execution.reconciler, activation.selected, observed, workload, snapshot)
		if err != nil {
			return hostNetworkLeaseFence{}, err
		}
		return hostNetworkLeaseFence{snapshot: snapshot, leases: leases, activation: observed}, nil
	}, nil
}

func validateHostNetworkCohort(activation activationStageReceipt) error {
	if len(activation.selected) == 0 || len(activation.groups) == 0 || activation.DeploymentUID == "" {
		return fmt.Errorf("host network activation cohort is incomplete")
	}
	seen := map[int]bool{}
	for host, indexes := range activation.groups {
		if len(indexes) != 1 {
			return fmt.Errorf("host network requires exactly one selected DPU per host")
		}
		index := indexes[0]
		if index < 0 || index >= len(activation.selected) || seen[index] || activation.selected[index].host.ID != host {
			return fmt.Errorf("host network activation group is invalid")
		}
		seen[index] = true
	}
	if len(seen) != len(activation.selected) {
		return fmt.Errorf("host network activation groups omit a selected DPU")
	}
	return nil
}
