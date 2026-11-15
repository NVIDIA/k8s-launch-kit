// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/catalog"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/probe"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	activationIntegrationDeploymentUID = types.UID("123e4567-e89b-12d3-a456-426614174001")
	activationIntegrationSetUID        = types.UID("123e4567-e89b-12d3-a456-426614174002")
	activationIntegrationDPUUID        = types.UID("123e4567-e89b-12d3-a456-426614174003")
	activationIntegrationNodeUID       = types.UID("123e4567-e89b-12d3-a456-426614174004")
	activationIntegrationMaintUID      = types.UID("123e4567-e89b-12d3-a456-426614174005")
	activationIntegrationGuardUID      = types.UID("123e4567-e89b-12d3-a456-426614174006")
)

type activationIntegrationFake struct {
	*softwarePreparationFake
	execution                    *managementExecution
	versions                     []unstructured.Unstructured
	deployment                   *unstructured.Unstructured
	activationSelected           []activationSelectedDevice
	sets                         []unstructured.Unstructured
	dpus                         []unstructured.Unstructured
	maint                        []unstructured.Unstructured
	removedMaintenance           []unstructured.Unstructured
	guard                        *unstructured.Unstructured
	guards                       map[string]*unstructured.Unstructured
	guardCalls                   int
	patches                      []string
	mutateBoundaryDPU            string
	boundaryDPUChanged           bool
	advancedNodeRV               bool
	extra                        *unstructured.Unstructured
	nodeReadyOnCreate            string
	rejectRebootAckRemoval       bool
	resetSafetyError             error
	resetSafetyCalls             int
	resetSafetyExpiry            time.Time
	resetSafetySerials           []string
	omitAgentBaseline            bool
	changeRoundDuringResetSafety bool
}

func (f *activationIntegrationFake) verifyEnrollmentBMCs(_ context.Context, _ []enrollmentBMC, _ time.Duration, journal *executionJournal) error {
	if journal == nil {
		return fmt.Errorf("BMC verification requires its real journal")
	}
	if f.deployment == nil || f.boundaryDPUChanged || f.mutateBoundaryDPU == "" || len(f.dpus) == 0 {
		return nil
	}
	// UID/source-phase drift must occur during reboot verification, after
	// the node-effect holds are released and the request snapshot is read.
	// Earlier BMC reads also run before each hold release.
	if (f.mutateBoundaryDPU == "uid" || f.mutateBoundaryDPU == "phase") && !allActivationFixtureHoldsReleased(f.maint) {
		return nil
	}
	current := f.dpus[0].DeepCopy()
	switch f.mutateBoundaryDPU {
	case "uid":
		current.SetUID(types.UID("123e4567-e89b-12d3-a456-426614174099"))
	case "phase":
		if err := unstructured.SetNestedField(current.Object, "DPU Config", "status", "previousPhase"); err != nil {
			return err
		}
	case "device-uid":
		object := f.base.resources["DPUDevice/"+f.deployment.GetNamespace()+"/"+strings.ToLower(f.activationSelected[0].item.device.Serial)]
		if object == nil {
			return fmt.Errorf("selected fixture DPUDevice is missing")
		}
		object.SetUID(types.UID("123e4567-e89b-12d3-a456-426614174098"))
	case "node-uid":
		object := f.base.resources["DPUNode/"+f.deployment.GetNamespace()+"/"+f.activationSelected[0].nodeName]
		if object == nil {
			return fmt.Errorf("selected fixture DPUNode is missing")
		}
		object.SetUID(types.UID("123e4567-e89b-12d3-a456-426614174097"))
	case "secret-uid", "secret-password":
		secret := f.base.resources["Secret/"+f.deployment.GetNamespace()+"/"+enrollmentSecretName]
		if secret == nil {
			return fmt.Errorf("fixture BMC Secret is missing")
		}
		if f.mutateBoundaryDPU == "secret-uid" {
			secret.SetUID(types.UID("123e4567-e89b-12d3-a456-426614174096"))
		} else if err := unstructured.SetNestedField(secret.Object, "dGFtcGVyZWQ=", "data", enrollmentSecretDataKey); err != nil {
			return err
		}
	case "extra-dpu", "extra-dpuset":
		kind, apiVersion, name := "DPU", "provisioning.dpu.nvidia.com/v1alpha1", "unselected-dpu"
		if f.mutateBoundaryDPU == "extra-dpuset" {
			kind, apiVersion, name = "DPUSet", "svc.dpu.nvidia.com/v1alpha1", "unselected-dpuset"
		}
		f.extra = &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": apiVersion, "kind": kind,
			"metadata": map[string]any{"name": name, "namespace": "another-namespace", "uid": "123e4567-e89b-12d3-a456-426614174095", "resourceVersion": "1"},
		}}
	default:
		return fmt.Errorf("unknown fixture DPU mutation %q", f.mutateBoundaryDPU)
	}
	f.dpus[0] = *current
	f.boundaryDPUChanged = true
	return nil
}

