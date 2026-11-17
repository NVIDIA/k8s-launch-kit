// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

// hbnCatalogInputs keeps product support failures separate from missing site
// information. Inventory remains useful even when no provisioning recipe exists.
func hbnCatalogInputs(site config.Site, inv *inventory.Inventory) (*config.Provisioning, []inventory.Check) {
	checks := []inventory.Check{}
	add := func(subject, code, message string) {
		checks = append(checks, inventory.Check{ID: "dpf.hbn.catalog", Subject: subject, Vantage: "executor", RequiredPhase: "generate", State: "unknown", ReasonCode: code, Message: message})
	}
	selected := []config.ProvisioningDevice{}
	if inv != nil {
		for _, device := range inv.DPUs {
			psid, err := ObservedDevicePSID(inv, device)
			if err != nil {
				add(device.SerialNumber, "SELECTED_PSID_UNVERIFIED", err.Error())
				continue
			}
			selected = append(selected, config.ProvisioningDevice{Model: device.Model, PSID: psid})
		}
	}
	resolved, issues, err := config.ResolveProvisioningDefaults(site, selected)
	if err != nil {
		add("release catalog", "PROVISIONING_DEFAULTS_INVALID", err.Error())
		return site.Provisioning, checks
	}
	for _, issue := range issues {
		message := issue.Message
		if message == "" {
			message = "The selected release catalog lacks a supported provisioning recipe; update Launch Kit or select supported hardware. Do not supply raw firmware or DPUFlavor configuration."
		}
		add(issue.Field, issue.ReasonCode, message)
	}
	if len(checks) == 0 {
		checks = append(checks, inventory.Check{ID: "dpf.hbn.catalog", Subject: "release and use-case defaults", Vantage: "executor", RequiredPhase: "generate", State: "passed", ReasonCode: "PROVISIONING_DEFAULTS_RESOLVED", Message: "Software and device configuration were selected from the release catalog and maintained HBN-BGP profile; artifact access and runtime readiness are checked separately."})
	}
	return resolved, checks
}
