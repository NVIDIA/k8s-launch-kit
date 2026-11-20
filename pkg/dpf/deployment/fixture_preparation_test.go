// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
)

func TestFixturePreparationSeparatesValidationFromExecution(t *testing.T) {
	input := preflightGenerationInputs(t)
	input.SourceConfig = append(bytes.Clone(input.SourceConfig), []byte("\n# independent fixture preparation: "+t.TempDir()+"\n")...)
	retained, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatal(err)
	}
	const readers = 4
	results := make([]*managementDesired, readers)
	errors := make([]error, readers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for i := range results {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			results[i], errors[i] = prepareValidationFixtureDesired(retained)
			if errors[i] == nil {
				_, errors[i] = results[i].verifier.markedDesired(results[i].configObject)
			}
		}()
	}
	close(start)
	group.Wait()
	desired := results[0]
	for i := range results {
		if errors[i] != nil || results[i] == nil || results[i] != desired {
			t.Fatalf("concurrent first publication %d returned a different or incomplete owner: %v", i, errors[i])
		}
	}
	fixturePreparations.Lock()
	executionCreated := fixturePreparations.values[retained.Index().BundleDigest].execution != nil
	fixturePreparations.Unlock()
	if executionCreated {
		t.Fatal("read-only fixture preparation constructed an unused execution")
	}
	execution, err := prepareStageFixtureExecution(retained)
	if err != nil {
		t.Fatal(err)
	}
	if desired.operator == execution.operator.operatorDesired || desired.operator.chart == execution.operator.chart ||
		desired.configObject == execution.configObject || desired.manager == execution.manager {
		t.Fatal("validation fixture borrowed executable chart or mutable objects")
	}
	plan, err := prepareFixturePlan(retained)
	if err != nil {
		t.Fatal(err)
	}
	*plan = generation.Plan{}
	current, err := prepareFixturePlan(retained)
	if err != nil || current.Digest() != retained.Index().BundleDigest {
		t.Fatalf("returned plan wrapper changed the registry: %v", err)
	}

	// Mutate a private execution, never another test's cached fixed execution.
	// This exercises nested constructor ownership as well as wrapper inequality.
	before := fixtureDesiredSnapshot(t, desired)
	private, err := prepareManagementExecution(current, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	private.operator.chart.Metadata.Name = "private-execution"
	private.operator.archive[0] ^= 1
	private.operator.expectedManifest[0] ^= 1
	private.operator.renderedManifest[0] ^= 1
	private.operator.valuesYAML[0] ^= 1
	private.operator.expectedValuesJSON[0] ^= 1
	private.manager.Object["fixtureMutation"] = true
	private.configObject.Object["fixtureMutation"] = true
	private.namespaces[0].SetName("private-execution")
	private.crds[0].spec["fixtureMutation"] = true
	private.crds[0].versions[0] = "private-execution"
	*private.plan = generation.Plan{}
	if fixtureDesiredSnapshot(t, desired) != before {
		t.Fatal("execution mutation changed read-only validation desired")
	}
	one := prepareValidationFixture(t, []string{"SERIAL-A"})
	two := prepareValidationFixture(t, []string{"SERIAL-A", "SERIAL-B"})
	if one.desired == two.desired || one.desired.plan.Digest() == two.desired.plan.Digest() {
		t.Fatal("different retained cohorts shared a desired owner")
	}
}

func fixtureDesiredSnapshot(t *testing.T, desired *managementDesired) string {
	t.Helper()
	chart, err := chartFingerprint(desired.operator.chart)
	if err != nil {
		t.Fatal(err)
	}
	crds := make([]any, 0, len(desired.crds))
	for _, crd := range desired.crds {
		crds = append(crds, map[string]any{"name": crd.name, "spec": crd.spec, "versions": crd.versions, "normalized": crd.normalizedSpecJSON})
	}
	encoded, err := json.Marshal(map[string]any{
		"intent": desired.plan.Intent(), "bundle": desired.retained.Files(), "chart": chart,
		"manager": desired.manager, "config": desired.configObject, "namespaces": desired.namespaces, "crds": crds,
		"operatorArchive": desired.operator.archive, "operatorManifest": desired.operator.renderedManifest,
		"operatorExpectedManifest": desired.operator.expectedManifest, "operatorValues": desired.operator.valuesYAML,
		"operatorExpectedValues": desired.operator.expectedValuesJSON,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestFixturePreparationRejectsInvalidInputsWithoutPublication(t *testing.T) {
	fixturePreparations.Lock()
	before := len(fixturePreparations.values)
	fixturePreparations.Unlock()
	for _, retained := range []*bundle.Bundle{nil, {}} {
		if plan, err := prepareFixturePlan(retained); plan != nil || err == nil {
			t.Fatal("invalid fixture plan was accepted")
		}
		if desired, err := prepareValidationFixtureDesired(retained); desired != nil || err == nil {
			t.Fatal("invalid desired fixture was accepted")
		}
		if execution, err := prepareStageFixtureExecution(retained); execution != nil || err == nil {
			t.Fatal("invalid executable fixture was accepted")
		}
	}
	fixturePreparations.Lock()
	after := len(fixturePreparations.values)
	fixturePreparations.Unlock()
	if after != before {
		t.Fatal("failed fixture preparation was published")
	}
}

func TestFixturePreparationKeepsMutableManagerDesiredPrivate(t *testing.T) {
	first := directManagerTestObserver(t, "", false)
	first.desired.configObject.Object["fixtureMutation"] = true
	second := directManagerTestObserver(t, "", false)
	if first.desired == second.desired || first.desired.configObject == second.desired.configObject {
		t.Fatal("mutable manager fixtures share desired objects")
	}
	if _, found := second.desired.configObject.Object["fixtureMutation"]; found {
		t.Fatal("desired mutation escaped its manager fixture")
	}
}

func TestFixturePreparationKeepsTrustFilesAndAccessIndependent(t *testing.T) {
	var firstDesired *managementDesired
	var firstPath string
	if !t.Run("first owner", func(t *testing.T) {
		desired, input := physicalScopeFixture(t)
		firstDesired = desired
		firstPath = input.Path
		initial := physicalScopeCAPEM(t)
		initial[0] ^= 1
		if bytes.Equal(initial, physicalScopeCAPEM(t)) {
			t.Fatal("baseline certificate bytes escaped their owner")
		}
		if err := os.Remove(filepath.Join(filepath.Dir(input.Path), "bmc-ca.pem")); err != nil {
			t.Fatal(err)
		}
		if request, issues := resolvePhysicalScope(t, input); len(issues) == 0 || len(request.Targets) != 1 {
			t.Fatal("removed trust file was hidden by cached desired preparation")
		}
	}) {
		return
	}
	if _, err := os.Stat(filepath.Dir(firstPath)); !os.IsNotExist(err) {
		t.Fatal("first fixture directory was not cleaned up")
	}
	secondDesired, input := physicalScopeFixture(t)
	if firstDesired != secondDesired || input.Path == firstPath {
		t.Fatal("fixtures must share desired bytes but own separate access files")
	}
	request, issues := resolvePhysicalScope(t, input)
	if len(issues) != 0 {
		t.Fatalf("first fixture cleanup affected second access: %v", issues)
	}
	if err := validateValidationPhysicalScope(secondDesired, request); err != nil {
		t.Fatalf("fresh resolved trust no longer matches retained scope: %v", err)
	}
	if bytes.Equal(physicalScopeCAPEM(t), softwareTestCAPEM(t)) {
		t.Fatal("replacement trust must differ from the baseline")
	}
}
