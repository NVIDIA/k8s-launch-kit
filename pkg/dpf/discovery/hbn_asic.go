// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func hbnASICCheck(sw inventory.FabricSwitch, port inventory.SwitchPort, profile hbn.Profile) inventory.Check {
	check := inventory.Check{ID: "dpf.hbn.port-asic", Subject: sw.Name + "/" + port.Name, Vantage: "executor", RequiredPhase: "deploy", State: "unknown", ReasonCode: "ASIC_BINDING_UNMEASURED", Message: "The selected physical port's ASIC RIF/VRF binding is unmeasured; qualified SDK and switchd read access is required."}
	for _, issue := range sw.Snapshot.Issues {
		if issue.Input == "asic" {
			check.ReasonCode = issue.Code
			check.Message = issue.Message
			return check
		}
	}
	if sw.Snapshot.ASIC == nil {
		return check
	}
	count := 0
	for _, observed := range sw.Snapshot.ASIC.Ports {
		if observed.Name != port.Name {
			continue
		}
		count++
		check.State = "passed"
		check.ReasonCode = "ASIC_BINDING_VERIFIED"
		check.Message = fmt.Sprintf("Direct SDK RIF %d, RouterID %d maps to table %d and Linux VRF %s; MTU %d, IPv4/IPv6 unicast enabled. This is a collection-time prerequisite observation.", observed.RIF, observed.RouterID, observed.RouterTable, observed.LinuxVRF, observed.MTU)
		if observed.LinuxVRF != profile.FabricVRF || observed.RouterTable != observed.LinuxTable || observed.MTU < uint32(profile.UplinkMTU) || !observed.IPv4Enabled || !observed.IPv6Enabled {
			check.State = "failed"
			check.ReasonCode = "ASIC_BINDING_MISMATCH"
			check.Message = fmt.Sprintf("Direct SDK RIF %d RouterID %d maps to table %d, while Linux VRF %s uses table %d; expected VRF %s, MTU at least %d and IPv4/IPv6 unicast enabled. Correct the switch programming outside discovery and rerun.", observed.RIF, observed.RouterID, observed.RouterTable, observed.LinuxVRF, observed.LinuxTable, profile.FabricVRF, profile.UplinkMTU)
		}
	}
	if count != 1 {
		check.State = "unknown"
		check.ReasonCode = "ASIC_BINDING_UNMEASURED"
		check.Message = "Exactly one qualified direct ASIC RIF observation is required for every selected physical port."
	}
	return check
}
