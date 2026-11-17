// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"reflect"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func psidUnavailable(code, message string) inventory.Observation {
	return inventory.Observation{State: "unavailable", ReasonCode: code, Message: message, Tool: "redfish"}
}

// bf4PSID follows the supported BF4 reader's AssetTag selection: a non-nil
// System AssetTag wins, otherwise the identity-selected chassis is used. The
// selected source and value must remain unchanged through identity rereads.
// No raw documents or malformed values are persisted.
func bf4PSID(system, card, currentSystem, currentCard Document) inventory.Observation {
	source, raw, present := selectedBF4PSID(system, card)
	currentSource, currentRaw, currentPresent := selectedBF4PSID(currentSystem, currentCard)
	if source != currentSource || present != currentPresent || !reflect.DeepEqual(raw, currentRaw) {
		return psidUnavailable("PSID_CHANGED", "The selected Redfish PSID source changed during collection; rerun discovery before using this PSID.")
	}
	if !present {
		return inventory.Observation{State: "unknown", ReasonCode: "PSID_NOT_EXPOSED", Message: "The supported BF4 System and identity-selected chassis do not expose a PSID AssetTag.", Tool: "redfish"}
	}
	value, ok := raw.(string)
	if !ok || strings.TrimSpace(value) == "" || strings.EqualFold(strings.TrimSpace(value), "N/A") || !inventory.ValidRedfishPSID(value) {
		return psidUnavailable("PSID_INVALID", "The AssetTag selected by DPF's System-first PSID lookup is not a valid bounded ASCII PSID identifier.")
	}
	return inventory.Observation{State: "observed", Values: []string{value}, ReasonCode: "PSID_OBSERVED", Message: "The PSID selected by DPF's System-first AssetTag lookup was verified unchanged.", Tool: "redfish"}
}

func selectedBF4PSID(system, card Document) (source string, value any, present bool) {
	if value, exists := system["AssetTag"]; exists && value != nil {
		return "system", value, true
	}
	if value, exists := card["AssetTag"]; exists && value != nil {
		return "chassis", value, true
	}
	return "", nil, false
}
