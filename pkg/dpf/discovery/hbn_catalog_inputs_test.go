// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func TestHBNCatalogMissingIdentityCannotPass(t *testing.T) {
	site := config.Site{}
	useCase := "hbn-bgp"
	site.DPF.UseCase = &useCase
	result, checks := hbnCatalogInputs(site, &inventory.Inventory{})
	if result == nil || len(checks) == 0 {
		t.Fatal("missing identities must retain usable site intent and blocking checks")
	}
	for _, check := range checks {
		if check.State == "passed" || check.Message == "" || check.ReasonCode == "" {
			t.Fatalf("missing identity passed or lost its reason: %#v", check)
		}
		if strings.Contains(check.Message, "Supply a credential-free HTTPS") {
			t.Fatal("catalog gap asks user to compose software")
		}
	}
}

func TestHBNCatalogSelectsObservedCohortAndRejectsPSIDDrift(t *testing.T) {
	useCase, release := "hbn-bgp", "26.8.0"
	site := config.Site{}
	site.DPF.UseCase, site.DPF.Release = &useCase, &release
	inv := hostHBNTestInventory()
	inv.DPUs[0].Model = "B4240"
	inv.DPUs[0].PSID = "MT_0000001774"
	resolved, checks := hbnCatalogInputs(site, &inv)
	if len(checks) != 1 || checks[0].ReasonCode != "PROVISIONING_DEFAULTS_RESOLVED" || checks[0].State != "passed" {
		t.Fatalf("observed cohort did not select maintained defaults: %#v", checks)
	}
	if resolved == nil || len(resolved.Flavor) == 0 || resolved.Software.OSISO.SHA256 == "" || len(resolved.Software.PLDM) != 1 {
		t.Fatalf("incomplete maintained recipe: %#v", resolved)
	}
	if site.Provisioning != nil {
		t.Fatal("catalog selection mutated authored inventory")
	}
	inv.DPUs[0].PSID = "MT_0000009999"
	_, checks = hbnCatalogInputs(site, &inv)
	if len(checks) == 0 {
		t.Fatal("unknown PSID lost its blocker")
	}
	for _, check := range checks {
		if check.State == "passed" || check.ReasonCode == "" || check.Message == "" {
			t.Fatalf("unknown PSID passed or lost its reason: %#v", check)
		}
	}
}
