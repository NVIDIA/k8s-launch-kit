// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func hbnSoftwareChecks(input *config.Provisioning, inv *inventory.Inventory) []inventory.Check {
	checks := []inventory.Check{}
	add := func(subject, state, code, message string) {
		checks = append(checks, inventory.Check{ID: "dpf.hbn.software-inputs", Subject: subject, Vantage: "executor", RequiredPhase: "generate", State: state, ReasonCode: code, Message: message})
	}
	psids := []string{}
	complete := inv != nil && len(inv.DPUs) > 0
	if inv != nil {
		for _, dpu := range inv.DPUs {
			psid, err := ObservedDevicePSID(inv, dpu)
			if err != nil {
				add(dpu.SerialNumber, "unknown", "SELECTED_PSID_UNVERIFIED", err.Error())
				complete = false
				continue
			}
			psids = append(psids, psid)
		}
	}
	// A partial observed PSID set must not reject valid inputs for an unobserved
	// selected card as an "unused" firmware entry.
	if !complete {
		psids = nil
	}
	_, issues, err := config.ValidateProvisioningSoftware(input, psids)
	if err != nil {
		add("provisioning.software", "failed", "SOFTWARE_INPUT_INVALID", err.Error())
		return checks
	}
	for _, issue := range issues {
		add(issue.Field, "unknown", issue.ReasonCode, "Supply a credential-free HTTPS artifact URL and its SHA-256 digest for the selected hardware; firmware selection requires an observed PSID for every DPU.")
	}
	if len(checks) == 0 {
		add("provisioning.software", "passed", "SOFTWARE_INPUTS_COMPLETE", "An OS ISO and exact PSID-specific PLDM URLs and SHA-256 digests are configured; controller download, consumed bytes and release compatibility require separate verification.")
	}
	return checks
}

// ObservedDevicePSID combines only the selected card's Redfish fact and the exact
// associated host adapter's hardware observation, never PCI addresses from a
// different host or ARM domain.
func ObservedDevicePSID(inv *inventory.Inventory, dpu inventory.Device) (string, error) {
	if inv == nil {
		return "", fmt.Errorf("DPU inventory is required")
	}
	_, host, adapter, state, _, message := resolveHostHBNAssociation(inv, dpu)
	if state != "passed" {
		return "", fmt.Errorf("cannot select firmware: %s", message)
	}
	values := []string{}
	add := func(value string) {
		if value != "" {
			values = append(values, strings.ToUpper(value))
		}
	}
	observation := dpu.Observations["psid"]
	if observation.State == "observed" {
		if len(observation.Values) != 1 {
			return "", fmt.Errorf("DPU PSID observation is ambiguous")
		}
		add(observation.Values[0])
	}
	if dpu.PSID != "" && observation.State != "" && observation.State != "observed" {
		return "", fmt.Errorf("DPU PSID value contradicts its failed observation")
	}
	add(dpu.PSID)
	if host.Runtime != nil && host.Runtime.Hardware != nil {
		for _, card := range host.Runtime.Hardware.Cards {
			if card.SerialNumber != dpu.SerialNumber || card.PCIAddress != adapter.PCIAddress || card.Domain != "host-pci" {
				continue
			}
			if card.PSID.State != "observed" {
				continue
			}
			if len(card.PSID.Values) != 1 {
				return "", fmt.Errorf("associated host card PSID observation is ambiguous")
			}
			add(card.PSID.Values[0])
		}
	}
	slices.Sort(values)
	values = slices.Compact(values)
	if len(values) != 1 {
		return "", fmt.Errorf("the selected DPU requires one observed, nonconflicting PSID before firmware can be selected")
	}
	return values[0], nil
}
