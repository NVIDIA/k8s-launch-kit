// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

func TestEnrollmentSelectorPatchOnlyCreatesOwnedMissingKey(t *testing.T) {
	for _, tc := range []struct {
		name    string
		labels  any
		wanted  map[string]string
		changed bool
		valid   bool
	}{
		{name: "absent labels", wanted: map[string]string{enrollmentSelectorKey: "card-a"}, changed: true, valid: true},
		{name: "preserve other labels", labels: map[string]any{"vendor.example/device": "other"}, wanted: map[string]string{enrollmentSelectorKey: "card-a"}, changed: true, valid: true},
		{name: "match", labels: map[string]any{enrollmentSelectorKey: "card-a"}, wanted: map[string]string{enrollmentSelectorKey: "card-a"}, valid: true},
		{name: "preexisting custom", labels: map[string]any{"inventory.example/card": "card-a"}, wanted: map[string]string{"inventory.example/card": "card-a"}, valid: true},
		{name: "conflict", labels: map[string]any{enrollmentSelectorKey: "other"}, wanted: map[string]string{enrollmentSelectorKey: "card-a"}},
		{name: "absent custom", wanted: map[string]string{"inventory.example/card": "card-a"}},
		{name: "nonstring labels", labels: map[string]any{"other": int64(7)}, wanted: map[string]string{enrollmentSelectorKey: "card-a"}},
		{name: "wrong labels shape", labels: []any{"bad"}, wanted: map[string]string{enrollmentSelectorKey: "card-a"}},
		{name: "empty selector", wanted: map[string]string{}},
		{name: "invalid selector", wanted: map[string]string{"bad key": "bad"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := readyEnrolledDeviceFixture()
			if tc.labels != nil {
				_ = unstructured.SetNestedField(object.Object, tc.labels, "metadata", "labels")
			} else {
				unstructured.RemoveNestedField(object.Object, "metadata", "labels")
			}
			before := object.DeepCopy()
			wanted := maps.Clone(tc.wanted)
			merged, changed, err := enrollmentSelectorLabels(object, tc.wanted)
			if (err == nil) != tc.valid || err == nil && changed != tc.changed {
				t.Fatalf("unexpected selector decision changed=%t err=%v", changed, err)
			}
			if !reflect.DeepEqual(before, object) || !maps.Equal(wanted, tc.wanted) {
				t.Fatal("selector validation mutated caller input")
			}
			patchData, patchErr := enrollmentSelectorPatch(object, tc.wanted)
			if !tc.valid || !tc.changed {
				if patchErr == nil {
					t.Fatal("no-op/conflict emitted a patch")
				}
				return
			}
			if patchErr != nil {
				t.Fatal(patchErr)
			}
			var ops []map[string]any
			if err := json.Unmarshal(patchData, &ops); err != nil || len(ops) != 3 || ops[0]["path"] != "/metadata/uid" || ops[1]["path"] != "/metadata/resourceVersion" || ops[2]["op"] != "add" {
				t.Fatalf("patch omitted identity tests: %s", patchData)
			}
			patch, err := jsonpatch.DecodePatch(patchData)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(object.Object)
			raw, err = patch.Apply(raw)
			if err != nil {
				t.Fatal(err)
			}
			updated := &unstructured.Unstructured{}
			if err := updated.UnmarshalJSON(raw); err != nil {
				t.Fatal(err)
			}
			updated.SetResourceVersion("bound-version")
			if !maps.Equal(updated.GetLabels(), merged) || !enrollmentSelectorUpdateMatches(object, updated, tc.wanted) {
				t.Fatal("patch changed unrelated intent or labels")
			}
			for _, change := range []func(*unstructured.Unstructured){func(o *unstructured.Unstructured) { o.SetUID("replaced") }, func(o *unstructured.Unstructured) { o.SetResourceVersion("concurrent") }} {
				other := object.DeepCopy()
				change(other)
				raw, _ = json.Marshal(other.Object)
				if _, err := patch.Apply(raw); err == nil {
					t.Fatal("stale patch ignored UID/RV test")
				}
			}
		})
	}
}

