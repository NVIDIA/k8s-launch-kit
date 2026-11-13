// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDOCAHostDefaultPreservesPresenceAndCoverage(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		cli        *bool
		second     config.DOCAHostObservation
		want       bool
		choice     bool
	}{
		{name: "all installed", want: false},
		{name: "null is unset", yaml: "docaDriver: {enable: null}\n", want: false},
		{name: "explicit true", yaml: "docaDriver: {enable: true}\n", want: true},
		{name: "explicit false", yaml: "docaDriver: {enable: false}\n", want: false},
		{name: "CLI true", cli: boolValue(true), want: true},
		{name: "CLI false", cli: boolValue(false), want: false},
		{name: "mixed absent", second: config.DOCAHostObservation{Node: "worker-b", Status: config.DOCAHostNotInstalled}, want: true, choice: true},
		{name: "mixed unknown", second: config.DOCAHostObservation{Node: "worker-b", Status: config.DOCAHostUnknown}, want: true, choice: true},
		{name: "mixed explicit choice", yaml: "docaDriver: {enable: false}\n", second: config.DOCAHostObservation{Node: "worker-b", Status: config.DOCAHostUnknown}, want: false},
		{name: "mixed explicit true", yaml: "docaDriver: {enable: true}\n", second: config.DOCAHostObservation{Node: "worker-b", Status: config.DOCAHostUnknown}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, err := config.DecodeInput([]byte(tc.yaml), "input.yaml")
			require.NoError(t, err)
			input.Config.ClusterConfig = []config.ClusterConfig{{WorkerNodes: []string{"worker-a", "worker-b"}}}
			second := tc.second
			if second.Node == "" {
				second = config.DOCAHostObservation{Node: "worker-b", Status: config.DOCAHostInstalled, Version: "3.5.0-041000-26.07-ubuntu2404"}
			}
			input.Config.DiscoveredDOCAHost = []config.DOCAHostObservation{
				{Node: "worker-a", Status: config.DOCAHostInstalled, Version: "3.5.0-041000-26.07-ubuntu2404"}, second,
			}
			original, err := config.CloneConfig(input.Config)
			require.NoError(t, err)
			result, err := Resolve(Request{Input: input, Options: options.Options{EnableDocaDriver: tc.cli}, ApplyHardwareDefaults: true})
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.Config.DOCADriver.Enable)
			assert.Equal(t, original, input.Config, "resolution must leave the discovery input unchanged")
			present := input.Present
			if tc.cli != nil {
				present = config.PathSet{"docaDriver.enable": {}}
			}
			err = ValidateDOCAHostDriverChoice(result.Config, present)
			assert.Equal(t, tc.choice, err != nil)
		})
	}
}

func boolValue(value bool) *bool { return &value }

func TestDOCAHostDefaultRemainsEnabledWithoutDiscoveredHostInstallation(t *testing.T) {
	canonical, err := config.DefaultLaunchKitConfig()
	require.NoError(t, err)
	assert.True(t, canonical.DOCADriver.Enable, "the canonical container-driver default remains true")
	for _, status := range []string{"", config.DOCAHostNotInstalled, config.DOCAHostUnknown} {
		t.Run("snapshot-status="+status, func(t *testing.T) {
			input, err := config.DecodeInput([]byte("{}"), "input.yaml")
			require.NoError(t, err)
			input.Config.ClusterConfig = []config.ClusterConfig{{WorkerNodes: []string{"worker-a"}}}
			if status != "" {
				input.Config.DiscoveredDOCAHost = []config.DOCAHostObservation{{Node: "worker-a", Status: status}}
			}
			result, err := Resolve(Request{Input: input, ApplyHardwareDefaults: true})
			require.NoError(t, err)
			assert.True(t, result.Config.DOCADriver.Enable)
			require.NoError(t, ValidateDOCAHostDriverChoice(result.Config, input.Present))
		})
	}
}

func TestDOCAHostDefaultRequiresKnownMembershipAndIgnoresOtherNodes(t *testing.T) {
	cfg := &config.LaunchKitConfig{
		DOCADriver: &config.DOCADriverConfig{Enable: true},
		ClusterConfig: []config.ClusterConfig{
			{WorkerNodes: []string{"worker-a"}}, {},
		},
		DiscoveredDOCAHost: []config.DOCAHostObservation{
			{Node: "worker-a", Status: config.DOCAHostInstalled, Version: "3.5.0-1"},
			{Node: "excluded", Status: config.DOCAHostInstalled, Version: "3.4.0-1"},
		},
	}
	assert.False(t, ApplyDOCAHostDefault(cfg, nil), "group with unknown membership must prevent a global disable")
	assert.True(t, cfg.DOCADriver.Enable)
	installed, total, membershipKnown := config.DOCAHostCoverage(cfg)
	assert.Equal(t, 1, installed)
	assert.Equal(t, 1, total, "unknown groups must not invent workers")
	assert.False(t, membershipKnown)
	require.ErrorContains(t, ValidateDOCAHostDriverChoice(cfg, nil), "some inventory groups lack complete workerNodes")
	cfg.ClusterConfig = cfg.ClusterConfig[:1]
	assert.True(t, ApplyDOCAHostDefault(cfg, nil))
	assert.False(t, cfg.DOCADriver.Enable)
}