func (f *activationIntegrationFake) verifyActivationRebootBMCs(_ context.Context, selected []activationRebootBMC, _ time.Duration, journal *executionJournal) (time.Time, error) {
	if len(selected) == 0 || journal == nil {
		return time.Time{}, fmt.Errorf("reset verification requires selected BMCs and journal")
	}
	f.resetSafetyCalls++
	for _, item := range selected {
		f.resetSafetySerials = append(f.resetSafetySerials, item.selected.device.Serial)
	}
	if f.changeRoundDuringResetSafety {
		_ = unstructured.SetNestedField(f.dpus[0].Object, "2026-11-13T13:30:00Z", "status", "rebootStatus", "lastTransitionTime")
	}
	expires := f.resetSafetyExpiry
	if expires.IsZero() {
		expires = time.Now().Add(activationResetSafetyMaxAge)
	}
	return expires, f.resetSafetyError
}

func (f *activationIntegrationFake) checkActivationPermissions(context.Context, *managementExecution) []PreflightCheck {
	return []PreflightCheck{{ID: "activation.selected", State: "passed", ReasonCode: "ACTIVATION_ACCESS_READY", Message: "selected activation permissions were checked"}}
}

func (f *activationIntegrationFake) Create(ctx context.Context, desired *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	if desired.GetKind() == "ConfigMap" && desired.GetLabels()[activationRebootGuardLabel] == "true" {
		f.guardCalls++
		if f.guards == nil {
			f.guards = make(map[string]*unstructured.Unstructured)
		}
		if f.guards[desired.GetName()] != nil {
			return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, desired.GetName())
		}
		guard := desired.DeepCopy()
		guardUID := activationIntegrationGuardUID
		if f.guard != nil {
			guardUID = types.UID(fmt.Sprintf("123e4567-e89b-12d3-a456-%012d", 426614174006+len(f.guards)))
		}
		guard.SetUID(guardUID)
		guard.SetResourceVersion("1")
		f.guards[desired.GetName()] = guard
		if f.guard == nil {
			f.guard = guard.DeepCopy()
		}
		return guard.DeepCopy(), nil
	}
	created, err := f.softwarePreparationFake.Create(ctx, desired, dryRun)
	if err != nil || desired.GetKind() != "DPUDeployment" {
		return created, err
	}
	created.SetUID(activationIntegrationDeploymentUID)
	created.SetResourceVersion("1")
	created.SetGeneration(1)
	created.Object["status"] = map[string]any{
		"observedGeneration": int64(1),
		"conditions":         []any{conditionMap("Ready", "False", "Progressing", 1)},
	}
	f.deployment = created.DeepCopy()
	f.base.resources[tenantStageKey(created)] = created.DeepCopy()
	if err := f.seedActivationChildren(created); err != nil {
		return nil, err
	}
	if f.nodeReadyOnCreate != "" {
		for _, selected := range f.activationSelected {
			f.setDPUNodeReady(selected.nodeName, f.nodeReadyOnCreate)
		}
	}
	return created, nil
}

func (f *activationIntegrationFake) Get(ctx context.Context, wanted *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if wanted.GetKind() == "DPUDeployment" {
		if f.deployment == nil {
			return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "dpudeployments"}, wanted.GetName())
		}
		return f.deployment.DeepCopy(), nil
	}
	if wanted.GetKind() == "DPUNode" {
		key := tenantStageKey(wanted)
		if current := f.base.resources[key]; current != nil {
			return current.DeepCopy(), nil
		}
	}
	return f.softwarePreparationFake.Get(ctx, wanted)
}

func (f *activationIntegrationFake) List(ctx context.Context, wanted *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	switch wanted.GetKind() {
	case "DPUSet":
		return f.listWithExtra(f.sets, wanted.GetKind()), nil
	case "DPU":
		return f.listWithExtra(f.dpus, wanted.GetKind()), nil
	case "DPUNodeMaintenance":
		return deepCopyUnstructuredList(f.maint), nil
	case "DPUService", "DPUServiceChain":
		result := []unstructured.Unstructured{}
		for _, object := range f.versions {
			if object.GetKind() == wanted.GetKind() {
				result = append(result, *object.DeepCopy())
			}
		}
		return result, nil
	default:
		return f.softwarePreparationFake.List(ctx, wanted, selector)
	}
}

func (f *activationIntegrationFake) listWithExtra(objects []unstructured.Unstructured, kind string) []unstructured.Unstructured {
	result := deepCopyUnstructuredList(objects)
	if f.extra != nil && f.extra.GetKind() == kind {
		result = append(result, *f.extra.DeepCopy())
	}
	return result
}

