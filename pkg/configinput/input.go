// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

// Package configinput contains the transport-neutral representation of CLI
// values that participate in configuration resolution.
package configinput

const (
	ResolverNetworkOperatorRelease = "network-operator-release"
	ResolverSpectrumX              = "spectrum-x"
	ResolverSpectrumXConfig        = "spectrum-x-config"
)

// ValidResolver reports whether name identifies a coordinated CLI mapping.
func ValidResolver(name string) bool {
	switch name {
	case ResolverNetworkOperatorRelease, ResolverSpectrumX, ResolverSpectrumXConfig:
		return true
	default:
		return false
	}
}

// Override assigns one explicit CLI value to one YAML field path.
type Override struct {
	Flag  string
	Path  string
	Value any
}

// Request delegates a complex CLI value to a named domain resolver.
type Request struct {
	Flag  string
	Kind  string
	Value any
}

// Values contains every explicitly supplied config-backed CLI value.
type Values struct {
	Overrides []Override
	Requests  []Request
}

// Clone returns a copy whose slice-valued inputs can be mutated independently.
func (v Values) Clone() Values {
	clone := Values{
		Overrides: append([]Override(nil), v.Overrides...),
		Requests:  append([]Request(nil), v.Requests...),
	}
	for i := range clone.Overrides {
		clone.Overrides[i].Value = cloneValue(clone.Overrides[i].Value)
	}
	for i := range clone.Requests {
		clone.Requests[i].Value = cloneValue(clone.Requests[i].Value)
	}
	return clone
}

func cloneValue(value any) any {
	if values, ok := value.([]string); ok {
		clone := make([]string, len(values))
		copy(clone, values)
		return clone
	}
	return value
}

// Empty reports whether no config-backed CLI flag was supplied.
func (v Values) Empty() bool {
	return len(v.Overrides) == 0 && len(v.Requests) == 0
}
