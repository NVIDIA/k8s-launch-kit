// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/stretchr/testify/require"
	configyaml "gopkg.in/yaml.v2"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	renderedyaml "sigs.k8s.io/yaml"
)

type maintenanceProfileCase struct {
	dir                string
	fabric             string
	deployment         string
	wantOFEDRequestor  bool
	wantDrainRequestor bool
	wantExternalDrain  bool
}

var maintenanceProfileCases = []maintenanceProfileCase{
	{dir: "host-device-rdma", fabric: "ethernet", deployment: "host_device", wantOFEDRequestor: true},
	{dir: "ipoib-rdma-shared", fabric: "infiniband", deployment: "rdma_shared", wantOFEDRequestor: true},
	{dir: "macvlan-rdma-shared", fabric: "ethernet", deployment: "rdma_shared", wantOFEDRequestor: true},
	{
		dir:                "sriov-ethernet-rdma",
		fabric:             "ethernet",
		deployment:         "sriov",
		wantOFEDRequestor:  true,
		wantDrainRequestor: true,
		wantExternalDrain:  true,
	},
	{
		dir:                "sriov-ib-rdma",
		fabric:             "infiniband",
		deployment:         "sriov",
		wantOFEDRequestor:  true,
		wantDrainRequestor: true,
		wantExternalDrain:  true,
	},
	{
		dir:                "spectrum-x-ra2.1",
		fabric:             "ethernet",
		deployment:         "sriov",
		wantDrainRequestor: true,
		wantExternalDrain:  true,
	},
	{
		dir:                "spectrum-x",
		fabric:             "ethernet",
		deployment:         "sriov",
		wantDrainRequestor: true,
		wantExternalDrain:  true,
	},
}

func maintenanceFromYAML(t *testing.T, source string) *config.MaintenanceConfig {
	t.Helper()
	if source == "" {
		return nil
	}
	var parsed config.LaunchKitConfig
	require.NoError(t, configyaml.Unmarshal([]byte(source), &parsed))
	require.NotNil(t, parsed.Maintenance)
	return parsed.Maintenance
}

func renderProfileValuesForMaintenance(
	t *testing.T,
	dir string,
	release string,
	withNCO bool,
	maintenanceYAML string,
) map[string]any {
	t.Helper()
	cfg := &config.LaunchKitConfig{
		NetworkOperator: &config.NetworkOperatorConfig{
			OperatorRepository: "nvcr.io/nvidia/cloud-native",
			Namespace:          "network-operator",
			SelectedRelease:    release,
		},
		Maintenance: maintenanceFromYAML(t, maintenanceYAML),
	}
	if withNCO {
		cfg.NicConfigurationOperator = &config.NicConfigurationOperatorConfig{}
	}
	require.NoError(t, config.NormalizeMaintenance(cfg))

	templatePath, err := filepath.Abs(filepath.Join("..", "..", "profiles", dir, helmValuesTemplateName))
	require.NoError(t, err)
	rendered, err := renderForScope(templatePath, cfg, nil, nil)
	require.NoError(t, err)
	require.Contains(t, rendered, helmValuesOutputName)

	values := map[string]any{}
	require.NoError(t, renderedyaml.UnmarshalStrict([]byte(rendered[helmValuesOutputName]), &values))
	return values
}

func nestedMaintenanceMap(t *testing.T, root map[string]any, keys ...string) map[string]any {
	t.Helper()
	current := root
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		require.Truef(t, ok, "%q is missing or is not a map in %v", key, current)
		current = next
	}
	return current
}

func TestMaintenanceRequestorModeProfileMatrix(t *testing.T) {
	releases := []string{"25.10", "26.1", "26.4", ""}
	for _, profile := range maintenanceProfileCases {
		for _, release := range releases {
			releaseName := release
			if releaseName == "" {
				releaseName = "latest"
			}
			t.Run(profile.dir+"/"+releaseName, func(t *testing.T) {
				values := renderProfileValuesForMaintenance(t, profile.dir, release, false, "")
				requestorMode := versionGE(release, "26.1")

				chartMaintenance := nestedMaintenanceMap(t, values, "maintenanceOperator")
				require.Equal(t, requestorMode, chartMaintenance["enabled"])

				operator := nestedMaintenanceMap(t, values, "operator")
				requestorValues, hasRequestorValues := operator["maintenanceOperator"]
				if !requestorMode {
					require.False(t, hasRequestorValues)
				} else {
					want := map[string]any{}
					if profile.wantOFEDRequestor {
						want["useRequestor"] = true
					}
					if profile.wantDrainRequestor {
						want["useDrainControllerRequestor"] = true
					}
					require.Equal(t, want, requestorValues,
						"only the profile's requestor-mode booleans should override chart defaults")
				}

				sriovSubchart, hasSriovSubchart := values["sriov-network-operator"].(map[string]any)
				if requestorMode && profile.wantExternalDrain {
					require.True(t, hasSriovSubchart)
					require.Equal(t, map[string]any{
						"externalDrainer": map[string]any{"enabled": true},
					}, sriovSubchart["operator"])
				} else if hasSriovSubchart {
					require.NotContains(t, sriovSubchart, "operator")
				}

				operatorConfig := nestedMaintenanceMap(t, values, "maintenance-operator-chart", "operatorConfig")
				require.Equal(t, map[string]any{
					"deploy":                        true,
					"maxParallelOperations":         "4",
					"maxUnavailable":                "4",
					"maxNodeMaintenanceTimeSeconds": "3600",
				}, operatorConfig)
			})
		}
	}
}

