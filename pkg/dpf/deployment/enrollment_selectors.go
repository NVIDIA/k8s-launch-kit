// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const enrollmentSelectorKey = "launch-kit.nvidia.com/dpu"

// enrollmentSelectorLabels preserves all observed labels. Only the native
// selector key is ours to create; library-supplied selectors must already exist.
func enrollmentSelectorLabels(object *unstructured.Unstructured, wanted map[string]string) (map[string]string, bool, error) {
	if object == nil || len(wanted) == 0 {
		return nil, false, fmt.Errorf("retained device selectors are unavailable")
	}
	labels, _, err := unstructured.NestedStringMap(object.Object, "metadata", "labels")
	if err != nil {
		return nil, false, fmt.Errorf("device labels are malformed")
	}
	labels = maps.Clone(labels)
	if labels == nil {
		labels = map[string]string{}
	}
	changed := false
	for key, value := range wanted {
		if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
			return nil, false, fmt.Errorf("retained device selector is invalid")
		}
		if old, found := labels[key]; found {
			if old != value {
				return nil, false, fmt.Errorf("device selector conflicts with retained intent")
			}
			continue
		}
		if key != enrollmentSelectorKey {
			return nil, false, fmt.Errorf("an unowned retained device selector is absent")
		}
		labels[key], changed = value, true
	}
	return labels, changed, nil
}

func verifyEnrollmentSelectors(object *unstructured.Unstructured, wanted map[string]string) error {
	_, changed, err := enrollmentSelectorLabels(object, wanted)
	if err != nil {
		return err
	}
	if changed {
		return fmt.Errorf("the selected device is missing its retained selector")
	}
	owners, _, err := unstructured.NestedSlice(object.Object, "metadata", "ownerReferences")
	if err != nil || len(owners) != 0 {
		return fmt.Errorf("selected device has an unexpected owner")
	}
	return nil
}

