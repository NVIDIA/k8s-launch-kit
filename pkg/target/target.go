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

// Package target defines the target-neutral lifecycle boundary used by the
// Launch Kit CLI. It deliberately contains no Cobra, host configuration, or
// DPF configuration types. Target-specific CLI adapters bind their typed
// arguments into an Operation before the common runtime executes it.
package target

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Name identifies a managed Launch Kit domain.
type Name string

const (
	// Host is the existing Network Operator workflow and remains the default.
	Host Name = "host"
	// DPF is the DPU-plane workflow. The name is reserved before the driver is
	// implemented so CLI and automation contracts can evolve additively.
	DPF Name = "dpf"
)

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ParseName parses a public target name. An omitted value resolves to Host for
// backwards compatibility. Registry lookup, rather than this parser, decides
// whether a syntactically valid target is installed.
func ParseName(value string) (Name, error) {
	if value == "" {
		return Host, nil
	}
	if strings.TrimSpace(value) != value || !validName.MatchString(value) {
		return "", fmt.Errorf("invalid target %q: use a lowercase name containing letters, digits, or hyphens", value)
	}
	return Name(value), nil
}

// Phase identifies one lifecycle operation. Pipeline is an internal composite
// used by the backwards-compatible root command; the public target contract is
// Discover, Generate, Deploy, and Validate.
type Phase string

const (
	Discover Phase = "discover"
	Generate Phase = "generate"
	Deploy   Phase = "deploy"
	Validate Phase = "validate"
	Pipeline Phase = "pipeline"
)

var knownPhases = map[Phase]struct{}{
	Discover: {},
	Generate: {},
	Deploy:   {},
	Validate: {},
	Pipeline: {},
}

// PublicPhases returns the stable four-phase lifecycle in execution order.
func PublicPhases() []Phase {
	return []Phase{Discover, Generate, Deploy, Validate}
}

// Capability describes whether a target implements a phase in this build.
type Capability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// Descriptor describes one target without importing its implementation.
type Descriptor struct {
	Name        Name                 `json:"name"`
	Description string               `json:"description"`
	Phases      map[Phase]Capability `json:"phases"`
}

// Capability returns the declared capability for phase. Missing phase entries
// are treated as unavailable, never as implicit no-op support.
func (d Descriptor) Capability(phase Phase) Capability {
	capability, ok := d.Phases[phase]
	if !ok {
		return Capability{Available: false, Reason: "phase is not declared by the target"}
	}
	return capability
}

func (d Descriptor) validate() error {
	if d.Name == "" {
		return fmt.Errorf("target descriptor name must not be empty")
	}
	if _, err := ParseName(string(d.Name)); err != nil {
		return fmt.Errorf("invalid target descriptor name: %w", err)
	}
	if d.Phases == nil {
		return fmt.Errorf("target %q must declare phase capabilities", d.Name)
	}
	for phase, capability := range d.Phases {
		if _, ok := knownPhases[phase]; !ok {
			return fmt.Errorf("target %q declares unknown phase %q", d.Name, phase)
		}
		if !capability.Available && strings.TrimSpace(capability.Reason) == "" {
			return fmt.Errorf("target %q phase %q is unavailable without a reason", d.Name, phase)
		}
	}
	for _, phase := range PublicPhases() {
		if _, ok := d.Phases[phase]; !ok {
			return fmt.Errorf("target %q must declare capability for phase %q", d.Name, phase)
		}
	}
	return nil
}

// OutputOptions are target-neutral presentation controls.
type OutputOptions struct {
	Format      string
	Quiet       bool
	AutoApprove bool
}

// ExecutionOptions are target-neutral phase execution controls.
type ExecutionOptions struct {
	DryRun  bool
	Timeout time.Duration
}

// Invocation is the target-neutral input presented to a CLI adapter. Config,
// artifact, and cluster-context arguments stay target-owned until their exact
// semantics are shared by every driver.
type Invocation struct {
	Target    Name
	Phase     Phase
	Output    OutputOptions
	Execution ExecutionOptions
}

