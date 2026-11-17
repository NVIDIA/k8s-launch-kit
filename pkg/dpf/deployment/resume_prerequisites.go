// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sshprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// ownedResumePrerequisites is transient proof for one exact checkpoint and one
// fresh collection. It never changes the inventory or the initial discovery
// policy, and is not a capability supplied by site configuration.
type ownedResumePrerequisites struct {
	selected  []activationSelectedDevice
	scope     enrollmentScopeSnapshot
	parentUID types.UID
}

func assessOwnedResumePrerequisites(ctx context.Context, client resumePrerequisiteReader, prepared *preparedControlPlaneRun, control controlPlaneReceipts, workload workloadPreparationReceipt, verification PrerequisiteVerification, access config.Request, journal *executionJournal, timeout time.Duration, phase PrerequisitePhase, inspect hostNetworkConfigurer) (PrerequisiteVerification, error) {
	if phase != BeforeActivation && phase != AfterActivation {
		return ownedPrerequisiteFailure(ctx, verification, phase, "OWNED_PREREQUISITE_PHASE_INVALID", "A supported deployment prerequisite phase is required.", nil)
	}
	if prepared == nil || prepared.operation == nil || client == nil {
		return ownedPrerequisiteFailure(ctx, verification, phase, "OWNED_RESUME_PROOF_MISSING", "A bound recorded operation is required before resume prerequisites can be assessed.", nil)
	}
	if err := validateSelectedInventory(prepared.execution.plan, verification.Inventory); err != nil {
		return ownedPrerequisiteFailure(ctx, verification, phase, "OWNED_RESUME_PHYSICAL_SCOPE_UNVERIFIED", "The fresh selected physical inventory does not prove the recorded cohort; preserve the operation and inspect its current identity.", err)
	}
	proof, err := verifyOwnedResumeResources(ctx, client, prepared, control, workload, verification.Inventory, access, journal)
	if err != nil {
		return ownedPrerequisiteFailure(ctx, verification, phase, "OWNED_RESUME_RESOURCES_UNVERIFIED", "Current management, tenant, credential and selected resource ownership could not be proven against the operation; preserve its resources and inspect the report.", err)
	}
	// Preserve the raw discovery checks in its report. The execution assessment
	// records the lifecycle-specific positive proof instead of claiming vacancy.
	verification.Checks = proof.assessInitialChecks(verification.Checks, control.tenant.Client.expectation().Name)
	pendingChecks, pendingErr := assessPendingResetHosts(ctx, client, prepared, proof, access.Input.Site.Inventory, verification.Inventory, verification.Checks, phase)
	if pendingErr != nil {
		return ownedPrerequisiteFailure(ctx, verification, phase, "OWNED_PENDING_RESET_HOST_UNVERIFIED", "Pending-reset host identity, measured interface absence or separate management paths could not be proven; preserve the operation and do not reset manually.", pendingErr)
	}
	verification.Checks = pendingChecks

	hostChecks, err := inspectOwnedResumeHosts(ctx, prepared, proof, verification.Inventory, timeout, inspect)
	if err != nil {
		return ownedPrerequisiteFailure(ctx, verification, phase, "OWNED_RESUME_HOST_TRANSACTION_UNVERIFIED", "An original host transaction failed inspect-only runtime/file proof; do not replay configuration or remove its journal.", err)
	}
	for _, check := range hostChecks {
		for i, old := range verification.Checks {
			if old.ID == check.ID && old.Subject == check.Subject {
				verification.Checks[i] = check
			}
		}
	}

	allocations, err := observeOwnedResumeAllocations(ctx, client, prepared, control, workload, proof, timeout)
	if err != nil {
		return ownedPrerequisiteFailure(ctx, verification, phase, "OWNED_RESUME_ALLOCATIONS_UNVERIFIED", "Current selected allocation identities or their original host request bindings could not be proven; preserve owned host and network state.", err)
	}
	if verification.Inventory != nil && verification.Inventory.HBN != nil {
		for _, sw := range verification.Inventory.HBN.Switches {
			for _, pool := range []struct{ name, cidr string }{{"workload", prepared.execution.plan.Intent().Networking.WorkloadCIDR}, {"loopback", prepared.execution.plan.Intent().Generation.LoopbackCIDR}} {
				check := discovery.ResumeFabricPoolCheck(verification.Inventory, sw, prepared.execution.plan.Intent().Networking.FabricVRF, pool.cidr, pool.name, allocations)
				for i, old := range verification.Checks {
					if old.ID == check.ID && old.Subject == check.Subject {
						verification.Checks[i] = check
					}
				}
			}
		}
	}
	// Slow physical/host reads must not hide a replaced checkpoint or changed
	// tenant, workload, enrollment or shared credential before another effect.
	if _, err = verifyOwnedResumeResources(ctx, client, prepared, control, workload, verification.Inventory, access, journal); err != nil {
		return ownedPrerequisiteFailure(ctx, verification, phase, "OWNED_RESUME_RESOURCES_CHANGED", "Recorded resource ownership changed or became unobservable during prerequisite collection; stop before another effect.", err)
	}
	current, err := client.getOperation(ctx, prepared.operation.object.Namespace, prepared.operation.object.Name)
	if err != nil || !operationObjectMatches(current, prepared.operation.object.Namespace, prepared.operation.object.Name) || current.UID != prepared.operation.object.UID || current.ResourceVersion != prepared.operation.object.ResourceVersion || !reflect.DeepEqual(current.Data, prepared.operation.object.Data) {
		return ownedPrerequisiteFailure(ctx, verification, phase, "OWNED_RESUME_CHECKPOINT_CHANGED", "The recorded operation checkpoint changed or became unobservable; reread current evidence before another attempt.", err)
	}
	for _, check := range verification.Checks {
		status := "PASS"
		if check.State != "passed" {
			status = "BLOCKED"
			if check.RequiredPhase == "generate" {
				status = "UNAVAILABLE"
			}
		}
		ui.FromContext(ctx).Info("[%s] HBN %s %q (%s): %s", status, phase, check.Subject, check.ReasonCode, check.Message)
	}
	return verification, nil
}

