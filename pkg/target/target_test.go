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

package target

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDriver struct {
	descriptor Descriptor
	bind       func(Invocation) (Operation, error)
}

func (f fakeDriver) Descriptor() Descriptor { return f.descriptor }

func (f fakeDriver) Bind(invocation Invocation) (Operation, error) {
	if f.bind == nil {
		return OperationFunc(func(context.Context) error { return nil }), nil
	}
	return f.bind(invocation)
}

func availableDescriptor(name Name, phases ...Phase) Descriptor {
	capabilities := make(map[Phase]Capability, len(PublicPhases()))
	for _, phase := range PublicPhases() {
		capabilities[phase] = Capability{Available: false, Reason: "not implemented in test driver"}
	}
	for _, phase := range phases {
		capabilities[phase] = Capability{Available: true}
	}
	return Descriptor{Name: name, Description: string(name), Phases: capabilities}
}

func TestParseName(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		expected  Name
		wantError string
	}{
		{name: "omitted defaults to host", value: "", expected: Host},
		{name: "host", value: "host", expected: Host},
		{name: "dpf", value: "dpf", expected: DPF},
		{name: "future target syntax", value: "test-target2", expected: "test-target2"},
		{name: "uppercase rejected", value: "HOST", wantError: "invalid target"},
		{name: "surrounding whitespace rejected", value: " host", wantError: "invalid target"},
		{name: "punctuation rejected", value: "dpf/cluster", wantError: "invalid target"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual, err := ParseName(tt.value)
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				assert.Empty(t, actual)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, actual)
		})
	}
}

func TestPublicPhasesReturnsStableCopy(t *testing.T) {
	first := PublicPhases()
	require.Equal(t, []Phase{Discover, Generate, Deploy, Validate}, first)
	first[0] = Pipeline
	assert.Equal(t, Discover, PublicPhases()[0])
}

func TestDescriptorMissingCapabilityIsUnavailable(t *testing.T) {
	descriptor := Descriptor{
		Name: Host,
		Phases: map[Phase]Capability{
			Discover: {Available: true},
		},
	}
	assert.True(t, descriptor.Capability(Discover).Available)
	capability := descriptor.Capability(Generate)
	assert.False(t, capability.Available)
	assert.NotEmpty(t, capability.Reason)
}

func TestNewRegistryRejectsInvalidDrivers(t *testing.T) {
	tests := []struct {
		name      string
		drivers   []Driver
		wantError string
	}{
		{name: "no drivers", wantError: "must contain at least one driver"},
		{name: "nil driver", drivers: []Driver{nil}, wantError: "must not be nil"},
		{
			name:      "empty name",
			drivers:   []Driver{fakeDriver{descriptor: Descriptor{Phases: map[Phase]Capability{}}}},
			wantError: "name must not be empty",
		},
		{
			name:      "invalid name",
			drivers:   []Driver{fakeDriver{descriptor: Descriptor{Name: "DPF", Phases: map[Phase]Capability{}}}},
			wantError: "invalid target descriptor name",
		},
		{
			name:      "nil phases",
			drivers:   []Driver{fakeDriver{descriptor: Descriptor{Name: Host}}},
			wantError: "must declare phase capabilities",
		},
		{
			name: "missing public phase",
			drivers: []Driver{fakeDriver{descriptor: Descriptor{
				Name: Host,
				Phases: map[Phase]Capability{
					Discover: {Available: true},
					Generate: {Available: true},
					Deploy:   {Available: true},
				},
			}}},
			wantError: `must declare capability for phase "validate"`,
		},
		{
			name: "unknown phase",
			drivers: []Driver{fakeDriver{descriptor: func() Descriptor {
				descriptor := availableDescriptor(Host)
				descriptor.Phases["upgrade"] = Capability{Available: true}
				return descriptor
			}()}},
			wantError: "unknown phase",
		},
		{
			name: "unavailable without reason",
			drivers: []Driver{fakeDriver{descriptor: func() Descriptor {
				descriptor := availableDescriptor(Host)
				descriptor.Phases[Discover] = Capability{Available: false}
				return descriptor
			}()}},
			wantError: "unavailable without a reason",
		},
		{
			name: "duplicate",
			drivers: []Driver{
				fakeDriver{descriptor: availableDescriptor(Host, Discover)},
				fakeDriver{descriptor: availableDescriptor(Host, Generate)},
			},
			wantError: "registered more than once",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry, err := NewRegistry(tt.drivers...)
			require.ErrorContains(t, err, tt.wantError)
			assert.Nil(t, registry)
		})
	}
}

func TestRegistryNamesAreSortedAndDefensive(t *testing.T) {
	registry, err := NewRegistry(
		fakeDriver{descriptor: availableDescriptor("zeta", Discover)},
		fakeDriver{descriptor: availableDescriptor(Host, Discover)},
		fakeDriver{descriptor: availableDescriptor(DPF, Discover)},
	)
	require.NoError(t, err)

	names := registry.Names()
	require.Equal(t, []Name{DPF, Host, "zeta"}, names)
	names[0] = "changed"
	assert.Equal(t, []Name{DPF, Host, "zeta"}, registry.Names())
	assert.Nil(t, (*Registry)(nil).Names())
}

