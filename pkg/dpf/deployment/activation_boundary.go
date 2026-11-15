// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// verifyActivationBoundary is the full physical policy for mutation boundaries
// and runtime phase acceptance. Keep BMC reads between the control checks and
// the final shared-credential/selected-scope inspection.
func verifyActivationBoundary(ctx context.Context, client activationStageClient, prepared *preparedControlPlaneRun, controlPlane controlPlaneReceipts,
	workload workloadPreparationReceipt, selected []activationSelectedDevice, deploymentUID types.UID, objects []*unstructured.Unstructured, journal *executionJournal, requestTimeout time.Duration,
) error {
	if err := verifyActivationControlResources(ctx, client, prepared, controlPlane, workload, objects, journal); err != nil {
		return err
	}
	if err := client.verifyEnrollmentBMCs(ctx, prepared.selected, requestTimeout, journal); err != nil {
		return activationFailure(journal, "ACTIVATION_BMC_IDENTITY_CHANGED", "Selected BMC identities could not be reverified at the activation boundary.", err)
	}
	return verifyActivationEnrollmentResources(ctx, client, prepared, controlPlane, selected, deploymentUID, journal)
}

// verifyActivationResourceBoundary is the read-only runtime policy. It retains
// every control/resource check, including shared credentials and complete
// selected ownership, without collecting physical BMC identity per measurement.
// Mutation callers must use verifyActivationBoundary instead.
func verifyActivationResourceBoundary(ctx context.Context, client activationStageClient, prepared *preparedControlPlaneRun, controlPlane controlPlaneReceipts, workload workloadPreparationReceipt, selected []activationSelectedDevice, deploymentUID types.UID, objects []*unstructured.Unstructured, journal *executionJournal) error {
	if err := verifyActivationControlResources(ctx, client, prepared, controlPlane, workload, objects, journal); err != nil {
		return err
	}
	return verifyActivationEnrollmentResources(ctx, client, prepared, controlPlane, selected, deploymentUID, journal)
}

func verifyActivationControlResources(ctx context.Context, client activationStageClient, prepared *preparedControlPlaneRun, controlPlane controlPlaneReceipts, workload workloadPreparationReceipt, objects []*unstructured.Unstructured, journal *executionJournal) error {
	if err := client.VerifyIdentity(ctx); err != nil {
		return activationFailure(journal, "MANAGEMENT_IDENTITY_CHANGED", "Management identity changed at the activation boundary.", err)
	}
	if err := controlPlane.tenant.Client.VerifyIdentity(ctx); err != nil {
		return activationFailure(journal, "TENANT_API_IDENTITY_CHANGED", "Tenant API identity changed at the activation boundary.", err)
	}
	if err := verifyManagementBootstrap(ctx, client, prepared.execution, controlPlane.management); err != nil {
		return activationFailure(journal, "ACTIVATION_BOOTSTRAP_CHANGED", "Management bootstrap changed at the activation boundary.", err)
	}
	if err := validateWorkloadTargeting(ctx, client, prepared.execution, controlPlane.tenant, prepared.selected, objects); err != nil {
		return activationFailure(journal, "ACTIVATION_WORKLOAD_TARGET_CHANGED", "The retained workload resources no longer resolve only to the selected tenant and device cohort.", err)
	}
	if err := verifyWorkloadReceiptSet(ctx, client, prepared.execution, controlPlane.tenant.Client, objects, workload.Resources); err != nil {
		return activationFailure(journal, "ACTIVATION_WORKLOAD_RECEIPT_CHANGED", "A retained workload prerequisite changed at the activation boundary.", err)
	}
	if err := verifyActivationSoftware(ctx, client, prepared.execution, workload.Software); err != nil {
		return activationFailure(journal, "ACTIVATION_SOFTWARE_RECEIPT_CHANGED", "The retained software resource, component receipt or release-bound registry identity changed at the activation boundary.", err)
	}
	return nil
}

