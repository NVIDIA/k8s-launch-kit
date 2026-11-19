// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"
	"slices"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// managementDesired owns the retained read-only desired snapshot, shared by
// deployment preparation and fresh observation. All fields are private and
// immutable after construction. Readers pass defensive object copies to clients.
// There is no Reconciler, receipt, journal, preflight or installation plan here.
type managementDesired struct {
	retained          *bundle.Bundle
	plan              *generation.Plan
	operator          *operatorDesired
	operatorVersion   string
	rules             *controllercontract.Rules
	verifier          *desiredVerifier
	namespaces        []*unstructured.Unstructured
	operatorNamespace string
	configObject      *unstructured.Unstructured
	manager           *unstructured.Unstructured
	crds              []operatorCRDExpectation
}

// prepareManagementDesired validates and compiles retained bytes only. It
// requires neither access credentials nor an execution deadline and cannot
// acquire API, host, filesystem-operation or installation authority.
func prepareManagementDesired(retained *bundle.Bundle) (*managementDesired, error) {
	plan, err := generation.LoadPlan(retained)
	if err != nil {
		return nil, fmt.Errorf("validate retained DPF plan before management access: %w", err)
	}
	return prepareManagementDesiredFromPlan(retained, plan)
}

// prepareManagementDesiredFromPlan reuses the caller-isolated validated snapshot.
func prepareManagementDesiredFromPlan(retained *bundle.Bundle, plan *generation.Plan) (*managementDesired, error) {
	if retained == nil || plan == nil || plan.Digest() == "" || plan.Digest() != retained.Index().BundleDigest {
		return nil, fmt.Errorf("validated plan does not identify the retained bundle")
	}
	// Snapshot exported outer wrappers before retaining them. Callers must not
	// mutate an argument concurrently with handoff; subsequent replacement is isolated.
	snapshot := *plan
	plan = &snapshot
	retained = plan.Bundle()
	if retained == nil || plan.ObjectSchemas() == nil {
		return nil, fmt.Errorf("validated plan has no retained bundle or schemas")
	}
	inputs, archive := plan.Operator()
	manifest, values, err := operatorArtifactsFromValidatedPlan(retained, plan)
	if err != nil {
		return nil, fmt.Errorf("prepare retained operator projection: %w", err)
	}
	operator, err := compileOperatorDesired(archive, inputs, manifest, values)
	if err != nil {
		return nil, fmt.Errorf("prepare retained operator projection: %w", err)
	}
	manager, crds, err := operatorDesiredReadinessObjects(operator)
	if err != nil {
		return nil, err
	}
	schemas := plan.ObjectSchemas()
	index := retained.Index()
	rules, err := controllercontract.Select(index.Release, index.Profile, index.Renderer, schemas)
	if err != nil {
		return nil, fmt.Errorf("select retained controller contract before management access: %w", err)
	}
	metadata, err := controllerMetadataForPlan(rules, plan)
	if err != nil {
		return nil, fmt.Errorf("prepare retained controller metadata: %w", err)
	}
	verifier, err := newDesiredVerifier(rules, plan.Digest(), metadata)
	if err != nil {
		return nil, fmt.Errorf("prepare retained DPF resource verifier: %w", err)
	}
	namespaces, namespace, configObject, err := managementBootstrapObjects(retained, inputs)
	if err != nil {
		return nil, err
	}
	return &managementDesired{retained: retained, plan: plan, operator: operator,
		operatorVersion: "v" + inputs.ReleaseVersion, rules: rules, verifier: verifier,
		namespaces: namespaces, operatorNamespace: namespace, configObject: configObject, manager: manager, crds: crds}, nil
}

// operatorArtifactsFromValidatedPlan reads raw artifacts only after the caller
// binds this immutable bundle to a plan that passed full regeneration. Stage
// projections are not apply documents and must not be serialized from objects.
func operatorArtifactsFromValidatedPlan(retained *bundle.Bundle, plan *generation.Plan) ([]byte, []byte, error) {
	valuesPath := plan.Intent().OperatorValues.Path
	var projectionPath string
	for _, view := range plan.Stages() {
		stage := view.Stage()
		if stage.ID != generation.StageOperator {
			continue
		}
		if len(stage.ProjectionFiles) != 1 || !slices.Contains(stage.Files, valuesPath) {
			return nil, nil, fmt.Errorf("validated operator stage must identify one projection and its retained values")
		}
		projectionPath = stage.ProjectionFiles[0]
		break
	}
	if projectionPath == "" || valuesPath == "" {
		return nil, nil, fmt.Errorf("validated operator projection or values path is unavailable")
	}
	var manifest, values []byte
	for _, file := range retained.Files() {
		switch file.Path {
		case projectionPath:
			manifest = file.Data
		case valuesPath:
			values = file.Data
		}
	}
	if len(manifest) == 0 || len(values) == 0 {
		return nil, nil, fmt.Errorf("validated operator projection %q or values %q is unavailable", projectionPath, valuesPath)
	}
	return manifest, values, nil
}