func (f *activationIntegrationFake) patchActivationAnnotation(_ context.Context, object *unstructured.Unstructured, uid types.UID, rv, key string, value *string) (*unstructured.Unstructured, error) {
	if object == nil || object.GetUID() != uid || object.GetResourceVersion() != rv {
		return nil, fmt.Errorf("fixture patch identity mismatch")
	}
	var authoritative *unstructured.Unstructured
	if object.GetKind() == "DPUNodeMaintenance" {
		for i := range f.maint {
			if tenantStageKey(&f.maint[i]) == tenantStageKey(object) {
				authoritative = f.maint[i].DeepCopy()
				break
			}
		}
	} else if object.GetKind() == "DPUNode" {
		authoritative = f.base.resources[tenantStageKey(object)]
		if authoritative != nil {
			authoritative = authoritative.DeepCopy()
		}
	}
	if authoritative == nil || authoritative.GetUID() != uid || authoritative.GetResourceVersion() != rv {
		return nil, fmt.Errorf("fixture rejected stale authoritative UID/resourceVersion")
	}
	current := authoritative
	if key == activationHoldKey && value != nil && *value == "false" {
		annotations := current.GetAnnotations()
		annotations[activationHoldKey] = "false"
		current.SetAnnotations(annotations)
		f.replaceActivationObject(f.maint, current)
	} else if key == activationExternalRebootKey && value == nil {
		if f.rejectRebootAckRemoval {
			return nil, fmt.Errorf("fixture rejected external reboot acknowledgement")
		}
		annotations := current.GetAnnotations()
		delete(annotations, activationExternalRebootKey)
		current.SetAnnotations(annotations)
		setDPUNodeReady(current, "True")
		f.base.resources[tenantStageKey(current)] = current.DeepCopy()
		for i := range f.dpus {
			for _, selected := range f.activationSelected {
				if selected.nodeName == current.GetName() && selected.dpuName == f.dpus[i].GetName() {
					setActivationDPUReady(&f.dpus[i])
				}
			}
		}
		remaining := f.maint[:0]
		for _, maintenance := range f.maint {
			if maintenance.GetLabels()[activationDPUNodeLabel] != current.GetName() {
				remaining = append(remaining, maintenance)
			} else {
				f.removedMaintenance = append(f.removedMaintenance, *maintenance.DeepCopy())
			}
		}
		f.maint = remaining
		if f.deployment != nil {
			setActivationReady(f.deployment)
			f.base.resources[tenantStageKey(f.deployment)] = f.deployment.DeepCopy()
		}
	} else {
		return nil, fmt.Errorf("fixture rejected unsupported activation patch")
	}
	current.SetResourceVersion(fmt.Sprintf("%d", len(f.patches)+2))
	f.patches = append(f.patches, key)
	if key == activationHoldKey && value != nil {
		f.replaceActivationObject(f.maint, current)
	} else {
		f.base.resources[tenantStageKey(current)] = current.DeepCopy()
	}
	return current, nil
}

// completeReboot explicitly models both the host return and fresh Arm kernel/
// agent reports. Tests that model a failed or incomplete reset must not call it.
func (f *activationIntegrationFake) completeReboot(name string) {
	if f.deployment == nil {
		return
	}
	key := "DPUNode/" + f.deployment.GetNamespace() + "/" + name
	current := f.base.resources[key]
	if current == nil {
		return
	}
	updated := current.DeepCopy()
	updated.SetResourceVersion("3")
	f.base.resources[key] = updated
	f.advancedNodeRV = true
	for i := range f.dpus {
		node, _, _ := unstructured.NestedString(f.dpus[i].Object, "spec", "dpuNodeName")
		if node == name {
			_ = unstructured.SetNestedField(f.dpus[i].Object, "2026-11-13T14:00:00Z", "status", "agentStatus", "lastStartupTime")
			_ = unstructured.SetNestedField(f.dpus[i].Object, "22222222-2222-4222-8222-222222222222", "status", "agentStatus", "lastObservedPendingNvconfig", "bootID")
		}
	}
}

func (f *activationIntegrationFake) setDPUNodeReady(name, status string) {
	if f.deployment == nil {
		return
	}
	key := "DPUNode/" + f.deployment.GetNamespace() + "/" + name
	current := f.base.resources[key]
	if current == nil {
		return
	}
	updated := current.DeepCopy()
	setDPUNodeReady(updated, status)
	updated.SetResourceVersion("2")
	f.base.resources[key] = updated
}

func setDPUNodeReady(node *unstructured.Unstructured, status string) {
	conditions, _, _ := unstructured.NestedSlice(node.Object, "status", "conditions")
	for i, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if ok && condition["type"] == "Ready" {
			condition["status"] = status
			condition["reason"] = "Rebooting"
			conditions[i] = condition
			_ = unstructured.SetNestedSlice(node.Object, conditions, "status", "conditions")
			return
		}
	}
	conditions = append(conditions, map[string]any{"type": "Ready", "status": status, "reason": "Rebooting", "observedGeneration": int64(1)})
	_ = unstructured.SetNestedSlice(node.Object, conditions, "status", "conditions")
}

func dpuNodeReadyStatus(node *unstructured.Unstructured) string {
	if node == nil {
		return ""
	}
	conditions, _, _ := unstructured.NestedSlice(node.Object, "status", "conditions")
	for _, raw := range conditions {
		if condition, ok := raw.(map[string]any); ok && condition["type"] == "Ready" {
			if status, ok := condition["status"].(string); ok {
				return status
			}
		}
	}
	return ""
}

func (f *activationIntegrationFake) replaceActivationObject(objects []unstructured.Unstructured, replacement *unstructured.Unstructured) {
	for i := range objects {
		if tenantStageKey(&objects[i]) == tenantStageKey(replacement) {
			objects[i] = *replacement.DeepCopy()
			return
		}
	}
}

