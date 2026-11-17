// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func runtimeBudgetInventory(count int) *inventory.Inventory {
	inv := &inventory.Inventory{}
	for i := 0; i < count; i++ {
		serial, hostID, dpuID, key := fmt.Sprintf("SERIAL-%03d", i), fmt.Sprintf("host-%d", i), fmt.Sprintf("dpu-%d", i), fmt.Sprintf("adapter-%d", i)
		inv.DPUs = append(inv.DPUs, inventory.Device{ID: dpuID, SerialNumber: serial, ObservationState: "observed"})
		inv.Hosts = append(inv.Hosts, inventory.Device{ID: hostID, ObservationState: "observed", Adapters: []inventory.Adapter{{Key: key, SerialNumber: serial}}})
		inv.Associations = append(inv.Associations, inventory.Association{DPUID: dpuID, SerialNumber: serial, HostID: hostID, HostAdapterKey: key})
	}
	return inv
}

func TestHBNRuntimeBudgetUsesOnlyCompleteSelectedPhysicalHosts(t *testing.T) {
	for _, count := range []int{1, 2, 42, 43, 44} {
		inv := runtimeBudgetInventory(count)
		// Unselected inventory hosts must not increase the acceptance matrix.
		for i := 0; i < 50; i++ {
			inv.Hosts = append(inv.Hosts, inventory.Device{ID: fmt.Sprintf("unselected-%d", i), ObservationState: "observed"})
		}
		before, _ := json.Marshal(inv)
		check := hbnRuntimeBudgetCheck(inv)
		after, _ := json.Marshal(inv)
		if string(before) != string(after) {
			t.Fatal("budget observation mutated inventory")
		}
		want := "passed"
		if count >= 43 {
			want = "failed"
		}
		if check.State != want || check.RequiredPhase != "deploy" {
			t.Fatalf("count=%d check=%+v", count, check)
		}
	}
	for _, inv := range []*inventory.Inventory{nil, {}, runtimeBudgetInventory(2)} {
		if inv != nil && len(inv.Associations) > 0 {
			inv.Associations = inv.Associations[:1]
		}
		if check := hbnRuntimeBudgetCheck(inv); check.State != "unknown" {
			t.Fatalf("incomplete cohort became known: %+v", check)
		}
	}
	inv := runtimeBudgetInventory(2)
	inv.Hosts[0].Adapters = append(inv.Hosts[0].Adapters, inv.Hosts[1].Adapters...)
	inv.Associations[1].HostID = inv.Hosts[0].ID
	if check := hbnRuntimeBudgetCheck(inv); check.State != "failed" || check.ReasonCode != "HOST_NETWORK_MULTIPLE_DPUS_UNSUPPORTED" {
		t.Fatalf("duplicate physical host accepted: %+v", check)
	}
}

func TestHBNRuntimeBudgetBlocksPublishedUseCaseReadiness(t *testing.T) {
	inv := runtimeBudgetInventory(44)
	profile, err := hbn.Resolve(hbn.Options{})
	if err != nil {
		t.Fatal(err)
	}
	profile.DPUASNStride = 1
	report := NewReport()
	report.Inventory = inv
	collectHBN(context.Background(), config.Request{HBN: &profile}, &report, nil, Dependencies{})
	if report.Readiness == nil || report.Readiness.State != "blocked" {
		t.Fatalf("impossible native matrix did not block readiness: %+v", report.Readiness)
	}
	for _, check := range report.Readiness.Checks {
		if check.ID == "dpf.hbn.runtime-budget" {
			if check.State != "failed" || check.ReasonCode != "HBN_RUNTIME_BUDGET_INSUFFICIENT" || check.RequiredPhase != "deploy" {
				t.Fatalf("incorrect matrix check: %+v", check)
			}
			return
		}
	}
	t.Fatal("published readiness omitted native matrix budget check")
}
