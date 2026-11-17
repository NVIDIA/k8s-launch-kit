// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	activationExternalRebootKey     = "provisioning.dpu.nvidia.com/dpunode-external-reboot-required"
	activationHoldKey               = "provisioning.dpu.nvidia.com/wait-for-external-nodeeffect"
	activationDPUNodeLabel          = "provisioning.dpu.nvidia.com/dpunode-name"
	activationDPUNodeNamespaceLabel = "provisioning.dpu.nvidia.com/dpunode-namespace"
	activationDPFVersionLabel       = "operator.dpu.nvidia.com/dpf-version"
	activationDPUSetNameLabel       = "provisioning.dpu.nvidia.com/dpuset-name"
	activationDPUSetNSLabel         = "provisioning.dpu.nvidia.com/dpuset-namespace"
	activationDPUDeviceLabel        = "provisioning.dpu.nvidia.com/dpudevice-name"
	activationParentLabel           = "svc.dpu.nvidia.com/owned-by-dpudeployment"
	activationManualWaitReason      = "WaitingForManualPowerCycleOrReboot"
)

type activationStageClient interface {
	ResourceClient
	activationBoundaryReader
	enrollmentBindingReader
	checkActivationPermissions(context.Context, *managementVerificationInputs) []PreflightCheck
	patchActivationAnnotation(context.Context, *unstructured.Unstructured, types.UID, string, string, *string) (*unstructured.Unstructured, error)
	verifyActivationRebootBMCs(context.Context, []activationRebootBMC, time.Duration, *executionJournal) (time.Time, error)
}

var _ activationStageClient = (*APIClient)(nil)

// activationStageReceipt reports controller objects and selected physical-host
// reboot transitions. It is not evidence of BGP adjacency, packet forwarding,
// or application traffic.
type activationStageReceipt struct {
	BundleDigest  string
	DeploymentUID types.UID
	DPUSets       []activationObjectReceipt
	Nodes         []activationObjectReceipt
	DPUs          []activationObjectReceipt
	Maintenance   []activationObjectReceipt
	Reboots       []activationRebootReceipt

	// Keep the already-bound physical/SSH cohort private to this execution.
	// Public summaries deliberately omit these credential-bearing bindings.
	selected []activationSelectedDevice
	groups   map[string][]int
	progress map[string]activationNodeProgress
}

type activationObjectReceipt struct {
	Name       string
	UID        types.UID
	Generation int64
}

type activationRebootReceipt struct {
	HostInputRef string
	NodeNames    []string
	PreviousBoot string
	CurrentBoot  string

	// Completion evidence is private and is emitted only after this exact
	// native reset round has been acknowledged successfully.
	completion []activationNodeProgress
}

// activationNodeProgress is owned by this execution's receipt. It records
// evidence produced by an effect, never inferred from a false/missing hold.
// Current snapshots may skip transient controller phases; no snapshot history
// or ordering of requestors is used as an authorization source.
type activationNodeProgress struct {
	nodeName          string
	maintenanceUID    types.UID
	releasedVersion   string
	resetAcknowledged bool
	sets              map[string]types.UID
	dpus              map[string]types.UID
	versions          map[string]types.UID
}

func activationProgressForNode(observed activationObservation, selected []activationSelectedDevice, nodeName, releasedVersion string, resetAcknowledged bool) (activationNodeProgress, error) {
	maintenance := observed.maintenance[nodeName+"-hold"]
	if maintenance == nil || maintenance.GetUID() == "" || maintenance.GetResourceVersion() == "" {
		return activationNodeProgress{}, fmt.Errorf("qualified maintenance identity is missing")
	}
	progress := activationNodeProgress{nodeName: nodeName, maintenanceUID: maintenance.GetUID(), releasedVersion: releasedVersion,
		resetAcknowledged: resetAcknowledged, versions: map[string]types.UID{}, sets: map[string]types.UID{}, dpus: map[string]types.UID{}}
	for name, uid := range observed.versions {
		progress.versions[name] = uid
	}
	for _, item := range selected {
		if item.nodeName != nodeName {
			continue
		}
		set, dpu := observed.sets[item.dpuSet.GetName()], observed.dpus[item.dpuName]
		if set == nil || dpu == nil || set.GetUID() == "" || dpu.GetUID() == "" {
			return activationNodeProgress{}, fmt.Errorf("qualified selected child identities are missing")
		}
		progress.sets[set.GetName()] = set.GetUID()
		progress.dpus[dpu.GetName()] = dpu.GetUID()
	}
	if len(progress.dpus) == 0 {
		return activationNodeProgress{}, fmt.Errorf("qualified selected node has no DPUs")
	}
	return progress, nil
}

func activationCompletionReady(rules *controllercontract.Rules, observed activationObservation, progress map[string]activationNodeProgress, selected []activationSelectedDevice) bool {
	if !activationDPUsReady(rules, observed, selected) {
		return false
	}
	for _, item := range selected {
		if !activationRemovalComplete(rules, observed, progress[item.nodeName]) {
			return false
		}
		maintenance := observed.maintenance[item.nodeName+"-hold"]
		if maintenance != nil {
			return false
		}
	}
	return true
}

func activationRemovalComplete(rules *controllercontract.Rules, observed activationObservation, progress activationNodeProgress) bool {
	if progress.maintenanceUID == "" || progress.releasedVersion == "" && !progress.resetAcknowledged || len(progress.dpus) == 0 {
		return false
	}
	for name, uid := range progress.sets {
		if set := observed.sets[name]; set == nil || set.GetUID() != uid {
			return false
		}
	}
	for name, uid := range progress.dpus {
		dpu := observed.dpus[name]
		if dpu == nil || dpu.GetUID() != uid || !rules.ResourceReadiness(dpu).Ready || !rules.NodeEffectRemoved(dpu).Ready {
			return false
		}
		phase, found, err := unstructured.NestedString(dpu.Object, "status", "phase")
		if err != nil || !found || phase != "Ready" {
			return false
		}
	}
	return true
}

type activationSelectedDevice struct {
	item       enrollmentBMC
	receipt    enrolledDeviceReceipt
	device     inventory.Device
	host       inventory.Device
	adapter    inventory.Adapter
	hostTarget config.Target
	nodeName   string
	dpuName    string
	dpuSet     *unstructured.Unstructured
}

type activationObservation struct {
	deployment  *unstructured.Unstructured
	sets        map[string]*unstructured.Unstructured
	dpus        map[string]*unstructured.Unstructured
	nodes       map[string]*unstructured.Unstructured
	maintenance map[string]*unstructured.Unstructured
	requestors  map[string][]string
	removed     map[string]bool
	versions    map[string]types.UID
	complete    bool
}

