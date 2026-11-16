// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func softwareExecutionSnapshot(t *testing.T, execution *managementExecution) string {
	t.Helper()
	chart, err := chartFingerprint(execution.operator.chart)
	if err != nil {
		t.Fatal(err)
	}
	crds := make([]any, 0, len(execution.crds))
	for _, crd := range execution.crds {
		crds = append(crds, map[string]any{"name": crd.name, "spec": crd.spec, "versions": crd.versions, "normalized": crd.normalizedSpecJSON})
	}
	metadata := map[string][]string{}
	for identity, tokens := range execution.reconciler.controllerMetadata {
		key := identity.namespace + "/" + identity.name
		for token := range tokens {
			metadata[key] = append(metadata[key], token)
		}
		sort.Strings(metadata[key])
	}
	encoded, err := json.Marshal(map[string]any{
		"intent": execution.plan.Intent(), "bundle": execution.retained.Files(), "chart": chart,
		"manager": execution.manager, "config": execution.configObject, "namespaces": execution.namespaces,
		"crds": crds, "operatorManifest": execution.operator.renderedManifest, "operatorValues": execution.operator.valuesYAML,
		"preflightProjection": execution.preflight.projection, "preflightIntent": execution.preflight.intent,
		"controllerMetadata": metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestSoftwareExecutionFixtureOutlivesFirstHarnessWithoutSharingMutableState(t *testing.T) {
	var execution *managementExecution
	var first *runtimeStageFixture
	var before string
	if !t.Run("first owner exits", func(t *testing.T) {
		execution = prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
		before = softwareExecutionSnapshot(t, execution)
		first = newRuntimeStageFixtureFromExecution(t, execution)
		first.client.base.resources[tenantStageKey(execution.manager)].SetUID("first-only")
		first.snapshot.CIDRPool.SetUID("first-only")
		first.controlPlane.enrollment.Devices[0].DeviceUID = "first-only"
		first.commandReads = 91
		first.denyTenant = true
	}) {
		return
	}
	if first == nil || !errors.Is(first.ctx.Err(), context.Canceled) {
		t.Fatal("first harness cleanup did not release its own context")
	}
	current := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	if current != execution || softwareExecutionSnapshot(t, current) != before {
		t.Fatal("same bundle did not retain the unchanged desired snapshot after first owner cleanup")
	}
	second := newRuntimeStageFixtureFromExecution(t, current)
	if second.ctx.Err() != nil || second.client == first.client || second.client.base == first.client.base || second.journal == first.journal ||
		second.client.base.resources[tenantStageKey(current.manager)].GetUID() == "first-only" || second.snapshot.CIDRPool.GetUID() == "first-only" ||
		second.controlPlane.enrollment.Devices[0].DeviceUID == "first-only" || second.commandReads != 0 || second.denyTenant {
		t.Fatal("later retrieval reused a closed or mutated harness")
	}
	if softwareExecutionSnapshot(t, current) != before {
		t.Fatal("fresh harness construction mutated the shared desired snapshot")
	}
}

func TestSoftwareExecutionFixtureCacheBindsDifferentBundlesAndConcurrentLookups(t *testing.T) {
	one := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	two := prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-B"})
	if one == two || one.plan.Digest() == two.plan.Digest() || len(one.plan.Intent().Devices) != 1 || len(two.plan.Intent().Devices) != 2 {
		t.Fatal("different fixture inputs shared one cached execution")
	}
	const workers = 4
	results := make([]*managementExecution, workers)
	verificationErrors := make([]error, workers)
	var group sync.WaitGroup
	for i := range results {
		group.Add(1)
		go func() {
			defer group.Done()
			results[i] = prepareSoftwareStageExecution(t, []string{"SERIAL-A", "SERIAL-C"})
			_, verificationErrors[i] = results[i].reconciler.markedDesired(results[i].configObject)
		}()
	}
	group.Wait()
	for i, execution := range results {
		if execution == nil || verificationErrors[i] != nil {
			t.Fatalf("concurrent fixture preparation or desired-schema verification failed: %v", verificationErrors[i])
		}
	}
	before := softwareExecutionSnapshot(t, results[0])
	for _, execution := range results {
		if execution != results[0] || execution == one || execution == two || len(execution.plan.Intent().Devices) != 2 || execution.plan.Intent().Devices[1].Serial != "SERIAL-C" {
			t.Fatal("concurrent exact-bundle lookups disagreed or reused another input")
		}
		copy := execution.manager.DeepCopy()
		_ = unstructured.SetNestedField(copy.Object, "changed", "spec", "template", "metadata", "labels", "fixture-canary")
		intent := execution.plan.Intent()
		intent.Devices[0].Serial = "changed"
		if softwareExecutionSnapshot(t, execution) != before {
			t.Fatal("caller-owned copies mutated the cached desired snapshot")
		}
	}
}
