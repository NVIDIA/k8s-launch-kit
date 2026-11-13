// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDOCAHostScopePreservesDiscoveryMembershipAcrossTopology(t *testing.T) {
	cfg := &LaunchKitConfig{
		ClusterConfig: []ClusterConfig{{WorkerNodes: []string{"worker-b", "worker-a"}}},
		DiscoveredDOCAHost: []DOCAHostObservation{
			{Node: "worker-a", Status: DOCAHostInstalled, Version: "3.5.0-1"},
			{Node: "excluded", Status: DOCAHostInstalled, Version: "0.0.0-1"},
		},
	}
	scope := DOCAHostScopeForConfig(cfg)
	assert.Equal(t, []string{"worker-a", "worker-b"}, scope.WorkerNodes)
	assert.Equal(t, []string{"worker-b", "worker-a"}, cfg.ClusterConfig[0].WorkerNodes)
	cfg.DiscoveredDOCAHostScope = &scope
	cfg.ClusterConfig = []ClusterConfig{{Identifier: "render-topology"}}
	selected := SelectedDOCAHost(cfg)
	require.Len(t, selected, 2)
	assert.Equal(t, "worker-a", selected[0].Node)
	assert.Equal(t, DOCAHostUnknown, selected[1].Status, "missing evidence remains unknown")
	installed, total, known := DOCAHostCoverage(cfg)
	assert.Equal(t, 1, installed)
	assert.Equal(t, 2, total)
	assert.True(t, known)
	path, err := WriteEffectiveConfig(t.TempDir(), cfg)
	require.NoError(t, err)
	loaded, err := LoadEffectiveConfig(path)
	require.NoError(t, err)
	assert.Equal(t, cfg.DiscoveredDOCAHostScope, loaded.DiscoveredDOCAHostScope)
	assert.Equal(t, cfg.DiscoveredDOCAHost, loaded.DiscoveredDOCAHost)
	assert.Equal(t, "render-topology", loaded.ClusterConfig[0].Identifier)
	assert.Empty(t, loaded.ClusterConfig[0].WorkerNodes)
	assert.Equal(t, selected, SelectedDOCAHost(loaded))
	copied := DOCAHostScopeForConfig(loaded)
	copied.WorkerNodes[0] = "mutated"
	assert.Equal(t, "worker-a", loaded.DiscoveredDOCAHostScope.WorkerNodes[0], "scope snapshots must not alias caller storage")
}

func TestDOCAHostScopeRejectsAmbiguousEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		scope         DOCAHostScope
	}{
		{"complete empty", "requires nonempty workerNodes", DOCAHostScope{MembershipKnown: true}},
		{"duplicate", "unique, nonempty workerNodes", DOCAHostScope{WorkerNodes: []string{"worker-a", "worker-a"}, MembershipKnown: true}},
		{"empty node", "unique, nonempty workerNodes", DOCAHostScope{WorkerNodes: []string{""}, MembershipKnown: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, ValidateDOCAHostObservations(&LaunchKitConfig{DiscoveredDOCAHostScope: &tc.scope}), tc.message)
		})
	}
	require.NoError(t, ValidateDOCAHostObservations(&LaunchKitConfig{DiscoveredDOCAHostScope: &DOCAHostScope{MembershipKnown: false}}))
	cfg := &LaunchKitConfig{ClusterConfig: []ClusterConfig{{WorkerNodes: []string{"worker-a"}}, {}}}
	scope := DOCAHostScopeForConfig(cfg)
	assert.Equal(t, []string{"worker-a"}, scope.WorkerNodes)
	assert.False(t, scope.MembershipKnown, "partial membership cannot become complete when topology changes")
}