// bindEnrollmentSelectors establishes eligibility only after full enrollment.
// Each write has a fresh whole-cohort fence; successful earlier writes remain
// recorded if a later device fails. Observers never publish labels implicitly.
func bindEnrollmentSelectors(ctx context.Context, client enrollmentManagementClient, execution *managementVerificationInputs,
	bootstrap managementBootstrapReceipt, tenant tenantStageReceipt, selected []enrollmentBMC, receipt enrollmentStageReceipt,
	password string, journal *executionJournal, requestTimeout time.Duration,
) error {
	if len(selected) == 0 || len(receipt.Devices) != len(selected) {
		return enrollmentStageFailure(journal, "ENROLLMENT_SELECTOR_SCOPE_CHANGED", "Selector binding requires the complete selected enrollment receipt.", nil)
	}
	retained := execution.plan.Intent().Devices
	if len(retained) != len(selected) {
		return enrollmentStageFailure(journal, "ENROLLMENT_SELECTOR_SCOPE_CHANGED", "Selector binding differs from the retained device cohort.", nil)
	}
	for i, item := range selected {
		if item.device.Serial != retained[i].Serial || !maps.Equal(item.device.SelectorLabels, retained[i].SelectorLabels) {
			return enrollmentStageFailure(journal, "ENROLLMENT_SELECTOR_SCOPE_CHANGED", "Selector binding differs from the retained device intent.", nil)
		}
	}
	for i, item := range selected {
		if err := verifyEnrollmentBoundary(ctx, client, execution, bootstrap, tenant, selected, requestTimeout, receipt.Secret, password, journal); err != nil {
			return err
		}
		if _, ready, err := observeSelectedEnrollment(ctx, client, execution, selected, receipt.Devices, journal); err != nil || !ready {
			return enrollmentStageFailure(journal, "ENROLLMENT_SELECTOR_SCOPE_CHANGED", "The complete selected enrollment scope is not current before selector binding.", err)
		}
		if err := verifyEnrollmentSelectorReceipts(ctx, client, execution.operatorNamespace, selected[:i], receipt.Devices[:i]); err != nil {
			return enrollmentStageFailure(journal, "ENROLLMENT_SELECTOR_SCOPE_CHANGED", "A previously bound selected-device selector changed before the next binding.", err)
		}
		identity := &unstructured.Unstructured{}
		identity.SetAPIVersion(enrollmentDeviceAPIVersion)
		identity.SetKind("DPUDevice")
		identity.SetNamespace(execution.operatorNamespace)
		identity.SetName(strings.ToLower(item.device.Serial))
		current, err := client.Get(ctx, identity)
		event := executionEvent{Stage: generation.StageEnrollment, Operation: "bind-device-selector", Subject: identity.GetNamespace() + "/" + identity.GetName()}
		if err != nil || current == nil || current.GetUID() != receipt.Devices[i].DeviceUID || validateEnrolledDevice(current, item, execution.operatorNamespace, true) != nil || !enrolledDeviceReadiness(current).Ready {
			return journalFailure(journal, event, false, "ENROLLMENT_SELECTOR_IDENTITY_CHANGED", "The selected ready DPUDevice identity changed before selector binding.", err)
		}
		owners, _, ownerErr := unstructured.NestedSlice(current.Object, "metadata", "ownerReferences")
		if ownerErr != nil || len(owners) != 0 {
			return journalFailure(journal, event, false, "ENROLLMENT_SELECTOR_OWNER_CONFLICT", "The selected DPUDevice has an unexpected owner; no selector was changed.", nil)
		}
		_, changed, err := enrollmentSelectorLabels(current, item.device.SelectorLabels)
		if err != nil {
			return journalFailure(journal, event, false, "ENROLLMENT_SELECTOR_CONFLICT", "Retained selectors conflict with device labels, are malformed, or require creating an unowned label; no selector was changed.", nil)
		}
		if !changed {
			event.State, event.ReasonCode, event.Message = "verified", "ENROLLMENT_SELECTOR_MATCHED", "The verified DPU already belongs to its selected deployment group; no labels were changed."
			if err := journal.record(event); err != nil {
				return err
			}
			continue
		}
		event.State, event.ReasonCode, event.Message = "attempted", "ENROLLMENT_SELECTOR_BIND_STARTED", "Assign this verified DPU to its generated deployment group."
		if err := journal.record(event); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return journalFailure(journal, event, false, "ENROLLMENT_SELECTOR_BIND_CANCELED", "Selector binding was canceled before its patch request.", err)
		}
		updated, err := client.patchEnrollmentSelectors(ctx, current, item.device.SelectorLabels)
		if err != nil || !enrollmentSelectorUpdateMatches(current, updated, item.device.SelectorLabels) {
			return journalFailure(journal, event, true, "ENROLLMENT_SELECTOR_BIND_UNCERTAIN", "The selected-device selector patch has no verified result; inspect the exact device before retrying.", err)
		}
		event.State, event.ReasonCode, event.Message = "confirmed", "ENROLLMENT_SELECTOR_BOUND", "The verified DPU belongs to its selected deployment group."
		if err := journal.record(event); err != nil {
			return err
		}
	}
	return nil
}

func verifyEnrollmentSelectorReceipts(ctx context.Context, client resourceReader, namespace string, selected []enrollmentBMC, receipts []enrolledDeviceReceipt) error {
	if len(selected) != len(receipts) {
		return fmt.Errorf("selector receipt cohort differs")
	}
	for i, item := range selected {
		identity := &unstructured.Unstructured{}
		identity.SetAPIVersion(enrollmentDeviceAPIVersion)
		identity.SetKind("DPUDevice")
		identity.SetName(strings.ToLower(item.device.Serial))
		identity.SetNamespace(namespace)
		current, err := client.Get(ctx, identity)
		if err != nil || current == nil || current.GetUID() != receipts[i].DeviceUID || validateEnrolledDevice(current, item, namespace, true) != nil || !enrolledDeviceReadiness(current).Ready || verifyEnrollmentSelectors(current, item.device.SelectorLabels) != nil {
			return fmt.Errorf("selected ready-device identity or retained selector changed")
		}
	}
	return nil
}