// runActivationStage consumes the same retained execution and receipts as the
// preceding stages. It creates only the retained DPUDeployment, releases only
// exact selected DPUNodeMaintenance holds, and requests one host reboot per
// selected physical host when DPF reports its external reboot gate.
func runActivationStage(ctx context.Context, client activationStageClient, prepared *preparedControlPlaneRun,
	controlPlane controlPlaneReceipts, workload workloadPreparationReceipt, inv *inventory.Inventory, request config.Request,
	journal *executionJournal, interval, requestTimeout time.Duration, reboot ExternalRebooter,
) (activationStageReceipt, error) {
	receipt := activationStageReceipt{progress: map[string]activationNodeProgress{}}
	if ctx == nil || client == nil || prepared == nil || prepared.execution == nil || prepared.execution.plan == nil || prepared.execution.reconciler == nil ||
		journal == nil || reboot == nil || interval <= 0 || requestTimeout < time.Second || requestTimeout > 2*time.Minute || len(prepared.selected) == 0 {
		return receipt, activationFailure(journal, "ACTIVATION_INPUT_INVALID", "Activation requires one retained execution, selected identities, a reboot executor and bounded controls.", nil)
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return receipt, activationFailure(journal, "ACTIVATION_DEADLINE_MISSING", "Activation requires an execution deadline.", nil)
	}
	receipt.progress = prepared.operation.restoreProgress()
	receipt.Reboots = prepared.operation.rebootHistory()
	execution := prepared.execution
	receipt.BundleDigest = execution.plan.Digest()
	if journal.digest != receipt.BundleDigest || prepared.execution.reconciler.BundleDigest() != receipt.BundleDigest ||
		controlPlane.management.BundleDigest != receipt.BundleDigest || controlPlane.tenant.ClusterUID == "" || workload.BundleDigest != receipt.BundleDigest ||
		workload.ControlPlane.enrollment.BundleDigest != receipt.BundleDigest || workload.Software.SoftwareUID == "" {
		return receipt, activationFailure(journal, "ACTIVATION_RECEIPT_MISMATCH", "Management, tenant, enrollment, software, workload and report receipts do not identify one retained plan.", nil)
	}
	if err := validateEnrollmentStageInputs(ctx, client, execution.verificationInputs(), controlPlane.management, controlPlane.tenant, prepared.selected, prepared.password, journal, interval, requestTimeout); err != nil {
		return receipt, activationFailure(journal, "ACTIVATION_ENROLLMENT_RECEIPT_INVALID", "The selected enrollment receipt is not bound to this retained plan and tenant API.", err)
	}
	if !sameEnrollmentReceipts(workload.ControlPlane.enrollment, controlPlane.enrollment) {
		return receipt, activationFailure(journal, "ACTIVATION_ENROLLMENT_RECEIPT_CHANGED", "The workload receipt does not contain the selected DPU identities from the current enrollment stage.", nil)
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		diagnostic := managementIdentityDiagnostic(ctx, err)
		return receipt, activationFailure(journal, diagnostic.ReasonCode, diagnostic.Message+" DPUDeployment activation remains stopped.", diagnostic)
	}
	objects, err := retainedWorkloadObjects(execution.verificationInputs())
	if err != nil {
		return receipt, activationFailure(journal, "ACTIVATION_WORKLOAD_PLAN_INVALID", "The retained workload prerequisite set is invalid.", err)
	}
	if err := verifyWorkloadReceiptSet(ctx, client, execution.verificationInputs(), controlPlane.tenant.Client, objects, workload.Resources); err != nil {
		return receipt, activationFailure(journal, "ACTIVATION_WORKLOAD_RECEIPT_CHANGED", "A workload prerequisite or tenant child changed before activation.", err)
	}
	deployment, err := retainedDeploymentObject(execution.verificationInputs())
	if err != nil {
		return receipt, activationFailure(journal, "ACTIVATION_PLAN_INVALID", "The retained DPUDeployment stage is missing or ambiguous.", err)
	}
	selected, groups, err := bindActivationHosts(prepared.execution.rules, prepared.selected, controlPlane.enrollment, inv, request.Targets, deployment)
	if err != nil {
		return receipt, activationFailure(journal, "ACTIVATION_HOST_BINDING_INVALID", "Selected DPUs cannot be mapped to unique, explicit authenticated host targets.", err)
	}
	if err := validateActivationDeployment(deployment, selected, execution.operatorNamespace, controlPlane.tenant.Client.expectation()); err != nil {
		return receipt, activationFailure(journal, "ACTIVATION_TARGET_SCOPE_INVALID", "The retained DPUDeployment does not select exactly the enrolled DPUs and tenant.", err)
	}
	checks := client.checkActivationPermissions(ctx, execution.verificationInputs())
	if err := journalActivationChecks(journal, checks); err != nil {
		return receipt, err
	}
	if len(checks) == 0 {
		return receipt, activationFailure(journal, "ACTIVATION_PERMISSION_CHECKS_MISSING", "Selected DPU node-effect patch permissions were not verified.", nil)
	}
	for _, check := range checks {
		if check.State != "passed" {
			return receipt, activationFailure(journal, "ACTIVATION_PERMISSION_DENIED", "The caller lacks a required selected-scope DPU activation permission.", nil)
		}
	}
	var created *unstructured.Unstructured
	var established types.UID
	if prepared.operation != nil {
		established = prepared.operation.deploymentUID()
		current, readErr := client.Get(ctx, deployment.DeepCopy())
		if readErr == nil {
			if current == nil || execution.reconciler.Verify(deployment, current) != nil || established != "" && current.GetUID() != established {
				return receipt, fmt.Errorf("OPERATION_PARENT_CHANGED: retained parent identity changed")
			}
			established = current.GetUID()
			created = current
		} else if !apierrors.IsNotFound(readErr) || established != "" {
			return receipt, fmt.Errorf("OPERATION_PARENT_CHANGED: established parent missing/unavailable")
		}
	}
	if err := verifyActivationBoundary(ctx, client, newControlPlaneVerificationInputs(prepared, controlPlane, workload), controlPlane.tenant.Client, selected, established, objects, journal, requestTimeout); err != nil {
		return receipt, err
	}
	if created == nil {
		if prepared.beforeDeploymentCreate != nil {
			if err := prepared.beforeDeploymentCreate(ctx); err != nil {
				return receipt, err
			}
			// Source reads may block; refresh physical/selected safety before creation.
			if err := verifyActivationBoundary(ctx, client, newControlPlaneVerificationInputs(prepared, controlPlane, workload), controlPlane.tenant.Client, selected, "", objects, journal, requestTimeout); err != nil {
				return receipt, err
			}
		}
		if err := hbn.ValidateRuntimeTrafficBudget(len(selected), time.Until(deadline(ctx))); err != nil {
			return receipt, activationFailure(journal, "HBN_RUNTIME_BUDGET_INSUFFICIENT", "Preparation consumed the time required for the complete selected traffic matrix; no DPUDeployment will be created.", err)
		}
		created, err = executeNativeObject(prepared.execution.rules, ctx, client, execution.reconciler, deployment, generation.StageDeployment, journal, false, interval)
		if err != nil {
			return receipt, err
		}
	}
	receipt.DeploymentUID = created.GetUID()
	if err := prepared.operation.bindDeployment(ctx, created.GetUID()); err != nil {
		return receipt, err
	}

	refreshHoldScope := func(ctx context.Context) (activationObservation, error) {
		if err := verifyActivationBoundary(ctx, client, newControlPlaneVerificationInputs(prepared, controlPlane, workload), controlPlane.tenant.Client, selected, created.GetUID(), objects, journal, requestTimeout); err != nil {
			return activationObservation{}, err
		}
		current, err := observeActivationScope(ctx, client, execution.verificationInputs(), deployment, created.GetUID(), selected, receipt.progress)
		if err != nil || !current.complete {
			return activationObservation{}, activationFailure(journal, "ACTIVATION_HOLD_SCOPE_CHANGED", "The complete selected activation scope changed before hold release.", err)
		}
		return current, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return receipt, activationFailure(journal, "ACTIVATION_CANCELED", "Selected DPU activation stopped; inspect durable receipts before retrying.", err)
		}
		observed, err := observeActivationScope(ctx, client, execution.verificationInputs(), deployment, created.GetUID(), selected, receipt.progress)
		if err != nil {
			code := "ACTIVATION_SCOPE_CHANGED"
			var diagnostic *ReconciliationError
			if errors.As(err, &diagnostic) && diagnostic != nil && diagnostic.ReasonCode == "ACTIVATION_REBOOT_SCOPE_CHANGED" {
				code = diagnostic.ReasonCode
			}
			return receipt, activationFailure(journal, code, "The selected DPUDeployment child scope is invalid or changed.", err)
		}
		if err := prepared.operation.bindChildren(ctx, observed); err != nil {
			return receipt, err
		}
		if observed.complete {
			if err := prepared.operation.verifyHoldHistory(observed); err != nil {
				return receipt, err
			}
			if err := resolveCompletedResets(prepared.execution.rules, ctx, prepared.operation, client, created, execution.plan.Digest(), observed, reboot); err != nil {
				return receipt, err
			}
		}
		if activationDPUsReady(prepared.execution.rules, observed, selected) && len(observed.dpus) == len(selected) {
			for _, item := range selected {
				proof, known := receipt.progress[item.nodeName]
				if !known || proof.releasedVersion == "" && !proof.resetAcknowledged {
					return receipt, activationFailure(journal, "ACTIVATION_COMPLETION_HISTORY_UNKNOWN", "Current selected DPU readiness does not establish this run's hold release or acknowledged reset history; inspect prior effects before retrying.", nil)
				}
			}
		}
		if !observed.complete {
			if err := activationWait(ctx, interval); err != nil {
				return receipt, activationFailure(journal, "ACTIVATION_CHILDREN_TIMEOUT", "DPF did not create all selected DPUSet, DPU and node-effect objects before the deadline.", err)
			}
			continue
		}
		// Once the controller is waiting for tenant registration, a known absent
		// or unusable bootstrap credential is actionable, not a readiness timeout.
		for i, item := range selected {
			phase, _, _ := unstructured.NestedString(observed.dpus[item.dpuName].Object, "status", "phase")
			if phase == "DPU Cluster Config" {
				if err := verifySelectedBootstrapCredentials(ctx, client, controlPlane.tenant.Client, selected, []int{i}, observed, journal); err != nil {
					return receipt, err
				}
			}
		}
		receipt.DPUSets, receipt.DPUs, receipt.Nodes, receipt.Maintenance = activationReceipts(observed)
		released, releaseErr := releaseNextSelectedHold(ctx, client, selected, observed, journal, refreshHoldScope, prepared.operation)
		if released != nil {
			receipt.progress[released.nodeName] = *released
			if err := prepared.operation.confirmHoldReleased(ctx, *released); err != nil {
				return receipt, err
			}
		}
		if releaseErr != nil {
			return receipt, releaseErr
		}
		// Persist this effect's proof before another node may progress or vanish.
		if released != nil {
			continue
		}
		readyForReboot, err := selectedNodeEffectsReady(observed)
		if err != nil {
			return receipt, activationFailure(journal, "ACTIVATION_NODE_EFFECT_INVALID", "A selected node-effect state is malformed or does not match retained intent.", err)
		}
		if !readyForReboot {
			if err := activationWait(ctx, interval); err != nil {
				return receipt, activationFailure(journal, "ACTIVATION_NODE_EFFECT_TIMEOUT", "Selected DPU node effects did not reach their current applied conditions before the deadline.", err)
			}
			continue
		}
		anyRebootPending := false
		for hostID, indexes := range groups {
			pending, actionable, err := selectedHostRebootPending(observed, selected, indexes)
			if err != nil {
				return receipt, activationFailure(journal, "ACTIVATION_REBOOT_STATE_INVALID", "DPF reported an unsupported or malformed selected host reboot state.", err)
			}
			if pending {
				anyRebootPending = true
				if actionable {
					reboots, err := invokeSelectedHostReboots(ctx, client, prepared, controlPlane, workload, selected, map[string][]int{hostID: indexes}, objects, deployment, created.GetUID(), observed, receipt.progress, reboot, journal, interval, requestTimeout)
					receipt.Reboots = append(receipt.Reboots, reboots...)
					for _, reset := range reboots {
						for _, progress := range reset.completion {
							receipt.progress[progress.nodeName] = progress
						}
					}
					if err != nil {
						return receipt, err
					}
				}
			}
		}
		if !anyRebootPending && activationCompletionReady(prepared.execution.rules, observed, receipt.progress, selected) {
			break
		}
		if err := activationWait(ctx, interval); err != nil {
			code, message := "ACTIVATION_DPU_READY_TIMEOUT", "Selected DPUs did not report current Ready state before the deadline."
			if anyRebootPending {
				code, message = "ACTIVATION_EXTERNAL_REBOOT_TIMEOUT", "Selected DPU external reboot requests did not reach a verified actionable state before the deadline."
			}
			return receipt, activationFailure(journal, code, message, err)
		}
	}
	if err := verifyActivationBoundary(ctx, client, newControlPlaneVerificationInputs(prepared, controlPlane, workload), controlPlane.tenant.Client, selected, created.GetUID(), objects, journal, requestTimeout); err != nil {
		return receipt, err
	}
	readyDeployment, err := waitNativeReady(prepared.execution.rules, ctx, client, deployment, created.GetUID(), execution.reconciler.Verify, interval, func(state controllercontract.Readiness, terminal bool) error {
		status := "waiting"
		if terminal {
			status = "failed"
		}
		if terminal && state.Ready {
			status = "ready"
		}
		return journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "readiness", Subject: deployment.GetNamespace() + "/" + deployment.GetName(), State: status,
			ReasonCode: state.ReasonCode, Message: state.Message})
	})
	if err != nil {
		return receipt, activationFailure(journal, "ACTIVATION_DEPLOYMENT_NOT_READY", "The selected DPUDeployment did not report current controller readiness.", err)
	}
	if err := verifyActivationBoundary(ctx, client, newControlPlaneVerificationInputs(prepared, controlPlane, workload), controlPlane.tenant.Client, selected, created.GetUID(), objects, journal, requestTimeout); err != nil {
		return receipt, err
	}
	currentDeployment, err := client.Get(ctx, deployment.DeepCopy())
	if err != nil || currentDeployment == nil || currentDeployment.GetUID() != created.GetUID() || execution.reconciler.Verify(deployment, currentDeployment) != nil ||
		!prepared.execution.rules.ResourceReadiness(currentDeployment).Ready || readyDeployment == nil || readyDeployment.GetUID() != created.GetUID() {
		return receipt, activationFailure(journal, "ACTIVATION_DEPLOYMENT_CHANGED", "DPUDeployment readiness or retained identity changed before final receipt.", nil)
	}
	final, err := observeActivationScope(ctx, client, execution.verificationInputs(), deployment, created.GetUID(), selected, receipt.progress)
	if err != nil || !final.complete || final.deployment == nil || !prepared.execution.rules.ResourceReadiness(final.deployment).Ready || !activationCompletionReady(prepared.execution.rules, final, receipt.progress, selected) {
		return receipt, activationFailure(journal, "ACTIVATION_SCOPE_CHANGED", "The complete selected activation scope changed while waiting for deployment readiness.", err)
	}
	for _, indexes := range groups {
		pending, _, err := selectedHostRebootPending(final, selected, indexes)
		if err != nil || pending {
			return receipt, activationFailure(journal, "ACTIVATION_REBOOT_REQUEST_CHANGED", "A selected host request changed before the final activation receipt.", err)
		}
	}
	receipt.DPUSets, receipt.DPUs, receipt.Nodes, receipt.Maintenance = activationReceipts(final)
	if err := ctx.Err(); err != nil {
		return receipt, activationFailure(journal, "ACTIVATION_CANCELED", "Activation was canceled before its final receipt.", err)
	}
	if err := journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "activation", Subject: deployment.GetNamespace() + "/" + deployment.GetName(), State: "confirmed",
		ReasonCode: "SELECTED_DPU_ACTIVATION_COMPLETE", Message: "The selected DPUDeployment and DPUs report current DPF readiness; network traffic qualification was not performed."}); err != nil {
		return receipt, err
	}
	receipt.selected, receipt.groups = selected, groups
	return receipt, nil
}