func TestNativeEnrollmentPublishesSelectorsAndNoopsMatchingValues(t *testing.T) {
	execution, fake, bootstrap, tenant, password := enrollmentStageFixtureWithSelectors(t, map[string]string{enrollmentSelectorKey: "selected-card-a"})
	fake.scanHook = func(*unstructured.Unstructured) {
		for _, o := range fake.base.resources {
			if o.GetKind() == "DPUDevice" {
				o.SetLabels(map[string]string{"vendor.example/keep": "unchanged"})
				o.SetAnnotations(map[string]string{"vendor.example/annotation": "unchanged"})
			}
		}
	}
	intentBefore := execution.plan.Intent()
	for attempt := 0; attempt < 2; attempt++ {
		journal := testJournal(execution)
		receipt, err := runEnrollmentStage(tenantStageContext(t), fake, execution.verificationInputs(), bootstrap, tenant, fake.selected, password, journal, time.Millisecond, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if len(receipt.Devices) != 1 || fake.selectorPatches != 1 {
			t.Fatalf("missing binding or repeated effect: receipts=%d patches=%d", len(receipt.Devices), fake.selectorPatches)
		}
		for _, o := range fake.base.resources {
			if o.GetKind() == "DPUDevice" {
				if !labels.SelectorFromSet(fake.selected[0].device.SelectorLabels).Matches(labels.Set(o.GetLabels())) || o.GetLabels()["vendor.example/keep"] != "unchanged" || o.GetAnnotations()["vendor.example/annotation"] != "unchanged" {
					t.Fatal("completed enrollment does not preserve selected eligibility")
				}
			}
		}
		report := journal.file.(*managementMemoryFile).String()
		code := "ENROLLMENT_SELECTOR_BOUND"
		if attempt == 1 {
			code = "ENROLLMENT_SELECTOR_MATCHED"
		}
		if !strings.Contains(report, code) || strings.Contains(report, password) {
			t.Fatal("binding receipt absent or leaked credential")
		}
		message := "The verified DPU belongs to its selected deployment group."
		if attempt == 1 {
			message = "The verified DPU already belongs to its selected deployment group; no labels were changed."
		}
		if !strings.Contains(report, message) || strings.Contains(report, "UID and resourceVersion preconditions") || strings.Contains(report, "missing native selector on") {
			t.Fatal("normal binding progress is not plain user-facing language")
		}
	}
	if !reflect.DeepEqual(intentBefore, execution.plan.Intent()) {
		t.Fatal("binding mutated retained intent")
	}
}

func TestNativeEnrollmentSelectorFailuresRefuseOrRemainUncertain(t *testing.T) {
	prepared := prepareEnrollmentStageExecution(t, map[string]string{enrollmentSelectorKey: "selected-card-a"})
	assertEnrollmentFixtureIsolation(t, prepared)
	for _, tc := range []struct {
		name    string
		edit    func(*enrollmentStageFake)
		code    string
		patches int
	}{
		{name: "conflicting value", edit: func(f *enrollmentStageFake) {
			f.scanHook = func(*unstructured.Unstructured) {
				for _, o := range f.base.resources {
					if o.GetKind() == "DPUDevice" {
						o.SetLabels(map[string]string{enrollmentSelectorKey: "foreign-canary"})
					}
				}
			}
		}, code: "ENROLLMENT_SELECTOR_CONFLICT"},
		{name: "malformed labels", edit: func(f *enrollmentStageFake) {
			f.scanHook = func(*unstructured.Unstructured) {
				for _, o := range f.base.resources {
					if o.GetKind() == "DPUDevice" {
						_ = unstructured.SetNestedField(o.Object, map[string]any{"other": int64(4)}, "metadata", "labels")
					}
				}
			}
		}, code: "ENROLLMENT_SELECTOR_CONFLICT"},
		{name: "foreign owner", edit: func(f *enrollmentStageFake) {
			f.scanHook = func(*unstructured.Unstructured) {
				for _, o := range f.base.resources {
					if o.GetKind() == "DPUDevice" {
						_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"uid": "foreign-canary"}}, "metadata", "ownerReferences")
					}
				}
			}
		}, code: "ENROLLMENT_SELECTOR_OWNER_CONFLICT"},
		{name: "stale version", edit: func(f *enrollmentStageFake) {
			f.selectorPatchHook = func(o *unstructured.Unstructured) error {
				f.base.resources[tenantStageKey(o)].SetResourceVersion("raced")
				return nil
			}
		}, code: "ENROLLMENT_SELECTOR_BIND_UNCERTAIN", patches: 1},
		{name: "replacement UID", edit: func(f *enrollmentStageFake) {
			f.selectorPatchHook = func(o *unstructured.Unstructured) error {
				f.base.resources[tenantStageKey(o)].SetUID("foreign-canary")
				return nil
			}
		}, code: "ENROLLMENT_SELECTOR_BIND_UNCERTAIN", patches: 1},
		{name: "timeout", edit: func(f *enrollmentStageFake) {
			f.selectorPatchHook = func(*unstructured.Unstructured) error {
				return fmt.Errorf("private-remote-canary: %w", context.DeadlineExceeded)
			}
		}, code: "ENROLLMENT_SELECTOR_BIND_UNCERTAIN", patches: 1},
		{name: "label removed after patch", edit: func(f *enrollmentStageFake) {
			f.getHook = func(w *unstructured.Unstructured) {
				if f.selectorPatches > 0 && w.GetKind() == "DPUDevice" {
					f.base.resources[tenantStageKey(w)].SetLabels(nil)
				}
			}
		}, code: "ENROLLMENT_SELECTOR_READBACK_FAILED", patches: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execution, fake, bootstrap, tenant, password := enrollmentStageFixtureFromExecution(t, prepared)
			tc.edit(fake)
			journal := testJournal(execution)
			_, err := runEnrollmentStage(tenantStageContext(t), fake, execution.verificationInputs(), bootstrap, tenant, fake.selected, password, journal, time.Millisecond, time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.code) || fake.selectorPatches != tc.patches {
				t.Fatalf("wrong failure/effects: patches=%d err=%v", fake.selectorPatches, err)
			}
			report := journal.file.(*managementMemoryFile).String()
			if strings.Contains(report, "private-remote-canary") || strings.Contains(report, "foreign-canary") || strings.Contains(report, password) {
				t.Fatal("private value escaped binding diagnostics")
			}
			for _, o := range fake.base.resources {
				if o.GetKind() == "DPUDeployment" || o.GetKind() == "DPU" {
					t.Fatal("enrollment failure advanced deployment")
				}
			}
		})
	}
}

