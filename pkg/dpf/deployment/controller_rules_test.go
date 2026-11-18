// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
)

var selectedFixtureRules struct {
	sync.Once
	rules *controllercontract.Rules
	err   error
}

func testRules(t *testing.T) *controllercontract.Rules {
	t.Helper()
	selectedFixtureRules.Do(func() {
		generated, err := generateFixtureBundle(preflightGenerationInputs(t))
		if err != nil {
			selectedFixtureRules.err = err
			return
		}
		plan, err := generation.LoadPlan(generated)
		if err != nil {
			selectedFixtureRules.err = err
			return
		}
		inputs, archive := plan.Operator()
		operator, err := prepareOperator(OperatorRequest{Archive: archive, Inputs: inputs, Timeout: time.Minute})
		if err != nil {
			selectedFixtureRules.err = err
			return
		}
		schemas, err := generation.NewObjectSchemas(operator.renderedManifest)
		if err != nil {
			selectedFixtureRules.err = err
			return
		}
		selectedFixtureRules.rules, selectedFixtureRules.err = controllercontract.Select("26.8.0", "hbn-bgp", "native-dpf-generation/v1", schemas)
	})
	if selectedFixtureRules.err != nil {
		t.Fatal(selectedFixtureRules.err)
	}
	return selectedFixtureRules.rules
}

func testRulesForManifest(t *testing.T, manifest []byte) *controllercontract.Rules {
	t.Helper()
	schemas, err := generation.NewObjectSchemas(manifest)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := controllercontract.Select("26.8.0", "hbn-bgp", "native-dpf-generation/v1", schemas)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func TestMissingControllerSelectionRejectsBeforeNativeAccessOrEffects(t *testing.T) {
	reconciler, desired := newReconcileFixture(t)
	client := &executionTestAPI{t: t, file: &executionTestFile{}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	journal := &executionJournal{digest: reconciler.BundleDigest(), file: client.file}
	if _, err := executeNativeObject(nil, ctx, client, reconciler, desired, "test", journal, false, time.Millisecond); err == nil {
		t.Fatal("missing selection accepted")
	}
	if client.gets != 0 || client.creates != 0 || client.file.Len() != 0 {
		t.Fatal("unselected execution accessed API or recorded an effect attempt")
	}
	source := &tenantSourceStub{}
	if _, err := newTenantClient(ctx, nil, source, controllercontract.TenantReadinessExpected{}, "uid", time.Second, nil, nil); err == nil || source.calls != 0 {
		t.Fatal("unselected tenant construction accessed sources")
	}
}
