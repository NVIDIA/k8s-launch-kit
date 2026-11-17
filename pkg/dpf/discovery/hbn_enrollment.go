// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
)

func selectedSerials(inv *inventory.Inventory) map[string]bool {
	selected := map[string]bool{}
	if inv != nil {
		for _, dpu := range inv.DPUs {
			if dpu.SerialNumber != "" {
				selected[dpu.SerialNumber] = true
			}
		}
	}
	return selected
}

// Enrollment is global to physical serial identity, even when its Kubernetes
// objects belong to a different namespace. Unrelated known serials are preserved
// without being copied to the selected inventory or treated as target conflicts.
func enrollmentCheck(s *managementprobe.Snapshot, inv *inventory.Inventory) managementCheck {
	check := managementCheck{"enrollments", "selected DPUs", "unknown", "DPU_ENROLLMENTS_UNOBSERVED", "Cross-namespace DPUDevice/DPU ownership could not be completely enumerated."}
	selected := selectedSerials(inv)
	if len(selected) == 0 || s == nil {
		return check
	}
	complete := func(state managementprobe.CollectionState) bool {
		return state == managementprobe.CollectionComplete || state == managementprobe.CollectionAbsent
	}
	unknown := ""
	for _, device := range s.DPUDevices.Items {
		spec, status := deref(device.SpecSerialNumber), deref(device.StatusSerialNumber)
		identity := fmt.Sprintf("DPUDevice %s/%s", device.Namespace, device.Name)
		if spec != "" && status != "" && spec != status && (selected[spec] || selected[status]) {
			check.state, check.reason, check.message = "failed", "DPU_ENROLLMENT_IDENTITY_CONFLICT", identity+" reports conflicting serial identities; resolve ownership before deployment."
			return check
		}
		serial := firstNonEmpty(status, spec)
		if selected[serial] {
			check.state, check.reason, check.message = "failed", "DPU_ALREADY_ENROLLED", identity+" already claims a selected DPU; explicit adoption or cleanup is required before deployment."
			return check
		}
		if serial == "" {
			unknown = identity
		}
	}
	for _, dpu := range s.DPUs.Items {
		identity := fmt.Sprintf("DPU %s/%s", dpu.Namespace, dpu.Name)
		serial := deref(dpu.SerialNumber)
		if selected[serial] {
			check.state, check.reason, check.message = "failed", "DPU_ALREADY_ENROLLED", identity+" already claims a selected DPU; explicit adoption or cleanup is required before deployment."
			return check
		}
		if serial == "" {
			unknown = identity
		}
	}
	if !complete(s.DPUDevices.State) || !complete(s.DPUs.State) {
		return check
	}
	if unknown != "" {
		check.reason, check.message = "DPU_ENROLLMENT_IDENTITY_UNKNOWN", unknown+" has no observed serial; selected DPU ownership remains unknown."
		return check
	}
	check.state, check.reason, check.message = "passed", "DPU_SCOPE_UNCLAIMED", "No DPUDevice or DPU in the complete cross-namespace observation claims a selected physical DPU."
	return check
}

func projectEnrollments(s *managementprobe.Snapshot, selected map[string]bool) []inventory.HBNManagementEnrollment {
	result := []inventory.HBNManagementEnrollment{}
	for _, item := range s.DPUDevices.Items {
		spec, status := deref(item.SpecSerialNumber), deref(item.StatusSerialNumber)
		serial := firstNonEmpty(status, spec)
		if selected[spec] || selected[status] {
			result = append(result, inventory.HBNManagementEnrollment{Kind: "DPUDevice", Namespace: item.Namespace, Name: item.Name, UID: item.UID, SerialNumber: serial, PSID: deref(item.PSID), Owners: ownerSummaries(item.Owners)})
		} else if serial == "" {
			result = append(result, inventory.HBNManagementEnrollment{Kind: "DPUDevice", Namespace: item.Namespace, Name: item.Name})
		}
	}
	for _, item := range s.DPUs.Items {
		serial := deref(item.SerialNumber)
		if selected[serial] {
			result = append(result, inventory.HBNManagementEnrollment{Kind: "DPU", Namespace: item.Namespace, Name: item.Name, UID: item.UID, SerialNumber: serial, DeviceName: deref(item.DeviceName), NodeName: deref(item.NodeName), Owners: ownerSummaries(item.Owners)})
		} else if serial == "" {
			result = append(result, inventory.HBNManagementEnrollment{Kind: "DPU", Namespace: item.Namespace, Name: item.Name})
		}
	}
	slices.SortFunc(result, func(a, b inventory.HBNManagementEnrollment) int {
		return strings.Compare(a.Namespace+"/"+a.Kind+"/"+a.Name, b.Namespace+"/"+b.Kind+"/"+b.Name)
	})
	return result
}