func verifyActivationEnrollmentResources(ctx context.Context, client activationStageClient, prepared *preparedControlPlaneRun, controlPlane controlPlaneReceipts, selected []activationSelectedDevice, deploymentUID types.UID, journal *executionJournal) error {
	secret := controlPlane.enrollment.Secret
	if controlPlane.enrollment.BundleDigest != prepared.execution.plan.Digest() || controlPlane.enrollment.TenantUID != controlPlane.tenant.ClusterUID || secret.Namespace != prepared.execution.operatorNamespace || secret.UID == "" {
		return activationFailure(journal, "ACTIVATION_ENROLLMENT_SCOPE_CHANGED", "The enrollment receipt differs from the retained plan, tenant or credential identity.", nil)
	}
	current, err := client.Get(ctx, secretIdentity(secret.Namespace))
	if err != nil {
		return activationFailure(journal, "ACTIVATION_ENROLLMENT_SECRET_CHANGED", "The shared BMC credential could not be reverified at the activation boundary.", err)
	}
	verified, err := verifyEnrollmentSecret(current, secret.Namespace, prepared.password)
	if err != nil || verified.UID != secret.UID {
		return activationFailure(journal, "ACTIVATION_ENROLLMENT_SECRET_CHANGED", "The shared BMC credential changed at the activation boundary.", err)
	}
	if !softwareEnrollmentReceiptMatches(controlPlane.enrollment.Devices, prepared.selected) || len(selected) != len(prepared.selected) {
		return activationFailure(journal, "ACTIVATION_ENROLLMENT_SCOPE_CHANGED", "The selected enrollment receipt differs from the retained cohort.", nil)
	}
	snapshot, err := collectEnrollmentScope(ctx, client, prepared.execution, prepared.selected, true)
	if err == nil {
		err = validateActivationScope(snapshot, prepared.execution, selected, deploymentUID)
	}
	if err != nil {
		return activationFailure(journal, "ACTIVATION_ENROLLMENT_SCOPE_CHANGED", "Selected DPU enrollment or activation ownership changed.", err)
	}
	if err := ctx.Err(); err != nil {
		return activationFailure(journal, "ACTIVATION_CANCELED", "Activation boundary verification was canceled.", err)
	}
	return nil
}

// verifyActivationSoftware refreshes resource and registry identity against the
// earlier byte-verification receipt. It does not rehash or attest later downloads.
func verifyActivationSoftware(ctx context.Context, client workloadStageClient, execution *managementExecution, receipt softwareVerificationReceipt) error {
	if err := verifyStagedSoftwareResource(ctx, client, execution, receipt); err != nil {
		return err
	}
	desired, err := retainedBlueFieldSoftware(execution)
	if err != nil {
		return err
	}
	current, err := client.Get(ctx, desired.DeepCopy())
	if err != nil {
		return safeAPIError("read activation software identity", err)
	}
	if current == nil || current.GetUID() != receipt.SoftwareUID || current.GetGeneration() != receipt.Generation || execution.reconciler.Verify(desired, current) != nil {
		return fmt.Errorf("verified software identity or retained intent changed")
	}
	targets, err := softwareCacheTargets(desired, current, execution.plan.Intent().Software, selectedSoftwarePSIDs(execution.plan.Intent()))
	if err != nil || len(targets) != len(receipt.Components) || receipt.VerifiedAt.IsZero() {
		return fmt.Errorf("verified software component receipt is incomplete")
	}
	for i, target := range targets {
		component := receipt.Components[i]
		if component.Component != target.component || component.PSID != target.psid || component.SHA256 != target.sha256 || component.Bytes <= 0 {
			return fmt.Errorf("verified software component receipt differs from retained locks")
		}
	}
	release, err := releases.Resolve(execution.plan.Intent().Release)
	if err != nil {
		return err
	}
	registry, err := client.observeRegistrySnapshot(ctx, execution.operatorNamespace)
	if err != nil {
		return err
	}
	if registry.namespace != execution.operatorNamespace || registry.manager.container.image != release.Operator.Image {
		return fmt.Errorf("active software registry does not match the retained release and namespace")
	}
	return nil
}

func (s enrollmentScopeSnapshot) objects(kind string) []unstructured.Unstructured {
	for _, observed := range s.resources {
		if observed.resource.kind == kind {
			return observed.objects
		}
	}
	return nil
}

