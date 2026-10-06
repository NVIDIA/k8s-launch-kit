// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

func TestDiscoverySavesEmptyPlatformAndWarning(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "refresh"}[refresh], func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			path := filepath.Join(dir, "cluster-config.yaml")
			opts := options.Options{SaveClusterConfig: path}
			if refresh {
				require.NoError(t, os.WriteFile(path, []byte(profileDiscoveryBaseConfig), 0600))
				opts.UserConfig = path
			}
			l := newProfileDiscoveryLauncher(opts, []config.ClusterConfig{{Identifier: "machine-a", GPUType: "unmapped-product", LinkType: "Ethernet"}})
			output := ui.NewJSON(io.Discard, io.Discard)
			l.ui = output
			require.NoError(t, l.discoverClusterConfig())
			got, err := config.LoadFullConfig(path, l.logger)
			require.NoError(t, err)
			require.NotNil(t, got.ClusterConfig[0].SpectrumX)
			require.Empty(t, got.ClusterConfig[0].SpectrumX.PlatformType)
			require.Contains(t, string(mustRead(t, path)), `platformType: ""`)
			messages := output.Messages()
			found := false
			for _, m := range messages {
				if m.Level == "warning" {
					found = true
				}
			}
			require.True(t, found)
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

func TestDiscoveryRailAssignmentsPreservedOnlyForSameHardware(t *testing.T) {
	rail := 0
	prior := []config.ClusterConfig{{Identifier: "machine-a", GPUType: "NVIDIA-GB300", WorkerNodes: []string{"worker-a"}, SpectrumX: &config.SpectrumXGroupConfig{SwPlaneByRail: map[int]int{0: 1}}, PFs: []config.PFConfig{{Traffic: "east-west", PciAddress: "0000:05:00.0", DeviceID: "1023", Rail: &rail}}}}
	cloned, err := config.CloneConfig(&config.LaunchKitConfig{ClusterConfig: prior})
	require.NoError(t, err)
	current := cloned.ClusterConfig
	current[0].SpectrumX = nil
	output := ui.NewJSON(io.Discard, io.Discard)
	preserveSpectrumXRailAssignments(prior, current, output)
	config.PopulateSpectrumXPlatforms(current)
	require.Equal(t, 1, current[0].SpectrumX.SwPlaneByRail[0])
	require.Equal(t, "gb300", current[0].SpectrumX.PlatformType)
	current[0].SpectrumX.SwPlaneByRail[0] = 2
	require.Equal(t, 1, prior[0].SpectrumX.SwPlaneByRail[0])
	current[0].SpectrumX = nil
	current[0].PFs[0].PciAddress = "0000:06:00.0"
	preserveSpectrumXRailAssignments(prior, current, output)
	require.Nil(t, current[0].SpectrumX)
	require.NotEmpty(t, output.Messages())
}

func TestDiscoveryRefreshRailAssignmentsFollowWorkerMembership(t *testing.T) {
	for _, tc := range []struct {
		name              string
		previous, current []string
		preserve          bool
	}{
		{name: "same", previous: []string{"worker-a", "worker-b"}, current: []string{"worker-a", "worker-b"}, preserve: true},
		{name: "reordered", previous: []string{"worker-a", "worker-b"}, current: []string{"worker-b", "worker-a"}, preserve: true},
		{name: "duplicate observations", previous: []string{"worker-a", "worker-b"}, current: []string{"worker-a", "worker-a", "worker-b"}, preserve: true},
		{name: "replaced", previous: []string{"worker-a", "worker-b"}, current: []string{"worker-c", "worker-d"}, preserve: false},
		{name: "added", previous: []string{"worker-a"}, current: []string{"worker-a", "worker-b"}, preserve: false},
		{name: "removed", previous: []string{"worker-a", "worker-b"}, current: []string{"worker-a"}, preserve: false},
		{name: "unknown previous", previous: nil, current: []string{"worker-a"}, preserve: false},
		{name: "unknown current", previous: []string{"worker-a"}, current: nil, preserve: false},
		{name: "both unknown", previous: nil, current: nil, preserve: false},
		{name: "blank worker", previous: []string{"worker-a", ""}, current: []string{"worker-a", ""}, preserve: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			path := filepath.Join(dir, "cluster-config.yaml")
			rail := 0
			group := config.ClusterConfig{
				Identifier: "machine-a", GPUType: "NVIDIA-GB300", LinkType: "Ethernet", WorkerNodes: tc.previous,
				SpectrumX: &config.SpectrumXGroupConfig{PlatformType: "stale-platform", SwPlaneByRail: map[int]int{0: 1}},
				PFs:       []config.PFConfig{{Traffic: "east-west", PciAddress: "0000:05:00.0", DeviceID: "1023", Rail: &rail}},
			}
			inventory, err := yaml.Marshal(map[string]interface{}{"clusterConfig": []config.ClusterConfig{group}})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, append([]byte(profileDiscoveryBaseConfig), inventory...), 0600))
			group.WorkerNodes = tc.current
			group.SpectrumX = nil
			l := newProfileDiscoveryLauncher(options.Options{UserConfig: path, SaveClusterConfig: path}, []config.ClusterConfig{group})
			output := ui.NewJSON(io.Discard, io.Discard)
			l.ui = output
			require.NoError(t, l.discoverClusterConfig())
			got, err := config.LoadFullConfig(path, l.logger)
			require.NoError(t, err)
			require.Len(t, got.ClusterConfig, 1)
			spectrumX := got.ClusterConfig[0].SpectrumX
			require.NotNil(t, spectrumX)
			require.Equal(t, "gb300", spectrumX.PlatformType)
			var warnings []string
			for _, message := range output.Messages() {
				if message.Level == "warning" {
					warnings = append(warnings, message.Message)
				}
			}
			if tc.preserve {
				require.Equal(t, map[int]int{0: 1}, spectrumX.SwPlaneByRail)
				require.Empty(t, warnings)
			} else {
				require.Empty(t, spectrumX.SwPlaneByRail)
				require.Len(t, warnings, 1)
				require.Contains(t, warnings[0], "worker membership changed or could not be verified")
				require.Contains(t, warnings[0], "reapply the intended assignments before generation")
			}
		})
	}
}
