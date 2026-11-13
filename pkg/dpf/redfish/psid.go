// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"reflect"
	"regexp"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

var psidToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func psidUnavailable(code, message string) inventory.Observation {
	return inventory.Observation{State: "unavailable", ReasonCode: code, Message: message, Tool: "redfish"}
}

// bf4PSID uses the supported BF4 reader's AssetTag semantics: System first,
// then the identity-selected chassis. Both advertised values must agree and
// remain unchanged through the existing identity rereads. No raw documents or
// malformed AssetTag values are persisted, and no SSH runtime is manufactured.
func bf4PSID(system, card Resource, currentSystem, currentCard Document, add func(string, string) string) inventory.Observation {
	for _, source := range []struct{ initial, current Document }{{system.Document, currentSystem}, {card.Document, currentCard}} {
		initial, initialPresent := source.initial["AssetTag"]
		current, currentPresent := source.current["AssetTag"]
		if initialPresent != currentPresent || !reflect.DeepEqual(initial, current) {
			return psidUnavailable("PSID_CHANGED", "A Redfish PSID source changed during collection; rerun discovery before using this PSID.")
		}
	}
	value := ""
	for _, source := range []Resource{system, card} {
		raw, present := source.Document["AssetTag"]
		if !present || raw == nil {
			continue
		}
		text, ok := raw.(string)
		if !ok {
			return psidUnavailable("PSID_INVALID", "A supported BF4 AssetTag is not a valid PSID identifier string.")
		}
		if strings.TrimSpace(text) == "" || strings.EqualFold(strings.TrimSpace(text), "N/A") {
			continue
		}
		if !psidToken.MatchString(text) {
			return psidUnavailable("PSID_INVALID", "A supported BF4 AssetTag is not a valid bounded ASCII PSID identifier.")
		}
		if value != "" && value != text {
			return psidUnavailable("PSID_CONFLICT", "The supported BF4 System and chassis AssetTags report contradictory PSIDs.")
		}
		if value == "" {
			value = text
		}
	}
	if value == "" {
		return inventory.Observation{State: "unknown", ReasonCode: "PSID_NOT_EXPOSED", Message: "The supported BF4 System and identity-selected chassis do not expose a PSID AssetTag.", Tool: "redfish"}
	}
	for _, source := range []Resource{system, card} {
		if Text(source.Document, "AssetTag") == value {
			add(source.URI, "AssetTag")
		}
	}
	return inventory.Observation{State: "observed", Values: []string{value}, ReasonCode: "PSID_OBSERVED", Message: "The supported BF4 AssetTag PSID was verified against unchanged identity-bound System and chassis observations.", Tool: "redfish"}
}
