// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestValidationNativeMalformedConditionsBlockRunnerWithoutRetry(t *testing.T) {
	prepared := prepareValidationFixture(t, []string{"SERIAL-A"})
	for _, test := range []struct {
		name, kind             string
		entry, missing, empty  bool
		parentMissing, pending bool
		conditions             any
	}{
		{name: "HBN condition map", kind: "DPUService", conditions: map[string]any{"Ready": "malformed"}},
		{name: "HBN scalar collection", kind: "DPUService", conditions: "malformed"},
		{name: "HBN malformed list entry", kind: "DPUService", conditions: []any{"malformed"}},
		{name: "HBN missing collection remains Pending", kind: "DPUService", missing: true, pending: true},
		{name: "HBN empty collection remains Pending", kind: "DPUService", empty: true, pending: true},
		{name: "NSI selected entry condition map", kind: "NodeServiceInterfaces", entry: true, conditions: map[string]any{"Ready": "malformed"}},
		{name: "NSI selected entry malformed list", kind: "NodeServiceInterfaces", entry: true, conditions: []any{int64(1)}},
		{name: "NSI malformed entry behind missing parent conditions", kind: "NodeServiceInterfaces", entry: true, parentMissing: true, conditions: map[string]any{"Ready": "malformed"}},
		{name: "NSI missing selected entry collection remains Pending", kind: "NodeServiceInterfaces", entry: true, missing: true, pending: true},
		{name: "NSI empty selected entry collection remains Pending", kind: "NodeServiceInterfaces", entry: true, empty: true, pending: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual, children := n3cRuntimeAdapterFixtureFromPreparation(t, prepared)
			objects := actual.objects
			id := validation.CheckID("hbn/SERIAL-A")
			if test.entry {
				objects = children
				id = "workloads/SERIAL-A"
			}
			var target *unstructured.Unstructured
			for _, object := range objects {
				if object.GetKind() == test.kind {
					target = object
				}
			}
			if target == nil {
				t.Fatal("selected fixture target is absent")
			}
			original := target.DeepCopy()
			actual.before = func(call int) {
				target.Object = original.DeepCopy().Object
				if call > 1 {
					return // A retry would see healthy state; malformed evidence must prevent it.
				}
				conditions := test.conditions
				if test.empty {
					conditions = []any{}
				}
				if test.entry {
					statuses, _, _ := unstructured.NestedSlice(target.Object, "status", "interfaceStatuses")
					entry := statuses[0].(map[string]any)
					if test.missing {
						delete(entry, "conditions")
					} else {
						entry["conditions"] = conditions
					}
					_ = unstructured.SetNestedSlice(target.Object, statuses, "status", "interfaceStatuses")
					if test.parentMissing {
						unstructured.RemoveNestedField(target.Object, "status", "conditions")
					}
				} else if test.missing {
					unstructured.RemoveNestedField(target.Object, "status", "conditions")
				} else {
					_ = unstructured.SetNestedField(target.Object, conditions, "status", "conditions")
				}
			}
			checks, scope, err := validationRuntimeChecks(actual.native.desired.plan)
			if err != nil {
				t.Fatal(err)
			}
			boundary := &n3cRunnerBoundary{n3cRuntimeAdapter: actual, independent: []validation.Outcome{}}
			for _, area := range []validation.Area{validation.AreaManagement, validation.AreaOperator, validation.AreaDefinitions, validation.AreaSystem, validation.AreaTenant, validation.AreaSoftware, validation.AreaPhysical} {
				spec := validation.CheckSpec{ID: validation.CheckID("fixture/" + string(area)), Area: area, Subject: "Independent " + string(area), Required: true, Coverage: true, FullOnly: area == validation.AreaPhysical}
				checks = append(checks, spec)
				if !spec.FullOnly {
					boundary.independent = append(boundary.independent, validation.Outcome{ID: spec.ID, State: validation.Passed})
				}
			}
			plan, err := validation.NewPlan(validation.PlanInput{Mode: validation.ModeResources, Checks: checks, Runtime: scope})
			if err != nil {
				t.Fatal(err)
			}
			runner, err := validation.NewRunner(plan, boundary, validation.Options{Wait: 5 * time.Second, PollInterval: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			result := runner.Run(context.Background())
			wantCalls, wantState := 1, validation.Failed
			if test.pending {
				wantCalls, wantState = 2, validation.Pending
			}
			if actual.calls != wantCalls || (result.Verdict == validation.VerdictResourcesPassed) != test.pending {
				t.Fatalf("pending=%v calls=%d verdict=%s", test.pending, actual.calls, result.Verdict)
			}
			found := false
			for _, row := range actual.snapshots[0].Checks {
				if row.ID == id {
					found = true
					if row.State != wantState {
						t.Fatalf("wanted %s: %+v", wantState, row)
					}
				}
			}
			if !found {
				t.Fatal("actual adapter omitted selected condition outcome")
			}
		})
	}
}