func (f *activationIntegrationFake) seedActivationChildren(deployment *unstructured.Unstructured) error {
	if err := f.seedActivationVersions(deployment); err != nil {
		return err
	}
	software, softwareFound, softwareErr := unstructured.NestedString(deployment.Object, "spec", "dpus", "blueFieldSoftware")
	flavor, flavorFound, flavorErr := unstructured.NestedString(deployment.Object, "spec", "dpus", "flavor")
	if softwareErr != nil || !softwareFound || software == "" || flavorErr != nil || !flavorFound || flavor == "" {
		return fmt.Errorf("fixture retained consumed software and flavor are missing")
	}
	for _, selected := range f.activationSelected {
		if selected.dpuSet == nil {
			return fmt.Errorf("fixture selected DPUSet intent is missing")
		}
		set := selected.dpuSet.DeepCopy()
		set.SetUID(activationIntegrationUID(activationIntegrationSetUID, selected.item.device.Serial))
		set.SetResourceVersion("1")
		set.SetGeneration(1)
		set.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: deployment.GetAPIVersion(), Kind: deployment.GetKind(), Name: deployment.GetName(), UID: deployment.GetUID()}})
		_ = unstructured.SetNestedMap(set.Object, map[string]any{"hold": true}, "spec", "dpuTemplate", "spec", "nodeEffect")
		f.sets = append(f.sets, *set)

		dpu := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPU",
			"metadata": map[string]any{"name": selected.dpuName, "namespace": deployment.GetNamespace(), "uid": string(activationIntegrationUID(activationIntegrationDPUUID, selected.item.device.Serial)), "resourceVersion": "1", "generation": int64(1),
				"ownerReferences": []any{map[string]any{"apiVersion": set.GetAPIVersion(), "kind": set.GetKind(), "name": set.GetName(), "uid": string(set.GetUID())}},
				"labels":          map[string]any{activationDPUSetNameLabel: set.GetName(), activationDPUSetNSLabel: set.GetNamespace(), activationDPUDeviceLabel: selected.deviceName()}},
			// Mirror DPF's selected tenant reference for post-activation lease joins.
			"spec": map[string]any{"blueFieldSoftware": software, "dpuFlavor": flavor, "dpuNodeName": selected.nodeName, "dpuDeviceName": selected.deviceName(), "serialNumber": selected.item.device.Serial,
				"cluster": map[string]any{"name": f.tenant.expected.Name, "namespace": f.tenant.expected.Namespace}},
			"status": map[string]any{"phase": "Rebooting", "previousPhase": "Update Firmware", "rebootStatus": map[string]any{"phase": "Pending", "reason": activationManualWaitReason, "method": "SystemLevelReset", "lastTransitionTime": "2026-11-13T13:00:00Z"},
				"agentStatus": map[string]any{"lastStartupTime": "2026-11-13T12:00:00Z", "initialBootID": "11111111-1111-4111-8111-111111111111", "lastObservedPendingNvconfig": map[string]any{"bootID": "11111111-1111-4111-8111-111111111111"}},
				"conditions":  []any{conditionMap("NodeEffectReady", "True", "Success", 1)}},
		}}
		if f.omitAgentBaseline {
			unstructured.RemoveNestedField(dpu.Object, "status", "agentStatus")
		}
		f.dpus = append(f.dpus, *dpu)

		node, err := f.base.Get(context.Background(), dpuNodeIdentity(deployment.GetNamespace(), selected.nodeName))
		if err != nil {
			return err
		}
		annotations := node.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[activationExternalRebootKey] = "true"
		node.SetAnnotations(annotations)
		_ = unstructured.SetNestedField(node.Object, "SystemLevelReset", "status", "rebootMethod")
		f.base.resources[tenantStageKey(node)] = node.DeepCopy()

		maintenance := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUNodeMaintenance",
			"metadata": map[string]any{"name": selected.nodeName + "-hold", "namespace": deployment.GetNamespace(), "uid": string(activationIntegrationUID(activationIntegrationMaintUID, selected.item.device.Serial)), "resourceVersion": "1",
				"labels": map[string]any{activationDPUNodeLabel: selected.nodeName}, "annotations": map[string]any{activationHoldKey: "true"}},
			"spec":   map[string]any{"dpuNodeName": selected.nodeName, "nodeEffect": map[string]any{"hold": true}, "requestor": []any{selected.dpuName}},
			"status": map[string]any{"conditions": []any{conditionMap("NodeEffectApplied", "True", "Success", 1)}},
		}}
		f.maint = append(f.maint, *maintenance)
	}
	parent := deployment.GetNamespace() + "_" + deployment.GetName()
	versionLabels := map[string]string{activationParentLabel: parent, "svc.dpu.nvidia.com/dpuservice-doca-hbn-version": "doca-hbn-random", "svc.dpu.nvidia.com/dpuservicechain-version": deployment.GetName() + "-current"}
	additional := []string{parent + "_doca-hbn-random", parent + "_" + deployment.GetName() + "-current"}
	for i := range f.sets {
		if err := unstructured.SetNestedStringMap(f.sets[i].Object, versionLabels, "spec", "dpuTemplate", "spec", "cluster", "nodeLabels"); err != nil {
			return err
		}
		if err := unstructured.SetNestedStringSlice(f.sets[i].Object, additional, "spec", "dpuTemplate", "spec", "nodeEffect", "nodeMaintenanceAdditionalRequestors"); err != nil {
			return err
		}
	}
	for i := range f.dpus {
		// Pinned createDPU merges user labels and then stamps its system trio.
		// Keep this fixture independent of the production expected-label helper.
		dpuLabels := map[string]string{}
		for key, value := range versionLabels {
			dpuLabels[key] = value
		}
		nodeName, found, nodeErr := unstructured.NestedString(f.dpus[i].Object, "spec", "dpuNodeName")
		if nodeErr != nil || !found || nodeName == "" {
			return fmt.Errorf("fixture DPU node binding is missing")
		}
		dpuLabels["provisioning.dpu.nvidia.com/dpunode-name"] = nodeName
		dpuLabels["provisioning.dpu.nvidia.com/dpunode-namespace"] = f.dpus[i].GetNamespace()
		dpuLabels["operator.dpu.nvidia.com/dpf-version"] = f.execution.operatorVersion
		if err := unstructured.SetNestedStringMap(f.dpus[i].Object, dpuLabels, "spec", "cluster", "nodeLabels"); err != nil {
			return err
		}
		if err := unstructured.SetNestedMap(f.dpus[i].Object, map[string]any{"hold": true, "nodeMaintenanceAdditionalRequestors": []any{additional[0], additional[1]}}, "spec", "nodeEffect"); err != nil {
			return err
		}
	}
	for i := range f.maint {
		requestors, _, err := unstructured.NestedStringSlice(f.maint[i].Object, "spec", "requestor")
		if err != nil {
			return err
		}
		requestors = append(requestors, additional...)
		if err := unstructured.SetNestedStringSlice(f.maint[i].Object, requestors, "spec", "requestor"); err != nil {
			return err
		}
	}
	return nil
}

