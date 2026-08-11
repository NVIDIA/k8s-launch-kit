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
	"fmt"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/yaml"
)

func TestSpectrumXPCIAddresses(t *testing.T) {
	group := &config.ClusterConfig{
		Identifier: "machine-a",
		PFs: []config.PFConfig{
			{PciAddress: " 000A:19:00.0 ", Traffic: "east-west"},
			{PciAddress: "000a:19:00.0", Traffic: "east-west"},
			{PciAddress: "0000:03:00.0", Traffic: "north-south"},
		},
	}

	addresses, err := spectrumXPCIAddresses(group)
	require.NoError(t, err)
	require.Equal(t, []string{"000a:19:00.0"}, addresses,
		"the selector must normalize, deduplicate, and exclude north-south PCI addresses")

	group.PFs[0].PciAddress = ""
	group.PFs = group.PFs[:1]
	_, err = spectrumXPCIAddresses(group)
	require.ErrorContains(t, err, `group "machine-a" has an east-west PF without a pciAddress`)
}

func TestSpectrumXNicConfigurationTemplateExcludesSameTypeNorthSouthDevices(t *testing.T) {
	tests := []struct {
		name        string
		profileDir  string
		spcxVersion string
	}{
		{name: "RA2.1", profileDir: "spectrum-x-ra2.1", spcxVersion: "RA2.1"},
		{name: "RA2.2", profileDir: "spectrum-x-ra2.2", spcxVersion: "RA2.2"},
		{name: "RA2.3", profileDir: "spectrum-x", spcxVersion: "RA2.3"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
			cfg, err := config.LoadFullConfig(
				filepath.Join("testdata", "grouping", "same-ew-different-ns.yaml"),
				ctrllog.Log,
			)
			require.NoError(t, err)

			for groupIndex := range cfg.ClusterConfig {
				machineLabel := "machine-a-gpu-model-x"
				if groupIndex == 1 {
					machineLabel = "machine-b-gpu-model-x"
				}
				cfg.ClusterConfig[groupIndex].NodeSelector = map[string]string{
					config.MachineLabelKey: machineLabel,
				}
				for pfIndex := range cfg.ClusterConfig[groupIndex].PFs {
					// Model the reported failure: both the east-west SuperNIC and
					// north-south DPU expose the same BlueField-3 device ID.
					cfg.ClusterConfig[groupIndex].PFs[pfIndex].DeviceID = "a2dc"
				}
			}
			cfg.Profile = &config.Profile{
				Fabric:     "ethernet",
				Deployment: "sriov",
				Multirail:  true,
				SpectrumX: &config.ProfileSpectrumX{
					Enable:         true,
					SPCXVersion:    test.spcxVersion,
					MultiplaneMode: "none",
					NumberOfPlanes: 1,
					ConfigMapName:  "site-ra23-profile",
				},
			}

			profile := loadProfileFromDir(t, test.profileDir)
			var nicConfigurationTemplate string
			for _, templatePath := range profile.Templates {
				if filepath.Base(templatePath) == "30-nicconfigurationtemplate.yaml" {
					nicConfigurationTemplate = templatePath
					break
				}
			}
			require.NotEmpty(t, nicConfigurationTemplate)
			profile.Templates = []string{nicConfigurationTemplate}

			rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
			require.NoError(t, err)
			require.Equal(t, []string{
				"30-nicconfigurationtemplate-gpu-model-x.yaml",
				"30-nicconfigurationtemplate-group-1.yaml",
			}, fileNamesMatching(rendered, "30-nicconfigurationtemplate"),
				"a merged hardware bucket must still emit one PCI-scoped template per source group")

			for groupIndex, group := range cfg.ClusterConfig {
				fileName := fmt.Sprintf("30-nicconfigurationtemplate-group-%d.yaml", groupIndex)
				if groupIndex == 0 {
					fileName = "30-nicconfigurationtemplate-gpu-model-x.yaml"
				}
				manifest := rendered[fileName]
				require.NotEmpty(t, manifest)

				var object struct {
					Spec struct {
						NodeSelector map[string]string `yaml:"nodeSelector"`
						NicSelector  struct {
							NicType      string   `yaml:"nicType"`
							PCIAddresses []string `yaml:"pciAddresses"`
						} `yaml:"nicSelector"`
					} `yaml:"spec"`
				}
				require.NoError(t, yaml.Unmarshal([]byte(manifest), &object))
				require.Equal(t, "a2dc", object.Spec.NicSelector.NicType)
				require.Equal(t, group.NodeSelector, object.Spec.NodeSelector)

				var eastWestPCIs []string
				var northSouthPCIs []string
				for _, pf := range group.PFs {
					switch pf.Traffic {
					case "east-west":
						eastWestPCIs = append(eastWestPCIs, pf.PciAddress)
					case "north-south":
						northSouthPCIs = append(northSouthPCIs, pf.PciAddress)
					}
				}
				require.Equal(t, eastWestPCIs, object.Spec.NicSelector.PCIAddresses)
				for _, pciAddress := range northSouthPCIs {
					require.NotContains(t, object.Spec.NicSelector.PCIAddresses, pciAddress,
						"north-south PCI address %s must not be selected", pciAddress)
				}
			}
		})
	}
}
