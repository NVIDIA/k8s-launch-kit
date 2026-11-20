// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
)

var fixtureBundles = struct {
	sync.Mutex
	values map[[32]byte]*bundle.Bundle
}{values: make(map[[32]byte]*bundle.Bundle)}

// generateFixtureBundle avoids repeatedly rendering identical, valid test
// fixture inputs across lifecycle failure cases. Bundle is immutable: its
// accessors return copies. Tests construct fresh clients and journals; only the
// fixed stage fixtures also reuse their immutable prepared desired execution.
// Run callers explicitly obtain a production LoadPlan token before execution.
// Tests of generation or invalid input types must call generation.Generate
// directly; this cache identifies fixture inputs by their serialized values.
func generateFixtureBundle(input generation.Inputs) (*bundle.Bundle, error) {
	serialized, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	key := sha256.Sum256(serialized)
	fixtureBundles.Lock()
	defer fixtureBundles.Unlock()
	if cached := fixtureBundles.values[key]; cached != nil {
		return cached, nil
	}
	generated, err := generation.Generate(input)
	if err == nil {
		fixtureBundles.values[key] = generated
	}
	return generated, err
}

var fixturePreparations = struct {
	sync.Mutex
	values map[string]*fixturePreparation
}{values: make(map[string]*fixturePreparation)}

// Only fixed, valid fixture inputs use this registry. Constructor, tamper and
// authority tests continue to use the production constructors directly. The
// validation projection is compiled separately from the executable projection:
// in particular, it never borrows the chart handed to Helm installation.
type fixturePreparation struct {
	plan              *generation.Plan
	execution         *managementExecution
	validationDesired *managementDesired
}

// The caller holds fixturePreparations. Publish only successfully validated plans.
func fixturePreparationLocked(retained *bundle.Bundle) (*fixturePreparation, error) {
	if retained != nil {
		if cached := fixturePreparations.values[retained.Index().BundleDigest]; cached != nil {
			return cached, nil
		}
	}
	plan, err := generation.LoadPlan(retained)
	if err != nil {
		return nil, err
	}
	prepared := &fixturePreparation{plan: plan}
	fixturePreparations.values[plan.Digest()] = prepared
	return prepared, nil
}

// prepareFixturePlan returns an isolated outer wrapper. Its private retained
// contents are immutable and their public accessors return copies.
func prepareFixturePlan(retained *bundle.Bundle) (*generation.Plan, error) {
	fixturePreparations.Lock()
	defer fixturePreparations.Unlock()
	prepared, err := fixturePreparationLocked(retained)
	if err != nil {
		return nil, err
	}
	plan := *prepared.plan
	return &plan, nil
}

// prepareValidationFixtureDesired is for read-only observation fixtures. A test
// that mutates desired objects must instead compile a fresh projection using
// prepareManagementDesiredFromPlan and prepareFixturePlan.
func prepareValidationFixtureDesired(retained *bundle.Bundle) (*managementDesired, error) {
	fixturePreparations.Lock()
	defer fixturePreparations.Unlock()
	prepared, err := fixturePreparationLocked(retained)
	if err != nil {
		return nil, err
	}
	if prepared.validationDesired == nil {
		desired, err := prepareManagementDesiredFromPlan(prepared.plan.Bundle(), prepared.plan)
		if err != nil {
			return nil, err
		}
		prepared.validationDesired = desired
	}
	return prepared.validationDesired, nil
}

// prepareStageFixtureExecution shares only a successful immutable desired
// snapshot for an exact bundle. Its operator timeout is always one minute,
// matching the fixed software, enrollment, and tenant fixture constructors.
// Contexts, clients, live objects, credentials, receipts and journals belong to
// each fresh harness. Run composes fresh execution from this exact validated
// plan; dynamic-deadline stage wrappers keep their real constructor paths.
func prepareStageFixtureExecution(retained *bundle.Bundle) (*managementExecution, error) {
	fixturePreparations.Lock()
	defer fixturePreparations.Unlock()
	prepared, err := fixturePreparationLocked(retained)
	if err != nil {
		return nil, err
	}
	if prepared.execution == nil {
		execution, err := prepareManagementExecution(prepared.plan, time.Minute)
		if err != nil {
			return nil, err
		}
		prepared.execution = execution
	}
	return prepared.execution, nil
}
