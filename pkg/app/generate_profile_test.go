// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin"
	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

func TestGeneratePersistsResolvedProfileToOriginalConfig(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	t.Chdir(filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..")))

	cfg, err := config.DefaultLaunchKitConfig()
	require.NoError(t, err)
	require.NotEmpty(t, cfg.ClusterConfig)
	cfg.Profile = &config.Profile{Deployment: "host_device"}
	cfg.ClusterConfig[0].LinkType = "Ethernet"
	cfg.NvIpam.ReserveFirstIPs = 2
	cfg.NvIpam.Subnets = []config.NvIpamSubnetConfig{{
		Subnet:  "192.168.50.0/24",
		Gateway: "192.168.50.1",
		Exclusions: []config.NvIpamExclusion{{
			StartIP: "192.168.50.20",
			EndIP:   "192.168.50.21",
		}},
	}}

	raw, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	source := "# original config comment\n" + string(raw)
	source = strings.Replace(source, "clusterConfig:\n", "clusterConfig: # hardware inventory comment\n", 1)
	source = strings.Replace(source, "profile:\n", "profile:\n  # profile settings comment\n", 1)
	source = strings.Replace(source, "linkType: Ethernet", "linkType: Ethernet # hardware detail comment", 1)

	configPath := filepath.Join(t.TempDir(), "cluster-config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(source), 0o600))

	launcher := New(options.Options{
		DeploymentType: "sriov",
		Multirail:      false,
		MultirailSet:   true,
	})
	launcher.ui = ui.NewSilent()
	launcher.plugins[networkoperatorplugin.PluginName] = &networkoperatorplugin.NetworkOperatorPlugin{}

	require.NoError(t, launcher.executeGeneration(configPath))

	got, err := config.LoadFullConfig(configPath, launcher.logger)
	require.NoError(t, err)
	require.NotNil(t, got.Profile)
	assert.Equal(t, "ethernet", got.Profile.Fabric, "hardware default must be persisted")
	assert.Equal(t, "sriov", got.Profile.Deployment, "CLI override must be persisted")
	assert.False(t, got.Profile.Multirail, "explicit CLI false must be persisted")
	assert.True(t, got.Profile.MultirailSet)

	updated, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Contains(t, string(updated), "# original config comment")
	assert.Contains(t, string(updated), "# hardware inventory comment")
	assert.Contains(t, string(updated), "# hardware detail comment")
	assert.Contains(t, string(updated), "# profile settings comment")
	var persisted config.LaunchKitConfig
	require.NoError(t, yaml.Unmarshal(updated, &persisted))
	require.Len(t, persisted.NvIpam.Subnets, 1)
	assert.Len(t, persisted.NvIpam.Subnets[0].Exclusions, 1,
		"computed reserve exclusions must not be written back as explicit input")

	info, err := os.Stat(configPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "in-place write must preserve file permissions")

	require.NoError(t, launcher.executeGeneration(configPath))
	secondUpdate, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, string(updated), string(secondUpdate), "repeated generation must produce stable config YAML")
}
