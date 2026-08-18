// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package crstate

import (
	"context"
	"fmt"
	"sort"

	"github.com/Mellanox/nic-configuration-operator/pkg/consts"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	nicopGroup                     = "configuration.net.nvidia.com"
	nicopVersion                   = "v1alpha1"
	nicopKindInterfaceNameTemplate = "NicInterfaceNameTemplate"
	nicopKindConfigurationTemplate = "NicConfigurationTemplate"
	nicopKindFirmwareTemplate      = "NicFirmwareTemplate"
	nicopKindDevice                = "NicDevice"
)

// templateKind identifies which NicDevice spec field a template populates.
type templateKind int

const (
	templateKindInterfaceName templateKind = iota
	templateKindConfiguration
	templateKindFirmware
)

// nicTemplateValidator returns a Validator closure for the given template
// kind. The validator:
//  1. Verifies the template CR exists.
//  2. For configuration and firmware templates, waits for the operator's
//     authoritative status.nicDevices matched-device list. Interface-name
//     templates have no such status and retain node/spec-based discovery.
//  3. Lists and filters NicDevice CRs to the matched devices whose
//     template-relevant spec field reflects the current template payload.
//  4. Classifies each contributing device via its current-generation
//     conditions[] and
//     aggregates the per-device verdicts (any error→error, any
//     in-progress→in-progress, all success→success).
func nicTemplateValidator(kind templateKind) Validator {
	return func(ctx context.Context, c client.Client, obj *unstructured.Unstructured) (Result, error) {
		src := fmt.Sprintf("%s/%s", obj.GetKind(), obj.GetName())

		// 1. Look up the live template.
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(obj.GroupVersionKind())
		key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
		if err := c.Get(ctx, key, live); err != nil {
			if apierrors.IsNotFound(err) {
				return Result{State: StateNotDeployed, Reason: "not found in cluster", Source: src}, nil
			}
			return Result{State: StateError, Reason: fmt.Sprintf("get error: %v", err), Source: src}, err
		}

		var matchedDeviceNames []string
		if templateUsesMatchedDeviceStatus(kind) {
			var err error
			matchedDeviceNames, _, err = unstructured.NestedStringSlice(live.Object, "status", "nicDevices")
			if err != nil {
				return Result{State: StateError, Reason: fmt.Sprintf("read status.nicDevices: %v", err), Source: src}, nil
			}
			if len(matchedDeviceNames) == 0 {
				return Result{
					State:  StateInProgress,
					Reason: "waiting for nic-configuration-operator to populate status.nicDevices with matched devices",
					Source: src,
				}, nil
			}
		}

		// 2. List all NicDevice CRs in the template namespace. NicDevice is namespaced;
		//    the operator creates them in the operator namespace
		//    alongside the template.
		devices := &unstructured.UnstructuredList{}
		devices.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   nicopGroup,
			Version: nicopVersion,
			Kind:    nicopKindDevice + "List",
		})
		if err := c.List(ctx, devices, client.InNamespace(obj.GetNamespace())); err != nil {
			return Result{State: StateError, Reason: fmt.Sprintf("list NicDevice: %v", err), Source: src}, err
		}
		if templateUsesMatchedDeviceStatus(kind) {
			expectedDeviceNames, err := devicesMatchingCurrentSelectors(ctx, c, live, devices.Items)
			if err != nil {
				return Result{State: StateError, Reason: fmt.Sprintf("evaluate current template selectors: %v", err), Source: src}, err
			}
			if allDeviceNamesPresent(matchedDeviceNames, devices.Items) && !sameStringSet(matchedDeviceNames, expectedDeviceNames) {
				return Result{
					State: StateInProgress,
					Reason: fmt.Sprintf(
						"waiting for status.nicDevices to reflect current template selectors (reported: %v; expected: %v)",
						sortedStrings(matchedDeviceNames), sortedStrings(expectedDeviceNames)),
					Source: src,
				}, nil
			}
		}

		var (
			contributing         []unstructured.Unstructured
			details              = make(map[string]string)
			anyInProgress        bool
			anyRetryableError    bool
			anyNonRetryableError bool
		)
		if templateUsesMatchedDeviceStatus(kind) {
			devicesByName := make(map[string]*unstructured.Unstructured, len(devices.Items))
			for i := range devices.Items {
				device := &devices.Items[i]
				devicesByName[device.GetName()] = device
			}
			for _, name := range matchedDeviceNames {
				device, ok := devicesByName[name]
				if !ok {
					details[name] = "listed in template status but NicDevice not found yet"
					anyInProgress = true
					continue
				}
				if !deviceTargetedByTemplate(device, kind) {
					details[deviceLabel(device)] = fmt.Sprintf("matched by template; waiting for %s", templateSpecField(kind))
					anyInProgress = true
					continue
				}
				if current, reason := deviceSpecReflectsTemplate(device, live, kind); !current {
					details[deviceLabel(device)] = reason
					anyInProgress = true
					continue
				}
				contributing = append(contributing, *device)
			}
		} else {
			// NicInterfaceNameTemplate has no matched-device status. Retain
			// the existing node-selector and populated-spec discovery path.
			nodeSelector, _, _ := unstructured.NestedStringMap(live.Object, "spec", "nodeSelector")
			targetNodes, err := listNodesMatchingSelector(ctx, c, nodeSelector)
			if err != nil {
				return Result{State: StateError, Reason: fmt.Sprintf("list nodes: %v", err), Source: src}, err
			}
			if len(targetNodes) == 0 {
				return Result{State: StateError, Reason: "spec.nodeSelector matched no nodes — check labels", Source: src}, nil
			}
			targetSet := make(map[string]struct{}, len(targetNodes))
			for _, node := range targetNodes {
				targetSet[node] = struct{}{}
			}

			devicesOnTarget := 0
			for i := range devices.Items {
				device := &devices.Items[i]
				node, _, _ := unstructured.NestedString(device.Object, "status", "node")
				if _, ok := targetSet[node]; !ok {
					continue
				}
				devicesOnTarget++
				if deviceTargetedByTemplate(device, kind) {
					contributing = append(contributing, *device)
				}
			}
			if len(contributing) == 0 {
				var reason string
				if devicesOnTarget == 0 {
					reason = fmt.Sprintf("no NicDevice CRs on target nodes yet — waiting for nic-configuration-operator daemon to discover devices (selector: %v)", nodeSelector)
				} else {
					reason = fmt.Sprintf("found %d NicDevice(s) on target nodes; waiting for nic-configuration-operator to populate %s", devicesOnTarget, templateSpecField(kind))
				}
				return Result{State: StateInProgress, Reason: reason, Source: src}, nil
			}
		}

		// 4. Classify each contributing device.
		for i := range contributing {
			d := &contributing[i]
			label := deviceLabel(d)
			state, reason, retryable := classifyDevice(d, kind)
			details[label] = reason
			switch state {
			case StateError:
				if retryable {
					anyRetryableError = true
				} else {
					anyNonRetryableError = true
				}
			case StateInProgress:
				anyInProgress = true
			case StateSuccess:
				// no-op
			case StateNotDeployed:
				// treat unset as in-progress: the device hasn't been reconciled yet
				anyInProgress = true
			}
		}

		switch {
		case anyRetryableError || anyNonRetryableError:
			return Result{
				State:     StateError,
				Reason:    summarizeNodeStates(details),
				Details:   details,
				Source:    src,
				Retryable: anyRetryableError && !anyNonRetryableError,
			}, nil
		case anyInProgress:
			return Result{State: StateInProgress, Reason: summarizeNodeStates(details), Details: details, Source: src}, nil
		default:
			reason := fmt.Sprintf("%d device(s) reconciled", len(contributing))
			if templateUsesMatchedDeviceStatus(kind) {
				reason = fmt.Sprintf("%d matched device(s) reconciled", len(contributing))
			}
			return Result{State: StateSuccess, Reason: reason, Details: details, Source: src}, nil
		}
	}
}

