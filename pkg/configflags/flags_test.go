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
	"github.com/nvidia/k8s-launch-kit/pkg/target"
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

func TestTargetOwnershipDefaultsToHostAndRejectsUnknownTarget(t *testing.T) {
	require.Equal(t, target.Host, targetForTag(""))
	require.True(t, knownTarget(targetForTag("")))
	require.False(t, knownTarget(targetForTag("future")))

	definitions := map[string]Definition{}
	for _, definition := range Definitions() {
		definitions[definition.FlagName] = definition
	}
	assert.Equal(t, target.Host, definitions["fabric"].Target)
	assert.Equal(t, target.DPF, definitions["dpf-profile"].Target)
	assert.Equal(t, target.DPF, definitions["dpf-release"].Target)
	assert.Equal(t, []string{"profile.fabric"}, definitions["fabric"].ConfigPaths)
	assert.Equal(t, []string{"dpf.profile"}, definitions["dpf-profile"].ConfigPaths)
	assert.Equal(t, []string{"dpf.release"}, definitions["dpf-release"].ConfigPaths)
	assert.Empty(t, definitions["dpf-profile"].Resolver)
	assert.Empty(t, definitions["dpf-release"].Resolver)
	assert.Equal(t, []Scope{ScopeGenerate}, definitions["dpf-profile"].Scopes)
	assert.Equal(t, []Scope{ScopeGenerate}, definitions["dpf-release"].Scopes)
}

func TestConfigPathValidationUsesDefinitionTargetSchema(t *testing.T) {
	hostType, err := configPathType("profile.fabric", target.Host)
	require.NoError(t, err)
	assert.Equal(t, "string", valueType(hostType))
	_, err = configPathType("dpf.profile", target.Host)
	require.ErrorContains(t, err, `config path "dpf.profile" does not exist`)

	for _, path := range []string{"dpf.profile", "dpf.release"} {
		dpfType, err := configPathType(path, target.DPF)
		require.NoError(t, err)
		assert.Equal(t, "string", valueType(dpfType))
	}
	_, err = configPathType("profile.fabric", target.DPF)
	require.ErrorContains(t, err, `config path "profile.fabric" does not exist`)
}

func TestDPFFlagsAreGenerateOnlyAndPreserveExplicitEmptyValues(t *testing.T) {
	for _, scope := range []Scope{ScopeRoot, ScopeDiscover} {
		var opts options.Options
		flags := pflag.NewFlagSet(string(scope), pflag.ContinueOnError)
		require.NoError(t, Bind(flags, &opts, scope))
		assert.Nil(t, flags.Lookup("dpf-profile"))
		assert.Nil(t, flags.Lookup("dpf-release"))
	}

	var opts options.Options
	flags := pflag.NewFlagSet("generate", pflag.ContinueOnError)
	require.NoError(t, Bind(flags, &opts, ScopeGenerate))
	require.NotNil(t, flags.Lookup("dpf-profile"))
	require.NotNil(t, flags.Lookup("dpf-release"))
	require.NoError(t, flags.Parse([]string{"--dpf-profile=", "--dpf-release="}))
	require.NoError(t, Collect(flags, &opts))
	assert.Empty(t, opts.DPFProfile)
	assert.Empty(t, opts.DPFRelease)
	assert.Contains(t, opts.ConfigInputs.Overrides, configinput.Override{
		Flag: "dpf-profile", Path: "dpf.profile", Value: "",
	})
	assert.Contains(t, opts.ConfigInputs.Overrides, configinput.Override{
		Flag: "dpf-release", Path: "dpf.release", Value: "",
	})
	assert.Empty(t, opts.ConfigInputs.Requests)
}

func TestHostFlagDefinitionRemainsHostOwned(t *testing.T) {
	var fabric Definition
	for _, definition := range Definitions() {
		if definition.FlagName == "fabric" {
			fabric = definition
			break
		}
	}
	assert.Equal(t, target.Host, fabric.Target)
	assert.Equal(t, []string{"profile.fabric"}, fabric.ConfigPaths)
	assert.Equal(t, []Scope{ScopeRoot, ScopeGenerate, ScopeDiscover}, fabric.Scopes)
}
