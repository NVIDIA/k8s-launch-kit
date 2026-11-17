// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/gomega"
)

// Success and partial/failure fixtures exercise the collector's shared return
// slot and every Redfish record that previously carried evidence references.
func expectNoRedfishEvidence(device inventory.Device, evidence map[string]inventory.Evidence) {
	ExpectWithOffset(1, evidence).To(BeNil())
	ExpectWithOffset(1, device.EvidenceRefs).To(BeNil())
	for _, adapter := range device.Adapters {
		ExpectWithOffset(1, adapter.EvidenceRefs).To(BeNil())
	}
	for _, firmware := range device.Firmware {
		ExpectWithOffset(1, firmware.EvidenceRefs).To(BeNil())
	}
}