func enrollmentSelectorUpdateMatches(before, after *unstructured.Unstructured, wanted map[string]string) bool {
	if before == nil || after == nil || before.GetUID() == "" || after.GetUID() != before.GetUID() || after.GetResourceVersion() == "" || after.GetResourceVersion() == before.GetResourceVersion() ||
		after.GetAPIVersion() != before.GetAPIVersion() || after.GetKind() != before.GetKind() || after.GetNamespace() != before.GetNamespace() || after.GetName() != before.GetName() || after.GetDeletionTimestamp() != nil || after.GetGeneration() != before.GetGeneration() {
		return false
	}
	expected, _, err := enrollmentSelectorLabels(before, wanted)
	actual, _, labelsErr := unstructured.NestedStringMap(after.Object, "metadata", "labels")
	if err != nil || labelsErr != nil || !maps.Equal(expected, actual) || !reflect.DeepEqual(before.Object["spec"], after.Object["spec"]) {
		return false
	}
	for _, field := range []string{"annotations", "ownerReferences", "finalizers"} {
		old, _, oldErr := unstructured.NestedFieldNoCopy(before.Object, "metadata", field)
		value, _, valueErr := unstructured.NestedFieldNoCopy(after.Object, "metadata", field)
		if oldErr != nil || valueErr != nil || !reflect.DeepEqual(old, value) {
			return false
		}
	}
	return true
}

func enrollmentSelectorPatch(object *unstructured.Unstructured, wanted map[string]string) ([]byte, error) {
	if object == nil || object.GetAPIVersion() != enrollmentDeviceAPIVersion || object.GetKind() != "DPUDevice" || object.GetName() == "" || object.GetNamespace() == "" || object.GetUID() == "" || object.GetResourceVersion() == "" || object.GetDeletionTimestamp() != nil {
		return nil, fmt.Errorf("selector patch requires an exact current DPUDevice")
	}
	owners, _, ownerErr := unstructured.NestedSlice(object.Object, "metadata", "ownerReferences")
	if ownerErr != nil || len(owners) != 0 {
		return nil, fmt.Errorf("selector patch cannot modify an owned device")
	}
	labels, changed, err := enrollmentSelectorLabels(object, wanted)
	if err != nil || !changed {
		return nil, fmt.Errorf("selector patch requires only a missing native label")
	}
	ops := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(object.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": object.GetResourceVersion()}}
	_, found, _ := unstructured.NestedStringMap(object.Object, "metadata", "labels")
	if !found {
		ops = append(ops, map[string]any{"op": "add", "path": "/metadata/labels", "value": labels})
	} else {
		ops = append(ops, map[string]any{"op": "add", "path": "/metadata/labels/launch-kit.nvidia.com~1dpu", "value": labels[enrollmentSelectorKey]})
	}
	return json.Marshal(ops)
}

func (c *APIClient) patchEnrollmentSelectors(ctx context.Context, object *unstructured.Unstructured, wanted map[string]string) (*unstructured.Unstructured, error) {
	if c == nil || ctx == nil {
		return nil, fmt.Errorf("selector patch requires a bound client and context")
	}
	data, err := enrollmentSelectorPatch(object, wanted)
	if err != nil {
		return nil, err
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	resource, err := c.resource(ctx, object, true)
	if err != nil {
		return nil, err
	}
	current, err := resource.Patch(ctx, object.GetName(), types.JSONPatchType, data, metav1.PatchOptions{FieldManager: "k8s-launch-kit-dpf"})
	if err != nil {
		return nil, safeAPIError("bind selected DPUDevice selector", err)
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	return current, nil
}