func ownedResumeCheck(check inventory.Check, code, message string) inventory.Check {
	check.State, check.ReasonCode, check.Message = "passed", code, message+" The raw discovery report retains its initial-state "+check.ReasonCode+" predicate."
	return check
}

func verifyOwnedResumeResources(ctx context.Context, client activationControlReader, prepared *preparedControlPlaneRun, control controlPlaneReceipts, workload workloadPreparationReceipt, inv *inventory.Inventory, access config.Request, journal *executionJournal) (ownedResumePrerequisites, error) {
	var proof ownedResumePrerequisites
	deployment, err := retainedDeploymentObject(prepared.execution.verificationInputs())
	if err != nil {
		return proof, err
	}
	selected, _, err := bindActivationHosts(prepared.execution.rules, prepared.selected, control.enrollment, inv, access.Targets, deployment)
	if err != nil {
		return proof, err
	}
	parentUID := prepared.operation.deploymentUID()
	parent, err := client.Get(ctx, deployment.DeepCopy())
	if err == nil {
		if parent == nil || prepared.execution.reconciler.Verify(deployment, parent) != nil || parentUID != "" && parent.GetUID() != parentUID {
			return proof, fmt.Errorf("OWNED_RESUME_PARENT_CHANGED: recorded deployment differs")
		}
		parentUID = parent.GetUID()
	} else if !apierrors.IsNotFound(err) || parentUID != "" {
		return proof, fmt.Errorf("OWNED_RESUME_PARENT_CHANGED: recorded deployment missing or unavailable")
	}
	objects, err := retainedWorkloadObjects(prepared.execution.verificationInputs())
	if err != nil {
		return proof, err
	}
	if err = verifyActivationResourceBoundary(ctx, client, newControlPlaneVerificationInputs(prepared, control, workload), control.tenant.Client, selected, parentUID, objects, journal); err != nil {
		return proof, err
	}
	tenantSystemUID, tenantSources := prepared.operation.tenantBinding()
	sources, err := control.tenant.Client.operationSources(ctx)
	if err != nil || control.tenant.Client.systemIdentity() != tenantSystemUID || !reflect.DeepEqual(sources, tenantSources) {
		return proof, fmt.Errorf("OWNED_RESUME_TENANT_CHANGED: recorded tenant or credential sources differ")
	}
	scope, err := collectEnrollmentScope(ctx, client, prepared.execution.verificationInputs(), prepared.selected, true)
	if err != nil {
		return proof, err
	}
	if err = validateActivationScope(scope, prepared.execution.verificationInputs(), selected, parentUID); err != nil {
		return proof, err
	}
	for key, prior := range prepared.operation.childBindings() {
		kind, _, ok := strings.Cut(key, "/")
		current := uniqueObjectByName(scope.objects(kind), prior.Name)
		if !ok || current == nil || current.GetUID() != prior.UID || current.GetGeneration() != prior.Generation {
			return proof, fmt.Errorf("OWNED_RESUME_CHILD_CHANGED: recorded child missing, replaced or changed")
		}
	}
	return ownedResumePrerequisites{selected: selected, scope: scope, parentUID: parentUID}, nil
}

