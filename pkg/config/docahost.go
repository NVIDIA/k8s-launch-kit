// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"slices"
)

const (
	DOCAHostInstalled    = "installed"
	DOCAHostNotInstalled = "not-installed"
	DOCAHostUnknown      = "unknown"
)

// DOCAHostObservation records the host package database at discovery time.
// Version is the complete doca-host package version, not the daemon image tag.
type DOCAHostObservation struct {
	Node    string `yaml:"node"`
	Status  string `yaml:"status"`
	Version string `yaml:"version,omitempty"`
	Reason  string `yaml:"reason,omitempty"`
}

// SelectedDOCAHost returns one observation per inventory worker, in node order.
// Missing observations remain unknown; stale observations outside the selected
// inventory cannot influence driver defaults or release compatibility checks.
func SelectedDOCAHost(cfg *LaunchKitConfig) []DOCAHostObservation {
	if cfg == nil {
		return nil
	}
	workers := map[string]bool{}
	for _, group := range cfg.ClusterConfig {
		for _, node := range group.WorkerNodes {
			if node == "" {
				continue
			}
			workers[node] = true
		}
	}
	nodes := make([]string, 0, len(workers))
	for node := range workers {
		nodes = append(nodes, node)
	}
	slices.Sort(nodes)
	observations := make(map[string]DOCAHostObservation, len(cfg.DiscoveredDOCAHost))
	for _, observation := range cfg.DiscoveredDOCAHost {
		observations[observation.Node] = observation
	}
	result := make([]DOCAHostObservation, 0, len(nodes))
	for _, node := range nodes {
		observation, found := observations[node]
		if !found {
			observation = DOCAHostObservation{Node: node, Status: DOCAHostUnknown, Reason: "no discovery observation"}
		}
		result = append(result, observation)
	}
	return result
}

// DOCAHostCoverage reports installed and enumerated workers separately from
// whether every group supplied complete worker membership.
func DOCAHostCoverage(cfg *LaunchKitConfig) (installed, total int, membershipKnown bool) {
	observations := SelectedDOCAHost(cfg)
	for _, observation := range observations {
		if observation.Status == DOCAHostInstalled && observation.Version != "" {
			installed++
		}
	}
	total = len(observations)
	membershipKnown = true
	if cfg != nil {
		for _, group := range cfg.ClusterConfig {
			if len(group.WorkerNodes) == 0 || slices.Contains(group.WorkerNodes, "") {
				membershipKnown = false
			}
		}
	}
	return installed, total, membershipKnown
}

// ValidateDOCAHostObservations rejects ambiguous persisted evidence.
func ValidateDOCAHostObservations(cfg *LaunchKitConfig) error {
	if cfg == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, observation := range cfg.DiscoveredDOCAHost {
		if observation.Node == "" || seen[observation.Node] {
			return fmt.Errorf("discoveredDocaHost requires unique, nonempty node names: %q", observation.Node)
		}
		seen[observation.Node] = true
		switch observation.Status {
		case DOCAHostInstalled:
			if observation.Version == "" {
				return fmt.Errorf("discoveredDocaHost node %q is installed but has no package version", observation.Node)
			}
		case DOCAHostNotInstalled, DOCAHostUnknown:
			if observation.Version != "" {
				return fmt.Errorf("discoveredDocaHost node %q has version with status %q", observation.Node, observation.Status)
			}
		default:
			return fmt.Errorf("discoveredDocaHost node %q has invalid status %q", observation.Node, observation.Status)
		}
	}
	return nil
}