// Before creation, the strict no-child policy still applies. Afterwards only the
// retained parent UID and its exact selected controller children are permitted.
// DPUNode Ready may change during node effects/reboot; identity remains pinned.
func validateActivationScope(snapshot enrollmentScopeSnapshot, execution *managementExecution, selected []activationSelectedDevice, deploymentUID types.UID) error {
	enrollment := make([]enrollmentBMC, len(selected))
	for i, item := range selected {
		enrollment[i] = item.item
	}
	for _, observed := range snapshot.resources {
		switch observed.resource.kind {
		case "DPUDiscovery", "DPUDevice", "DPUNode":
			if err := validateEnrollmentObjects(observed.resource, observed.objects, execution, enrollment); err != nil {
				return err
			}
		case "DPU", "DPUSet", "DPUDeployment":
			if deploymentUID == "" && len(observed.objects) != 0 {
				return fmt.Errorf("existing %s objects block activation before parent creation", observed.resource.kind)
			}
		}
	}
	devices, nodes := snapshot.objects("DPUDevice"), snapshot.objects("DPUNode")
	if len(devices) != len(selected) || len(nodes) != uniqueNodeCount(selected) {
		return fmt.Errorf("selected enrollment scope is incomplete")
	}
	for _, item := range selected {
		device, node := uniqueObjectByName(devices, item.deviceName()), uniqueObjectByName(nodes, item.nodeName)
		if device == nil || node == nil || device.GetUID() != item.receipt.DeviceUID || node.GetUID() != item.receipt.DPUNodeUID {
			return fmt.Errorf("selected enrollment Device or Node UID changed")
		}
		if err := validateEnrolledDevice(device, item.item, execution.operatorNamespace, true); err != nil {
			return err
		}
		if err := verifyEnrollmentSelectors(device, item.item.device.SelectorLabels); err != nil {
			return err
		}
		if check := enrolledDeviceReadiness(device); !check.Ready {
			return fmt.Errorf("selected DPUDevice lost current credential, certificate or hardware readiness: %s", check.ReasonCode)
		}
		if err := validateActivationDPUNode(node, item, selected); err != nil {
			return err
		}
		invalid, check := findCondition(node, "InvalidDPUDetails")
		if !check.Ready || invalid != nil && invalid.status != "False" {
			return fmt.Errorf("selected DPUNode details are invalid or unverified")
		}
	}
	if deploymentUID == "" {
		return nil
	}
	deployment, err := retainedDeploymentObject(execution)
	if err != nil {
		return err
	}
	parents := snapshot.objects("DPUDeployment")
	if len(parents) != 1 || parents[0].GetUID() != deploymentUID || execution.reconciler.Verify(deployment, &parents[0]) != nil {
		return fmt.Errorf("retained DPUDeployment identity or ownership changed")
	}
	sets := snapshot.objects("DPUSet")
	for i := range sets {
		set := &sets[i]
		item := activationSelectedSet(selected, set.GetName())
		if item == nil || set.GetNamespace() != deployment.GetNamespace() || !activationOwnerMatches(set, deployment.GetAPIVersion(), deployment.GetKind(), deployment.GetName(), deploymentUID) || !activationDPUSetMatches(set, *item) || !activationSelectorsMatch(set, item.dpuSet) {
			return fmt.Errorf("an unrelated or changed DPUSet exists")
		}
	}
	dpus := snapshot.objects("DPU")
	for i := range dpus {
		dpu := &dpus[i]
		item := activationSelectedDPU(selected, dpu.GetName())
		if item == nil || dpu.GetNamespace() != deployment.GetNamespace() || !activationDPUMatches(dpu, *item) {
			return fmt.Errorf("an unrelated or changed DPU exists")
		}
		set := uniqueObjectByName(sets, item.dpuSet.GetName())
		if set == nil || !activationOwnerMatches(dpu, set.GetAPIVersion(), set.GetKind(), set.GetName(), set.GetUID()) {
			return fmt.Errorf("a selected DPU has a missing or changed DPUSet owner")
		}
	}
	return nil
}

func activationSelectedSet(selected []activationSelectedDevice, name string) *activationSelectedDevice {
	for i := range selected {
		if selected[i].dpuSet.GetName() == name {
			return &selected[i]
		}
	}
	return nil
}

func activationSelectedDPU(selected []activationSelectedDevice, name string) *activationSelectedDevice {
	for i := range selected {
		if selected[i].dpuName == name {
			return &selected[i]
		}
	}
	return nil
}

func activationSelectorsMatch(set, desired *unstructured.Unstructured) bool {
	for _, path := range [][]string{{"spec", "dpuDeviceSelector"}, {"spec", "dpuTemplate", "spec", "cluster", "selector"}} {
		current, found, err := unstructured.NestedMap(set.Object, path...)
		wanted, wantFound, wantErr := unstructured.NestedMap(desired.Object, path...)
		if err != nil || wantErr != nil || !found || !wantFound || !reflect.DeepEqual(current, wanted) {
			return false
		}
	}
	return true
}
