// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package crstate

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Registry maps a GroupVersionKind to its registered Validator. A GVK
// without a registered Validator falls back to existence-only checking
// (see DefaultExistenceValidator).
type Registry struct {
	validators map[schema.GroupVersionKind]Validator
}

// NewRegistry returns an empty registry. Callers must register validators
// manually. Prefer NewDefault for the standard built-in set.
func NewRegistry() *Registry {
	return &Registry{validators: make(map[schema.GroupVersionKind]Validator)}
}

// NewDefault returns a Registry pre-populated with all built-in validators.
func NewDefault() *Registry {
	r := NewRegistry()
	registerStatusStringValidators(r)
	registerSriovValidators(r)
	registerNicConfigValidators(r)
	registerSpectrumXValidators(r)
	return r
}

// Register associates a Validator with a GVK. Re-registering overwrites
// the previous entry; this is intentional so tests can swap validators.
func (r *Registry) Register(gvk schema.GroupVersionKind, v Validator) {
	r.validators[gvk] = v
}

// Validate classifies obj's current cluster state. If no validator is
// registered for obj's GVK, DefaultExistenceValidator runs instead.
func (r *Registry) Validate(ctx context.Context, c client.Client, obj *unstructured.Unstructured) (Result, error) {
	if obj == nil {
		return Result{}, fmt.Errorf("crstate.Validate: nil object")
	}
	gvk := obj.GroupVersionKind()
	if v, ok := r.validators[gvk]; ok {
		return v(ctx, c, obj)
	}
	return DefaultExistenceValidator(ctx, c, obj)
}