func retainedDeploymentObject(execution *managementVerificationInputs) (*unstructured.Unstructured, error) {
	if execution == nil || execution.plan == nil {
		return nil, fmt.Errorf("retained execution is required")
	}
	stage, found := execution.planStage(generation.StageDeployment)
	if !found || stage.Stage().Namespace != execution.plan.Intent().Namespace {
		return nil, fmt.Errorf("retained StageDeployment is missing or has unexpected namespace")
	}
	documents := stage.Documents()
	if len(documents) != 1 {
		return nil, fmt.Errorf("retained StageDeployment must contain one fixed object")
	}
	object := documents[0].ObjectCopy()
	if object == nil || object.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" || object.GetKind() != "DPUDeployment" || object.GetName() == "" || object.GetNamespace() == "" {
		return nil, fmt.Errorf("retained DPUDeployment identity is incomplete")
	}
	hold, found, err := unstructured.NestedBool(object.Object, "spec", "dpus", "nodeEffect", "hold")
	if err != nil || !found || !hold {
		return nil, fmt.Errorf("retained DPUDeployment must use the reviewed hold node effect")
	}
	force, found, err := unstructured.NestedBool(object.Object, "spec", "dpus", "nodeEffect", "force")
	if err != nil || found && force {
		return nil, fmt.Errorf("retained DPUDeployment must not force node effects")
	}
	return object, nil
}

func bindActivationHosts(rules *controllercontract.Rules, selected []enrollmentBMC, enrollment enrollmentStageReceipt, inv *inventory.Inventory, targets []config.Target,
	deployment *unstructured.Unstructured,
) ([]activationSelectedDevice, map[string][]int, error) {
	if inv == nil || deployment == nil || len(selected) == 0 || len(enrollment.Devices) != len(selected) {
		return nil, nil, fmt.Errorf("selected inventory and enrollment receipt are incomplete")
	}
	devices := make([]generation.ResolvedDevice, len(selected))
	for i := range selected {
		devices[i] = selected[i].device
	}
	sets, err := rules.ActivationSetIntents(deployment, devices)
	if err != nil {
		return nil, nil, err
	}
	result := make([]activationSelectedDevice, len(selected))
	groups := make(map[string][]int)
	for index, item := range selected {
		device := uniqueInventoryDPU(inv, item.device.Serial)
		if device == nil || enrollment.Devices[index].Serial != item.device.Serial || enrollment.Devices[index].DeviceUID == "" || enrollment.Devices[index].DPUNodeUID == "" {
			return nil, nil, fmt.Errorf("selected DPU inventory or enrollment identity is ambiguous")
		}
		associations := make([]inventory.Association, 0, 1)
		for _, association := range inv.Associations {
			if association.DPUID == device.ID && association.SerialNumber == item.device.Serial {
				associations = append(associations, association)
			}
		}
		if len(associations) != 1 || associations[0].HostID == "" || associations[0].HostAdapterKey == "" {
			return nil, nil, fmt.Errorf("selected DPU has no unique inventory host association")
		}
		host := uniqueInventoryHost(inv, associations[0].HostID)
		if host == nil || host.InputRef == "" || host.SSHEndpoint == "" || host.SystemUUID == "" {
			return nil, nil, fmt.Errorf("associated host lacks a stable observed SSH and system identity")
		}
		adapters := make([]inventory.Adapter, 0, 1)
		for _, adapter := range host.Adapters {
			if adapter.Key == associations[0].HostAdapterKey && adapter.SerialNumber == item.device.Serial {
				adapters = append(adapters, adapter)
			}
		}
		if len(adapters) != 1 || adapters[0].IdentityState != "known" {
			return nil, nil, fmt.Errorf("associated host adapter identity is missing or ambiguous")
		}
		hostTargets := make([]config.Target, 0, 1)
		for _, target := range targets {
			if target.Role == "hosts" && target.InputRef == host.InputRef && target.Endpoint == host.SSHEndpoint && target.SSH != nil {
				hostTargets = append(hostTargets, target)
			}
		}
		if len(hostTargets) != 1 {
			return nil, nil, fmt.Errorf("associated host has no unique explicit SSH access target")
		}
		nodeName := "dpu-node-" + strings.ToLower(item.device.Serial)
		deviceName := strings.ToLower(item.device.Serial)
		if len(deviceName) == 0 || len(deviceName) > 62 || len(validation.IsDNS1123Label(deviceName)) != 0 {
			return nil, nil, fmt.Errorf("selected serial cannot produce a valid DPF device name")
		}
		dpuName := rules.GeneratedDPUEName(nodeName, deviceName)
		set, found := sets[strings.ToLower(item.device.Serial)]
		if !found {
			return nil, nil, fmt.Errorf("retained DPUDeployment has no exact selector for a selected serial")
		}
		result[index] = activationSelectedDevice{item: item, receipt: enrollment.Devices[index], device: *device, host: *host, adapter: adapters[0],
			hostTarget: hostTargets[0], nodeName: nodeName, dpuName: dpuName, dpuSet: set}
		groups[host.ID] = append(groups[host.ID], index)
	}
	hostIDs := make([]string, 0, len(groups))
	for hostID := range groups {
		hostIDs = append(hostIDs, hostID)
	}
	sort.Strings(hostIDs)
	for _, hostID := range hostIDs {
		indexes := groups[hostID]
		sort.Slice(indexes, func(i, j int) bool { return result[indexes[i]].nodeName < result[indexes[j]].nodeName })
	}
	return result, groups, nil
}

func uniqueInventoryDPU(inv *inventory.Inventory, serial string) *inventory.Device {
	var result *inventory.Device
	for i := range inv.DPUs {
		if inv.DPUs[i].SerialNumber != serial {
			continue
		}
		if result != nil {
			return nil
		}
		result = &inv.DPUs[i]
	}
	return result
}

func uniqueInventoryHost(inv *inventory.Inventory, id string) *inventory.Device {
	var result *inventory.Device
	for i := range inv.Hosts {
		if inv.Hosts[i].ID != id {
			continue
		}
		if result != nil {
			return nil
		}
		result = &inv.Hosts[i]
	}
	return result
}

func validateActivationDeployment(deployment *unstructured.Unstructured, selected []activationSelectedDevice, namespace string, tenant controllercontract.TenantReadinessExpected) error {
	if deployment == nil || deployment.GetNamespace() != namespace || deployment.GetName() == "" || tenant.Namespace == "" || len(selected) == 0 {
		return fmt.Errorf("retained DPUDeployment identity differs from the selected tenant")
	}
	for _, selectedDevice := range selected {
		clusterSelector, found, err := unstructured.NestedStringMap(selectedDevice.dpuSet.Object, "spec", "dpuTemplate", "spec", "cluster", "selector", "matchLabels")
		if err != nil || !found || !activationClusterSelectorMatches(clusterSelector, tenant.Labels) {
			return fmt.Errorf("retained DPUSet selector does not target the selected tenant")
		}
	}
	return nil
}