func TestRegistryBindAndRun(t *testing.T) {
	var captured Invocation
	var ran bool
	driver := fakeDriver{
		descriptor: availableDescriptor(Host, Generate),
		bind: func(invocation Invocation) (Operation, error) {
			captured = invocation
			return OperationFunc(func(context.Context) error {
				ran = true
				return nil
			}), nil
		},
	}
	registry, err := NewRegistry(driver)
	require.NoError(t, err)

	invocation := Invocation{
		Target: Host,
		Phase:  Generate,
		Output: OutputOptions{
			Format:      "json",
			Quiet:       true,
			AutoApprove: true,
		},
		Execution: ExecutionOptions{DryRun: true, Timeout: time.Minute},
	}
	operation, err := registry.Bind(invocation)
	require.NoError(t, err)
	assert.Equal(t, invocation, captured)
	require.NoError(t, operation.Run(context.Background()))
	assert.True(t, ran)
}

func TestRegistryBindErrors(t *testing.T) {
	bindErr := errors.New("required host argument missing")
	driver := fakeDriver{
		descriptor: Descriptor{
			Name: Host,
			Phases: map[Phase]Capability{
				Discover: {Available: true},
				Generate: {Available: false, Reason: "not built"},
				Deploy:   {Available: true},
				Validate: {Available: true},
			},
		},
		bind: func(invocation Invocation) (Operation, error) {
			switch invocation.Phase {
			case Discover:
				return nil, bindErr
			case Deploy:
				return nil, nil
			default:
				return OperationFunc(func(context.Context) error { return nil }), nil
			}
		},
	}
	registry, err := NewRegistry(driver)
	require.NoError(t, err)

	t.Run("nil registry", func(t *testing.T) {
		operation, bindErr := (*Registry)(nil).Bind(Invocation{Target: Host, Phase: Discover})
		require.ErrorContains(t, bindErr, "must not be nil")
		assert.Nil(t, operation)
	})

	t.Run("invalid invocation", func(t *testing.T) {
		operation, bindErr := registry.Bind(Invocation{Target: Host, Phase: Discover, Execution: ExecutionOptions{Timeout: -1}})
		require.ErrorContains(t, bindErr, "must not be negative")
		assert.Nil(t, operation)
	})

	t.Run("empty invocation target", func(t *testing.T) {
		operation, bindErr := registry.Bind(Invocation{Phase: Discover})
		require.ErrorContains(t, bindErr, "target must not be empty")
		assert.Nil(t, operation)
	})

	t.Run("invalid target syntax", func(t *testing.T) {
		operation, bindErr := registry.Bind(Invocation{Target: "HOST", Phase: Discover})
		require.ErrorContains(t, bindErr, "invalid target")
		assert.Nil(t, operation)
	})

	t.Run("unknown phase", func(t *testing.T) {
		operation, bindErr := registry.Bind(Invocation{Target: Host, Phase: "upgrade"})
		require.ErrorContains(t, bindErr, "unknown phase")
		assert.Nil(t, operation)
	})

	t.Run("unknown target", func(t *testing.T) {
		operation, bindErr := registry.Bind(Invocation{Target: DPF, Phase: Discover})
		var unknown *UnknownTargetError
		require.ErrorAs(t, bindErr, &unknown)
		assert.Equal(t, DPF, unknown.Name)
		assert.Equal(t, []Name{Host}, unknown.Supported)
		assert.Nil(t, operation)
	})

	t.Run("unavailable phase", func(t *testing.T) {
		operation, bindErr := registry.Bind(Invocation{Target: Host, Phase: Generate})
		var unavailable *PhaseUnavailableError
		require.ErrorAs(t, bindErr, &unavailable)
		assert.Equal(t, "not built", unavailable.Reason)
		assert.Nil(t, operation)
	})

	t.Run("binder validation", func(t *testing.T) {
		operation, actualErr := registry.Bind(Invocation{Target: Host, Phase: Discover})
		require.ErrorIs(t, actualErr, bindErr)
		assert.Nil(t, operation)
	})

	t.Run("nil operation", func(t *testing.T) {
		operation, bindErr := registry.Bind(Invocation{Target: Host, Phase: Deploy})
		require.ErrorContains(t, bindErr, "returned a nil operation")
		assert.Nil(t, operation)
	})
}

func TestNilOperationFuncReturnsError(t *testing.T) {
	var operation OperationFunc
	require.ErrorContains(t, operation.Run(context.Background()), "must not be nil")
}

func TestTargetErrors(t *testing.T) {
	assert.Equal(t,
		`unknown target "future"; registered targets: [dpf host]`,
		(&UnknownTargetError{Name: "future", Supported: []Name{DPF, Host}}).Error(),
	)
	assert.Equal(t,
		`target "dpf" does not implement phase "discover"`,
		(&PhaseUnavailableError{Name: DPF, Phase: Discover}).Error(),
	)
	assert.Equal(t,
		`target "dpf" does not implement phase "discover": not built`,
		(&PhaseUnavailableError{Name: DPF, Phase: Discover, Reason: "not built"}).Error(),
	)
}
