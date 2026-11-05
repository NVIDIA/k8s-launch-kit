// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
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

func TestGenerateWarnsWhenNetplanManagementIsUnknown(t *testing.T) {
	cfg := loadNetplanManagedRenderConfig(t)
	baseline, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma"), cfg,
	)
	require.NoError(t, err)
	cfg.ClusterConfig[0].NetplanManagementUnknown = true
	cfg.ClusterConfig[2].NetplanManagementUnknown = true
	var stdout, stderr bytes.Buffer
	output := ui.NewJSON(&stdout, &stderr)

	rendered, err := (&NetworkOperatorPlugin{Output: output}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma"),
		cfg,
	)
	require.NoError(t, err)
	assert.Equal(t, baseline, rendered, "uncertainty must not change generated manifests")
	assert.True(t, cfg.ClusterConfig[0].NetplanManagementUnknown, "generation must preserve discovery evidence")
	assert.False(t, cfg.ClusterConfig[0].NetplanManaged)

	messages := output.Messages()
	require.Len(t, messages, 1)
	assert.Equal(t, "warning", messages[0].Level)
	assert.Contains(t, messages[0].Message, "Netplan management could not be verified")
	assert.Contains(t, messages[0].Message, `clusterConfig group "group-0", clusterConfig group "group-2"`)
	assert.Contains(t, messages[0].Message, "no netplan naming match was confirmed")
	assert.Contains(t, stderr.String(), messages[0].Message)
	assert.Empty(t, stdout.String(), "warnings must not corrupt JSON stdout")

	files := make([]bundle.File, 0, len(rendered))
	for name, content := range rendered {
		files = append(files, bundle.File{Name: name, Content: content})
	}
	_, err = bundle.FromFiles(files)
	require.NoError(t, err)
}

func TestGenerateRejectsConfirmedNetplanMatchDespiteUnknownStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sameGroup bool
	}{
		{name: "different groups", sameGroup: false},
		{name: "same group", sameGroup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadNetplanManagedRenderConfig(t)
			cfg.ClusterConfig[0].NetplanManaged = true
			if tc.sameGroup {
				cfg.ClusterConfig[0].NetplanManagementUnknown = true
			} else {
				cfg.ClusterConfig[1].NetplanManagementUnknown = true
			}
			output := ui.NewJSON(io.Discard, io.Discard)
			_, err := (&NetworkOperatorPlugin{Output: output}).GenerateProfileDeploymentFiles(
				loadProfileFromDir(t, "sriov-ethernet-rdma"), cfg,
			)
			require.ErrorContains(t, err, "conflicting netplan configuration")
			assert.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(err))
			assert.Empty(t, output.Messages(), "a confirmed conflict must not be reported as warning-only")
		})
	}
}

func TestUnknownNetplanStatusStillValidatesInterfaceNamingLayout(t *testing.T) {
	cfg := loadNetplanManagedRenderConfig(t)
	cfg.ClusterConfig[0].NetplanManagementUnknown = true
	cfg.ClusterConfig[0].PFs[0].PciAddress = "0000:03:00.0"
	cfg.ClusterConfig[0].PFs[1].PciAddress = "0000:03:00.1"
	_, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma"), cfg,
	)
	require.ErrorContains(t, err, "appears in multiple rails")
	assert.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(err))
}

func TestGenerateAllowsInterfaceNameTemplatesWhenNetplanIsVerifiedUnmanaged(t *testing.T) {
	cfg := loadNetplanManagedRenderConfig(t)
	output := ui.NewJSON(io.Discard, io.Discard)

	rendered, err := (&NetworkOperatorPlugin{Output: output}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, "sriov-ethernet-rdma"),
		cfg,
	)
	require.NoError(t, err)
	assert.NotEmpty(t, fileNamesMatching(rendered, "30-nicinterfacenametemplate"))
	assert.Empty(t, output.Messages())
}

func TestGenerateAllowsInterfaceNameTemplatesWhenAffectedGroupIsFilteredOut(t *testing.T) {
	t.Run("managed group", func(t *testing.T) {
		cfg := loadNetplanManagedRenderConfig(t)
		cfg.ClusterConfig[0].NetplanManaged = true

		rendered, err := (&NetworkOperatorPlugin{Groups: []string{"group-1", "group-2"}}).
			GenerateProfileDeploymentFiles(loadProfileFromDir(t, "sriov-ethernet-rdma"), cfg)
		require.NoError(t, err)
		assert.Len(t, fileNamesMatching(rendered, "30-nicinterfacenametemplate"), 2)
	})

	t.Run("unknown group", func(t *testing.T) {
		cfg := loadNetplanManagedRenderConfig(t)
		cfg.ClusterConfig[0].NetplanManagementUnknown = true
		output := ui.NewJSON(io.Discard, io.Discard)

		rendered, err := (&NetworkOperatorPlugin{Groups: []string{"group-1", "group-2"}, Output: output}).
			GenerateProfileDeploymentFiles(loadProfileFromDir(t, "sriov-ethernet-rdma"), cfg)
		require.NoError(t, err)
		assert.Len(t, fileNamesMatching(rendered, "30-nicinterfacenametemplate"), 2)
		assert.Empty(t, output.Messages())
	})
}