// observeActivationScope reads live state without changing progress; only the
// stage's confirmed effects may add to progress. Missing maintenance never authorizes a new operation.
func observeActivationScope(ctx context.Context, client activationObservationReader, execution *managementVerificationInputs, deployment *unstructured.Unstructured,
	deploymentUID types.UID, selected []activationSelectedDevice, progress map[string]activationNodeProgress,
) (activationObservation, error) {
	observation := activationObservation{requestors: map[string][]string{}, removed: map[string]bool{}, sets: map[string]*unstructured.Unstructured{}, dpus: map[string]*unstructured.Unstructured{}, nodes: map[string]*unstructured.Unstructured{}, maintenance: map[string]*unstructured.Unstructured{}}
	if err := client.VerifyIdentity(ctx); err != nil {
		return observation, err
	}
	currentDeployment, err := client.Get(ctx, deployment.DeepCopy())
	if err != nil || currentDeployment == nil || currentDeployment.GetUID() != deploymentUID || execution.reconciler.Verify(deployment, currentDeployment) != nil {
		return observation, activationCoordinationError(activationParticipantOwner)
	}
	observation.deployment = currentDeployment.DeepCopy()
	sets, err := client.List(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUSet", "metadata": map[string]any{"namespace": deployment.GetNamespace()}}}, "")
	if err != nil {
		return observation, err
	}
	dpus, err := client.List(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPU", "metadata": map[string]any{"namespace": deployment.GetNamespace()}}}, "")
	if err != nil {
		return observation, err
	}
	maintenances, err := client.List(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUNodeMaintenance", "metadata": map[string]any{"namespace": deployment.GetNamespace()}}}, "")
	if err != nil {
		return observation, err
	}
	// APIClient.List returns nil for a successful empty list. Its error,
	// pagination and identity checks establish completeness; nil is not failure.
	for _, want := range selected {
		set := uniqueObjectByName(sets, want.dpuSet.GetName())
		if set == nil {
			return observation, nil
		}
		if set.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" || set.GetKind() != "DPUSet" || set.GetNamespace() != deployment.GetNamespace() || set.GetResourceVersion() == "" || set.GetGeneration() < 1 || set.GetDeletionTimestamp() != nil || set.GetUID() == "" || !activationOwnerMatches(set, deployment.GetAPIVersion(), deployment.GetKind(), deployment.GetName(), deploymentUID) || !activationDPUSetMatches(execution.rules, set, want) {
			return observation, activationCoordinationError(activationParticipantOwner)
		}
		observation.sets[set.GetName()] = set
	}
	versionGraph, graphComplete, err := activationVersionRequestors(ctx, client, execution, currentDeployment, observation.sets)
	if err != nil || !graphComplete {
		return observation, err
	}
	observation.versions = versionGraph.identities
	versionRequestors := versionGraph.requestors
	for _, prior := range progress {
		if !reflect.DeepEqual(prior.versions, observation.versions) {
			return observation, activationCoordinationError(activationParticipantOwner)
		}
	}
	for _, want := range selected {
		requestors := append([]string(nil), versionRequestors...)
		for _, item := range selected {
			if item.nodeName == want.nodeName {
				requestors = append(requestors, item.dpuName)
			}
		}
		sort.Strings(requestors)
		observation.requestors[want.nodeName] = requestors
		set := observation.sets[want.dpuSet.GetName()]
		if !activationAdditionalRequestorsMatch(set, versionRequestors, "spec", "dpuTemplate", "spec", "nodeEffect") {
			return observation, activationCoordinationError(activationParticipantPolicy)
		}
	}
	// Physical actions may target one host at a time, but every boundary read
	// must still verify the complete selected activation cohort.
	for _, want := range selected {
		node, err := client.Get(ctx, dpuNodeIdentity(deployment.GetNamespace(), want.nodeName))
		if apierrors.IsNotFound(err) {
			return observation, nil
		}
		if err != nil || node == nil || node.GetUID() != want.receipt.DPUNodeUID {
			return observation, activationCoordinationError(activationParticipantOwner)
		}
		if err := validateActivationDPUNode(node, want, selected); err != nil {
			return observation, activationCoordinationError(activationParticipantOwner)
		}
		observation.nodes[want.nodeName] = node
		set := observation.sets[want.dpuSet.GetName()]
		dpu := uniqueObjectByName(dpus, want.dpuName)
		if dpu == nil {
			continue
		}
		if dpu.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" || dpu.GetKind() != "DPU" || dpu.GetNamespace() != deployment.GetNamespace() || dpu.GetResourceVersion() == "" || dpu.GetGeneration() < 1 || dpu.GetDeletionTimestamp() != nil || dpu.GetUID() == "" || !activationOwnerMatches(dpu, "provisioning.dpu.nvidia.com/v1alpha1", "DPUSet", set.GetName(), set.GetUID()) || !activationDPUMatches(execution.rules, dpu, want) {
			return observation, activationCoordinationError(activationParticipantOwner)
		}
		if policyErr := activationNativeNodeEffectPolicy(dpu, "spec", "nodeEffect"); policyErr != nil {
			return observation, policyErr
		}
		setLabels, setLabelsFound, setLabelsErr := unstructured.NestedStringMap(set.Object, "spec", "dpuTemplate", "spec", "cluster", "nodeLabels")
		dpuLabels, dpuLabelsFound, dpuLabelsErr := unstructured.NestedStringMap(dpu.Object, "spec", "cluster", "nodeLabels")
		if setLabelsErr != nil || dpuLabelsErr != nil || !setLabelsFound || !dpuLabelsFound || !activationDPUClusterLabelsMatch(setLabels, dpuLabels, want.nodeName, want.dpuSet.GetNamespace(), execution.operatorVersion) {
			return observation, activationCoordinationError(activationParticipantClusterLabels)
		}
		if !activationNodeEffectMatches(dpu, set) {
			return observation, activationCoordinationError(activationParticipantPolicy)
		}
		if !activationAdditionalRequestorsMatch(dpu, versionRequestors, "spec", "nodeEffect") {
			return observation, activationCoordinationError(activationParticipantPolicy)
		}
		observation.dpus[want.dpuName] = dpu
		if prior, exists := progress[want.nodeName]; exists {
			if prior.sets[set.GetName()] != set.GetUID() || prior.dpus[dpu.GetName()] != dpu.GetUID() {
				return observation, activationCoordinationError("ACTIVATION_REBOOT_SCOPE_CHANGED")
			}
		}
		maintenanceName := want.nodeName + "-hold"
		maintenance := uniqueObjectByName(maintenances, maintenanceName)
		if maintenance == nil {
			continue
		}
		prior, anchored := progress[want.nodeName]
		released := anchored && (prior.releasedVersion != "" || prior.resetAcknowledged)
		if anchored && (prior.maintenanceUID != maintenance.GetUID() || maintenance.GetAnnotations()[activationHoldKey] != "false") {
			return observation, activationCoordinationError(activationParticipantMaintenance)
		}
		if maintenance.GetUID() == "" || validateActivationMaintenanceRequestors(maintenance, want, observation.requestors[want.nodeName], released || maintenance.GetAnnotations()[activationHoldKey] == "false") != nil {
			return observation, activationCoordinationError(activationParticipantRequestors)
		}

		observation.maintenance[maintenanceName] = maintenance
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		return observation, err
	}
	observation.complete = len(observation.sets) == len(selected) && len(observation.dpus) == len(selected) &&
		len(observation.nodes) == uniqueNodeCount(selected)
	if observation.complete {
		for _, want := range selected {
			maintenance := observation.maintenance[want.nodeName+"-hold"]
			removed := activationRemovalComplete(execution.rules, observation, progress[want.nodeName])
			if maintenance != nil && maintenance.GetDeletionTimestamp() != nil && !removed {
				proof := progress[want.nodeName]
				if proof.releasedVersion == "" && !proof.resetAcknowledged {
					return observation, activationCoordinationError(activationParticipantMaintenance)
				}
				observation.complete = false
			}
			if maintenance == nil && !removed {
				observation.complete = false
			}
			if removed {
				observation.removed[want.nodeName] = true
			}
		}
	}
	return observation, nil
}

func validateActivationDPUNode(node *unstructured.Unstructured, selected activationSelectedDevice, cohort []activationSelectedDevice) error {
	if err := validateEnrolledNode(node, selected.item, selected.dpuSet.GetNamespace()); err != nil {
		return err
	}
	refs, found, err := unstructured.NestedSlice(node.Object, "spec", "dpus")
	if err != nil || !found || len(refs) == 0 {
		return fmt.Errorf("selected DPUNode DPU membership is unavailable")
	}
	expected := map[string]bool{}
	for _, item := range cohort {
		if item.nodeName == selected.nodeName {
			expected[item.deviceName()] = true
		}
	}
	if len(refs) != len(expected) {
		return fmt.Errorf("selected DPUNode includes an unselected device or lacks a selected device")
	}
	for _, raw := range refs {
		ref, ok := raw.(map[string]any)
		name, nameOK := ref["name"].(string)
		if !ok || !nameOK || !expected[name] {
			return fmt.Errorf("selected DPUNode includes an unselected device")
		}
	}
	return nil
}

func (s activationSelectedDevice) deviceName() string { return strings.ToLower(s.item.device.Serial) }

// Read only the qualified consumed references; other controller-populated
// template fields retain their existing dedicated activation checks.

func activationDPUSetMatches(rules *controllercontract.Rules, set *unstructured.Unstructured, selected activationSelectedDevice) bool {
	software, flavor, err := rules.ActivationSetReferences(set)
	wantSoftware, wantFlavor, wantErr := rules.ActivationSetReferences(selected.dpuSet)
	if err != nil || wantErr != nil || software != wantSoftware || flavor != wantFlavor {
		return false
	}
	selector, found, err := unstructured.NestedStringMap(set.Object, "spec", "dpuDeviceSelector", "matchLabels")
	if err != nil || !found || !reflect.DeepEqual(selector, selected.item.device.SelectorLabels) {
		return false
	}
	clusterSelector, found, err := unstructured.NestedStringMap(set.Object, "spec", "dpuTemplate", "spec", "cluster", "selector", "matchLabels")
	wantClusterSelector, wantFound, wantErr := unstructured.NestedStringMap(selected.dpuSet.Object, "spec", "dpuTemplate", "spec", "cluster", "selector", "matchLabels")
	if err != nil || !found || wantErr != nil || !wantFound || !reflect.DeepEqual(clusterSelector, wantClusterSelector) {
		return false
	}
	if set.GetLabels()[activationParentLabel] != selected.dpuSet.GetLabels()[activationParentLabel] {
		return false
	}
	hold, found, err := unstructured.NestedBool(set.Object, "spec", "dpuTemplate", "spec", "nodeEffect", "hold")
	if err != nil || !found || !hold {
		return false
	}
	force, found, err := unstructured.NestedBool(set.Object, "spec", "dpuTemplate", "spec", "nodeEffect", "force")
	return err == nil && (!found || !force)
}

func activationDPUMatches(rules *controllercontract.Rules, dpu *unstructured.Unstructured, selected activationSelectedDevice) bool {
	wantSoftware, wantFlavor, wantErr := rules.ActivationSetReferences(selected.dpuSet)
	software, softwareFound, softwareErr := unstructured.NestedString(dpu.Object, "spec", "blueFieldSoftware")
	flavor, flavorFound, flavorErr := unstructured.NestedString(dpu.Object, "spec", "dpuFlavor")
	if wantErr != nil || softwareErr != nil || !softwareFound || software != wantSoftware || flavorErr != nil || !flavorFound || flavor != wantFlavor || nestedFieldPresent(dpu.Object, "spec", "bfb") {
		return false
	}
	nodeName, _, nodeErr := unstructured.NestedString(dpu.Object, "spec", "dpuNodeName")
	deviceName, _, deviceErr := unstructured.NestedString(dpu.Object, "spec", "dpuDeviceName")
	serial, _, serialErr := unstructured.NestedString(dpu.Object, "spec", "serialNumber")
	labels := dpu.GetLabels()
	return nodeErr == nil && deviceErr == nil && serialErr == nil && nodeName == selected.nodeName && deviceName == selected.deviceName() && serial == selected.item.device.Serial &&
		labels[activationDPUSetNameLabel] == selected.dpuSet.GetName() && labels[activationDPUSetNSLabel] == selected.dpuSet.GetNamespace() && labels[activationDPUDeviceLabel] == selected.deviceName()
}

// A pending true hold needs the complete initial set. Observing an already
// false hold may show a bounded subset as controllers remove their requestors;
// that observation alone never creates this run's release/completion evidence.
func validateActivationMaintenanceRequestors(object *unstructured.Unstructured, selected activationSelectedDevice, allowed []string, allowSubset bool) error {
	if object == nil || object.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" || object.GetKind() != "DPUNodeMaintenance" || object.GetNamespace() != selected.dpuSet.GetNamespace() || object.GetName() != selected.nodeName+"-hold" {
		return fmt.Errorf("maintenance identity differs")
	}
	nodeName, found, err := unstructured.NestedString(object.Object, "spec", "dpuNodeName")
	if err != nil || !found || nodeName != selected.nodeName || object.GetLabels()[activationDPUNodeLabel] != selected.nodeName {
		return fmt.Errorf("DPUNodeMaintenance node binding differs")
	}
	hold, found, err := unstructured.NestedBool(object.Object, "spec", "nodeEffect", "hold")
	if err != nil || !found || !hold {
		return fmt.Errorf("DPUNodeMaintenance action differs from retained hold")
	}
	force, found, err := unstructured.NestedBool(object.Object, "spec", "nodeEffect", "force")
	if err != nil || found && force {
		return fmt.Errorf("DPUNodeMaintenance force flag is not allowed")
	}
	requestors, found, err := unstructured.NestedStringSlice(object.Object, "spec", "requestor")
	if err != nil || !found && !allowSubset {
		return fmt.Errorf("DPUNodeMaintenance requestor set is incomplete")
	}
	seen := map[string]bool{}
	participants := map[string]bool{}
	for _, name := range allowed {
		participants[name] = true
	}
	for _, name := range requestors {
		if name == "" || seen[name] || !participants[name] {
			return fmt.Errorf("maintenance has duplicate or foreign participants")
		}
		seen[name] = true
	}
	if !allowSubset && len(seen) != len(participants) {
		return fmt.Errorf("initial maintenance participant set is incomplete")
	}
	if holdAnnotation, found := object.GetAnnotations()[activationHoldKey]; found && holdAnnotation != "true" && holdAnnotation != "false" {
		return fmt.Errorf("DPUNodeMaintenance hold annotation is malformed")
	}
	return nil
}

