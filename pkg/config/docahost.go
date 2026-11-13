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

// DOCAHostScope retains the discovery membership used for host-package decisions,
// independently of render topology. It does not select deployment targets.
// An incomplete scope retains only its enumerated workers.
type DOCAHostScope struct {
	WorkerNodes     []string `yaml:"workerNodes"`
	MembershipKnown bool     `yaml:"membershipKnown"`
}

// DOCAHostScopeForConfig returns an independent, sorted snapshot of host-package
// membership. An explicit preserved scope wins over render topology; otherwise
// membership comes from the original cluster inventory.
func DOCAHostScopeForConfig(cfg *LaunchKitConfig) DOCAHostScope {
	if cfg == nil {
		return DOCAHostScope{}
	}
	if cfg.DiscoveredDOCAHostScope != nil {
		scope := *cfg.DiscoveredDOCAHostScope
		scope.WorkerNodes = slices.Clone(scope.WorkerNodes)
		slices.Sort(scope.WorkerNodes)
		return scope
	}
	scope := DOCAHostScope{MembershipKnown: len(cfg.ClusterConfig) > 0}
	workers := map[string]bool{}
	for _, group := range cfg.ClusterConfig {
		if len(group.WorkerNodes) == 0 || slices.Contains(group.WorkerNodes, "") {
			scope.MembershipKnown = false
		}
		for _, node := range group.WorkerNodes {
			if node != "" {
				workers[node] = true
			}
		}
	}
	for node := range workers {
		scope.WorkerNodes = append(scope.WorkerNodes, node)
	}
	slices.Sort(scope.WorkerNodes)
	return scope
}

// SelectedDOCAHost returns one observation per discovery worker, in node order.
// A captured host scope takes precedence over ClusterConfig render membership.
// Missing observations remain unknown; observations outside that scope cannot
// influence driver defaults or release compatibility checks.
func SelectedDOCAHost(cfg *LaunchKitConfig) []DOCAHostObservation {
	if cfg == nil {
		return nil
	}
	scope := DOCAHostScopeForConfig(cfg)
	observations := make(map[string]DOCAHostObservation, len(cfg.DiscoveredDOCAHost))
	for _, observation := range cfg.DiscoveredDOCAHost {
		observations[observation.Node] = observation
	}
	result := make([]DOCAHostObservation, 0, len(scope.WorkerNodes))
	for _, node := range scope.WorkerNodes {
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
	membershipKnown = DOCAHostScopeForConfig(cfg).MembershipKnown
	return installed, total, membershipKnown
}

// ValidateDOCAHostObservations rejects ambiguous persisted evidence.
func ValidateDOCAHostObservations(cfg *LaunchKitConfig) error {
	if cfg == nil {
		return nil
	}
	if scope := cfg.DiscoveredDOCAHostScope; scope != nil {
		if scope.MembershipKnown && len(scope.WorkerNodes) == 0 {
			return fmt.Errorf("discoveredDocaHostScope membershipKnown=true requires nonempty workerNodes")
		}
		seen := map[string]bool{}
		for _, node := range scope.WorkerNodes {
			if node == "" || seen[node] {
				return fmt.Errorf("discoveredDocaHostScope requires unique, nonempty workerNodes: %q", node)
			}
			seen[node] = true
		}
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