func (f *activationIntegrationFake) setSelected(selected []activationSelectedDevice, enrollment []enrollmentBMC) {
	f.activationSelected = selected
	f.selected = enrollment
}

func activationIntegrationUID(base types.UID, serial string) types.UID {
	if serial == "SERIAL-A" {
		return base
	}
	return types.UID(string(base) + "-" + strings.ToLower(serial))
}

func setActivationDPUReady(dpu *unstructured.Unstructured) {
	_ = unstructured.SetNestedField(dpu.Object, "Ready", "status", "phase")
	_ = unstructured.SetNestedField(dpu.Object, int64(1), "status", "observedGeneration")
	_ = unstructured.SetNestedSlice(dpu.Object, []any{
		conditionMap("NodeEffectReady", "True", "Success", 1), conditionMap("NodeEffectRemoved", "True", "Success", 1), conditionMap("Ready", "True", "Success", 1),
	}, "status", "conditions")
}

func setActivationReady(object *unstructured.Unstructured) {
	_ = unstructured.SetNestedField(object.Object, int64(1), "status", "observedGeneration")
	_ = unstructured.SetNestedSlice(object.Object, []any{conditionMap("Ready", "True", "Success", 1)}, "status", "conditions")
}

func deepCopyUnstructuredList(objects []unstructured.Unstructured) []unstructured.Unstructured {
	result := make([]unstructured.Unstructured, len(objects))
	for i := range objects {
		result[i] = *objects[i].DeepCopy()
	}
	return result
}

var _ activationStageClient = (*activationIntegrationFake)(nil)

func TestRunActivationStageCreatesClaimRebootsSelectedHostAndReachesReady(t *testing.T) {
	for _, nodeReady := range []string{"False", "Unknown"} {
		t.Run("node-ready-"+strings.ToLower(nodeReady), func(t *testing.T) {
			testRunActivationStageCreatesClaimRebootsSelectedHostAndReachesReady(t, nodeReady)
		})
	}
}

func testRunActivationStageCreatesClaimRebootsSelectedHostAndReachesReady(t *testing.T, nodeReady string) {
	fixture := newActivationIntegrationFixture(t)
	fixture.client.nodeReadyOnCreate = nodeReady
	var callbacks int
	rebooter := RebooterFunc(func(_ context.Context, request ActivationRequest) (ActivationObservation, error) {
		callbacks++
		if len(request.DPUNodes) != 1 || len(request.DPUNodes[0].SelectedDPUs) != 1 || request.HostSystemUUID != fixture.host.SystemUUID {
			t.Fatalf("reboot callback received an unexpected physical cohort: %#v", request)
		}
		fixture.client.completeReboot(request.DPUNodes[0].NodeName)
		nodeKey := "DPUNode/" + fixture.client.deployment.GetNamespace() + "/" + request.DPUNodes[0].NodeName
		if node := fixture.client.base.resources[nodeKey]; node == nil || dpuNodeReadyStatus(node) != nodeReady {
			t.Fatalf("DPUNode Ready status was %q while reboot was pending, want %q", dpuNodeReadyStatus(node), nodeReady)
		}
		return ActivationObservation{HostSystemUUID: request.HostSystemUUID, HostAdapterSerial: request.HostAdapterSerial,
			PreviousBootID: "boot-before", CurrentBootID: "boot-after", Reconnected: true}, nil
	})

	receipt, err := runActivationStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload,
		fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, rebooter)
	if err != nil {
		_, _, scopeErr := observeSelectedEnrollment(fixture.ctx, fixture.client, fixture.prepared.execution, fixture.prepared.selected,
			fixture.controlPlane.enrollment.Devices, fixture.journal)
		t.Fatalf("runActivationStage(): %v; final enrollment observation: %v; boundary mutation applied=%v", err, scopeErr, fixture.client.boundaryDPUChanged)
	}
	if receipt.DeploymentUID != activationIntegrationDeploymentUID || len(receipt.DPUs) != 1 || callbacks != 1 || fixture.client.guardCalls != 1 {
		t.Fatalf("receipt=%#v reboot callbacks=%d guard creates=%d", receipt, callbacks, fixture.client.guardCalls)
	}
	if len(fixture.client.patches) != 2 || fixture.client.patches[0] != activationHoldKey || fixture.client.patches[1] != activationExternalRebootKey {
		t.Fatalf("selected hold/reboot acknowledgement patches=%v", fixture.client.patches)
	}
	if fixture.client.guard == nil || !activationRebootGuardMatches(fixture.client.guard, fixture.guardDesired, fixture.guardDigest) {
		t.Fatal("stage did not leave the exact immutable selected-cohort claim")
	}
	if !fixture.client.advancedNodeRV {
		t.Fatal("fixture did not advance the authoritative DPUNode resourceVersion during reboot")
	}
	report := fixture.journal.file.(*managementMemoryFile).String()
	if !strings.Contains(report, "EXTERNAL_REBOOT_GUARD_CREATED") || !strings.Contains(report, "EXTERNAL_HOST_REBOOT_VERIFIED") ||
		!strings.Contains(report, "EXTERNAL_REBOOT_ACKNOWLEDGED") || !strings.Contains(report, "SELECTED_DPU_ACTIVATION_COMPLETE") {
		t.Fatalf("stage report omitted claim, reboot, acknowledgement or readiness confirmation: %s", report)
	}
}