func (proof ownedResumePrerequisites) assessInitialChecks(checks []inventory.Check, tenantName string) []inventory.Check {
	checks = slices.Clone(checks)
	if len(proof.selected) == 0 || len(proof.scope.resources) == 0 {
		return checks
	}
	for i, check := range checks {
		switch {
		case check.ID == "dpf.hbn.management.enrollments" && check.Subject == "selected DPUs" && check.Vantage == "executor" && check.RequiredPhase == "deploy" && check.State == "failed" && check.ReasonCode == "DPU_ALREADY_ENROLLED":
			checks[i] = ownedResumeCheck(check, "OWNED_RESUME_ENROLLMENT_VERIFIED", "Current all-namespace enrollment, exact Device/Node UIDs, selectors, credential and selected parent/child ownership match the recorded operation.")
		case check.ID == "dpf.hbn.management.tenant-network" && check.Subject == "tenant "+tenantName+" network selection" && check.Vantage == "management-cluster" && check.RequiredPhase == "deploy" && check.State == "unknown" && check.ReasonCode == "TENANT_EXISTING_NETWORK_UNVERIFIED":
			checks[i] = ownedResumeCheck(check, "OWNED_RESUME_TENANT_NETWORK_VERIFIED", "The recorded tenant API is reachable with unchanged tenant/system, endpoint, controller and credential-source UIDs; this is existing endpoint proof, not VIP vacancy.")
		}
	}
	return checks
}

func inspectOwnedResumeHosts(ctx context.Context, prepared *preparedControlPlaneRun, proof ownedResumePrerequisites, inv *inventory.Inventory, timeout time.Duration, inspect hostNetworkConfigurer) ([]inventory.Check, error) {
	if prepared == nil || prepared.operation == nil || inspect == nil {
		return nil, fmt.Errorf("OWNED_RESUME_HOST_PROOF_MISSING")
	}
	checks := []inventory.Check{}
	for _, item := range proof.selected {
		prior, known := prepared.operation.hostTransaction(item.item.device.Serial)
		if !known {
			continue
		}
		identity, identityErr := sshprobe.SelectHostWorkloadNetworkIdentity(item.host, item.adapter.Key, item.adapter.SerialNumber)
		if identityErr != nil || identity != prior.Request.Identity {
			return nil, fmt.Errorf("OWNED_RESUME_HOST_IDENTITY_CHANGED: original physical host request changed")
		}
		receipt, inspectErr := inspect(ctx, item.hostTarget, prior.Request, timeout)
		if inspectErr != nil || receipt.Blocking || receipt.State != "complete" && receipt.State != "verified-external" || !receipt.RuntimeVerified || !receipt.FileVerified {
			return nil, fmt.Errorf("OWNED_RESUME_HOST_UNVERIFIED: original host transaction must pass inspect-only runtime and file verification")
		}
		check := discovery.ResumeHostManagementPathCheck(inv, item.item.device.Serial, prior.Request.Identity, prior.Request.HostAddressPrefix)
		checks = append(checks, check)
	}
	return checks, nil
}

func ownedPrerequisiteFailure(ctx context.Context, verification PrerequisiteVerification, phase PrerequisitePhase, code, message string, cause error) (PrerequisiteVerification, error) {
	check := inventory.Check{ID: "dpf.hbn.deployment.owned-resume", Subject: "recorded deployment operation", Vantage: "cli", RequiredPhase: "deploy", State: "unknown", ReasonCode: code, Message: message}
	verification.Checks = append(slices.Clone(verification.Checks), check)
	ui.FromContext(ctx).Info("[BLOCKED] HBN %s (%s): %s", phase, code, message)
	return verification, reconcileError(code, message, cause)
}