func releaseNextSelectedHold(ctx context.Context, client activationStageClient, selected []activationSelectedDevice, observed activationObservation, journal *executionJournal, refresh func(context.Context) (activationObservation, error), operation *deploymentOperation) (*activationNodeProgress, error) {
	var released *activationNodeProgress
	if refresh == nil {
		return released, activationFailure(journal, "ACTIVATION_HOLD_FENCE_REQUIRED", "A complete fresh activation boundary is required before releasing a selected hold.", nil)
	}
	byName := make(map[string]activationSelectedDevice, len(selected))
	for _, item := range selected {
		byName[item.nodeName] = item
	}
	names := make([]string, 0, len(observed.maintenance))
	for name := range observed.maintenance {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		maintenance := observed.maintenance[name]
		item, exists := byName[strings.TrimSuffix(name, "-hold")]
		if !exists || maintenance == nil {
			return released, activationFailure(journal, "ACTIVATION_MAINTENANCE_SCOPE_INVALID", "An observed DPUNodeMaintenance is outside the selected cohort.", nil)
		}
		hold, hasHold := maintenance.GetAnnotations()[activationHoldKey]
		if hasHold && hold == "false" {
			continue
		}
		if !hasHold || hold != "true" {
			continue
		}
		// Child creation can take time, and each earlier release authorizes
		// controller progress. Refresh every dependency before each new effect,
		// then use the same maintenance UID with its freshly validated version.
		current, err := refresh(ctx)
		if err != nil {
			return released, err
		}
		fresh := current.maintenance[name]
		if !current.complete || fresh == nil || maintenance.GetUID() == "" || fresh.GetUID() != maintenance.GetUID() || fresh.GetResourceVersion() == "" || fresh.GetDeletionTimestamp() != nil || validateActivationMaintenanceRequestors(fresh, item, current.requestors[item.nodeName], false) != nil {
			return released, activationFailure(journal, "ACTIVATION_HOLD_SCOPE_CHANGED", "The selected maintenance identity, action or requestors changed before hold release.", nil)
		}
		hold, hasHold = fresh.GetAnnotations()[activationHoldKey]
		if hasHold && hold == "false" {
			continue
		}
		if !hasHold || hold != "true" {
			return released, activationFailure(journal, "ACTIVATION_HOLD_STATE_CHANGED", "The selected maintenance no longer has its observed pending hold.", nil)
		}
		maintenance = fresh
		if err := journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "release-node-effect", Subject: item.nodeName,
			State: "attempted", ReasonCode: "SELECTED_NODE_EFFECT_RELEASE_STARTED", Message: "Release the exact selected DPUNodeMaintenance hold after verifying its UID, resourceVersion, action and requestors."}); err != nil {
			return released, err
		}
		if operation != nil {
			intent, intentErr := activationProgressForNode(current, selected, item.nodeName, maintenance.GetResourceVersion(), false)
			if intentErr != nil {
				return released, intentErr
			}
			if err := operation.recordHoldIntent(ctx, intent); err != nil {
				return released, err
			}
		}
		updated, err := client.patchActivationAnnotation(ctx, maintenance, maintenance.GetUID(), maintenance.GetResourceVersion(), activationHoldKey, stringPointer("false"))
		if err != nil {
			return released, activationFailure(journal, "SELECTED_NODE_EFFECT_RELEASE_FAILED", "The selected DPUNodeMaintenance hold could not be released with its observed identity preconditions.", err)
		}
		if updated == nil || updated.GetUID() != maintenance.GetUID() || updated.GetResourceVersion() == "" || updated.GetAnnotations()[activationHoldKey] != "false" {
			return released, activationFailure(journal, "SELECTED_NODE_EFFECT_RELEASE_UNCERTAIN", "The selected hold patch returned no verified same-object false annotation.", nil)
		}
		proof, err := activationProgressForNode(current, selected, item.nodeName, updated.GetResourceVersion(), false)
		if err != nil {
			return released, activationFailure(journal, "SELECTED_NODE_EFFECT_RELEASE_UNCERTAIN", "Released selected child identities could not be retained.", err)
		}
		released = &proof
		if err := journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "release-node-effect", Subject: item.nodeName,
			State: "confirmed", ReasonCode: "SELECTED_NODE_EFFECT_RELEASED", Message: "The exact selected DPUNodeMaintenance hold is false; DPF must still report NodeEffectApplied and DPU NodeEffectReady."}); err != nil {
			return released, err
		}
		return released, nil
	}
	if err := client.VerifyIdentity(ctx); err != nil {
		diagnostic := managementIdentityDiagnostic(ctx, err)
		return released, activationFailure(journal, diagnostic.ReasonCode, diagnostic.Message+" Inspect selected hold state before retrying.", diagnostic)
	}
	return released, nil
}

func selectedNodeEffectsReady(observed activationObservation) (bool, error) {
	for _, node := range observed.nodes {
		if node.GetDeletionTimestamp() != nil {
			return false, fmt.Errorf("selected DPUNode is deleting")
		}
	}
	for name, maintenance := range observed.maintenance {
		if observed.removed[strings.TrimSuffix(name, "-hold")] {
			continue
		}
		hold, found := maintenance.GetAnnotations()[activationHoldKey]
		if !found || hold != "false" || activationCondition(maintenance, "NodeEffectApplied") != "True" {
			return false, nil
		}
	}
	for _, dpu := range observed.dpus {
		if activationCondition(dpu, "NodeEffectReady") != "True" {
			return false, nil
		}
	}
	return true, nil
}

func selectedHostRebootPending(observed activationObservation, selected []activationSelectedDevice, indexes []int) (pending, actionable bool, err error) {
	if len(indexes) == 0 {
		return false, false, fmt.Errorf("selected physical host has no DPU nodes")
	}
	allReady := true
	for _, index := range indexes {
		if index < 0 || index >= len(selected) {
			return false, false, fmt.Errorf("selected host index is invalid")
		}
		node := observed.nodes[selected[index].nodeName]
		if node == nil {
			return false, false, fmt.Errorf("selected DPUNode is not observed")
		}
		annotation, annotated := node.GetAnnotations()[activationExternalRebootKey]
		if !annotated {
			allReady = false
			continue
		}
		if annotation != "true" {
			return false, false, fmt.Errorf("selected DPUNode external reboot annotation is malformed")
		}
		pending = true
		method, found, err := unstructured.NestedString(node.Object, "status", "rebootMethod")
		if err != nil {
			return false, false, fmt.Errorf("selected host reboot method is malformed")
		}
		if found && method != "SystemLevelReset" {
			return false, false, fmt.Errorf("selected host reboot method requires unsupported power control")
		}
		if !found {
			allReady = false
			continue
		}
		dpu := observed.dpus[selected[index].dpuName]
		if dpu == nil {
			allReady = false
			continue
		}
		phase, found, err := unstructured.NestedString(dpu.Object, "status", "phase")
		if err != nil {
			return false, false, fmt.Errorf("selected DPU phase is malformed")
		}
		if !found || phase != "Rebooting" {
			allReady = false
			continue
		}
		if _, err := activationRebootSourcePhase(dpu); err != nil {
			return false, false, err
		}
		status, found, err := unstructured.NestedMap(dpu.Object, "status", "rebootStatus")
		if err != nil {
			return false, false, fmt.Errorf("selected DPU reboot status is malformed")
		}
		if !found {
			allReady = false
			continue
		}
		dpuRebootPhase, phaseOK := status["phase"].(string)
		reason, reasonOK := status["reason"].(string)
		if !phaseOK && status["phase"] != nil || !reasonOK && status["reason"] != nil {
			return false, false, fmt.Errorf("selected DPU reboot status fields are malformed")
		}
		if phaseOK && dpuRebootPhase != "" && dpuRebootPhase != "Pending" || reasonOK && reason != "" && reason != activationManualWaitReason {
			return false, false, fmt.Errorf("selected DPU reboot status is not the supported external wait state")
		}
		if dpuRebootPhase == "Pending" && reason == activationManualWaitReason {
			actionable = true
		} else {
			allReady = false
		}
	}
	return pending, pending && actionable && allReady, nil
}