func TestNativeEnrollmentSelectorPatchUsesRealIdentityBoundAPI(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprintf("raced=%t", race), func(t *testing.T) {
			device := readyEnrolledDeviceFixture()
			device.SetLabels(map[string]string{"vendor.example/keep": "same"})
			before := device.DeepCopy()
			calls := 0
			fixture := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/prefix/apis/provisioning.dpu.nvidia.com/v1alpha1" {
					fmt.Fprint(w, `{"kind":"APIResourceList","groupVersion":"provisioning.dpu.nvidia.com/v1alpha1","resources":[{"name":"dpudevices","kind":"DPUDevice","namespaced":true,"verbs":["get","patch"]}]}`)
					return
				}
				if r.Method != http.MethodPatch || !strings.HasSuffix(r.URL.Path, "/dpudevices/"+device.GetName()) {
					t.Errorf("unexpected API request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				calls++
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				patch, err := jsonpatch.DecodePatch(data)
				if err != nil {
					t.Error(err)
				}
				if race {
					device.SetResourceVersion("raced")
				}
				body, _ := json.Marshal(device.Object)
				body, err = patch.Apply(body)
				if err != nil {
					http.Error(w, `{"kind":"Status","apiVersion":"v1","reason":"Conflict","code":409}`, http.StatusConflict)
					return
				}
				updated := &unstructured.Unstructured{}
				if err := updated.UnmarshalJSON(body); err != nil {
					t.Error(err)
				}
				updated.SetResourceVersion("bound")
				device = updated
				_ = json.NewEncoder(w).Encode(updated.Object)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := NewAPIClient(ctx, fixture.request, fixture.expected, "v1.35.6")
			if err != nil {
				t.Fatal(err)
			}
			updated, err := client.patchEnrollmentSelectors(ctx, before, map[string]string{enrollmentSelectorKey: "card-a"})
			if calls != 1 || fixture.writes.Load() != 1 {
				t.Fatalf("wrong patch count %d", calls)
			}
			if race {
				if err == nil || device.GetLabels()[enrollmentSelectorKey] != "" {
					t.Fatal("raced patch wrote labels")
				}
			} else if err != nil || !enrollmentSelectorUpdateMatches(before, updated, map[string]string{enrollmentSelectorKey: "card-a"}) {
				t.Fatalf("real API patch invalid: %v", err)
			}
		})
	}
}

