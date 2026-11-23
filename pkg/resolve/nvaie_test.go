// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/configinput"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNVAIEReleasePresenceAndProjection(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		opts       options.Options
		wantError  string
		enabled    bool
	}{
		{name: "omitted", yaml: "{}"},
		{name: "disabled", yaml: "nvaieRelease: ''"},
		{name: "null", yaml: "nvaieRelease: null"},
		{name: "YAML selects cohort", yaml: "nvaieRelease: '9.0'", enabled: true},
		{name: "CLI selects cohort", yaml: "{}", opts: options.Options{NVAIERelease: "9.0"}, enabled: true},
		{name: "explicit matching selection", yaml: "nvaieRelease: '9.0'\nnetworkOperator: {selectedRelease: '26.7'}", enabled: true},
		{name: "YAML conflict", yaml: "nvaieRelease: '9.0'\nnetworkOperator: {selectedRelease: '26.4'}", wantError: "conflicts with explicit"},
		{name: "explicit empty NO conflicts", yaml: "nvaieRelease: '9.0'\nnetworkOperator: {selectedRelease: ''}", wantError: "conflicts with explicit"},
		{name: "CLI NO overrides YAML before checking", yaml: "nvaieRelease: '9.0'\nnetworkOperator: {selectedRelease: '26.4'}", opts: options.Options{NetworkOperatorRelease: "26.7"}, enabled: true},
		{name: "CLI conflict", yaml: "nvaieRelease: '9.0'", opts: options.Options{NetworkOperatorRelease: "26.4"}, wantError: "conflicts with explicit"},
		{name: "unreviewed", yaml: "nvaieRelease: '8.0'", wantError: "unsupported nvaieRelease"},
		{name: "CLI explicit empty rejects", yaml: "nvaieRelease: '9.0'", opts: options.Options{ConfigInputs: configinput.Values{Overrides: []configinput.Override{{Flag: "nvaie-release", Path: "nvaieRelease", Value: ""}}}}, wantError: "must not be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, err := config.DecodeInput([]byte(tc.yaml), "test.yaml")
			require.NoError(t, err)
			original, err := config.CloneConfig(input.Config)
			require.NoError(t, err)
			result, err := Resolve(Request{Input: input, Options: tc.opts, ApplyHardwareDefaults: true})
			assert.Equal(t, original, input.Config, "resolution mutated input")
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			if tc.enabled {
				assert.Equal(t, "9.0", result.Config.NVAIERelease)
				assert.Equal(t, "26.7", result.Config.NetworkOperator.SelectedRelease)
				assert.Equal(t, "v26.7.1", result.Config.NetworkOperator.Version)
				assert.Equal(t, "doca3.5.0-26.07-0.7.7.0-1", result.Config.DOCADriver.Version)
			} else {
				assert.Empty(t, result.Config.NVAIERelease)
			}
		})
	}
}

func TestNVAIEIgnoresCanonicalReleaseButRejectsResolvedDrift(t *testing.T) {
	input, err := config.DecodeInput([]byte("nvaieRelease: '9.0'"), "source")
	require.NoError(t, err)
	defaults, err := config.DefaultInput()
	require.NoError(t, err)
	defaults.Config.NetworkOperator.SelectedRelease = "26.10"
	result, err := Resolve(Request{Input: input, Defaults: defaults})
	require.NoError(t, err)
	require.Equal(t, "26.7", result.Config.NetworkOperator.SelectedRelease)
	result.Config.NetworkOperator.Version = "v26.7.2"
	require.ErrorContains(t, ValidateNVAIECohort(result.Config), "exact Network Operator/DOCA")
}