func invokeSelectedHostReboots(ctx context.Context, client activationStageClient, prepared *preparedControlPlaneRun,
	controlPlane controlPlaneReceipts, workload workloadPreparationReceipt, selected []activationSelectedDevice, groups map[string][]int,
	objects []*unstructured.Unstructured, deployment *unstructured.Unstructured, deploymentUID types.UID,
	observed activationObservation, progress map[string]activationNodeProgress, reboot ExternalRebooter, journal *executionJournal, interval, requestTimeout time.Duration,
) ([]activationRebootReceipt, error) {
	var verified []activationRebootReceipt
	// First-observed false is not release evidence. These local anchors only
	// bind the already-qualified current maintenance/children through this reset.
	resetProgress := make(map[string]activationNodeProgress, len(progress)+len(groups))
	for name, proof := range progress {
		resetProgress[name] = proof
	}
	for _, indexes := range groups {
		for _, index := range indexes {
			nodeName := selected[index].nodeName
			if _, exists := resetProgress[nodeName]; !exists {
				anchor, err := activationProgressForNode(observed, selected, nodeName, "", false)
				if err != nil {
					return verified, activationFailure(journal, "ACTIVATION_REBOOT_SCOPE_CHANGED", "The current selected maintenance identity is unavailable before reset.", err)
				}
				resetProgress[nodeName] = anchor
			}
		}
	}
	for _, indexes := range groups {
		if err := ctx.Err(); err != nil {
			return verified, activationFailure(journal, "ACTIVATION_CANCELED", "Host reboot was canceled before a selected operation.", err)
		}
		host := selected[indexes[0]].host
		request := ActivationRequest{HostInputRef: host.InputRef, HostEndpoint: host.SSHEndpoint, HostSystemUUID: host.SystemUUID,
			HostAdapterKey: selected[indexes[0]].adapter.Key, HostAdapterSerial: selected[indexes[0]].adapter.SerialNumber}
		sourcePhases := make(map[types.UID]string)
		nodesByName := map[string]*ActivationNode{}
		requested := false
		for _, index := range indexes {
			item := selected[index]
			node := observed.nodes[item.nodeName]
			if node == nil || node.GetUID() != item.receipt.DPUNodeUID {
				return verified, activationFailure(journal, "ACTIVATION_REBOOT_REQUEST_CHANGED", "A selected DPUNode identity changed before the host operation.", nil)
			}
			annotation, hasAnnotation := node.GetAnnotations()[activationExternalRebootKey]
			if hasAnnotation && annotation != "true" {
				return verified, activationFailure(journal, "ACTIVATION_REBOOT_REQUEST_CHANGED", "A selected DPUNode identity or external reboot annotation changed before the host operation.", nil)
			}
			if hasAnnotation {
				requested = true
			}
			nodeRequest := nodesByName[item.nodeName]
			if nodeRequest == nil {
				nodeRequest = &ActivationNode{NodeName: item.nodeName, NodeUID: node.GetUID(), RebootMethod: "SystemLevelReset"}
				nodesByName[item.nodeName] = nodeRequest
			}
			for _, nodeIndex := range indexes {
				selectedDPU := selected[nodeIndex]
				if selectedDPU.nodeName != item.nodeName {
					continue
				}
				dpu := observed.dpus[selectedDPU.dpuName]
				if dpu == nil || dpu.GetUID() == "" {
					return verified, activationFailure(journal, "ACTIVATION_DPU_IDENTITY_MISSING", "The selected DPU identity is not available for the host reboot request.", nil)
				}
				sourcePhase, phaseErr := activationRebootSourcePhase(dpu)
				if phaseErr != nil {
					return verified, activationFailure(journal, "ACTIVATION_REBOOT_PHASE_UNSUPPORTED", "The selected DPU reboot cycle does not have a supported controller source phase.", phaseErr)
				}
				if prior, exists := sourcePhases[dpu.GetUID()]; exists && prior != sourcePhase {
					return verified, activationFailure(journal, "ACTIVATION_REBOOT_PHASE_CHANGED", "A selected DPU source phase changed while building the host reboot cohort.", nil)
				}
				sourcePhases[dpu.GetUID()] = sourcePhase
				if containsActivationDPU(nodeRequest.SelectedDPUs, selectedDPU.item.device.Serial) {
					continue
				}
				nodeRequest.SelectedDPUs = append(nodeRequest.SelectedDPUs, ActivationDPU{
					SerialNumber: selectedDPU.item.device.Serial, DPUName: selectedDPU.dpuName, DPUUID: dpu.GetUID(),
					HostAdapterKey: selectedDPU.adapter.Key, AdapterSerial: selectedDPU.adapter.SerialNumber,
				})
			}
			sort.Slice(nodeRequest.SelectedDPUs, func(i, j int) bool {
				return nodeRequest.SelectedDPUs[i].SerialNumber < nodeRequest.SelectedDPUs[j].SerialNumber
			})
		}
		nodeNames := make([]string, 0, len(nodesByName))
		for name := range nodesByName {
			nodeNames = append(nodeNames, name)
		}
		sort.Strings(nodeNames)
		for _, name := range nodeNames {
			nodeRequest := nodesByName[name]
			sort.Slice(nodeRequest.SelectedDPUs, func(i, j int) bool {
				return nodeRequest.SelectedDPUs[i].SerialNumber < nodeRequest.SelectedDPUs[j].SerialNumber
			})
			if len(nodeRequest.SelectedDPUs) == 0 {
				return verified, activationFailure(journal, "ACTIVATION_NODE_SCOPE_INVALID", "The host reboot request has no selected DPU for a DPUNode.", nil)
			}
			request.DPUNodes = append(request.DPUNodes, *nodeRequest)
		}
		if !requested {
			return verified, activationFailure(journal, "ACTIVATION_REBOOT_REQUEST_CHANGED", "DPF no longer reports an external reboot request for this selected host.", nil)
		}
		if err := verifyActivationBoundary(ctx, client, newControlPlaneVerificationInputs(prepared, controlPlane, workload), controlPlane.tenant.Client, selected, deploymentUID, objects, journal, requestTimeout); err != nil {
			return verified, err
		}
		current, err := observeActivationScope(ctx, client, prepared.execution.verificationInputs(), deployment, deploymentUID, selected, resetProgress)
		if err != nil {
			return verified, activationFailure(journal, "ACTIVATION_REBOOT_SCOPE_CHANGED", "The selected DPU child scope changed during host verification; no host reboot was requested.", err)
		}
		if !current.complete {
			return verified, recordPendingRebootScope(ctx, journal, host.InputRef)
		}
		pending, actionable, err := selectedHostRebootPending(current, selected, indexes)
		if err != nil || !pending || !actionable {
			return verified, activationFailure(journal, "ACTIVATION_REBOOT_REQUEST_CHANGED", "The complete selected host cohort no longer has the exact external reboot wait state; no host reboot was requested.", err)
		}
		for _, node := range request.DPUNodes {
			for _, selectedDPU := range node.SelectedDPUs {
				currentDPU := current.dpus[selectedDPU.DPUName]
				currentPhase, phaseErr := activationRebootSourcePhase(currentDPU)
				if currentDPU == nil || currentDPU.GetUID() != selectedDPU.DPUUID || phaseErr != nil || currentPhase != sourcePhases[selectedDPU.DPUUID] {
					return verified, activationFailure(journal, "ACTIVATION_REBOOT_SCOPE_CHANGED", "A selected DPU identity or reboot source phase changed during final verification; no host reboot was requested.", phaseErr)
				}
			}
		}
		if !activationResetParticipantsPresent(current, selected, indexes) {
			return verified, activationFailure(journal, "ACTIVATION_REBOOT_SCOPE_CHANGED", "The current reset cohort no longer participates in its selected maintenance objects.", activationCoordinationError(activationParticipantRequestors))
		}
		observed = current
		rounds := map[types.UID]activationRebootRound{}
		bmcs := []activationRebootBMC{}
		for _, index := range indexes {
			item := selected[index]
			round, err := readActivationRebootRound(current.dpus[item.dpuName])
			if err != nil {
				source, _ := activationRebootSourcePhase(current.dpus[item.dpuName])
				code := "ACTIVATION_REBOOT_ROUND_UNVERIFIED"
				if source == "Update Firmware" || source == "Initialize Interface" {
					code = "ACTIVATION_PRE_OS_RESET_UNSUPPORTED"
				}
				return verified, activationFailure(journal, code, "Selected source "+source+": "+err.Error()+"; a measured request/agent/current-kernel baseline is required and agent-less pre-OS resets are unsupported. No host reboot was requested.", nil)
			}
			rounds[round.uid] = round
			bmcs = append(bmcs, activationRebootBMC{selected: item.item, startup: round.startup})
		}
		boundDeployment := deployment.DeepCopy()
		boundDeployment.SetUID(deploymentUID)
		rounds, recovered, err := prepareOperationReset(ctx, prepared.operation, client, boundDeployment, prepared.execution.plan.Digest(), &request, sourcePhases, rounds, reboot)
		if err != nil {
			return verified, err
		}
		var observation ActivationObservation
		if recovered != nil {
			observation = *recovered
		} else {
			expires, err := client.verifyActivationRebootBMCs(ctx, bmcs, requestTimeout, journal)
			if err != nil {
				return verified, err
			}
			request.ResetSafetyExpiresAt = expires
			if err := verifySelectedBootstrapCredentials(ctx, client, controlPlane.tenant.Client, selected, indexes, current, journal); err != nil {
				return verified, err
			}
			// BMC and bootstrap reads can outlast earlier control policy. Refresh
			// every resource/credential prerequisite after them, without another
			// slow physical read, then read the exact request/Arm round below.
			if err := verifyActivationResourceBoundary(ctx, client, newControlPlaneVerificationInputs(prepared, controlPlane, workload), controlPlane.tenant.Client, selected, deploymentUID, objects, journal); err != nil {
				return verified, err
			}
			// The BMC read can take time. A different request or restarted Arm
			// invalidates its shutdown proof before the one-time claim is created.
			current, err = observeActivationScope(ctx, client, prepared.execution.verificationInputs(), deployment, deploymentUID, selected, resetProgress)
			if err != nil {
				return verified, activationFailure(journal, "ACTIVATION_REBOOT_REQUEST_CHANGED", "The selected cohort changed during the physical reset-safety read.", err)
			}
			if !current.complete {
				return verified, recordPendingRebootScope(ctx, journal, host.InputRef)
			}
			if err := verifyPendingActivationReset(current, selected, indexes, rounds, journal); err != nil {
				return verified, err
			}
			// The native adapter invokes this after authenticating the host and
			// immediately before dispatch. Keep the existing durable baseline/CAS
			// callback intact; a failed guard never clears or renews the claim.
			beforeSubmit := request.BeforeSubmit
			request.BeforeSubmit = func(observation ActivationObservation) error {
				if err := verifySelectedBootstrapCredentials(ctx, client, controlPlane.tenant.Client, selected, indexes, current, journal); err != nil {
					return err
				}
				if err := verifyActivationResourceBoundary(ctx, client, newControlPlaneVerificationInputs(prepared, controlPlane, workload), controlPlane.tenant.Client, selected, deploymentUID, objects, journal); err != nil {
					return err
				}
				// The new credential and policy reads can outlast the observed
				// shutdown round. Collect again before persisting the host baseline.
				fresh, err := observeActivationScope(ctx, client, prepared.execution.verificationInputs(), deployment, deploymentUID, selected, resetProgress)
				if err != nil || ctx.Err() != nil {
					return activationFailure(journal, "ACTIVATION_REBOOT_REQUEST_CHANGED", "The selected reset scope could not be refreshed before host submission.", errors.Join(err, ctx.Err()))
				}
				if err := verifyPendingActivationReset(fresh, selected, indexes, rounds, journal); err != nil {
					return err
				}
				if beforeSubmit != nil {
					return beforeSubmit(observation)
				}
				return nil
			}
			observation, err = claimAndRebootExternal(ctx, client, boundDeployment, prepared.execution.plan.Digest(), request, sourcePhases, journal, reboot)
			if err != nil {
				return verified, err
			}
		}
		if observation.HostSystemUUID != host.SystemUUID || observation.HostAdapterSerial != request.HostAdapterSerial || observation.PreviousBootID == "" || observation.CurrentBootID == "" || observation.PreviousBootID == observation.CurrentBootID || !observation.Reconnected {
			return verified, activationFailure(journal, "EXTERNAL_HOST_REBOOT_UNVERIFIED", "The reboot callback did not verify the same host and adapter with a changed boot ID and a returned connection.", nil)
		}
		resetKey := resetOperationKey(request, sourcePhases)
		matched, err := prepared.operation.recordResetObservation(ctx, resetKey, observation)
		// Preserve matched physical history even if its CAS failed, while an
		// observation rejected by saved history never becomes a reboot summary.
		if matched {
			verified = append(verified, activationRebootReceipt{HostInputRef: host.InputRef, NodeNames: append([]string(nil), nodeNames...), PreviousBoot: observation.PreviousBootID, CurrentBoot: observation.CurrentBootID})
		}
		if err != nil {
			return verified, err
		}

		if err := journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "external-host-reboot", Subject: host.InputRef,
			State: "confirmed", ReasonCode: "EXTERNAL_HOST_REBOOT_VERIFIED", Message: fmt.Sprintf("Same host and adapter reconnected with a changed boot ID (previous %s, current %s); DPF request acknowledgement is still pending.", observation.PreviousBootID, observation.CurrentBootID)}); err != nil {
			return verified, err
		}
		if err := client.VerifyIdentity(ctx); err != nil {
			diagnostic := managementIdentityDiagnostic(ctx, err)
			return verified, activationFailure(journal, diagnostic.ReasonCode, diagnostic.Message+" Preserve the host reboot receipt and pending request.", diagnostic)
		}
		returned, err := waitActivationAgentReboots(ctx, client, prepared.execution.verificationInputs(), deployment, deploymentUID, selected, indexes, rounds, resetProgress, interval)
		if err != nil {
			return verified, activationFailure(journal, "EXTERNAL_REBOOT_AGENT_UNVERIFIED", "The same pending DPU round did not return a new agent startup and current Arm kernel boot; leave the request in place and do not repeat the host reset.", err)
		}
		proofs := map[types.UID]activationRebootRound{}
		for _, index := range indexes {
			item := selected[index]
			proof, err := readActivationRebootRound(returned.dpus[item.dpuName])
			if err != nil {
				return verified, err
			}
			proofs[proof.uid] = proof
			if err := journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "verify-arm-reboot", Subject: item.item.device.Serial,
				State: "confirmed", ReasonCode: "EXTERNAL_REBOOT_AGENT_VERIFIED", Message: fmt.Sprintf("Same DPU UID %s and pending request returned agent startup %s and current Arm kernel boot %s (previous %s); initialBootID is not used.", proof.uid, proof.startup.UTC().Format(time.RFC3339Nano), proof.kernelBoot, rounds[proof.uid].kernelBoot)}); err != nil {
				return verified, err
			}
		}
		if err := prepared.operation.recordResetACKIntent(ctx, resetKey, proofs); err != nil {
			return verified, err
		}
		for _, nodeName := range nodeNames {
			var item activationSelectedDevice
			foundItem := false
			for _, index := range indexes {
				if selected[index].nodeName == nodeName {
					item = selected[index]
					foundItem = true
					break
				}
			}
			if !foundItem {
				return verified, activationFailure(journal, "EXTERNAL_REBOOT_ACK_SCOPE_INVALID", "A selected DPUNode could not be mapped to the authenticated host request.", nil)
			}
			// Reconnection and current-Arm observation can take minutes, including
			// on resume. ACK authorizes controller progress, so recheck the complete
			// policy before reading the exact node/round for the conditional patch.
			if err := verifyActivationBoundary(ctx, client, newControlPlaneVerificationInputs(prepared, controlPlane, workload), controlPlane.tenant.Client, selected, deploymentUID, objects, journal, requestTimeout); err != nil {
				return verified, err
			}
			// Fresh and observe-only recovery share this ACK boundary. Observing
			// a completed physical reset never waives registration prerequisites.
			if err := verifySelectedBootstrapCredentials(ctx, client, controlPlane.tenant.Client, selected, indexes, returned, journal); err != nil {
				return verified, err
			}
			if err := verifyActivationResourceBoundary(ctx, client, newControlPlaneVerificationInputs(prepared, controlPlane, workload), controlPlane.tenant.Client, selected, deploymentUID, objects, journal); err != nil {
				return verified, err
			}
			node, err := client.Get(ctx, dpuNodeIdentity(deployment.GetNamespace(), nodeName))
			if err != nil || node == nil || node.GetUID() != item.receipt.DPUNodeUID || validateEnrolledNode(node, item.item, deployment.GetNamespace()) != nil {
				return verified, activationFailure(journal, "EXTERNAL_REBOOT_ACK_IDENTITY_CHANGED", "The selected DPUNode identity changed after the host reboot; do not repeat the host reset automatically.", err)
			}
			annotation, found := node.GetAnnotations()[activationExternalRebootKey]
			if !found || annotation != "true" {
				return verified, activationFailure(journal, "EXTERNAL_REBOOT_ACK_STATE_CHANGED", "The selected DPUNode external reboot annotation changed after the host reboot; inspect before retrying.", nil)
			}
			fresh, err := observeActivationScope(ctx, client, prepared.execution.verificationInputs(), deployment, deploymentUID, selected, resetProgress)
			if err != nil || !fresh.complete {
				return verified, activationFailure(journal, "EXTERNAL_REBOOT_ACK_SCOPE_CHANGED", "The selected cohort could not be refreshed immediately before acknowledgement.", err)
			}
			nodeIndexes := []int{}
			for _, index := range indexes {
				if selected[index].nodeName == nodeName {
					nodeIndexes = append(nodeIndexes, index)
				}
			}
			if !activationResetParticipantsPresent(fresh, selected, nodeIndexes) {
				return verified, activationFailure(journal, "EXTERNAL_REBOOT_ACK_SCOPE_CHANGED", "The current reset cohort no longer participates immediately before acknowledgement.", activationCoordinationError(activationParticipantRequestors))
			}
			for _, index := range indexes {
				selectedDPU := selected[index]
				if selectedDPU.nodeName != nodeName {
					continue
				}
				now, err := readActivationRebootRound(fresh.dpus[selectedDPU.dpuName])
				proof, found := proofs[now.uid]
				if err != nil || !found || !proof.sameRequest(now) || !proof.startup.Equal(now.startup) || proof.kernelBoot != now.kernelBoot {
					return verified, activationFailure(journal, "EXTERNAL_REBOOT_ACK_ROUND_CHANGED", "The same DPU request and newly observed agent/kernel boot proof changed before acknowledgement; do not repeat the reset.", nil)
				}
			}
			if err := journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "acknowledge-external-reboot", Subject: nodeName,
				State: "attempted", ReasonCode: "EXTERNAL_REBOOT_ACK_STARTED", Message: "A changed host boot ID and returned authenticated session were verified before acknowledging DPF's external reboot request."}); err != nil {
				return verified, err
			}
			acknowledged, err := client.patchActivationAnnotation(ctx, node, node.GetUID(), node.GetResourceVersion(), activationExternalRebootKey, nil)
			if err != nil {
				return verified, activationFailure(journal, "EXTERNAL_REBOOT_ACK_FAILED", "The exact selected DPUNode reboot request could not be removed with UID and resourceVersion preconditions.", err)
			}
			if acknowledged == nil || acknowledged.GetUID() != node.GetUID() || acknowledged.GetResourceVersion() == "" {
				return verified, activationFailure(journal, "EXTERNAL_REBOOT_ACK_FAILED", "Acknowledgement returned no verified same-node identity.", nil)
			}
			if _, pending := acknowledged.GetAnnotations()[activationExternalRebootKey]; pending {
				return verified, activationFailure(journal, "EXTERNAL_REBOOT_ACK_FAILED", "Acknowledgement did not remove the exact selected request.", nil)
			}
			completion, err := activationProgressForNode(fresh, selected, nodeName, "", true)
			if err != nil {
				return verified, activationFailure(journal, "EXTERNAL_REBOOT_ACK_SCOPE_CHANGED", "Acknowledged reset did not retain its maintenance identity.", err)
			}
			resetProgress[nodeName] = completion
			verified[len(verified)-1].completion = append(verified[len(verified)-1].completion, completion)
		}
		if err := prepared.operation.confirmResetAcknowledged(ctx, resetKey, verified[len(verified)-1].completion); err != nil {
			return verified, err
		}
		if err := journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "acknowledge-external-reboot", Subject: host.InputRef,
			State: "confirmed", ReasonCode: "EXTERNAL_REBOOT_ACKNOWLEDGED", Message: "The selected external reboot requests were acknowledged after a durable same-host, changed-boot-ID receipt; DPF still must report selected DPUs Ready."}); err != nil {
			return verified, err
		}
	}
	return verified, nil
}

