// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"fmt"
	"slices"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
)

// resetContract selects only the installed, reviewed release contract. User YAML
// and observed firmware cannot introduce another implementation or support tuple.
func resetContract(version string) (releases.ResetSafetyContract, error) {
	if version == "" {
		return releases.ResetSafetyContract{}, &Error{Code: "RESET_RELEASE_REQUIRED"}
	}
	release, err := releases.Resolve(version)
	if err != nil || release.HBNWorkload == nil || release.HBNWorkload.ResetSafety == nil || release.HBNWorkload.ResetSafety.Contract != "bf4-reset-safety-v1" {
		return releases.ResetSafetyContract{}, &Error{Code: "RESET_PLATFORM_UNQUALIFIED"}
	}
	return *release.HBNWorkload.ResetSafety, nil
}

// resetSupportReads checks a static, already observable prerequisite. It does not
// establish live shutdown, task/copy-idle state or permission to reset a host.
func resetSupportReads(ctx context.Context, reader Reader, version, model string) inventory.Observation {
	observation := inventory.Observation{State: "unknown", Tool: "redfish", ReasonCode: "RESET_SUPPORT_UNVERIFIED", Message: "The selected release's host reset backend has not been verified for this DPU BMC."}
	contract, err := resetContract(version)
	if err != nil || !slices.Contains(contract.Models, model) {
		observation.State, observation.ReasonCode, observation.Message = "failed", "RESET_PLATFORM_UNQUALIFIED", "The selected release has no supported host reset backend for this DPU model."
		return observation
	}
	const uri = "/redfish/v1/UpdateService/FirmwareInventory/BlueField_FW_BMC_0"
	if reader == nil {
		return observation
	}
	first, err := reader.Get(ctx, uri)
	if err != nil {
		observation.ReasonCode = Code(err)
		observation.Message = "The BMC firmware required to establish host reset support could not be read: " + Code(err) + "."
		return observation
	}
	firmware := Text(first, "Version")
	if firmware == "" || len(firmware) > 128 {
		observation.ReasonCode = "RESET_FIRMWARE_VERSION_UNAVAILABLE"
		return observation
	}
	second, err := reader.Get(ctx, uri)
	if err != nil || Text(second, "Version") != firmware {
		observation.ReasonCode = "RESET_FIRMWARE_CHANGED"
		observation.Message = "BMC firmware changed or could not be rechecked during reset support discovery."
		return observation
	}
	if firmware != contract.BMCFirmware {
		observation.State, observation.ReasonCode = "failed", "RESET_PLATFORM_UNQUALIFIED"
		observation.Message = fmt.Sprintf("Host reset support requires BMC firmware %s for model %s; observed %q. Use a supported firmware/backend before provisioning.", contract.BMCFirmware, model, firmware)
		return observation
	}
	observation.State, observation.ReasonCode = "observed", "RESET_SUPPORT_VERIFIED"
	observation.Values = []string{version, contract.Contract, model, firmware}
	observation.Message = "The selected release supports the observed DPU model and BMC firmware for host reset; live shutdown and copy-state checks remain required before each reset."
	return observation
}

// ResetSupportVerified binds a static observation to the installed release tuple.
// It never grants permission to submit a reset or bypasses live safety checks.
func ResetSupportVerified(device inventory.Device, version string) bool {
	observed := device.Observations["host-reset-support"]
	contract, err := resetContract(version)
	return err == nil && observed.State == "observed" && observed.Tool == "redfish" && observed.ReasonCode == "RESET_SUPPORT_VERIFIED" && len(observed.Values) == 4 && observed.Values[0] == version && observed.Values[1] == contract.Contract && slices.Contains(contract.Models, observed.Values[2]) && device.Model == observed.Values[2] && observed.Values[3] == contract.BMCFirmware
}
