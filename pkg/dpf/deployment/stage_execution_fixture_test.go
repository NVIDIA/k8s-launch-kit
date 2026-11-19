// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
)

func TestStageFixtureCacheKeepsTenantHarnessesIndependentAfterCleanup(t *testing.T) {
	var first *tenantStageFake
	var original *managementExecution
	var firstContext context.Context
	var before string
	t.Run("first owner", func(t *testing.T) {
		execution, fake, receipt := newTenantStageFixture(t)
		original, first = execution, fake
		before = fixtureExecutionSnapshot(t, execution)
		var cancel context.CancelFunc
		firstContext, cancel = context.WithTimeout(context.Background(), time.Minute)
		t.Cleanup(cancel)
		result, err := runTenantStage(firstContext, fake, execution, receipt, testJournal(execution), time.Millisecond, time.Second,
			func(context.Context, bootstrap.Tenant) ([]inventory.Check, error) {
				return []inventory.Check{{ID: "network", Subject: "node-a", Vantage: "executor", RequiredPhase: "deploy", State: "passed", ReasonCode: "NETWORK_READY", Message: "fixture network ready", EvidenceRefs: []string{}}}, nil
			})
		if err != nil || result.Client != fake.tenant || fake.createCount != 1 {
			t.Fatalf("first tenant stage did not execute: result=%+v creates=%d err=%v", result, fake.createCount, err)
		}
		if fixtureExecutionSnapshot(t, execution) != before {
			t.Fatal("actual tenant stage mutated the shared desired snapshot")
		}
		fake.resources[tenantStageKey(execution.manager)].SetUID("first-only")
		fake.events = append(fake.events, "first-only")
		fake.tenant.systemUID = "first-only"
		for name := range receipt.NamespaceUIDs {
			receipt.NamespaceUIDs[name] = "first-only"
		}
	})
	if firstContext == nil || firstContext.Err() != context.Canceled {
		t.Fatal("first tenant harness did not release its own context")
	}
	execution, second, receipt := newTenantStageFixture(t)
	if execution != original || second == first || second.tenant == first.tenant ||
		second.resources[tenantStageKey(execution.manager)].GetUID() == "first-only" ||
		len(second.events) != 0 || second.tenant.systemUID == "first-only" {
		t.Fatal("cached tenant execution retained a closed or mutated harness")
	}
	for _, uid := range receipt.NamespaceUIDs {
		if uid == "first-only" {
			t.Fatal("tenant namespace receipt escaped its owner")
		}
	}
	if execution.operator.timeout != time.Minute || fixtureExecutionSnapshot(t, execution) != before {
		t.Fatal("tenant harness construction mutated the fixed desired snapshot")
	}
}

func TestStageFixtureCacheKeepsEnrollmentHarnessesIndependentAfterCleanup(t *testing.T) {
	var first *enrollmentStageFake
	var firstTenant tenantStageReceipt
	var original *managementExecution
	var firstContext context.Context
	var before string
	t.Run("first owner", func(t *testing.T) {
		execution, fake, bootstrap, tenant, password := enrollmentStageFixture(t)
		original, first, firstTenant = execution, fake, tenant
		before = fixtureExecutionSnapshot(t, execution)
		var cancel context.CancelFunc
		firstContext, cancel = context.WithTimeout(context.Background(), time.Minute)
		t.Cleanup(cancel)
		result, err := runEnrollmentStage(firstContext, fake, execution.verificationInputs(), bootstrap, tenant, fake.selected, password, testJournal(execution), time.Millisecond, time.Second)
		if err != nil || len(result.Devices) != 1 || !result.Secret.Created || fake.base.createCount != 2 {
			t.Fatalf("first enrollment stage did not execute: result=%+v creates=%d err=%v", result, fake.base.createCount, err)
		}
		if fixtureExecutionSnapshot(t, execution) != before {
			t.Fatal("actual enrollment stage mutated the shared desired snapshot")
		}
		fake.base.resources[tenantStageKey(execution.manager)].SetUID("first-only")
		fake.sources.sources.secret.SetUID("first-only")
		fake.selected[0].device.SelectorLabels[enrollmentSelectorKey] = "first-only"
		fake.base.events = append(fake.base.events, "first-only")
		for name := range bootstrap.NamespaceUIDs {
			bootstrap.NamespaceUIDs[name] = "first-only"
		}
	})
	if firstContext == nil || firstContext.Err() != context.Canceled {
		t.Fatal("first enrollment harness did not release its own context")
	}
	execution, second, bootstrap, tenant, _ := enrollmentStageFixture(t)
	if execution != original || second == first || second.base == first.base || second.sources == first.sources || tenant.Client == firstTenant.Client ||
		second.base.resources[tenantStageKey(execution.manager)].GetUID() == "first-only" ||
		second.sources.sources.secret.GetUID() == "first-only" ||
		second.selected[0].device.SelectorLabels[enrollmentSelectorKey] == "first-only" || len(second.base.events) != 0 {
		t.Fatal("cached enrollment execution retained a closed or mutated harness")
	}
	for _, uid := range bootstrap.NamespaceUIDs {
		if uid == "first-only" {
			t.Fatal("enrollment bootstrap receipt escaped its owner")
		}
	}
	if execution.operator.timeout != time.Minute || fixtureExecutionSnapshot(t, execution) != before {
		t.Fatal("enrollment harness construction mutated the fixed desired snapshot")
	}
}

func TestStageFixtureCacheBindsInputsAndConcurrentEnrollmentLookups(t *testing.T) {
	base := prepareEnrollmentStageExecution(t, nil)
	selected := prepareEnrollmentStageExecution(t, map[string]string{enrollmentSelectorKey: "selected-card-a"})
	if base == selected || base.plan.Digest() == selected.plan.Digest() {
		t.Fatal("different generated selector inputs shared a fixture execution")
	}
	before := fixtureExecutionSnapshot(t, base)
	const workers = 4
	results := make([]*managementExecution, workers)
	errors := make([]error, workers)
	var group sync.WaitGroup
	for i := range results {
		group.Add(1)
		go func() {
			defer group.Done()
			results[i] = prepareEnrollmentStageExecution(t, nil)
			_, errors[i] = results[i].reconciler.markedDesired(results[i].configObject)
		}()
	}
	group.Wait()
	for i, execution := range results {
		if execution != base || errors[i] != nil || execution.operator.timeout != time.Minute {
			t.Fatalf("concurrent fixed-input fixture lookup changed: %v", errors[i])
		}
	}
	if fixtureExecutionSnapshot(t, base) != before {
		t.Fatal("concurrent schema verification mutated the desired fixture")
	}
	assertEnrollmentFixtureIsolation(t, base)
}

func TestStageFixtureCacheDoesNotRetainFailedPreparation(t *testing.T) {
	fixturePreparations.Lock()
	before := len(fixturePreparations.values)
	fixturePreparations.Unlock()
	if execution, err := prepareStageFixtureExecution(nil); execution != nil || err == nil {
		t.Fatal("nil retained fixture was accepted")
	}
	fixturePreparations.Lock()
	after := len(fixturePreparations.values)
	fixturePreparations.Unlock()
	if after != before {
		t.Fatal("failed fixture preparation changed the success-only memo")
	}
}
