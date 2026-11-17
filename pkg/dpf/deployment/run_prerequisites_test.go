// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func activatedCurrentInventory(t *testing.T, f activationIntegrationFixture) *inventory.Inventory {
	t.Helper()
	current := *f.inventory
	current.Hosts = append([]inventory.Device(nil), f.inventory.Hosts...)
	current.DPUs = append([]inventory.Device(nil), f.inventory.DPUs...)
	for i := range current.Hosts {
		current.Hosts[i].RecordKey = current.Hosts[i].ID
		current.Hosts[i].ObservationState = "observed"
		current.Hosts[i].ReaderAdapter = "linux-host-ssh-v1"
	}
	for i := range current.DPUs {
		current.DPUs[i].RecordKey = current.DPUs[i].ID
		current.DPUs[i].ObservationState = "observed"
		current.DPUs[i].ReaderAdapter = "bluefield-card-v1"
		current.DPUs[i].BMCEndpoint = "https://" + f.prepared.selected[i].device.BMCIP
		current.DPUs[i].InputRef = "/discovery/dpus/0"
	}
	cloned, err := cloneInventory(&current)
	if err != nil {
		t.Fatal(err)
	}
	return cloned
}

func TestAfterActivationRebindRequiresFreshSamePhysicalCohortAndCompletion(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, test := range []struct {
		name string
		edit func(*activationIntegrationFixture, *inventory.Inventory)
		pass bool
	}{
		{"fresh runtime facts replace pre-reset facts", func(_ *activationIntegrationFixture, i *inventory.Inventory) {
			i.Hosts[0].Runtime.Hostname = "fresh-after-reset"
			i.Hosts[0].Adapters[0].PCIFunctions[0].Netdevs = []string{"renamed0"}
			i.Hosts[0].Runtime.Network.Links[0].Name = "renamed0"
		}, true},
		{"physical host replacement", func(_ *activationIntegrationFixture, i *inventory.Inventory) {
			i.Hosts[0].SystemUUID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
		}, false},
		{"PCI function replacement", func(_ *activationIntegrationFixture, i *inventory.Inventory) {
			i.Hosts[0].Adapters[0].PCIFunctions[0].Address = "0000:44:00.0"
		}, false},
		{"deployment no longer ready", func(f *activationIntegrationFixture, _ *inventory.Inventory) {
			f.client.deployment.Object["status"] = map[string]any{}
		}, false},
		{"deployment UID replacement", func(f *activationIntegrationFixture, _ *inventory.Inventory) {
			f.client.deployment.SetUID("replacement")
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, activation, _ := hostNetworkStageFixtureFromExecution(t, execution)
			current := activatedCurrentInventory(t, f)
			test.edit(&f, current)
			oldName := activation.selected[0].host.Runtime.Hostname
			oldClaims := f.client.guardCalls
			before := current.Hosts[0].Runtime.Hostname
			rebound, err := rebindActivatedHostInventory(f.ctx, f.client, f.prepared, f.controlPlane, activation, current, f.request.Targets)
			if (err == nil) != test.pass {
				t.Fatalf("rebind err=%v", err)
			}
			if test.pass && (rebound.selected[0].host.Runtime.Hostname != before || rebound.selected[0].adapter.PCIFunctions[0].Netdevs[0] != "renamed0") {
				t.Fatal("post-activation facts were not rebound")
			}
			if activation.selected[0].host.Runtime.Hostname != oldName || f.client.guardCalls != oldClaims {
				t.Fatal("rebind mutated original receipt or submitted physical effect")
			}
			if test.pass {
				rebound.selected[0].adapter.PCIFunctions[0].Netdevs[0] = "mutated"
				if current.Hosts[0].Adapters[0].PCIFunctions[0].Netdevs[0] != "renamed0" {
					t.Fatal("rebound inventory shares mutable facts")
				}
			}
		})
	}
}

func TestAfterActivationCollectorFailureStopsBeforeResourceOrHostEffects(t *testing.T) {
	sentinel := errors.New("fresh observer unavailable")
	called := false
	raw := []inventory.Check{{ID: "physical.current", Subject: "selected", Vantage: "executor", RequiredPhase: "deploy", State: "unknown", ReasonCode: "UNAVAILABLE"}}
	verification, _, err := verifyActivatedHostPrerequisites(context.Background(), nil, &preparedControlPlaneRun{execution: &managementExecution{}}, controlPlaneReceipts{}, workloadPreparationReceipt{}, activationStageReceipt{}, config.Request{}, nil, time.Second, func(_ context.Context, _ config.Request, _ *generation.Plan, phase PrerequisitePhase) (PrerequisiteVerification, error) {
		called = true
		if phase != AfterActivation {
			t.Fatalf("phase=%s", phase)
		}
		return PrerequisiteVerification{Checks: raw}, sentinel
	})
	if !called || err == nil || !strings.Contains(err.Error(), "post-activation prerequisite collection failed") || !reflect.DeepEqual(raw, verification.Checks) {
		t.Fatalf("result=%+v err=%v", verification, err)
	}
}
