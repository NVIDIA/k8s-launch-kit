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

var fixtureExecutions = struct {
	sync.Mutex
	values map[string]*managementExecution
}{values: make(map[string]*managementExecution)}

// prepareStageFixtureExecution shares only a successful immutable desired
// snapshot for an exact bundle. Its operator timeout is always one minute,
// matching the fixed software, enrollment, and tenant fixture constructors.
// Contexts, clients, live objects, credentials, receipts and journals belong to
// each fresh harness. Run composes fresh execution from this exact validated
// plan; dynamic-deadline stage wrappers keep their real constructor paths.
func prepareStageFixtureExecution(retained *bundle.Bundle) (*managementExecution, error) {
	if retained == nil {
		return loadManagementExecution(nil, time.Minute)
	}
	key := retained.Index().BundleDigest
	fixtureExecutions.Lock()
	defer fixtureExecutions.Unlock()
	if cached := fixtureExecutions.values[key]; cached != nil {
		return cached, nil
	}
	execution, err := loadManagementExecution(retained, time.Minute)
	if err == nil {
		fixtureExecutions.values[key] = execution
	}
	return execution, err
}