// Separate controller reads can cross normal child/maintenance transitions.
// Defer to the existing outer observation loop before this host's claim/action;
// an incomplete snapshot never supplies permission for an effect.
func recordPendingRebootScope(ctx context.Context, journal *executionJournal, subject string) error {
	if err := ctx.Err(); err != nil {
		return activationFailure(journal, "ACTIVATION_CANCELED", "Host reboot stopped while waiting for the complete selected activation cohort.", err)
	}
	const code = "ACTIVATION_REBOOT_SCOPE_PENDING"
	const message = "The complete selected activation cohort is not yet observable. No host reset was requested at this boundary; waiting for a fresh observation."
	if err := journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "external-host-reboot", Subject: subject,
		State: "waiting", ReasonCode: code, Message: message}); err != nil {
		return err
	}
	ui.FromContext(ctx).Info("[WAITING] Host %s: %s (%s)", subject, message, code)
	return nil
}

func activationRebootSourcePhase(dpu *unstructured.Unstructured) (string, error) {
	if dpu == nil || dpu.GetKind() != "DPU" {
		return "", fmt.Errorf("selected DPU source phase is unavailable")
	}
	phase, found, err := unstructured.NestedString(dpu.Object, "status", "previousPhase")
	if err != nil || !found {
		return "", fmt.Errorf("selected DPU source phase is missing or malformed")
	}
	if supportedActivationRebootSourcePhase(phase) {
		return phase, nil
	}
	return "", fmt.Errorf("selected DPU source phase is unsupported")
}

