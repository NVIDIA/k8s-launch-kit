// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

func loadNetplanManagedRenderConfig(t *testing.T) *config.LaunchKitConfig {
	t.Helper()

	cfg, err := config.LoadFullConfig(
		filepath.Join("testdata", "grouping", "mixed-same-type.yaml"),
		ctrllog.Log,
	)
	require.NoError(t, err)
	cfg.Profile = &config.Profile{
		Fabric:     "ethernet",
		Deployment: "sriov",
		Multirail:  true,
	}
	return cfg
}

func TestGenerateRejectsNetplanManagedInterfaceNameTemplates(t *testing.T) {
	cfg := loadNetplanManagedRenderConfig(t)
	cfg.ClusterConfig[0].NetplanManaged = true
	cfg.ClusterConfig[2].NetplanManaged = true

	_, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma"),
		cfg,
	)
	require.Error(t, err)

	var structured *apperrors.StructuredError
	require.True(t, errors.As(err, &structured))
	assert.Equal(t, apperrors.ExitValidation, structured.ExitCode)
	assert.Equal(t, "validation", structured.Category)
	assert.Contains(t, structured.Message, "NICs being configured")
	assert.Contains(t, structured.Message, "conflicting netplan configuration")
	assert.Contains(t, structured.Message, `clusterConfig group "group-0"`)
	assert.Contains(t, structured.Message, `clusterConfig group "group-2"`)
	assert.Equal(t,
		"Clean up the set-name stanzas for the affected NICs in the host netplan configuration, re-run 'l8k discover', then retry 'l8k generate'.",
		structured.Suggestion,
	)
	assert.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(err))
}

func TestGenerateAllowsInterfaceNameTemplatesWhenAffectedGroupIsFilteredOut(t *testing.T) {
	cfg := loadNetplanManagedRenderConfig(t)
	cfg.ClusterConfig[0].NetplanManaged = true

	rendered, err := (&NetworkOperatorPlugin{Groups: []string{"group-1", "group-2"}}).
		GenerateProfileDeploymentFiles(loadProfileFromDir(t, "sriov-ethernet-rdma"), cfg)
	require.NoError(t, err)
	assert.Len(t, fileNamesMatching(rendered, "30-nicinterfacenametemplate"), 2)
}

func TestGenerateAllowsNetplanManagedGroupWhenInterfaceNameTemplateIsNotRendered(t *testing.T) {
	t.Run("single source group uses PCI fallback", func(t *testing.T) {
		cfg := loadNetplanManagedRenderConfig(t)
		cfg.ClusterConfig = cfg.ClusterConfig[:1]
		cfg.ClusterConfig[0].NetplanManaged = true

		rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
			loadProfileFromDir(t, "sriov-ethernet-rdma"),
			cfg,
		)
		require.NoError(t, err)
		assert.Empty(t, fileNamesMatching(rendered, "30-nicinterfacenametemplate"))
	})

	t.Run("interface naming is disabled", func(t *testing.T) {
		cfg := loadNetplanManagedRenderConfig(t)
		cfg.ClusterConfig[0].NetplanManaged = true
		cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate = false

		rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
			loadProfileFromDir(t, "sriov-ethernet-rdma"),
			cfg,
		)
		require.NoError(t, err)
		assert.Empty(t, fileNamesMatching(rendered, "30-nicinterfacenametemplate"))
	})
}

func TestGenerateRejectsUnconditionallyRenderedSpectrumXInterfaceNameTemplate(t *testing.T) {
	cfg := loadNetplanManagedRenderConfig(t)
	cfg.ClusterConfig = cfg.ClusterConfig[:1]
	cfg.ClusterConfig[0].NetplanManaged = true
	cfg.Profile = &config.Profile{
		Fabric:     "ethernet",
		Deployment: "sriov",
		Multirail:  true,
		SpectrumX: &config.ProfileSpectrumX{
			Enable:         true,
			MultiplaneMode: "none",
			NumberOfPlanes: 1,
		},
	}
	cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate = false

	templatePath, err := filepath.Abs(filepath.Join(
		"..", "..", "profiles", "spectrum-x-ra2.2", "25-nicinterfacenametemplate.yaml",
	))
	require.NoError(t, err)
	profile := &profiles.Profile{Templates: []string{templatePath}}

	_, err = (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
	require.Error(t, err)
	assert.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(err))
}