func TestRunActivationStageRetryCannotReplayUncertainHostReboot(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	var callbacks int
	rebooter := RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
		callbacks++
		return ActivationObservation{}, errors.New("reboot response lost")
	})
	if _, err := runActivationStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload,
		fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, rebooter); err == nil {
		t.Fatal("uncertain external reboot was reported as successful")
	}
	if fixture.client.guard == nil || callbacks != 1 {
		t.Fatalf("first uncertain run guard=%v callback count=%d", fixture.client.guard != nil, callbacks)
	}
	if _, err := runActivationStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload,
		fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, rebooter); err == nil {
		t.Fatal("retry adopted the existing one-time claim")
	}
	if callbacks != 1 || fixture.client.guardCalls != 1 || len(fixture.client.patches) != 1 || fixture.client.patches[0] != activationHoldKey {
		t.Fatalf("retry replayed reboot or acknowledged it: callbacks=%d claimAttempts=%d patches=%v", callbacks, fixture.client.guardCalls, fixture.client.patches)
	}
}

func TestRunActivationStageRejectsDPUIdentityOrSourcePhaseChangeBeforeRebootClaim(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	tests := []struct {
		mutation string
		reason   string
	}{
		{mutation: "uid", reason: "ACTIVATION_REBOOT_SCOPE_CHANGED"},
		{mutation: "phase", reason: "ACTIVATION_REBOOT_SCOPE_CHANGED"},
		{mutation: "device-uid", reason: "ACTIVATION_ENROLLMENT_SCOPE_CHANGED"},
		{mutation: "node-uid", reason: "ACTIVATION_ENROLLMENT_SCOPE_CHANGED"},
		{mutation: "secret-uid", reason: "ACTIVATION_ENROLLMENT_SECRET_CHANGED"},
		{mutation: "secret-password", reason: "ACTIVATION_ENROLLMENT_SECRET_CHANGED"},
		{mutation: "extra-dpu", reason: "ACTIVATION_ENROLLMENT_SCOPE_CHANGED"},
		{mutation: "extra-dpuset", reason: "ACTIVATION_ENROLLMENT_SCOPE_CHANGED"},
	}
	for _, tc := range tests {
		t.Run(tc.mutation, func(t *testing.T) {
			fixture := newActivationIntegrationFixtureFromExecution(t, execution, false)
			fixture.client.mutateBoundaryDPU = tc.mutation
			var callbacks int
			rebooter := RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
				callbacks++
				return ActivationObservation{}, nil
			})
			_, err := runActivationStage(fixture.ctx, fixture.client, fixture.prepared, fixture.controlPlane, fixture.workload,
				fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, rebooter)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("changed %s returned error %v, want %s", tc.mutation, err, tc.reason)
			}
			if !fixture.client.boundaryDPUChanged || fixture.client.guardCalls != 0 || fixture.client.guard != nil || callbacks != 0 {
				t.Fatalf("changed %s reached an external effect: changed=%v guard attempts=%d guard=%v callbacks=%d",
					tc.mutation, fixture.client.boundaryDPUChanged, fixture.client.guardCalls, fixture.client.guard != nil, callbacks)
			}
		})
	}
}

type activationIntegrationFixture struct {
	ctx          context.Context
	client       *activationIntegrationFake
	prepared     *preparedControlPlaneRun
	controlPlane controlPlaneReceipts
	workload     workloadPreparationReceipt
	inventory    *inventory.Inventory
	request      config.Request
	journal      *executionJournal
	host         inventory.Device
	hosts        []inventory.Device
	guardDesired *unstructured.Unstructured
	guardDigest  string
}

func newActivationIntegrationFixture(t *testing.T) activationIntegrationFixture {
	return newActivationIntegrationFixtureForSerials(t, []string{"SERIAL-A"})
}

func newActivationIntegrationFixtureForSerials(t *testing.T, serials []string) activationIntegrationFixture {
	return newActivationIntegrationCohortFixture(t, serials, false)
}

// sameHost builds two physical cards on one host from the outset, rather than
// preparing an unrelated host/plan and overwriting its retained identities.
func newActivationIntegrationCohortFixture(t *testing.T, serials []string, sameHost bool) activationIntegrationFixture {
	t.Helper()
	return newActivationIntegrationFixtureFromExecution(t, prepareSoftwareStageExecution(t, serials), sameHost)
}

