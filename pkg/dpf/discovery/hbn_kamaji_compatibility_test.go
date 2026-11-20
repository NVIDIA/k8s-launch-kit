// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"testing"
)

func TestHBNKamajiCompatibilityNeedsCurrentMatchingObservation(t *testing.T) {
	for _, scenario := range []string{"clean", "existing tenant", "nil inventory", "missing release", "nil snapshot", "missing observation", "wrong namespace", "wrong release", "fabricated pass", "incompatible"} {
		t.Run(scenario, func(t *testing.T) {
			inv := hbnManagementTestInventoryPointer()
			release, err := releases.Resolve("26.8.0")
			if err != nil {
				t.Fatal(err)
			}
			inv.Software = &release
			snapshot := freshAbsentHBNManagementSnapshot()
			snapshot.KamajiCompatibility = &managementprobe.KamajiCompatibilityResult{State: "passed", ReasonCode: "KAMAJI_COMPATIBILITY_VERIFIED", Message: "Verified immutable producer; tenant state is checked separately.", Release: "26.8.0", Namespace: snapshot.Namespace}
			selected := &snapshot
			switch scenario {
			case "existing tenant":
				snapshot.DPUClusters.Items = []managementprobe.DPUCluster{{ObjectSummary: managementprobe.ObjectSummary{Name: "existing", UID: "existing-uid"}}}
			case "nil inventory":
				inv = nil
			case "missing release":
				inv.Software = nil
			case "nil snapshot":
				selected = nil
			case "missing observation":
				snapshot.KamajiCompatibility = nil
			case "wrong namespace":
				snapshot.KamajiCompatibility.Namespace = "elsewhere"
			case "wrong release":
				snapshot.KamajiCompatibility.Release = "26.4.0"
			case "fabricated pass":
				snapshot.KamajiCompatibility.ReasonCode = "UNKNOWN"
			case "incompatible":
				snapshot.KamajiCompatibility.State = "unknown"
				snapshot.KamajiCompatibility.ReasonCode = "KAMAJI_COMPATIBILITY_PROCESS_UNSUPPORTED"
			}
			check := kamajiCompatibilityCheck(selected, inv, snapshot.Namespace)
			want := scenario == "clean" || scenario == "existing tenant"
			if (check.state == "passed") != want {
				t.Fatalf("%s: %+v", scenario, check)
			}
			checks, _ := managementHBNChecks(selected, inv, snapshot.Namespace)
			if got := managementCheckByID(t, checks, "kamaji-compatibility"); got.RequiredPhase != "deploy" || got.State != check.state {
				t.Fatal(got)
			}
			// The prerequisite pass does not replace existing tenant/runtime or storage checks.
			if want {
				storage := managementCheckByID(t, checks, "storage-health")
				if storage.State == "passed" {
					t.Fatal("producer proof incorrectly proved backing storage")
				}
			}
		})
	}
}