func containsActivationDPU(devices []ActivationDPU, serial string) bool {
	for _, device := range devices {
		if device.SerialNumber == serial {
			return true
		}
	}
	return false
}

func activationReceipts(observed activationObservation) ([]activationObjectReceipt, []activationObjectReceipt, []activationObjectReceipt, []activationObjectReceipt) {
	sets := activationReceiptList(observed.sets)
	dpus := activationReceiptList(observed.dpus)
	nodes := activationReceiptList(observed.nodes)
	maintenance := activationReceiptList(observed.maintenance)
	return sets, dpus, nodes, maintenance
}

func activationReceiptList(objects map[string]*unstructured.Unstructured) []activationObjectReceipt {
	names := make([]string, 0, len(objects))
	for name := range objects {
		names = append(names, name)
	}
	sort.Strings(names)
	receipts := make([]activationObjectReceipt, 0, len(names))
	for _, name := range names {
		object := objects[name]
		receipts = append(receipts, activationObjectReceipt{Name: name, UID: object.GetUID(), Generation: object.GetGeneration()})
	}
	return receipts
}

func activationDPUsReady(rules *controllercontract.Rules, observed activationObservation, selected []activationSelectedDevice) bool {
	if len(observed.dpus) != len(selected) {
		return false
	}
	for _, item := range selected {
		dpu := observed.dpus[item.dpuName]
		if dpu == nil || dpu.GetUID() == "" {
			return false
		}
		readiness := rules.ResourceReadiness(dpu)
		if !readiness.Ready {
			return false
		}
	}
	return true
}

func uniqueObjectByName(objects []unstructured.Unstructured, name string) *unstructured.Unstructured {
	var found *unstructured.Unstructured
	for i := range objects {
		if objects[i].GetName() != name {
			continue
		}
		if found != nil {
			return nil
		}
		found = objects[i].DeepCopy()
	}
	return found
}

func uniqueNodeCount(selected []activationSelectedDevice) int {
	nodes := map[string]bool{}
	for _, item := range selected {
		nodes[item.nodeName] = true
	}
	return len(nodes)
}

func dpuNodeIdentity(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUNode", "metadata": map[string]any{"namespace": namespace, "name": name}}}
}

func activationOwnerMatches(object *unstructured.Unstructured, apiVersion, kind, name string, uid types.UID) bool {
	owners, found, err := unstructured.NestedSlice(object.Object, "metadata", "ownerReferences")
	if err != nil || !found || len(owners) != 1 {
		return false
	}
	owner, ok := owners[0].(map[string]any)
	if !ok {
		return false
	}
	return owner["apiVersion"] == apiVersion && owner["kind"] == kind && owner["name"] == name && owner["uid"] == string(uid)
}

func sameEnrollmentReceipts(left, right enrollmentStageReceipt) bool {
	return reflect.DeepEqual(left, right)
}

func activationCondition(object *unstructured.Unstructured, conditionType string) string {
	conditions, found, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil || !found {
		return ""
	}
	status := ""
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			return ""
		}
		if condition["type"] == conditionType {
			if status != "" {
				return ""
			}
			status, _ = condition["status"].(string)
		}
	}
	return status
}

func journalActivationChecks(journal *executionJournal, checks []PreflightCheck) error {
	for _, check := range checks {
		state := "confirmed"
		if check.State != "passed" {
			state = "failed"
		}
		if err := journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "permission-check", Subject: check.ID, State: state,
			ReasonCode: check.ReasonCode, Message: check.Message}); err != nil {
			return err
		}
	}
	return nil
}

func activationWait(ctx context.Context, interval time.Duration) error {
	if ctx == nil || interval <= 0 {
		return fmt.Errorf("activation wait requires context and a positive interval")
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Return only locally bounded prose while retaining original causes for
// errors.Is/As. In particular, unknown API/error text never becomes public text.
func activationFailure(journal *executionJournal, code, message string, cause error) error {
	message += activationCoordinationDiagnostic(cause)
	if code == "EXTERNAL_HOST_REBOOT_UNCERTAIN" {
		message += externalRebootSSHDiagnostic(cause)
		message += bootstrapCredentialDiagnostic(cause)
		message += externalRebootPolicyDiagnostic(cause)
	}
	if journal == nil {
		return reconcileError(code, message, cause)
	}
	recordErr := journal.record(executionEvent{Stage: generation.StageDeployment, Operation: "activation", State: "failed", ReasonCode: code, Message: message})
	if recordErr != nil {
		message += " The execution report could not persist this failure."
	}
	return reconcileError(code, message, errors.Join(cause, recordErr))
}

func activationAnnotationPatch(old, replacement *string) ([]byte, error) {
	if old == nil || (*old != "true" && *old != "false") || replacement != nil && *replacement != "false" {
		return nil, fmt.Errorf("unsupported activation annotation transition")
	}
	key := strings.ReplaceAll(activationHoldKey, "/", "~1")
	if replacement == nil {
		key = strings.ReplaceAll(activationExternalRebootKey, "/", "~1")
	}
	ops := []map[string]any{{"op": "test", "path": "/metadata/annotations/" + key, "value": *old}}
	if replacement == nil {
		ops = append(ops, map[string]any{"op": "remove", "path": "/metadata/annotations/" + key})
	} else {
		ops = append(ops, map[string]any{"op": "replace", "path": "/metadata/annotations/" + key, "value": *replacement})
	}
	return json.Marshal(ops)
}

func stringPointer(value string) *string { return &value }

func (c *APIClient) checkActivationPermissions(ctx context.Context, execution *managementVerificationInputs) []PreflightCheck {
	if c == nil || ctx == nil || execution == nil || execution.operatorNamespace == "" {
		return []PreflightCheck{blockedPreflight("caller.activation", "ACTIVATION_PERMISSION_INPUT_INVALID", "Selected activation permission inputs are unavailable.")}
	}
	namespace := execution.operatorNamespace
	return c.checkAccessRequirements(ctx, map[accessRequirement]struct{}{
		accessRequirement{verb: "list", group: "svc.dpu.nvidia.com", version: "v1alpha1", resource: "dpuservices", namespace: namespace}:                   {},
		accessRequirement{verb: "list", group: "svc.dpu.nvidia.com", version: "v1alpha1", resource: "dpuservicechains", namespace: namespace}:              {},
		accessRequirement{verb: "get", group: "svc.dpu.nvidia.com", version: "v1alpha1", resource: "dpudeployments", namespace: namespace}:                 {},
		accessRequirement{verb: "create", group: "svc.dpu.nvidia.com", version: "v1alpha1", resource: "dpudeployments", namespace: namespace}:              {},
		accessRequirement{verb: "get", group: "provisioning.dpu.nvidia.com", version: "v1alpha1", resource: "dpunodes", namespace: namespace}:              {},
		accessRequirement{verb: "list", group: "provisioning.dpu.nvidia.com", version: "v1alpha1", resource: "dpunodes", namespace: namespace}:             {},
		accessRequirement{verb: "patch", group: "provisioning.dpu.nvidia.com", version: "v1alpha1", resource: "dpunodes", namespace: namespace}:            {},
		accessRequirement{verb: "get", group: "provisioning.dpu.nvidia.com", version: "v1alpha1", resource: "dpus", namespace: namespace}:                  {},
		accessRequirement{verb: "list", group: "provisioning.dpu.nvidia.com", version: "v1alpha1", resource: "dpus", namespace: namespace}:                 {},
		accessRequirement{verb: "list", group: "provisioning.dpu.nvidia.com", version: "v1alpha1", resource: "dpusets", namespace: namespace}:              {},
		accessRequirement{verb: "list", group: "provisioning.dpu.nvidia.com", version: "v1alpha1", resource: "dpunodemaintenances", namespace: namespace}:  {},
		accessRequirement{verb: "get", group: "provisioning.dpu.nvidia.com", version: "v1alpha1", resource: "dpunodemaintenances", namespace: namespace}:   {},
		accessRequirement{verb: "patch", group: "provisioning.dpu.nvidia.com", version: "v1alpha1", resource: "dpunodemaintenances", namespace: namespace}: {},
		accessRequirement{verb: "get", version: "v1", resource: "configmaps", namespace: namespace}:                                                        {},
		accessRequirement{verb: "create", version: "v1", resource: "configmaps", namespace: namespace}:                                                     {},
	})
}

func (c *APIClient) patchActivationAnnotation(ctx context.Context, object *unstructured.Unstructured, uid types.UID, resourceVersion, key string, newValue *string) (*unstructured.Unstructured, error) {
	if c == nil || ctx == nil || object == nil || uid == "" || resourceVersion == "" || object.GetUID() != uid || object.GetResourceVersion() != resourceVersion {
		return nil, fmt.Errorf("activation patch requires the exact observed object identity")
	}
	old := "true"
	var patch []byte
	var patchErr error
	switch {
	case key == activationHoldKey && newValue != nil && *newValue == "false":
		patch, patchErr = activationAnnotationPatch(&old, newValue)
	case key == activationExternalRebootKey && newValue == nil:
		patch, patchErr = activationAnnotationPatch(&old, nil)
	default:
		return nil, fmt.Errorf("unsupported activation patch")
	}
	if patchErr != nil || len(patch) == 0 {
		return nil, fmt.Errorf("activation patch could not be encoded")
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	resource, err := c.resource(ctx, object, true)
	if err != nil {
		return nil, err
	}
	identityPatch := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(uid)}, {"op": "test", "path": "/metadata/resourceVersion", "value": resourceVersion}}
	var annotationOps []map[string]any
	if err := json.Unmarshal(patch, &annotationOps); err != nil {
		return nil, fmt.Errorf("activation patch could not be decoded")
	}
	patchOps := append(identityPatch, annotationOps...)
	patchData, err := json.Marshal(patchOps)
	if err != nil {
		return nil, fmt.Errorf("activation patch could not be encoded")
	}
	current, err := resource.Patch(ctx, object.GetName(), types.JSONPatchType, patchData, metav1.PatchOptions{FieldManager: "k8s-launch-kit-dpf"})
	if err != nil {
		return nil, safeAPIError("patch selected DPF node-effect annotation", err)
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	if current.GetUID() != uid || current.GetResourceVersion() == resourceVersion {
		return nil, fmt.Errorf("activation patch returned an invalid resource identity")
	}
	return current, nil
}

func activationClusterSelectorMatches(setIntent, expected map[string]string) bool {
	return len(setIntent) > 0 && reflect.DeepEqual(setIntent, expected)
}

func nestedFieldPresent(object map[string]any, fields ...string) bool {
	_, found, err := unstructured.NestedFieldNoCopy(object, fields...)
	return found || err != nil
}