func templateUsesMatchedDeviceStatus(kind templateKind) bool {
	return kind == templateKindConfiguration || kind == templateKindFirmware
}

func templateSpecField(kind templateKind) string {
	switch kind {
	case templateKindInterfaceName:
		return "spec.interfaceNameTemplate"
	case templateKindConfiguration:
		return "spec.configuration"
	case templateKindFirmware:
		return "spec.firmware"
	default:
		return "device spec"
	}
}

// deviceTargetedByTemplate reports whether the given device's spec field
// (for the template kind) was populated by the operator — i.e. the
// template's nicSelector picked this device.
func deviceTargetedByTemplate(d *unstructured.Unstructured, kind templateKind) bool {
	switch kind {
	case templateKindInterfaceName:
		_, found, _ := unstructured.NestedMap(d.Object, "spec", "interfaceNameTemplate")
		return found
	case templateKindConfiguration:
		_, found, _ := unstructured.NestedMap(d.Object, "spec", "configuration")
		return found
	case templateKindFirmware:
		_, found, _ := unstructured.NestedMap(d.Object, "spec", "firmware")
		return found
	default:
		return false
	}
}

// deviceSpecReflectsTemplate verifies that the template controller has
// propagated the current template payload to a status-listed device. This
// prevents standalone validation from accepting successful conditions left
// by a previous template generation before the device spec has been updated.
func deviceSpecReflectsTemplate(device, template *unstructured.Unstructured, kind templateKind) (bool, string) {
	waiting := fmt.Sprintf("matched by template; waiting for %s to reflect the current template", templateSpecField(kind))

	switch kind {
	case templateKindConfiguration:
		deviceConfig, found, err := unstructured.NestedMap(device.Object, "spec", "configuration")
		if err != nil || !found {
			return false, waiting
		}

		expectedReset, _, err := unstructured.NestedBool(template.Object, "spec", "resetToDefault")
		if err != nil {
			return false, waiting
		}
		actualReset, _, err := unstructured.NestedBool(deviceConfig, "resetToDefault")
		if err != nil || actualReset != expectedReset {
			return false, waiting
		}

		expectedTemplate, expectedFound, err := unstructured.NestedMap(template.Object, "spec", "template")
		if err != nil {
			return false, waiting
		}
		actualTemplate, actualFound, err := unstructured.NestedMap(deviceConfig, "template")
		if err != nil || actualFound != expectedFound || !apiequality.Semantic.DeepEqual(actualTemplate, expectedTemplate) {
			return false, waiting
		}
	case templateKindFirmware:
		deviceFirmware, found, err := unstructured.NestedMap(device.Object, "spec", "firmware")
		if err != nil || !found {
			return false, waiting
		}
		expectedFirmware, expectedFound, err := unstructured.NestedMap(template.Object, "spec", "template")
		if err != nil || !expectedFound || !apiequality.Semantic.DeepEqual(deviceFirmware, expectedFirmware) {
			return false, waiting
		}
	}

	return true, ""
}

