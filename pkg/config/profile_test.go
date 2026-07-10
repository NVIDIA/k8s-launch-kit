// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

func TestProfileUnmarshalTracksMultirailPresence(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		wantValue bool
		wantSet   bool
	}{
		{
			name: "missing",
			yaml: `profile:
  fabric: ethernet
`,
			wantValue: false,
			wantSet:   false,
		},
		{
			name: "explicit false",
			yaml: `profile:
  multirail: false
`,
			wantValue: false,
			wantSet:   true,
		},
		{
			name: "explicit true",
			yaml: `profile:
  multirail: true
`,
			wantValue: true,
			wantSet:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg LaunchKitConfig
			require.NoError(t, yaml.Unmarshal([]byte(tt.yaml), &cfg))
			require.NotNil(t, cfg.Profile)
			assert.Equal(t, tt.wantValue, cfg.Profile.Multirail)
			assert.Equal(t, tt.wantSet, cfg.Profile.MultirailSet)
		})
	}
}

func TestProfileUnmarshalTracksIgnoreARPPresence(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		wantValue bool
		wantSet   bool
	}{
		{
			name: "missing",
			yaml: `profile:
  fabric: ethernet
`,
			wantValue: false,
			wantSet:   false,
		},
		{
			name: "explicit false",
			yaml: `profile:
  ignoreARP: false
`,
			wantValue: false,
			wantSet:   true,
		},
		{
			name: "explicit true",
			yaml: `profile:
  ignoreARP: true
`,
			wantValue: true,
			wantSet:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg LaunchKitConfig
			require.NoError(t, yaml.Unmarshal([]byte(tt.yaml), &cfg))
			require.NotNil(t, cfg.Profile)
			assert.Equal(t, tt.wantValue, cfg.Profile.IgnoreARP)
			assert.Equal(t, tt.wantSet, cfg.Profile.IgnoreARPSet)
		})
	}
}

func TestProfileMultirailPresenceIsNotSerialized(t *testing.T) {
	cfg := LaunchKitConfig{
		Profile: &Profile{
			Fabric:       "ethernet",
			Deployment:   "sriov",
			Multirail:    false,
			MultirailSet: true,
		},
	}

	data, err := yaml.Marshal(&cfg)
	require.NoError(t, err)
	assert.Contains(t, string(data), "multirail: false")
	assert.NotContains(t, string(data), "multirailset")
	assert.NotContains(t, string(data), "multirailSet")
}

func TestProfileIgnoreARPPresenceControlsSerialization(t *testing.T) {
	tests := []struct {
		name         string
		profile      *Profile
		wantContains string
		wantOmit     bool
	}{
		{
			name:         "missing false omitted",
			profile:      &Profile{Fabric: "ethernet", Deployment: "sriov"},
			wantContains: "ignoreARP:",
			wantOmit:     true,
		},
		{
			name:         "explicit false preserved",
			profile:      &Profile{Fabric: "ethernet", Deployment: "sriov", IgnoreARPSet: true},
			wantContains: "ignoreARP: false",
		},
		{
			name:         "true serialized",
			profile:      &Profile{Fabric: "ethernet", Deployment: "sriov", IgnoreARP: true},
			wantContains: "ignoreARP: true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := yaml.Marshal(&LaunchKitConfig{Profile: tt.profile})
			require.NoError(t, err)
			if tt.wantOmit {
				assert.NotContains(t, string(data), tt.wantContains)
				return
			}
			assert.Contains(t, string(data), tt.wantContains)
		})
	}
}
