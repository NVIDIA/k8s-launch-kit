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

package presets

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

func TestHasTopologyDeviation(t *testing.T) {
	tests := []struct {
		name       string
		deviations []config.PresetDeviationEntry
		want       bool
	}{
		{
			name:       "nil — exact match",
			deviations: nil,
			want:       false,
		},
		{
			name:       "empty — exact match",
			deviations: []config.PresetDeviationEntry{},
			want:       false,
		},
		{
			name:       "pfCount deviation blocks",
			deviations: []config.PresetDeviationEntry{{Field: "pfCount", Expected: "12", Got: "6"}},
			want:       true,
		},
		{
			name:       "pciAddress deviation blocks",
			deviations: []config.PresetDeviationEntry{{Field: "pciAddress", Got: "0000:05:00.0"}},
			want:       true,
		},
		{
			name:       "deviceID deviation blocks",
			deviations: []config.PresetDeviationEntry{{Field: "deviceID", Expected: "1021@0000:21:00.0", Got: "a2dc@0000:21:00.0"}},
			want:       true,
		},
		{
			name: "any topology field among several blocks",
			deviations: []config.PresetDeviationEntry{
				{Field: "partNumber"},
				{Field: "pciAddress", Got: "0000:85:00.0"},
			},
			want: true,
		},
		{
			name:       "non-topology field does not block",
			deviations: []config.PresetDeviationEntry{{Field: "partNumber", Expected: "pn-a", Got: "pn-b"}},
			want:       false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, HasTopologyDeviation(tc.deviations))
		})
	}
}

// TestValidatePreset_DeviationFieldsAreDetectable guards the contract between
// ValidatePreset (which emits deviations) and HasTopologyDeviation (which
// classifies them): every deviation ValidatePreset produces for a shape
// mismatch must be recognized as topology-blocking.
func TestValidatePreset_DeviationFieldsAreDetectable(t *testing.T) {
	preset := &Topology{
		PFs: []PresetPF{
			{PciAddress: "0000:21:00.0", DeviceID: "1021"},
			{PciAddress: "0000:21:00.1", DeviceID: "1021"},
		},
	}
	discovered := []config.PFConfig{
		// Different count, different addresses, and a device-ID clash on
		// the one shared address — exercises all three deviation fields.
		{PciAddress: "0000:21:00.0", DeviceID: "a2dc"},
	}

	deviations := ValidatePreset(preset, discovered)
	assert.NotEmpty(t, deviations)
	assert.True(t, HasTopologyDeviation(deviations),
		"shape-mismatch deviations from ValidatePreset must be topology-blocking")
}