// devicesMatchingCurrentSelectors mirrors the NIC operator's template
// selector contract to prove that status.nicDevices belongs to the current
// template generation. It computes names only; specs and conditions are still
// validated exclusively for the operator-reported status devices.
func devicesMatchingCurrentSelectors(
	ctx context.Context,
	c client.Client,
	template *unstructured.Unstructured,
	devices []unstructured.Unstructured,
) ([]string, error) {
	nodeSelector, _, err := unstructured.NestedStringMap(template.Object, "spec", "nodeSelector")
	if err != nil {
		return nil, fmt.Errorf("read spec.nodeSelector: %w", err)
	}

	var targetNodes map[string]struct{}
	if len(nodeSelector) > 0 {
		nodes, err := listNodesMatchingSelector(ctx, c, nodeSelector)
		if err != nil {
			return nil, fmt.Errorf("list nodes: %w", err)
		}
		targetNodes = make(map[string]struct{}, len(nodes))
		for _, name := range nodes {
			targetNodes[name] = struct{}{}
		}
	}

	nicType, _, err := unstructured.NestedString(template.Object, "spec", "nicSelector", "nicType")
	if err != nil {
		return nil, fmt.Errorf("read spec.nicSelector.nicType: %w", err)
	}
	pciAddresses, _, err := unstructured.NestedStringSlice(template.Object, "spec", "nicSelector", "pciAddresses")
	if err != nil {
		return nil, fmt.Errorf("read spec.nicSelector.pciAddresses: %w", err)
	}
	serialNumbers, _, err := unstructured.NestedStringSlice(template.Object, "spec", "nicSelector", "serialNumbers")
	if err != nil {
		return nil, fmt.Errorf("read spec.nicSelector.serialNumbers: %w", err)
	}
	partNumbers, _, err := unstructured.NestedStringSlice(template.Object, "spec", "nicSelector", "partNumbers")
	if err != nil {
		return nil, fmt.Errorf("read spec.nicSelector.partNumbers: %w", err)
	}

	matches := make([]string, 0, len(devices))
	for i := range devices {
		device := &devices[i]
		node, _, _ := unstructured.NestedString(device.Object, "status", "node")
		if node == "" {
			continue
		}
		if targetNodes != nil {
			if _, ok := targetNodes[node]; !ok {
				continue
			}
		}
		deviceType, _, _ := unstructured.NestedString(device.Object, "status", "type")
		if nicType != "" && nicType != deviceType {
			continue
		}
		if !deviceMatchesPCIAddresses(device, pciAddresses) {
			continue
		}
		serialNumber, _, _ := unstructured.NestedString(device.Object, "status", "serialNumber")
		if len(serialNumbers) > 0 && !containsString(serialNumbers, serialNumber) {
			continue
		}
		partNumber, _, _ := unstructured.NestedString(device.Object, "status", "partNumber")
		if len(partNumbers) > 0 && !containsString(partNumbers, partNumber) {
			continue
		}
		matches = append(matches, device.GetName())
	}
	return matches, nil
}

