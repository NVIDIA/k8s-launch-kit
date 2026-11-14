// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// selectedEnrollmentDiscoveries validates the complete scan selection before
// starting the operator or publishing credentials. Order follows retained devices.
func selectedEnrollmentDiscoveries(execution *managementExecution, selected []enrollmentBMC) ([]*unstructured.Unstructured, error) {
	if execution == nil || execution.plan == nil || len(selected) == 0 {
		return nil, fmt.Errorf("a retained enrollment plan and selected devices are required")
	}
	intent := execution.plan.Intent()
	if intent.Bootstrap.BMCFactoryResetPolicy != bootstrap.BMCFactoryResetPolicyNever {
		return nil, fmt.Errorf("ENROLLMENT_RESET_POLICY_UNSUPPORTED: native enrollment requires Never; BMC address recovery after factory reset is not qualified")
	}
	if len(selected) != len(intent.Devices) {
		return nil, fmt.Errorf("selected enrollment devices differ from the retained plan")
	}
	discoveries := retainedEnrollmentDiscoveries(execution)
	if len(discoveries) != len(selected) {
		return nil, fmt.Errorf("the retained enrollment plan must contain one singleton scan per selected device")
	}
	result := make([]*unstructured.Unstructured, 0, len(selected))
	used := make(map[string]bool, len(discoveries))
	for i, item := range selected {
		want := intent.Devices[i]
		if item.device.Serial != want.Serial || item.device.PSID != want.PSID || item.device.BMCIP != want.BMCIP || item.device.BMCPort != want.BMCPort {
			return nil, fmt.Errorf("selected enrollment device identity differs from the retained plan")
		}
		var matched *unstructured.Unstructured
		for key, object := range discoveries {
			ip, found, err := unstructured.NestedString(object.Object, "spec", "ipRangeSpec", "ipRange", "startIP")
			if err != nil || !found || ip != item.device.BMCIP {
				continue
			}
			end, _, endErr := unstructured.NestedString(object.Object, "spec", "ipRangeSpec", "ipRange", "endIP")
			port, _, portErr := unstructured.NestedInt64(object.Object, "spec", "ipRangeSpec", "ipRange", "port")
			if endErr != nil || portErr != nil || end != ip || port != int64(item.device.BMCPort) || object.GetNamespace() != execution.operatorNamespace || object.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" || matched != nil || used[key] {
				return nil, fmt.Errorf("retained enrollment scans are ambiguous or extend beyond the selected BMC endpoint")
			}
			matched, used[key] = object, true
		}
		if matched == nil {
			return nil, fmt.Errorf("no retained singleton scan matches a selected BMC endpoint")
		}
		result = append(result, matched)
	}
	return result, nil
}