func newActivationIntegrationFixtureFromExecution(t *testing.T, execution *managementExecution, sameHost bool) activationIntegrationFixture {
	t.Helper()
	receipts, selected, _, stage, _ := softwareStageFixtureFromExecution(t, execution)
	client := &activationIntegrationFake{execution: execution, softwarePreparationFake: &softwarePreparationFake{softwareStageFake: stage}}
	if err := operatorBindingForExecution(stage.operator, execution); err != nil {
		t.Fatal(err)
	}
	expected, desiredCluster, err := tenantPlanInputs(execution.plan)
	if err != nil {
		t.Fatal(err)
	}
	currentCluster, err := execution.reconciler.markedDesired(desiredCluster)
	if err != nil {
		t.Fatal(err)
	}
	currentCluster.SetUID(types.UID("dc-uid"))
	currentCluster.SetResourceVersion("5")
	currentCluster.SetGeneration(1)
	currentCluster.Object["status"] = runtime.DeepCopyJSONValue(stage.sources.sources.observed.DPUCluster.Object["status"])
	client.base.resources[tenantStageKey(currentCluster)] = currentCluster.DeepCopy()
	client.base.clusters = []unstructured.Unstructured{*currentCluster.DeepCopy()}
	tenant, _ := tenantIdentityClientFixture(t, expected, stage.sources)
	tenant.source = client
	client.tenant = tenant
	client.base.tenant = tenant
	receipts.tenant.Client = tenant

	request := controlPlaneTestRequest(t)
	firstDPUTarget := -1
	for i := range request.Targets {
		if request.Targets[i].Endpoint == "https://192.0.2.20" {
			request.Targets[i].Endpoint = "https://192.0.2.30"
			firstDPUTarget = i
		}
	}
	if firstDPUTarget < 0 {
		t.Fatal("activation fixture is missing its initial DPU access target")
	}
	for i := 1; i < len(selected); i++ {
		target := request.Targets[firstDPUTarget]
		target.InputRef = fmt.Sprintf("dpu-input-%d", i+1)
		target.Endpoint = "https://" + selected[i].device.BMCIP
		request.Targets = append(request.Targets, target)
	}
	hosts := make([]inventory.Device, 0, len(selected))
	dpus := make([]inventory.Device, 0, len(selected))
	associations := make([]inventory.Association, 0, len(selected))
	for i, item := range selected {
		suffix := string(rune('a' + i))
		systemUUID := "12345678-1234-1234-1234-123456789abc"
		if i > 0 {
			systemUUID = fmt.Sprintf("12345678-1234-1234-1234-123456789ab%d", i+1)
		}
		host := inventory.Device{ID: "host-" + suffix, InputRef: "host-input-" + suffix,
			SSHEndpoint: "ssh://host-" + suffix + ".example.test:22", SystemUUID: systemUUID,
			Adapters: []inventory.Adapter{{Key: "adapter-" + suffix, SerialNumber: item.device.Serial, IdentityState: "known"}}}
		if sameHost && i > 0 {
			hosts[0].Adapters = append(hosts[0].Adapters, host.Adapters[0])
			host = hosts[0]
		} else {
			hosts = append(hosts, host)
			request.Targets = append(request.Targets, config.Target{Role: "hosts", InputRef: host.InputRef, Endpoint: host.SSHEndpoint, SSH: &config.SSHAccess{}})
		}
		dpu := inventory.Device{ID: "dpu-" + suffix, SerialNumber: item.device.Serial}
		dpus = append(dpus, dpu)
		associations = append(associations, inventory.Association{DPUID: dpu.ID, HostID: host.ID, HostAdapterKey: "adapter-" + suffix, SerialNumber: dpu.SerialNumber})
	}
	host := hosts[0]
	inv := &inventory.Inventory{FormatVersion: inventory.FormatVersion, Hosts: hosts, DPUs: dpus, Associations: associations}

	journal := testJournal(execution)
	ctx := tenantStageContext(t)
	probes := probe.Runner{Assets: catalog.ProbeAssets(), Journal: func([]probe.Receipt) error { return nil }}
	prepared, err := prepareControlPlaneWithExecution(ctx, client, execution, journal, probes, request, time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("prepareControlPlaneWithExecution(): %v", err)
	}
	chartVerifier := func(context.Context, *generation.Plan) (PreflightCheck, error) {
		return PreflightCheck{ID: workloadChartSourceCheckID, State: "passed", ReasonCode: "HBN_CHART_ARCHIVE_MATCHED", Message: "fixture retained chart bytes matched"}, nil
	}
	objects, err := checkWorkloadPreparationInputs(ctx, client, prepared, journal, chartVerifier)
	if err != nil {
		t.Fatalf("checkWorkloadPreparationInputs(): %v", err)
	}
	controlPlane, err := runPreparedControlPlane(ctx, client, prepared, probes, journal, time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("runPreparedControlPlane(): %v", err)
	}
	workload, err := runPreparedWorkload(ctx, client, prepared, controlPlane, probes, journal, time.Millisecond, time.Second, 1<<20, chartVerifier, objects)
	if err != nil {
		t.Fatalf("runPreparedWorkload(): %v", err)
	}
	if len(workload.Resources) != 8 { // BlueFieldSoftware is represented separately by workload.Software.
		t.Fatalf("runPreparedWorkload() returned %d non-software prerequisite receipts, want 8", len(workload.Resources))
	}
	if _, _, err := observeSelectedEnrollment(ctx, client, prepared.execution, prepared.selected, controlPlane.enrollment.Devices, journal); err != nil {
		t.Fatalf("fresh selected enrollment observation after workload preparation: %v", err)
	}
	deployment, err := retainedDeploymentObject(execution)
	if err != nil {
		t.Fatal(err)
	}
	deployment.SetUID(activationIntegrationDeploymentUID)
	bound, _, err := bindActivationHosts(prepared.selected, controlPlane.enrollment, inv, request.Targets, deployment)
	if err != nil {
		t.Fatalf("bindActivationHosts(): %v", err)
	}
	client.setSelected(bound, selected)
	phases := map[types.UID]string{}
	for _, item := range bound {
		if item.host.ID == host.ID {
			phases[activationIntegrationUID(activationIntegrationDPUUID, item.item.device.Serial)] = "Update Firmware"
		}
	}
	guardDesired, guardDigest, err := activationRebootGuardObject(deployment.DeepCopy(), execution.plan.Digest(), activationRequestForTest(host, bound), phases)
	if err != nil {
		t.Fatalf("activationRebootGuardObject(): %v", err)
	}
	return activationIntegrationFixture{ctx: ctx, client: client, prepared: prepared, controlPlane: controlPlane, workload: workload,
		inventory: inv, request: request, journal: journal, host: host, hosts: hosts, guardDesired: guardDesired, guardDigest: guardDigest}
}

