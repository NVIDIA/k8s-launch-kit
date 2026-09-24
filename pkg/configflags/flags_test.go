// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package configflags

import (
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/k8s-launch-kit/pkg/configinput"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
)

func TestBindAndCollectPreserveExplicitZeroValues(t *testing.T) {
	var opts options.Options
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	require.NoError(t, Bind(flags, &opts, ScopeGenerate))
	assert.Equal(t, "bool", flags.Lookup("enable-doca-driver").Value.Type())
	require.NoError(t, flags.Parse([]string{
		"--multirail=false",
		"--number-of-planes=0",
		"--network-namespaces=",
		"--enable-doca-driver=false",
	}))
	require.NoError(t, Collect(flags, &opts))

	assert.True(t, opts.MultirailSet)
	require.Len(t, opts.ConfigInputs.Overrides, 4)
	assert.Equal(t, false, opts.ConfigInputs.Overrides[0].Value)
	assert.Equal(t, 0, opts.ConfigInputs.Overrides[1].Value)
	assert.Empty(t, opts.ConfigInputs.Overrides[2].Value)
	assert.Equal(t, false, opts.ConfigInputs.Overrides[3].Value)
}

func TestComplexFlagsBecomeRequests(t *testing.T) {
	var opts options.Options
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	require.NoError(t, Bind(flags, &opts, ScopeGenerate))
	require.NoError(t, flags.Parse([]string{
		"--network-operator-release=26.7",
		"--spectrum-x=RA2.3",
	}))
	require.NoError(t, Collect(flags, &opts))

	assert.True(t, opts.SpectrumX)
	require.Len(t, opts.ConfigInputs.Requests, 2)
	assert.Equal(t, "network-operator-release", opts.ConfigInputs.Requests[0].Kind)
	assert.Equal(t, "spectrum-x", opts.ConfigInputs.Requests[1].Kind)
}

func TestFlavorFlagUsesConfigMapping(t *testing.T) {
	for _, scope := range []Scope{ScopeRoot, ScopeGenerate, ScopeDiscover} {
		var opts options.Options
		flags := pflag.NewFlagSet(string(scope), pflag.ContinueOnError)
		require.NoError(t, Bind(flags, &opts, scope))
		require.NoError(t, flags.Parse([]string{"--flavor=ocp"}))
		require.NoError(t, Collect(flags, &opts))
		require.Equal(t, "ocp", opts.Flavor)
		require.Contains(t, opts.ConfigInputs.Overrides, configinput.Override{
			Flag: "flavor", Path: "flavor", Value: "ocp",
		})
	}
}

func TestDefinitionsHaveUniqueFlagsAndMappings(t *testing.T) {
	require.NoError(t, ValidateDefinitions())

	seen := map[string]struct{}{}
	for _, definition := range Definitions() {
		assert.NotEmpty(t, definition.ConfigPaths)
		assert.NotEmpty(t, definition.Scopes)
		assert.NotEmpty(t, definition.Usage)
		_, duplicate := seen[definition.FlagName]
		assert.Falsef(t, duplicate, "duplicate --%s", definition.FlagName)
		seen[definition.FlagName] = struct{}{}
	}
}
