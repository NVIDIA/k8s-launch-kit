// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package crstate

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// These names are the Network Operator state factory's appliedStates names,
// not image or DaemonSet names. Keep this mapping aligned with its NCP API.
var ncpComponentStates = map[string]string{
	"ofedDriver":               "state-OFED",
	"rdmaSharedDevicePlugin":   "state-RDMA-device-plugin",
	"sriovDevicePlugin":        "state-SRIOV-device-plugin",
	"ibKubernetes":             "state-ib-kubernetes",
	"nvIpam":                   "state-nv-ipam-cni",
	"nicFeatureDiscovery":      "state-nic-feature-discovery",
	"docaTelemetryService":     "state-doca-telemetry-service",
	"nicConfigurationOperator": "state-nic-configuration-operator",
	"spectrumXOperator":        "state-spectrum-x-operator",
}

// openShiftPolicyResult overrides a non-ready aggregate only when external
// states explain it and all configured components have independently observed
// readiness. Unknown spec fields, malformed/duplicate status, and policy-level
// reasons retain the ordinary classifier; missing evidence never becomes ready.
func openShiftPolicyResult(live, desired *unstructured.Unstructured) (Result, bool) {
	state, _, stateErr := unstructured.NestedString(live.Object, "status", "state")
	reason, _, reasonErr := unstructured.NestedString(live.Object, "status", "reason")
	if stateErr != nil || reasonErr != nil || (state != netopStateNotReady && state != netopStateError) || reason != "" {
		return Result{}, false
	}
	required, supported := openShiftRequiredStates(desired)
	if !supported {
		return Result{}, false
	}
	liveRequired, supported := openShiftRequiredStates(live)
	if !supported {
		return Result{}, false
	}
	for name := range liveRequired {
		required[name] = true
	}
	if len(required) == 0 {
		return Result{}, false
	}
	raw, _, err := unstructured.NestedSlice(live.Object, "status", "appliedStates")
	if err != nil {
		return Result{}, false
	}
	seen := map[string]string{}
	var included []interface{}
	external := map[string]string{}
	externalNotReady := false
	managedReady := true
	for _, item := range raw {
		entry, ok := item.(map[string]interface{})
		if !ok {
			return Result{}, false
		}
		name, _, nameErr := unstructured.NestedString(entry, "name")
		componentState, _, stateErr := unstructured.NestedString(entry, "state")
		message, _, messageErr := unstructured.NestedString(entry, "message")
		if _, duplicate := seen[name]; duplicate || name == "" || nameErr != nil || stateErr != nil || messageErr != nil {
			return Result{}, false
		}
		seen[name] = componentState
		if name == "state-multus-cni" || name == "state-container-networking-plugins" {
			switch componentState {
			case netopStateReady, netopStateIgnore, netopStateNotReady, netopStateError:
			default:
				return Result{}, false
			}
			external[name] = fmt.Sprintf("UNVERIFIED: OpenShift-managed; controller reports %s", componentState)
			if message != "" {
				external[name] += ": " + message
			}
			externalNotReady = externalNotReady || (componentState != netopStateReady && componentState != netopStateIgnore)
			continue
		}
		included = append(included, item)
		switch componentState {
		case netopStateReady, netopStateIgnore:
		default:
			managedReady = false
		}
	}
	if !externalNotReady {
		return Result{}, false
	}
	var pending []string
	for name := range required {
		if seen[name] != netopStateReady {
			pending = append(pending, name)
		}
	}
	derived := StateSuccess
	if !managedReady || len(pending) > 0 {
		// Preserve the controller's error/progress distinction whenever any
		// managed component still blocks. Only external-only failures relax it.
		derived = StateInProgress
		if state == netopStateError {
			derived = StateError
		}
	}
	summary, details := summarizeAppliedStates(included)
	if summary == "" {
		summary = "no managed component states reported"
	}
	if details == nil {
		details = map[string]string{}
	}
	for name, value := range external {
		details[name] = value
	}
	if len(pending) > 0 {
		sort.Strings(pending)
		summary += "; waiting for configured components: " + strings.Join(pending, ", ")
	}
	summary += "; excludes OpenShift-managed Multus/CNI states"
	return Result{State: derived, Reason: summary, Details: details}, true
}

// openShiftRequiredStates requires explicit readiness coverage for every
// configured component. Unknown components retain the aggregate verdict until
// their status mapping is known; an empty mapping still configures a component.
func openShiftRequiredStates(obj *unstructured.Unstructured) (map[string]bool, bool) {
	spec, _, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		return nil, false
	}
	required := map[string]bool{}
	for field, value := range spec {
		if value == nil {
			continue
		}
		switch field {
		case "global", "nodeAffinity", "tolerations", "deploymentNodeAffinity", "deploymentTolerations":
			// Scheduling and global image configuration have no independent state.
		case "secondaryNetwork":
			secondary, ok := value.(map[string]interface{})
			if !ok {
				return nil, false
			}
			for component, componentSpec := range secondary {
				if componentSpec == nil {
					continue
				}
				if component != "ipoib" {
					// Multus/CNI sections express NCP ownership, including {}.
					return nil, false
				}
				if _, ok := componentSpec.(map[string]interface{}); !ok {
					return nil, false
				}
				required["state-ipoib-cni"] = true
			}
		default:
			name, known := ncpComponentStates[field]
			if _, ok := value.(map[string]interface{}); !known || !ok {
				return nil, false
			}
			required[name] = true
		}
	}
	return required, true
}