func activationRequestForTest(host inventory.Device, selected []activationSelectedDevice) ActivationRequest {
	request := ActivationRequest{HostInputRef: host.InputRef, HostEndpoint: host.SSHEndpoint, HostSystemUUID: host.SystemUUID,
		HostAdapterKey: "adapter-a"}
	nodes := map[string]*ActivationNode{}
	for _, item := range selected {
		if item.host.ID != host.ID {
			continue
		}
		request.HostAdapterKey = item.adapter.Key
		request.HostAdapterSerial = item.adapter.SerialNumber
		node := nodes[item.nodeName]
		if node == nil {
			node = &ActivationNode{NodeName: item.nodeName, NodeUID: item.receipt.DPUNodeUID, RebootMethod: "SystemLevelReset"}
			nodes[item.nodeName] = node
		}
		node.SelectedDPUs = append(node.SelectedDPUs, ActivationDPU{SerialNumber: item.item.device.Serial, DPUName: item.dpuName, DPUUID: activationIntegrationUID(activationIntegrationDPUUID, item.item.device.Serial),
			HostAdapterKey: item.adapter.Key, AdapterSerial: item.adapter.SerialNumber})
	}
	for _, node := range nodes {
		request.DPUNodes = append(request.DPUNodes, *node)
	}
	return request
}

// This is a semantic lifecycle condition, not a Get/List or BMC call count.
func allActivationFixtureHoldsReleased(maint []unstructured.Unstructured) bool {
	if len(maint) == 0 {
		return false
	}
	for _, m := range maint {
		if m.GetAnnotations()[activationHoldKey] != "false" {
			return false
		}
	}
	return true
}

type activationObservationOnlyDriftClient struct {
	*activationIntegrationFake
	mutation string
	changed  bool
}

func (c *activationObservationOnlyDriftClient) List(ctx context.Context, wanted *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	objects, err := c.activationIntegrationFake.List(ctx, wanted, selector)
	if err != nil || wanted.GetKind() != "DPU" || wanted.GetNamespace() == "" || c.changed || !allActivationFixtureHoldsReleased(c.maint) {
		return objects, err
	}
	if len(objects) != 1 {
		return nil, fmt.Errorf("observation-only fixture needs one selected DPU")
	}
	c.changed = true
	if c.mutation == "uid" {
		objects[0].SetUID("observation-only-uid")
	} else if err := unstructured.SetNestedField(objects[0].Object, "DPU Config", "status", "previousPhase"); err != nil {
		return nil, err
	}
	return objects, nil
}

func TestActivationRejectsObservationOnlyRebootSnapshotDrift(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, mutation := range []string{"uid", "phase"} {
		t.Run(mutation, func(t *testing.T) {
			f := newActivationIntegrationFixtureFromExecution(t, execution, false)
			c := &activationObservationOnlyDriftClient{activationIntegrationFake: f.client, mutation: mutation}
			f.controlPlane.tenant.Client.source = c
			callbacks := 0
			_, err := runActivationStage(f.ctx, c, f.prepared, f.controlPlane, f.workload, f.inventory, f.request, f.journal, time.Millisecond, time.Second,
				RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
					callbacks++
					return ActivationObservation{}, nil
				}))
			if !c.changed || err == nil || !strings.Contains(err.Error(), "ACTIVATION_REBOOT_SCOPE_CHANGED") || c.guardCalls != 0 || c.resetSafetyCalls != 0 || callbacks != 0 {
				t.Fatalf("changed=%v err=%v claims=%d resetchecks=%d callbacks=%d", c.changed, err, c.guardCalls, c.resetSafetyCalls, callbacks)
			}
			if len(c.dpus) != 1 || c.dpus[0].GetUID() != activationIntegrationDPUUID {
				t.Fatal("observation-only injection mutated authoritative DPU identity")
			}
			phase, _, readErr := unstructured.NestedString(c.dpus[0].Object, "status", "previousPhase")
			if readErr != nil || phase != "Update Firmware" {
				t.Fatalf("observation-only injection mutated authoritative phase: %q %v", phase, readErr)
			}
		})
	}
}
