// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
)

func TestManagementDesiredReusesExactValidatedOperatorArtifacts(t *testing.T) {
	input := preflightGenerationInputs(t)
	input.Operator.Values = map[string]any{"controllerManager": map[string]any{"replicas": 2}}
	input.Operator.Bootstrap.OperatorConfigName = "selected-operator-config"
	retained, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatal(err)
	}
	before := retained.Files()
	plan, err := generation.LoadPlan(retained)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := prepareManagementDesiredFromPlan(retained, plan)
	if err != nil {
		t.Fatal(err)
	}
	inputs, archive := plan.Operator()
	standalone, err := prepareOperatorDesired(archive, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(desired.operator, standalone) {
		t.Fatal("validated retained artifacts differ from independently rendered standalone operator preparation")
	}
	manager, crds, err := operatorDesiredReadinessObjects(standalone)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(desired.manager, manager) || !reflect.DeepEqual(desired.crds, crds) {
		t.Fatal("retained operator readiness differs from independent rendering")
	}

	manifest, values, err := operatorArtifactsFromValidatedPlan(retained, plan)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compileOperatorDesired(archive, inputs, manifest, values)
	if err != nil {
		t.Fatal(err)
	}
	// The compiler owns its inputs, and the extraction helper owns copies of
	// retained bytes. Neither layer may borrow another preparation's buffers.
	archive[0] ^= 0xff
	manifest[0] ^= 0xff
	values[0] ^= 0xff
	if !reflect.DeepEqual(compiled, standalone) || !reflect.DeepEqual(before, retained.Files()) {
		t.Fatal("operator artifact inputs alias compiled or retained state")
	}
	compiled.archive[0] ^= 0xff
	compiled.renderedManifest[0] ^= 0xff
	compiled.valuesYAML[0] ^= 0xff
	if !reflect.DeepEqual(desired.operator, standalone) || !reflect.DeepEqual(before, retained.Files()) {
		t.Fatal("one operator preparation mutated another preparation or the retained bundle")
	}
	manifestAgain, valuesAgain, err := operatorArtifactsFromValidatedPlan(retained, plan)
	if err != nil || !bytes.Equal(manifestAgain, standalone.renderedManifest) || !bytes.Equal(valuesAgain, standalone.valuesYAML) {
		t.Fatalf("retained projection/values changed after consumer mutation: %v", err)
	}

	_, validArchive := plan.Operator()
	for name, artifacts := range map[string]struct{ manifest, values []byte }{
		"malformed projection": {manifest: []byte("metadata: ["), values: valuesAgain},
		"malformed values":     {manifest: manifestAgain, values: []byte("controllerManager: [")},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := compileOperatorDesired(validArchive, inputs, artifacts.manifest, artifacts.values); err == nil || got != nil {
				t.Fatalf("compiler returned partial/valid desired for malformed artifacts: desired=%v error=%v", got != nil, err)
			}
		})
	}
}

func TestManagementDesiredRejectsUnboundOperatorArtifacts(t *testing.T) {
	input := preflightGenerationInputs(t)
	retained, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := generation.LoadPlan(retained)
	if err != nil {
		t.Fatal(err)
	}
	input.SourceConfig = append(bytes.Clone(input.SourceConfig), []byte("# another valid generation\n")...)
	other, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		retained *bundle.Bundle
		plan     *generation.Plan
	}{
		"missing bundle": {retained: nil, plan: plan},
		"missing plan":   {retained: retained, plan: nil},
		"empty plan":     {retained: retained, plan: &generation.Plan{}},
		"other bundle":   {retained: other, plan: plan},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := prepareManagementDesiredFromPlan(test.retained, test.plan); err == nil || got != nil {
				t.Fatalf("unbound preparation: desired=%v error=%v", got != nil, err)
			}
		})
	}
}

func TestManagementDesiredRejectsRehashedOperatorArtifactsBeforeAccess(t *testing.T) {
	retained, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	desired, err := prepareManagementDesired(retained)
	if err != nil {
		t.Fatal(err)
	}
	var projectionPath string
	for _, view := range desired.plan.Stages() {
		stage := view.Stage()
		if stage.ID == generation.StageOperator && len(stage.ProjectionFiles) == 1 {
			projectionPath = stage.ProjectionFiles[0]
		}
	}
	if projectionPath == "" {
		t.Fatal("fixture has no unique operator projection")
	}
	valuesPath := desired.plan.Intent().OperatorValues.Path
	for _, name := range []string{"projection bytes", "values bytes", "projection path"} {
		t.Run(name, func(t *testing.T) {
			index, files := retained.Index(), retained.Files()
			index.BundleDigest = ""
			for i := range index.Files {
				index.Files[i].SHA256 = ""
			}
			path := projectionPath
			if name == "values bytes" {
				path = valuesPath
			}
			for i := range files {
				if files[i].Path != path {
					continue
				}
				if name == "projection path" {
					files[i].Path = "projections/renamed-operator.yaml"
				} else {
					// Even a semantically identical projection with new valid
					// hashes must match exact regenerated retained bytes.
					files[i].Data = append(files[i].Data, []byte("\n# changed retained artifact\n")...)
				}
			}
			if name == "projection path" {
				for i := range index.Files {
					if index.Files[i].Path == projectionPath {
						index.Files[i].Path = "projections/renamed-operator.yaml"
					}
				}
				for i := range index.Stages {
					if index.Stages[i].ID == generation.StageOperator {
						index.Stages[i].ProjectionFiles = []string{"projections/renamed-operator.yaml"}
					}
				}
			}
			changed, err := bundle.New(index, files)
			if err != nil {
				t.Fatalf("tamper fixture must remain structurally valid with recalculated hashes: %v", err)
			}
			if got, err := prepareManagementDesired(changed); err == nil || got != nil {
				t.Fatalf("rehashed altered artifacts accepted: desired=%v error=%v", got != nil, err)
			}
			client := managementFakeWithReadiness(t, desired.plan.Digest(), desired.manager, desired.crds)
			file := &managementMemoryFile{}
			journal := &executionJournal{file: file, digest: changed.Index().BundleDigest}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if _, err := runManagement(ctx, client, changed, journal, time.Millisecond); err == nil || len(client.events) != 0 || file.Len() != 0 {
				t.Fatalf("altered bundle reached management access/effects: error=%v calls=%v journal bytes=%d", err, client.events, file.Len())
			}
		})
	}
}

type operatorArchiveMutatingValue struct {
	archive []byte
	called  bool
}

func (v *operatorArchiveMutatingValue) MarshalYAML() (any, error) {
	v.called = true
	v.archive[0] = 0
	return 2, nil
}

func TestStandaloneOperatorSnapshotsArchiveBeforeRenderingValues(t *testing.T) {
	request := operatorTestRequest(t)
	before := bytes.Clone(request.Archive)
	mutator := &operatorArchiveMutatingValue{archive: request.Archive, called: false}
	request.Inputs.Values["controllerManager"].(map[string]any)["replicas"] = mutator
	desired, err := prepareOperatorDesired(request.Archive, request.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	if !mutator.called || bytes.Equal(request.Archive, before) {
		t.Fatal("values marshaler did not mutate the caller-owned archive")
	}
	if !bytes.Equal(desired.archive, before) {
		t.Fatal("standalone rendering and compilation did not use the same initial archive snapshot")
	}
	fingerprint, err := chartFingerprint(desired.chart)
	if err != nil || fingerprint != desired.chartFingerprint {
		t.Fatalf("standalone operator lost original chart fingerprint: %v", err)
	}
}
