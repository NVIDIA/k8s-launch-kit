// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"

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
	inputs, archive := plan.Operator()
	operator, err := prepareOperatorDesired(archive, inputs)
	if err != nil {
		return nil, fmt.Errorf("prepare retained operator projection: %w", err)
	}
	manager, crds, err := operatorDesiredReadinessObjects(operator)
	if err != nil {
		return nil, err
	}
	schemas, err := generation.NewObjectSchemas(operator.renderedManifest)
	if err != nil {
		return nil, fmt.Errorf("prepare retained DPF resource verifier: compile retained operator CRD schemas: %w", err)
	}
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