func TestGenerateAllowsNetplanManagedGroupWhenInterfaceNameTemplateIsNotRendered(t *testing.T) {

	t.Run("interface naming is disabled", func(t *testing.T) {
		cfg := loadNetplanManagedRenderConfig(t)
		cfg.ClusterConfig[0].NetplanManaged = true
		cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate = false
		output := ui.NewJSON(io.Discard, io.Discard)

		rendered, err := (&NetworkOperatorPlugin{Output: output}).GenerateProfileDeploymentFiles(
			loadProfileFromDir(t, "sriov-ethernet-rdma"),
			cfg,
		)
		require.NoError(t, err)
		assert.Empty(t, fileNamesMatching(rendered, "30-nicinterfacenametemplate"))
		assert.Empty(t, output.Messages())
	})

	t.Run("unknown status when interface naming is disabled", func(t *testing.T) {
		cfg := loadNetplanManagedRenderConfig(t)
		cfg.ClusterConfig[0].NetplanManagementUnknown = true
		cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate = false
		output := ui.NewJSON(io.Discard, io.Discard)

		rendered, err := (&NetworkOperatorPlugin{Output: output}).GenerateProfileDeploymentFiles(
			loadProfileFromDir(t, "sriov-ethernet-rdma"),
			cfg,
		)
		require.NoError(t, err)
		assert.Empty(t, fileNamesMatching(rendered, "30-nicinterfacenametemplate"))
		assert.Empty(t, output.Messages())
	})
}

func TestGenerateNetplanChecksForUnconditionallyRenderedSpectrumXInterfaceNameTemplate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		managed bool
	}{
		{name: "managed", managed: true},
		{name: "unknown", managed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadNetplanManagedRenderConfig(t)
			cfg.ClusterConfig = cfg.ClusterConfig[:1]
			cfg.ClusterConfig[0].NetplanManaged = tc.managed
			cfg.ClusterConfig[0].NetplanManagementUnknown = !tc.managed
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
			output := ui.NewJSON(io.Discard, io.Discard)

			rendered, err := (&NetworkOperatorPlugin{Output: output}).GenerateProfileDeploymentFiles(profile, cfg)
			if tc.managed {
				require.ErrorContains(t, err, "conflicting netplan configuration")
				assert.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(err))
				assert.Empty(t, output.Messages())
			} else {
				require.NoError(t, err)
				assert.NotEmpty(t, fileNamesMatching(rendered, "nicinterfacenametemplate"))
				require.Len(t, output.Messages(), 1)
				assert.Equal(t, "warning", output.Messages()[0].Level)
			}
		})
	}
}

func TestGenerateAllowsNetplanManagedSingleSelectedGroupWithoutRenaming(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		cfg := loadNetplanManagedRenderConfig(t)
		cfg.ClusterConfig[0].NetplanManaged = true
		plugin := &NetworkOperatorPlugin{}
		if filtered {
			plugin.Groups = []string{cfg.ClusterConfig[0].Identifier}
		} else {
			cfg.ClusterConfig = cfg.ClusterConfig[:1]
		}
		rendered, err := plugin.GenerateProfileDeploymentFiles(loadProfileFromDir(t, "sriov-ethernet-rdma"), cfg)
		require.NoError(t, err)
		require.Empty(t, fileNamesMatching(rendered, "nicinterfacenametemplate"))
	}
}

func TestSingleGroupRdmaSharedNamingRetainsNetplanChecks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		managed bool
	}{
		{name: "managed", managed: true},
		{name: "unknown", managed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadNetplanManagedRenderConfig(t)
			cfg.ClusterConfig = cfg.ClusterConfig[:1]
			cfg.Profile.Deployment = "rdma_shared"
			cfg.ClusterConfig[0].NetplanManaged = tc.managed
			cfg.ClusterConfig[0].NetplanManagementUnknown = !tc.managed
			for i := range cfg.ClusterConfig[0].PFs {
				cfg.ClusterConfig[0].PFs[i].NetworkInterface = ""
			}
			output := ui.NewJSON(io.Discard, io.Discard)
			rendered, err := (&NetworkOperatorPlugin{Output: output}).GenerateProfileDeploymentFiles(loadProfileFromDir(t, "macvlan-rdma-shared"), cfg)
			if tc.managed {
				require.ErrorContains(t, err, "conflicting netplan configuration")
			} else {
				require.NoError(t, err)
				assert.NotEmpty(t, fileNamesMatching(rendered, "nicinterfacenametemplate"))
				require.Len(t, output.Messages(), 1)
				assert.Equal(t, "warning", output.Messages()[0].Level)
			}
		})
	}
}
