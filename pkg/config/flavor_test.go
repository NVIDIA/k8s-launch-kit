// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

func TestApplyFlavorDefaults(t *testing.T) {
	cfg := &LaunchKitConfig{}
	require.NoError(t, ApplyFlavorDefaults(cfg))
	require.Equal(t, FlavorK8s, cfg.Flavor)
	require.Nil(t, cfg.NFD)
	cfg.Flavor = FlavorOCP
	cfg.Sriov = &SriovConfig{OperatorNamespace: "custom-sriov"}
	cfg.NFD = &NFDConfig{ConfigurationName: "custom-nfd"}
	cfg.Maintenance = &MaintenanceConfig{OperatorNamespace: "custom-maintenance"}
	require.NoError(t, ApplyFlavorDefaults(cfg))
	require.Equal(t, "custom-sriov", cfg.Sriov.OperatorNamespace)
	require.Equal(t, "custom-nfd", cfg.NFD.ConfigurationName)
	require.Equal(t, DefaultNFDOperatorNamespace, cfg.NFD.OperatorNamespace)
	require.Equal(t, "custom-maintenance", cfg.Maintenance.OperatorNamespace)
	cfg.Flavor = "unknown"
	require.ErrorContains(t, ApplyFlavorDefaults(cfg), "flavor must be")
}

func TestFlavorOverrideKeepsOnlyExplicitNamespaceValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte("flavor: ocp\nsriov:\n  operatorNamespace: openshift-sriov-network-operator\nnfd:\n  configurationName: custom-nfd\n")
	require.NoError(t, os.WriteFile(path, content, 0600))
	cfg, err := LoadFullConfig(path, logr.Discard())
	require.NoError(t, err)
	require.Equal(t, DefaultNFDOperatorNamespace, cfg.NFD.OperatorNamespace)
	cfg.Flavor = FlavorK8s
	require.NoError(t, ApplyFlavorDefaults(cfg))
	require.Equal(t, DefaultSriovOperatorNamespace, cfg.Sriov.OperatorNamespace)
	require.Empty(t, cfg.NFD.OperatorNamespace)
	require.Equal(t, "custom-nfd", cfg.NFD.ConfigurationName)
	require.Empty(t, cfg.Maintenance.OperatorNamespace)
}
