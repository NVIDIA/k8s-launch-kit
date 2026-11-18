// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// newHostNetworkFenceObserver retains the full physical boundary around host
// configuration. It is intentionally distinct from the runtime read policy.
func newHostNetworkFenceObserver(ctx context.Context, client activationBoundaryReader, prepared *preparedControlPlaneRun,
	controlPlane controlPlaneReceipts, workload workloadPreparationReceipt, activation activationStageReceipt,
	journal *executionJournal, requestTimeout time.Duration, readLeases hostLeaseSnapshotReader, stage, prefix string,
) (func() (hostNetworkLeaseFence, error), error) {
	verification := newControlPlaneVerificationInputs(prepared, controlPlane, workload)
	return newAllocationFenceObserver(ctx, client, verification, controlPlane.tenant.Client, activation, journal, requestTimeout, readLeases, stage, prefix, func(objects []*unstructured.Unstructured) error {
		return verifyActivationBoundary(ctx, client, verification, controlPlane.tenant.Client, activation.selected, activation.DeploymentUID, objects, journal, requestTimeout)
	})
}

// newRuntimeAllocationFenceObserver brackets read-only HBN measurements with
// complete current control/resource and allocation checks. Physical BMC checks
// belong to the runtime phase boundaries, never to every route/ping read.
func newRuntimeAllocationFenceObserver(ctx context.Context, client activationControlReader, verification *controlPlaneVerificationInputs, tenant tenantVerificationReader, activation activationStageReceipt,
	journal *executionJournal, requestTimeout time.Duration, readLeases hostLeaseSnapshotReader, stage, prefix string,
) (func() (hostNetworkLeaseFence, error), error) {
	return newAllocationFenceObserver(ctx, client, verification, tenant, activation, journal, requestTimeout, readLeases, stage, prefix, func(objects []*unstructured.Unstructured) error {
		return verifyActivationResourceBoundary(ctx, client, verification, tenant, activation.selected, activation.DeploymentUID, objects, journal)
	})
}

// The named factories above own boundary selection. Shared lease/readiness
// observation never narrows the selected cohort or caches a previous success.
func newAllocationFenceObserver(ctx context.Context, client activationObservationReader, verification *controlPlaneVerificationInputs, tenant tenantVerificationReader, activation activationStageReceipt,
	journal *executionJournal, requestTimeout time.Duration, readLeases hostLeaseSnapshotReader, stage, prefix string,
	verifyBoundary func([]*unstructured.Unstructured) error,
) (func() (hostNetworkLeaseFence, error), error) {
	if ctx == nil || client == nil || verification == nil || verification.execution == nil || verification.execution.plan == nil || verification.execution.reconciler == nil || journal == nil || readLeases == nil || verifyBoundary == nil || tenant == nil || requestTimeout < time.Second || requestTimeout > 2*time.Minute {
		return nil, fmt.Errorf("host network fence requires complete retained inputs")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("host network fence requires a deadline")
	}
	if err := validateHostNetworkCohort(activation); err != nil {
		return nil, err
	}
	execution := verification.execution
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
		if err := verifyBoundary(objects); err != nil {
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
		if observed.deployment == nil || !verification.execution.rules.ResourceReadiness(observed.deployment).Ready || !activationDPUsReady(verification.execution.rules, observed, activation.selected) {
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
		leases, err := resolveHostWorkloadLeases(execution.plan, execution.reconciler, activation.selected, observed, verification.workloadDigest, verification.resources, snapshot)
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