func (i Invocation) validate() error {
	if i.Target == "" {
		return fmt.Errorf("target must not be empty")
	}
	if _, err := ParseName(string(i.Target)); err != nil {
		return err
	}
	if _, ok := knownPhases[i.Phase]; !ok {
		return fmt.Errorf("unknown phase %q", i.Phase)
	}
	if i.Execution.Timeout < 0 {
		return fmt.Errorf("phase timeout must not be negative")
	}
	return nil
}

// Operation is a target-specific, fully bound phase request. Implementations
// capture typed target requests; the runtime never receives a union options
// structure or Cobra flag set.
type Operation interface {
	Run(context.Context) error
}

// OperationFunc adapts a function to Operation.
type OperationFunc func(context.Context) error

// Run implements Operation.
func (f OperationFunc) Run(ctx context.Context) error {
	if f == nil {
		return fmt.Errorf("target operation must not be nil")
	}
	return f(ctx)
}

// Driver is the CLI-facing target adapter. Bind validates target-specific
// requirements, merges target configuration and explicit CLI overrides, and
// returns an operation that invokes a typed domain driver.
type Driver interface {
	Descriptor() Descriptor
	Bind(Invocation) (Operation, error)
}

// Registry resolves a target name to its CLI adapter.
type Registry struct {
	drivers map[Name]Driver
}

// NewRegistry creates a deterministic target registry. Invalid descriptors,
// nil drivers, and duplicate target names fail at construction time.
func NewRegistry(drivers ...Driver) (*Registry, error) {
	if len(drivers) == 0 {
		return nil, fmt.Errorf("target registry must contain at least one driver")
	}
	registry := &Registry{drivers: make(map[Name]Driver, len(drivers))}
	for index, driver := range drivers {
		if driver == nil {
			return nil, fmt.Errorf("target driver at index %d must not be nil", index)
		}
		descriptor := driver.Descriptor()
		if err := descriptor.validate(); err != nil {
			return nil, err
		}
		if _, exists := registry.drivers[descriptor.Name]; exists {
			return nil, fmt.Errorf("target %q is registered more than once", descriptor.Name)
		}
		registry.drivers[descriptor.Name] = driver
	}
	return registry, nil
}

// Names returns registered target names in stable order.
func (r *Registry) Names() []Name {
	if r == nil {
		return nil
	}
	names := make([]Name, 0, len(r.drivers))
	for name := range r.drivers {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

// Bind resolves and validates an invocation before asking the selected target
// adapter to construct its typed operation.
func (r *Registry) Bind(invocation Invocation) (Operation, error) {
	if r == nil {
		return nil, fmt.Errorf("target registry must not be nil")
	}
	if err := invocation.validate(); err != nil {
		return nil, err
	}
	driver, ok := r.drivers[invocation.Target]
	if !ok {
		return nil, &UnknownTargetError{Name: invocation.Target, Supported: r.Names()}
	}
	descriptor := driver.Descriptor()
	capability := descriptor.Capability(invocation.Phase)
	if !capability.Available {
		return nil, &PhaseUnavailableError{
			Name:   invocation.Target,
			Phase:  invocation.Phase,
			Reason: capability.Reason,
		}
	}
	operation, err := driver.Bind(invocation)
	if err != nil {
		return nil, err
	}
	if operation == nil {
		return nil, fmt.Errorf("target %q returned a nil operation for phase %q", invocation.Target, invocation.Phase)
	}
	return operation, nil
}

// UnknownTargetError reports a syntactically valid target that has no
// registered adapter.
type UnknownTargetError struct {
	Name      Name
	Supported []Name
}

func (e *UnknownTargetError) Error() string {
	return fmt.Sprintf("unknown target %q; registered targets: %v", e.Name, e.Supported)
}

// PhaseUnavailableError reports a known target whose selected phase is not
// implemented in this build.
type PhaseUnavailableError struct {
	Name   Name
	Phase  Phase
	Reason string
}

func (e *PhaseUnavailableError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("target %q does not implement phase %q", e.Name, e.Phase)
	}
	return fmt.Sprintf("target %q does not implement phase %q: %s", e.Name, e.Phase, e.Reason)
}
