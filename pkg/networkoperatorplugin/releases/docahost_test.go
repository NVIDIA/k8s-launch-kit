// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package releases

import (
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/assert"
)

func TestDOCAHostReleaseCompatibilityUsesSBOMDOCARelease(t *testing.T) {
	for _, tc := range []struct {
		name, release, version string
		wantIssue              bool
	}{
		{"matching Ubuntu package", "26.7", "3.5.0-041000-26.07-ubuntu2404", false},
		{"matching RPM package", "26.7", "3.5.0-041000_26.07.el9", false},
		{"different DOCA release", "26.10", "3.5.0-041000-26.07-ubuntu2404", true},
		{"missing selected release", "", "3.5.0-041000", true},
		{"unknown selected release", "99.99", "3.5.0-041000", true},
		{"invalid package version", "26.7", "not-a-version", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.LaunchKitConfig{
				NetworkOperator: &config.NetworkOperatorConfig{SelectedRelease: tc.release},
				DOCADriver:      &config.DOCADriverConfig{Enable: true, Version: "doca0.0.0-user-override"},
				ClusterConfig:   []config.ClusterConfig{{WorkerNodes: []string{"worker-a"}}},
				DiscoveredDOCAHost: []config.DOCAHostObservation{
					{Node: "worker-a", Status: config.DOCAHostInstalled, Version: tc.version},
					{Node: "excluded", Status: config.DOCAHostInstalled, Version: "0.0.0-1"},
				},
			}
			assert.Equal(t, tc.wantIssue, len(DOCAHostIssues(cfg)) > 0)
		})
	}
}
