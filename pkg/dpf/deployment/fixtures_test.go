// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"crypto/sha256"
	"encoding/json"
	"sync"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
)

var fixtureBundles = struct {
	sync.Mutex
	values map[[32]byte]*bundle.Bundle
}{values: make(map[[32]byte]*bundle.Bundle)}

// generateFixtureBundle avoids repeatedly rendering identical, valid test
// fixture inputs across lifecycle failure cases. Bundle is immutable: its
// accessors return copies. Each test still constructs fresh clients, executions
// and journals, and production LoadPlan validation remains fully exercised.
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