func TestMaintenanceOperatorRemainsEnabledForNCOBeforeRequestorMode(t *testing.T) {
	values := renderProfileValuesForMaintenance(t, "host-device-rdma", "25.10", true, "")
	require.Equal(t, true, nestedMaintenanceMap(t, values, "maintenanceOperator")["enabled"])
	require.NotContains(t, nestedMaintenanceMap(t, values, "operator"), "maintenanceOperator")
}

func TestMaintenanceOperatorValuesPreserveExplicitZero(t *testing.T) {
	values := renderProfileValuesForMaintenance(t, "host-device-rdma", "26.1", false, `maintenance:
  maxUnavailable: 0
  maxNodeMaintenanceTimeSeconds: 0
  maxParallelUpgrades: 0
`)
	require.Equal(t, map[string]any{
		"deploy":                        true,
		"maxParallelOperations":         "4",
		"maxUnavailable":                "0",
		"maxNodeMaintenanceTimeSeconds": "0",
	}, nestedMaintenanceMap(t, values, "maintenance-operator-chart", "operatorConfig"))
}

func renderFullProfileForMaintenance(
	t *testing.T,
	profile maintenanceProfileCase,
	release string,
	maintenanceYAML string,
) map[string]string {
	t.Helper()
	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
	cfg, err := config.LoadFullConfig(
		filepath.Join("testdata", "grouping", "mixed-same-type.yaml"),
		ctrllog.Log,
	)
	require.NoError(t, err)
	cfg.NetworkOperator.SelectedRelease = release
	cfg.Profile = &config.Profile{
		Fabric:     profile.fabric,
		Deployment: profile.deployment,
		Multirail:  true,
	}
	cfg.Maintenance = maintenanceFromYAML(t, maintenanceYAML)

	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
		loadProfileFromDir(t, profile.dir), cfg)
	require.NoError(t, err)
	return rendered
}

func TestMaxParallelUpgradesRendersInNCPAndNNP(t *testing.T) {
	maintenanceYAML := `maintenance:
  maxParallelUpgrades: 7
`
	for _, profile := range maintenanceProfileCases[:5] {
		for _, release := range []string{"25.10", "26.1", "26.4", ""} {
			releaseName := release
			if releaseName == "" {
				releaseName = "latest"
			}
			t.Run(profile.dir+"/"+releaseName, func(t *testing.T) {
				rendered := renderFullProfileForMaintenance(t, profile, release, maintenanceYAML)
				manifestFragment := "11-nicnodepolicy"
				if release == "25.10" || release == "26.1" {
					manifestFragment = "10-nicclusterpolicy"
				}
				names := fileNamesMatching(rendered, manifestFragment)
				require.NotEmpty(t, names)
				sawOFEDDriver := false
				for _, name := range names {
					if strings.Contains(rendered[name], "ofedDriver:") {
						sawOFEDDriver = true
						require.Contains(t, rendered[name], "maxParallelUpgrades: 7")
					}
				}
				require.True(t, sawOFEDDriver)
			})
		}
	}
}

func TestLegacySriovPoolConfigReleaseGate(t *testing.T) {
	maintenanceYAML := `maintenance:
  maxUnavailable: 37%
`
	for _, profile := range maintenanceProfileCases[3:5] {
		for _, release := range []string{"25.10", "26.1", "26.4", ""} {
			releaseName := release
			if releaseName == "" {
				releaseName = "latest"
			}
			t.Run(profile.dir+"/"+releaseName, func(t *testing.T) {
				rendered := renderFullProfileForMaintenance(t, profile, release, maintenanceYAML)
				names := fileNamesMatching(rendered, "35-sriovnetworkpoolconfig")
				if release != "25.10" {
					require.Empty(t, names, "requestor mode makes MaintenanceOperatorConfig authoritative")
					return
				}
				require.Len(t, names, 1)
				require.Contains(t, rendered[names[0]], "kind: SriovNetworkPoolConfig")
				require.Contains(t, rendered[names[0]], "maxUnavailable: 37%")
				require.Contains(t, rendered[names[0]], "nodeSelector:")
			})
		}
	}
}

func TestSpectrumXPoolDoesNotRenderIneffectiveMaxUnavailable(t *testing.T) {
	rendered := renderSpectrumXRA21(t, "mixed-same-type.yaml", "hwplb", 2)
	_, pool := fileMatching(t, rendered, "40-sriovnetworkpoolconfig")
	require.NotContains(t, pool, "maxUnavailable:")
}

func TestGenerateProfileDeploymentFilesNormalizesNilMaintenance(t *testing.T) {
	templatePath := filepath.Join(t.TempDir(), helmValuesTemplateName)
	require.NoError(t, os.WriteFile(templatePath, []byte(`maintenance-operator-chart:
  operatorConfig:
    maxParallelOperations: "{{ .Maintenance.MaxParallelOperations.String }}"
    maxUnavailable: "{{ .Maintenance.MaxUnavailable.String }}"
    maxNodeMaintenanceTimeSeconds: "{{ .Maintenance.MaxNodeMaintenanceTimeSeconds }}"
`), 0o644))
	cfg := &config.LaunchKitConfig{
		NetworkOperator: &config.NetworkOperatorConfig{},
		Profile:         &config.Profile{},
		Maintenance:     nil,
	}
	profile := &profiles.Profile{Templates: []string{templatePath}}

	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
	require.NoError(t, err)
	require.NotNil(t, cfg.Maintenance)
	require.Contains(t, rendered[helmValuesOutputName], `maxParallelOperations: "4"`)
	require.Contains(t, rendered[helmValuesOutputName], `maxUnavailable: "4"`)
	require.Contains(t, rendered[helmValuesOutputName], `maxNodeMaintenanceTimeSeconds: "3600"`)
}
