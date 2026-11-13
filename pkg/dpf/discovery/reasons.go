// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package discovery

import "github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"

// ExplainCheck completes report diagnostics at the orchestration boundary. Collectors
// retain more specific failure messages; this table describes acceptance predicates.
func ExplainCheck(c *inventory.Check) {
	if c == nil || c.State == "passed" || c.Message != "" {
		return
	}
	messages := map[string]string{
		"EXACTLY_THREE_CONTROL_PLANES":        "The management topology does not contain exactly three control-plane nodes.",
		"PHYSICAL_IDENTITY_REQUIRED":          "The selected source did not establish a usable physical identity; inspect its observation and error reasons.",
		"QUALIFIED_ADAPTER_REQUIRED":          "This hardware/firmware and reader combination has not passed release qualification; inventory success does not establish provisioning support.",
		"DPU_IDENTITY_REQUIRED":               "No DPU identity was established from the selected sources.",
		"UNIQUE_PHYSICAL_MATCH_REQUIRED":      "The DPU serial must match exactly one observed host adapter; missing, duplicate or conflicting identity prevents association.",
		"HOST_ADAPTER_VENDOR_CONFLICT":        "A host adapter with the DPU serial reports a non-NVIDIA PCI vendor. Verify the selected endpoints and the adapter serial/PCI inventory, then rerun discovery; a matching serial cannot override contradictory vendor identity.",
		"REDFISH_AUTHORIZATION_NOT_EVALUATED": "This run did not verify permission to read DPU Redfish inventory; SSH access is a separate authorization path.",
		"DPF_AUTHENTICATION_NOT_EVALUATED":    "The installed DPF controller authentication path is checked during provisioning, not initial inventory.",
		"INVENTORY_MODE":                      "Inventory mode omits management-network probes; later management readiness remains unevaluated.",
		"EXACT_NETWORK_COVERAGE_REQUIRED":     "Required management node, network-mode or destination evidence is missing or did not pass; inspect individual probe results.",
	}
	c.Message = messages[c.ReasonCode]
	if c.Message == "" {
		c.Message = "The check did not produce passing evidence; inspect the associated source observation and reason code."
	}
}
