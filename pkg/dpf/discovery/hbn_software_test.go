// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func TestHBNSoftwareInputsUseOnlyExactAssociatedPSID(t *testing.T) {
	inv := hostHBNTestInventory()
	card := inventory.CardHardware{SerialNumber: inv.DPUs[0].SerialNumber, PCIAddress: inv.Hosts[0].Adapters[0].PCIAddress, Domain: "host-pci", PSID: inventory.Observation{State: "observed", Values: []string{"MT_0000001774"}}}
	inv.Hosts[0].Runtime.Hardware = &inventory.HardwareObservation{Cards: []inventory.CardHardware{card}}
	artifact := config.Artifact{URL: "https://artifacts.example/bf4.bin", SHA256: strings.Repeat("a", 64)}
	input := &config.Provisioning{Software: config.Software{OSISO: artifact, PLDM: map[string]config.Artifact{"mt_0000001774": artifact}}}
	checks := hbnSoftwareChecks(input, &inv)
	if len(checks) != 1 || checks[0].State != "passed" {
		t.Fatalf("exact selected hardware: %#v", checks)
	}
	inv.Hosts[0].Runtime.Hardware.Cards[0].Domain = "arm-pci"
	checks = hbnSoftwareChecks(input, &inv)
	if len(checks) == 1 && checks[0].State == "passed" {
		t.Fatal("cross-domain PSID was accepted")
	}
	inv.Hosts[0].Runtime.Hardware.Cards[0] = card
	inv.DPUs[0].Observations = map[string]inventory.Observation{"psid": {State: "observed", Values: []string{"MT_0000001775"}}}
	if _, err := ObservedDevicePSID(&inv, inv.DPUs[0]); err == nil {
		t.Fatal("contradictory BMC/host PSIDs were accepted")
	}
	inv.DPUs[0].Observations = nil
	inv.Associations[0].HostAdapterKey = "other"
	if _, err := ObservedDevicePSID(&inv, inv.DPUs[0]); err == nil {
		t.Fatal("different adapter association was accepted")
	}
}

func TestHBNSoftwareMissingInputsRemainActionableWithoutBlockingInventoryCollection(t *testing.T) {
	checks := hbnSoftwareChecks(nil, nil)
	codes := map[string]bool{}
	for _, check := range checks {
		codes[check.ReasonCode] = true
		if check.State == "passed" {
			t.Fatal("missing input passed")
		}
	}
	if !codes["OS_ISO_ARTIFACT_MISSING"] || !codes["SELECTED_PSID_SCOPE_UNAVAILABLE"] {
		t.Fatalf("missing input reasons: %#v", checks)
	}
	input := &config.Provisioning{Software: config.Software{OSISO: config.Artifact{URL: "https://user:secret@artifacts.example/bf4.iso"}}}
	checks = hbnSoftwareChecks(input, nil)
	if len(checks) != 1 || checks[0].ReasonCode != "SOFTWARE_INPUT_INVALID" || strings.Contains(checks[0].Message, "secret") {
		t.Fatalf("unsafe artifact error: %#v", checks)
	}
}