func TestEnrollmentSelectorUpdateRejectsChangedIntent(t *testing.T) {
	before := readyEnrolledDeviceFixture()
	after := before.DeepCopy()
	after.SetResourceVersion("new")
	after.SetLabels(map[string]string{enrollmentSelectorKey: "card-a"})
	for _, edit := range []func(*unstructured.Unstructured){
		func(o *unstructured.Unstructured) { o.SetUID(types.UID("other")) },
		func(o *unstructured.Unstructured) { o.SetAnnotations(map[string]string{"new": "foreign"}) },
		func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "foreign", "spec", "bmcIp")
		},
		func(o *unstructured.Unstructured) {
			o.SetLabels(map[string]string{enrollmentSelectorKey: "card-a", "extra": "foreign"})
		},
	} {
		current := after.DeepCopy()
		edit(current)
		if enrollmentSelectorUpdateMatches(before, current, map[string]string{enrollmentSelectorKey: "card-a"}) {
			t.Fatal("accepted changed identity/intent")
		}
	}
	if enrollmentSelectorUpdateMatches(before, nil, map[string]string{enrollmentSelectorKey: "card-a"}) {
		t.Fatal("nil response accepted")
	}
}

type selectorFailingJournal struct {
	managementMemoryFile
	marker string
}

func (f *selectorFailingJournal) Sync() error {
	if strings.Contains(f.String(), f.marker) {
		return errors.New("report sync failed")
	}
	return nil
}

func TestEnrollmentSelectorJournalFailuresBracketPatch(t *testing.T) {
	prepared := prepareEnrollmentStageExecution(t, map[string]string{enrollmentSelectorKey: "selected-card-a"})
	for _, tc := range []struct {
		marker  string
		patches int
	}{{marker: "ENROLLMENT_SELECTOR_BIND_STARTED", patches: 0}, {marker: "ENROLLMENT_SELECTOR_BOUND", patches: 1}} {
		t.Run(tc.marker, func(t *testing.T) {
			execution, fake, bootstrap, tenant, password := enrollmentStageFixtureFromExecution(t, prepared)
			file := &selectorFailingJournal{marker: tc.marker}
			journal := &executionJournal{file: file, digest: execution.plan.Digest()}
			_, err := runEnrollmentStage(tenantStageContext(t), fake, execution.verificationInputs(), bootstrap, tenant, fake.selected, password, journal, time.Millisecond, time.Second)
			if err == nil || fake.selectorPatches != tc.patches {
				t.Fatalf("journal failure did not stop at effect boundary: patches=%d err=%v", fake.selectorPatches, err)
			}
		})
	}
}

func TestNativeEnrollmentSelectorCohortProgressAndDrift(t *testing.T) {
	prepared := prepareEnrollmentStageExecution(t,
		map[string]string{enrollmentSelectorKey: "card-a"}, map[string]string{enrollmentSelectorKey: "card-b"})
	for _, mode := range []string{"complete", "earlier label removed", "second UID replaced"} {
		t.Run(mode, func(t *testing.T) {
			execution, fake, bootstrap, tenant, password := enrollmentStageFixtureFromExecution(t, prepared)
			fake.bmcHook = func(int) error {
				if fake.selectorPatches != 1 {
					return nil
				}
				for _, object := range fake.base.resources {
					if object.GetKind() != "DPUDevice" {
						continue
					}
					if mode == "earlier label removed" && object.GetName() == "serial-a" {
						object.SetLabels(nil)
					}
					if mode == "second UID replaced" && object.GetName() == "serial-b" {
						object.SetUID("replacement")
					}
				}
				return nil
			}
			journal := testJournal(execution)
			_, err := runEnrollmentStage(tenantStageContext(t), fake, execution.verificationInputs(), bootstrap, tenant, fake.selected, password, journal, time.Millisecond, time.Second)
			if mode == "complete" {
				if err != nil || fake.selectorPatches != 2 {
					t.Fatalf("two-device native binding failed: patches=%d err=%v", fake.selectorPatches, err)
				}
				for _, object := range fake.base.resources {
					if object.GetKind() == "DPUDevice" {
						matches := 0
						for _, selected := range fake.selected {
							if labels.SelectorFromSet(selected.device.SelectorLabels).Matches(labels.Set(object.GetLabels())) {
								matches++
							}
						}
						if matches != 1 {
							t.Fatal("device is not eligible for exactly one retained selector")
						}
					}
				}
			} else if err == nil || fake.selectorPatches != 1 || !strings.Contains(journal.file.(*managementMemoryFile).String(), "ENROLLMENT_SELECTOR_BOUND") {
				t.Fatalf("partial effects lost or drift allowed next patch: patches=%d err=%v", fake.selectorPatches, err)
			}
		})
	}
}