func deviceMatchesPCIAddresses(device *unstructured.Unstructured, pciAddresses []string) bool {
	if len(pciAddresses) == 0 {
		return true
	}
	ports, _, _ := unstructured.NestedSlice(device.Object, "status", "ports")
	for _, raw := range ports {
		port, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		pci, _, _ := unstructured.NestedString(port, "pci")
		if containsString(pciAddresses, pci) {
			return true
		}
	}
	return false
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func sameStringSet(left, right []string) bool {
	leftSet := make(map[string]struct{}, len(left))
	for _, value := range left {
		leftSet[value] = struct{}{}
	}
	rightSet := make(map[string]struct{}, len(right))
	for _, value := range right {
		rightSet[value] = struct{}{}
	}
	if len(leftSet) != len(rightSet) {
		return false
	}
	for value := range rightSet {
		if _, ok := leftSet[value]; !ok {
			return false
		}
	}
	return true
}

func allDeviceNamesPresent(names []string, devices []unstructured.Unstructured) bool {
	present := make(map[string]struct{}, len(devices))
	for i := range devices {
		present[devices[i].GetName()] = struct{}{}
	}
	for _, name := range names {
		if _, ok := present[name]; !ok {
			return false
		}
	}
	return true
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

// deviceLabel returns "<node>/<first-pci-address>" for a NicDevice, used
// as the key in Result.Details.
func deviceLabel(d *unstructured.Unstructured) string {
	node, _, _ := unstructured.NestedString(d.Object, "status", "node")
	ports, _, _ := unstructured.NestedSlice(d.Object, "status", "ports")
	pci := ""
	if len(ports) > 0 {
		if port, ok := ports[0].(map[string]interface{}); ok {
			pci, _, _ = unstructured.NestedString(port, "pci")
		}
	}
	switch {
	case node != "" && pci != "":
		return node + "/" + pci
	case node != "":
		return node + "/" + d.GetName()
	default:
		return d.GetName()
	}
}

// classifyDevice maps the relevant condition type+status+reason to one of
// the four states. The mapping mirrors nic-configuration-operator's
// controller (internal/controller/nicdevice_controller.go).
func classifyDevice(d *unstructured.Unstructured, kind templateKind) (CRState, string, bool) {
	conds, _, _ := unstructured.NestedSlice(d.Object, "status", "conditions")
	byType := map[string]map[string]interface{}{}
	for _, raw := range conds {
		cond, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(cond, "type")
		if t != "" {
			byType[t] = cond
		}
	}

	switch kind {
	case templateKindInterfaceName:
		return classifyInterfaceName(byType)
	case templateKindConfiguration:
		state, reason := classifyConfiguration(d, byType)
		return state, reason, false
	case templateKindFirmware:
		state, reason := classifyFirmware(d, byType)
		return state, reason, false
	default:
		return StateInProgress, "unknown template kind", false
	}
}

// classifyFirmware inspects FirmwareUpdateInProgress for a device explicitly
// listed in a NicFirmwareTemplate's status.nicDevices.
func classifyFirmware(device *unstructured.Unstructured, byType map[string]map[string]interface{}) (CRState, string) {
	cond, ok := byType[consts.FirmwareUpdateInProgressCondition]
	if !ok {
		return StateInProgress, "FirmwareUpdateInProgress condition not yet set"
	}
	if pending, reason := conditionGenerationPending(device, cond, consts.FirmwareUpdateInProgressCondition); pending {
		return StateInProgress, reason
	}
	reason, _, _ := unstructured.NestedString(cond, "reason")
	message, _, _ := unstructured.NestedString(cond, "message")

	switch reason {
	case consts.DeviceFwMatchReason:
		return StateSuccess, "device firmware matches"
	case consts.FirmwareUpdateStartedReason, consts.PendingNodeMaintenanceReason:
		return StateInProgress, fallbackMessage(message, "firmware update in progress: "+reason)
	case consts.FirmwareUpdateFailedReason,
		consts.FirmwareSourceNotReadyReason,
		consts.FirmwareSourceFailedReason,
		consts.DeviceFwMismatchReason:
		return StateError, fallbackMessage(message, "firmware update failed: "+reason)
	case consts.DeviceFirmwareSpecEmptyReason:
		return StateInProgress, "firmware spec not yet observed"
	case "":
		return StateInProgress, "FirmwareUpdateInProgress reason not yet set"
	default:
		return StateInProgress, fallbackMessage(message, "FirmwareUpdateInProgress reason="+reason)
	}
}

// classifyInterfaceName inspects InterfaceNameApplied. A mismatch remains an
// error for one-shot validation, but is marked retryable so the deploy state
// machine can give newly-written udev rules a bounded reconciliation window.
func classifyInterfaceName(byType map[string]map[string]interface{}) (CRState, string, bool) {
	cond, ok := byType[consts.InterfaceNameCondition]
	if !ok {
		return StateInProgress, "InterfaceNameApplied condition not yet set", false
	}
	reason, _, _ := unstructured.NestedString(cond, "reason")
	status, _, _ := unstructured.NestedString(cond, "status")
	message, _, _ := unstructured.NestedString(cond, "message")

	switch {
	case reason == consts.InterfaceNameAppliedReason && status == "True":
		return StateSuccess, "InterfaceNameApplied", false
	case reason == consts.InterfaceNameMismatchReason:
		return StateError, fallbackMessage(message, "interface name mismatch — udev rules did not apply"), true
	default:
		return StateInProgress, fallbackMessage(fmt.Sprintf("InterfaceNameApplied=%s reason=%s", status, reason), "InterfaceNameApplied unknown"), false
	}
}

// classifyConfiguration inspects ConfigUpdateInProgress (primary) and, when
// the device is targeted by a NicFirmwareTemplate, FirmwareUpdateInProgress.
// Reason is the authoritative classifier — Status=False with
// Reason=UpdateSuccessful is the *success* terminal state, not failure.
func classifyConfiguration(device *unstructured.Unstructured, byType map[string]map[string]interface{}) (CRState, string) {
	if cond, ok := byType[consts.ConfigUpdateInProgressCondition]; ok {
		if pending, reason := conditionGenerationPending(device, cond, consts.ConfigUpdateInProgressCondition); pending {
			return StateInProgress, reason
		}
		reason, _, _ := unstructured.NestedString(cond, "reason")
		message, _, _ := unstructured.NestedString(cond, "message")
		switch reason {
		case consts.UpdateSuccessfulReason:
			// proceed to firmware check below if present
		case consts.UpdateStartedReason, consts.PendingRebootReason:
			return StateInProgress, fallbackMessage(message, "config update in progress: "+reason)
		case consts.PendingFirmwareUpdateReason:
			return StateInProgress, fallbackMessage(message, "config update pending firmware: "+reason)
		case consts.NonVolatileConfigUpdateFailedReason,
			consts.RuntimeConfigUpdateFailedReason,
			consts.SpecValidationFailed,
			consts.IncorrectSpecReason,
			consts.FirmwareError:
			return StateError, fallbackMessage(message, "config update failed: "+reason)
		case consts.DeviceConfigSpecEmptyReason:
			// status.nicDevices says this template targets the device, so
			// this is the previous empty-spec condition waiting to be replaced.
			return StateInProgress, "configuration spec not yet observed"
		case "":
			return StateInProgress, "ConfigUpdateInProgress reason not yet set"
		default:
			return StateInProgress, fallbackMessage(message, "ConfigUpdateInProgress reason="+reason)
		}
	} else {
		return StateInProgress, "ConfigUpdateInProgress condition not yet set"
	}

	// At this point ConfigUpdateInProgress.Reason=UpdateSuccessful.
	// Firmware conditions can remain on a NicDevice after a configuration-only
	// update increments its generation. Ignore that stale condition unless a
	// NicFirmwareTemplate populated spec.firmware for this device.
	if !deviceTargetedByTemplate(device, templateKindFirmware) {
		return StateSuccess, "config reconciled (no firmware spec)"
	}

	// A firmware payload is present, so gate on its terminal state when the
	// operator has published the corresponding condition.
	if cond, ok := byType[consts.FirmwareUpdateInProgressCondition]; ok {
		if pending, reason := conditionGenerationPending(device, cond, consts.FirmwareUpdateInProgressCondition); pending {
			return StateInProgress, reason
		}
		reason, _, _ := unstructured.NestedString(cond, "reason")
		message, _, _ := unstructured.NestedString(cond, "message")
		switch reason {
		case consts.DeviceFwMatchReason:
			return StateSuccess, "device firmware matches and config reconciled"
		case consts.FirmwareUpdateStartedReason, consts.PendingNodeMaintenanceReason:
			return StateInProgress, fallbackMessage(message, "firmware update in progress: "+reason)
		case consts.FirmwareUpdateFailedReason,
			consts.FirmwareSourceNotReadyReason,
			consts.FirmwareSourceFailedReason,
			consts.DeviceFwMismatchReason:
			return StateError, fallbackMessage(message, "firmware update failed: "+reason)
		case consts.DeviceFirmwareSpecEmptyReason:
			// No firmware section configured — config success alone is
			// sufficient.
			return StateSuccess, "config reconciled (no firmware spec)"
		default:
			return StateInProgress, fallbackMessage(message, "FirmwareUpdateInProgress reason="+reason)
		}
	}

	return StateSuccess, "config reconciled"
}

func conditionGenerationPending(device *unstructured.Unstructured, condition map[string]interface{}, conditionType string) (bool, string) {
	generation := device.GetGeneration()
	if generation <= 0 {
		return false, ""
	}

	observed, found, err := unstructured.NestedInt64(condition, "observedGeneration")
	if err != nil || !found || observed < generation {
		return true, fmt.Sprintf("%s has not observed NicDevice generation %d", conditionType, generation)
	}
	return false, ""
}

func fallbackMessage(msg, def string) string {
	if msg == "" {
		return def
	}
	return msg
}

func registerNicConfigValidators(r *Registry) {
	gv := schema.GroupVersion{Group: nicopGroup, Version: nicopVersion}
	r.Register(gv.WithKind(nicopKindInterfaceNameTemplate), nicTemplateValidator(templateKindInterfaceName))
	r.Register(gv.WithKind(nicopKindConfigurationTemplate), nicTemplateValidator(templateKindConfiguration))
	r.Register(gv.WithKind(nicopKindFirmwareTemplate), nicTemplateValidator(templateKindFirmware))
}
