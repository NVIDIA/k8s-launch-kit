// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"reflect"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// provisioningAPIReads checks the initial BF4 reads made by the selected DPF
// release, independently of the inventory reader's more general graph support.
// The paths follow DPF v26.8.0 commit fc4974ed's Redfish client. Passing this
// bounded check does not test firmware writes or controller authentication.
func provisioningAPIReads(ctx context.Context, reader Reader, release string, bf4 bool, serial string, system Resource) inventory.Observation {
	unknown := func(code, message string) inventory.Observation {
		return inventory.Observation{State: "unknown", Tool: "redfish", ReasonCode: code, Message: message}
	}
	if release != "26.8.0" || !bf4 {
		return unknown("DPF_PROVISIONING_API_UNSUPPORTED", "Release-specific enrollment API checks are implemented for DPF 26.8.0 on BlueField-4 only.")
	}
	if reader == nil || !ValidSerial(serial) || system.URI == "" {
		return unknown("DPF_PROVISIONING_IDENTITY_REQUIRED", "A verified DPU identity and selected system are required before release-specific API reads.")
	}
	const chassisURI = "/redfish/v1/Chassis/BlueField_0"
	const pf0URI = chassisURI + "/NetworkAdapters/BlueField_NIC_0/NetworkDeviceFunctions/0"
	chassis, err := reader.Get(ctx, chassisURI)
	if err != nil {
		return provisioningAPIReadFailure(chassisURI, err)
	}
	if Text(chassis, "SerialNumber") != serial || Text(chassis, "Model") != "B4240" {
		return unknown("DPF_PROVISIONING_IDENTITY_MISMATCH", "DPF's fixed BF4 chassis endpoint does not identify the inventory-selected card.")
	}
	psid := bf4PSID(system.Document, chassis, system.Document, chassis)
	if psid.State != "observed" {
		return unknown("DPF_PROVISIONING_PSID_UNAVAILABLE", "DPF's selected System or fixed BF4 chassis must expose a usable PSID AssetTag; host SSH PSID discovery cannot replace this controller requirement.")
	}
	function, err := reader.Get(ctx, pf0URI)
	if err != nil {
		return provisioningAPIReadFailure(pf0URI, err)
	}
	mac, valid := permanentUnicastMAC(Text(Object(function, "Ethernet"), "PermanentMACAddress"))
	if !valid || Text(function, "Id") != "0" || Text(function, "NetDevFuncType") != "Ethernet" {
		return unknown("DPF_PROVISIONING_PF0_UNAVAILABLE", "DPF's fixed BF4 function 0 endpoint must expose an Ethernet permanent MAC address.")
	}
	currentChassis, err := reader.Get(ctx, chassisURI)
	if err != nil {
		return provisioningAPIReadFailure(chassisURI, err)
	}
	currentSystem, err := reader.Get(ctx, system.URI)
	if err != nil {
		return provisioningAPIReadFailure(system.URI, err)
	}
	currentFunction, err := reader.Get(ctx, pf0URI)
	if err != nil {
		return provisioningAPIReadFailure(pf0URI, err)
	}
	if !reflect.DeepEqual(cardIdentity(chassis), cardIdentity(currentChassis)) || bf4PSID(system.Document, chassis, currentSystem, currentChassis).State != "observed" ||
		!reflect.DeepEqual(networkFunctionIdentity(function), networkFunctionIdentity(currentFunction)) {
		return unknown("DPF_PROVISIONING_API_CHANGED", "The selected identity, PSID or PF0 changed during release-specific API verification.")
	}
	return inventory.Observation{State: "observed", Tool: "redfish", Values: []string{release, mac}, ReasonCode: "DPF_PROVISIONING_API_READS_VERIFIED", Message: "DPF 26.8.0's BF4 chassis, PSID and PF0 reads identify this selected DPU; controller authentication and provisioning actions remain later checks."}
}

func provisioningAPIReadFailure(resource string, err error) inventory.Observation {
	code := Code(err)
	message := "DPF enrollment read " + resource + " failed: " + code + "."
	if code == "RESOURCE_NOT_FOUND" {
		code = "DPF_PROVISIONING_API_NOT_EXPOSED"
		message = "The BMC does not expose " + resource + ", required by DPF 26.8.0's BF4 enrollment client. Update to compatible BMC firmware before provisioning; physical inventory success does not establish this prerequisite."
	}
	return inventory.Observation{State: "unknown", Tool: "redfish", ReasonCode: code, Message: message}
}
