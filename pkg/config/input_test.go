// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/k8s-launch-kit/pkg/configinput"
)

func TestDecodeInputTracksPresenceAndTreatsNullAsUnset(t *testing.T) {
	input, err := DecodeInput([]byte(`profile:
  multirail: false
  spectrumX:
    numberOfPlanes: 0
networkNamespaces: []
workload: null
`), "test.yaml")
	require.NoError(t, err)

	assert.True(t, input.Present.Has("profile.multirail"))
	assert.True(t, input.Present.Has("profile.spectrumX.numberOfPlanes"))
	assert.True(t, input.Present.Has("networkNamespaces"))
	assert.False(t, input.Present.Has("workload"))
}

func TestDecodeInputTracksAliasesMergeKeysAndQuotedNull(t *testing.T) {
	input, err := DecodeInput([]byte(`driverDefaults: &driver
  enable: false
docaDriver: *driver
profile:
  <<: &profileDefaults
    multirail: false
  fabric: "null"
  ignoreARP: &disabled false
validation:
  gpuDirect:
    enabled: *disabled
`), "test.yaml")
	require.NoError(t, err)

	assert.True(t, input.Present.Has("docaDriver.enable"))
	assert.True(t, input.Present.Has("profile.multirail"))
	assert.True(t, input.Present.Has("profile.fabric"))
	assert.True(t, input.Present.Has("validation.gpuDirect.enabled"))
}

func TestDecodeInputMergePresenceMatchesShallowYAMLReplacement(t *testing.T) {
	for _, replacement := range []string{"{version: custom}", "{}", "null"} {
		t.Run(replacement, func(t *testing.T) {
			input, err := DecodeInput([]byte("base: &base\n  docaDriver:\n    enable: false\n<<: *base\ndocaDriver: "+replacement+"\n"), "merge.yaml")
			require.NoError(t, err)
			assert.False(t, input.Present.Has("docaDriver.enable"))
			require.NoError(t, MergeDefaults(input.Config, &LaunchKitConfig{DOCADriver: &DOCADriverConfig{Enable: true}}, input.Present))
			assert.True(t, input.Config.DOCADriver.Enable)
		})
	}

	input, err := DecodeInput([]byte(`first: &first
  docaDriver: {version: custom}
second: &second
  docaDriver: {enable: false}
<<: [*first, *second]
`), "sequence.yaml")
	require.NoError(t, err)
	assert.True(t, input.Present.Has("docaDriver.version"))
	assert.False(t, input.Present.Has("docaDriver.enable"))
}

func TestDecodeInputRejectsCyclicUnknownAlias(t *testing.T) {
	_, err := DecodeInput([]byte("extension: &loop {self: *loop}\n"), "cycle.yaml")
	require.Error(t, err)
}

func TestMergeDefaultsMergesProfileFieldsDespiteCustomYAMLMarshaling(t *testing.T) {
	input, err := DecodeInput([]byte(`profile:
  fabric: infiniband
`), "test.yaml")
	require.NoError(t, err)
	defaults := &LaunchKitConfig{Profile: &Profile{
		Fabric:     "ethernet",
		Deployment: "sriov",
		Multirail:  true,
		Routing:    RoutingDestinationBased,
		SpectrumX:  &ProfileSpectrumX{MultiplaneMode: "none"},
	}}

	require.NoError(t, MergeDefaults(input.Config, defaults, input.Present))
	assert.Equal(t, "infiniband", input.Config.Profile.Fabric)
	assert.Equal(t, "sriov", input.Config.Profile.Deployment)
	assert.True(t, input.Config.Profile.Multirail)
	assert.Equal(t, RoutingDestinationBased, input.Config.Profile.Routing)
	require.NotNil(t, input.Config.Profile.SpectrumX)
	assert.Equal(t, "none", input.Config.Profile.SpectrumX.MultiplaneMode)
}

func TestMergeDefaultsPreservesExplicitFalseZeroAndEmpty(t *testing.T) {
	input, err := DecodeInput([]byte(`profile:
  multirail: false
  spectrumX:
    numberOfPlanes: 0
networkNamespaces: []
`), "test.yaml")
	require.NoError(t, err)
	defaults := &LaunchKitConfig{
		Profile: &Profile{
			Multirail: true,
			SpectrumX: &ProfileSpectrumX{NumberOfPlanes: 4},
		},
		NetworkNamespaces: []string{"default"},
		ClusterConfig:     []ClusterConfig{{Identifier: "must-not-default"}},
	}

	require.NoError(t, MergeDefaults(input.Config, defaults, input.Present))
	assert.False(t, input.Config.Profile.Multirail)
	assert.Zero(t, input.Config.Profile.SpectrumX.NumberOfPlanes)
	assert.Empty(t, input.Config.NetworkNamespaces)
	assert.Empty(t, input.Config.ClusterConfig)
}

func TestMergeDefaultsTreatsNullAsUnset(t *testing.T) {
	input, err := DecodeInput([]byte(`profile:
  fabric: null
networkNamespaces: null
`), "test.yaml")
	require.NoError(t, err)
	defaults := &LaunchKitConfig{
		Profile:           &Profile{Fabric: "ethernet"},
		NetworkNamespaces: []string{"default"},
	}

	require.NoError(t, MergeDefaults(input.Config, defaults, input.Present))
	assert.Equal(t, "ethernet", input.Config.Profile.Fabric)
	assert.Equal(t, []string{"default"}, input.Config.NetworkNamespaces)
}

func TestMergeDefaultsTreatsYAMLScalarStructAsLeaf(t *testing.T) {
	input, err := DecodeInput([]byte(`maintenance:
  maxUnavailable: 0
`), "test.yaml")
	require.NoError(t, err)
	defaults := &LaunchKitConfig{Maintenance: DefaultMaintenanceConfig()}

	require.NoError(t, MergeDefaults(input.Config, defaults, input.Present))
	assert.Equal(t, "0", input.Config.Maintenance.MaxUnavailable.String())
}

func TestApplyOverridesAllocatesParentsAndMarksBooleanPresence(t *testing.T) {
	cfg := &LaunchKitConfig{}
	err := ApplyOverrides(cfg, []configinput.Override{
		{Flag: "fabric", Path: "profile.fabric", Value: "ethernet"},
		{Flag: "multirail", Path: "profile.multirail", Value: false},
		{Flag: "number-of-planes", Path: "profile.spectrumX.numberOfPlanes", Value: 0},
	})
	require.NoError(t, err)
	require.NotNil(t, cfg.Profile)
	assert.Equal(t, "ethernet", cfg.Profile.Fabric)
	assert.False(t, cfg.Profile.Multirail)
	assert.True(t, cfg.Profile.MultirailSet)
	require.NotNil(t, cfg.Profile.SpectrumX)
	assert.Zero(t, cfg.Profile.SpectrumX.NumberOfPlanes)
}

func TestGeneratedDefaultsMatchCanonicalRootFile(t *testing.T) {
	root, err := os.ReadFile("../../cluster-config.yaml")
	require.NoError(t, err)
	assert.Equal(t, root, DefaultConfigYAML(), "run go generate ./pkg/config after editing cluster-config.yaml")
}

func TestLoadInputWithoutPathReturnsEmptyUserLayer(t *testing.T) {
	input, err := LoadInput("", logr.Discard())
	require.NoError(t, err)

	assert.Empty(t, input.Present)
	assert.Nil(t, input.Config.Profile)
	assert.Nil(t, input.Config.NetworkOperator)
}
