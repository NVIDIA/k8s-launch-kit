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
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestSpectrumXWorkloadNamesMatchRailTopology(t *testing.T) {
	tests := []struct {
		name, profileDir, version, mode string
		planes                          int
		wantNames                       []string
	}{
		{
			name: "RA2.2 hwplb", profileDir: "spectrum-x-ra2.2", version: "RA2.2", mode: "hwplb",
			planes: 4, wantNames: []string{"rail0", "rail1"},
		},
		{
			name: "RA2.2 swplb", profileDir: "spectrum-x-ra2.2", version: "RA2.2", mode: "swplb",
			planes: 2, wantNames: []string{"rail0p0", "rail0p1", "rail1p0", "rail1p1"},
		},
		{
			name: "RA2.3 hwplb", profileDir: "spectrum-x", version: "RA2.3", mode: "hwplb",
			planes: 4, wantNames: []string{"rail0", "rail1"},
		},
		{
			name: "RA2.3 swplb", profileDir: "spectrum-x", version: "RA2.3", mode: "swplb",
			planes: 2, wantNames: []string{"rail0p0", "rail0p1", "rail1p0", "rail1p1"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.LaunchKitConfig{
				NetworkOperator:         &config.NetworkOperatorConfig{Namespace: "network-operator"},
				CurrentNetworkNamespace: "default",
				SpectrumX: &config.SpectrumXConfig{
					HWPLB: &config.SpectrumXInterfaceNamePrefixConfig{
						NetdevPrefix: "eth_r%rail_id%_p%plane_id%",
					},
					SWPLB: &config.SpectrumXInterfaceNamePrefixConfig{
						NetdevPrefix: "eth_r%rail_id%_p%plane_id%",
					},
				},
				Profile: &config.Profile{
					Fabric: "ethernet", Deployment: "sriov", Multirail: true,
					SpectrumX: &config.ProfileSpectrumX{
						Enable: true, SPCXVersion: test.version,
						MultiplaneMode: test.mode, NumberOfPlanes: test.planes,
					},
				},
				Validation: config.DefaultValidationConfig(),
				ClusterConfig: []config.ClusterConfig{{
					PFs: []config.PFConfig{
						{Traffic: "east-west", Rail: intPtr(0)},
						{Traffic: "east-west", Rail: intPtr(1)},
					},
				}},
			}

			profilePath := filepath.Join("..", "..", "profiles", test.profileDir)
			railPools, err := ProcessTemplate(filepath.Join(profilePath, "80-spectrumxrailpoolconfig.yaml"), cfg, "")
			require.NoError(t, err)
			railPool := railPools["80-spectrumxrailpoolconfig.yaml"]
			require.NotEmpty(t, railPool)

			var railPoolObject struct {
				Spec struct {
					RailTopology []struct {
						Name string `yaml:"name"`
					} `yaml:"railTopology"`
				} `yaml:"spec"`
			}
			require.NoError(t, yaml.Unmarshal([]byte(railPool), &railPoolObject))
			var topologyNames []string
			for _, rail := range railPoolObject.Spec.RailTopology {
				topologyNames = append(topologyNames, rail.Name)
			}
			require.Equal(t, test.wantNames, topologyNames)

			workloads, err := ProcessTemplate(filepath.Join(profilePath, "90-example-daemonset.yaml"), cfg, "")
			require.NoError(t, err)
			workload := workloads["90-example-daemonset.yaml"]
			require.NotEmpty(t, workload)

			var daemonSet struct {
				Spec struct {
					Template struct {
						Metadata struct {
							Annotations map[string]string `yaml:"annotations"`
						} `yaml:"metadata"`
						Spec struct {
							Containers []struct {
								Resources struct {
									Requests map[string]string `yaml:"requests"`
									Limits   map[string]string `yaml:"limits"`
								} `yaml:"resources"`
							} `yaml:"containers"`
						} `yaml:"spec"`
					} `yaml:"template"`
				} `yaml:"spec"`
			}
			require.NoError(t, yaml.Unmarshal([]byte(workload), &daemonSet))
			require.Equal(t, strings.Join(topologyNames, ","),
				daemonSet.Spec.Template.Metadata.Annotations["k8s.v1.cni.cncf.io/networks"])
			require.NotEmpty(t, daemonSet.Spec.Template.Spec.Containers)
			requests := daemonSet.Spec.Template.Spec.Containers[0].Resources.Requests
			limits := daemonSet.Spec.Template.Spec.Containers[0].Resources.Limits
			require.Len(t, requests, len(topologyNames))
			require.Len(t, limits, len(topologyNames))
			for _, railName := range topologyNames {
				resourceName := "nvidia.com/" + railName
				require.Equal(t, "1", requests[resourceName])
				require.Equal(t, "1", limits[resourceName])
			}
		})
	}
}

func TestSpectrumXRailPoolTemplatesStaySchemaCompatibleAndBounded(t *testing.T) {
	tests := []struct {
		name       string
		profileDir string
	}{
		{name: "RA2.3", profileDir: "spectrum-x"},
		{name: "RA2.2", profileDir: "spectrum-x-ra2.2"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identifier := config.GeneratedGroupIdentifier(
				"HPE-ProLiant-Compute-DL380-Gen12",
				"NVIDIA-AX800-Converged-Accelerator",
			)
			require.LessOrEqual(t, len(identifier), config.MaxGeneratedIdentifierLength)

			cfg := &config.LaunchKitConfig{
				NetworkOperator: &config.NetworkOperatorConfig{
					Namespace: "network-operator",
				},
				CurrentNetworkNamespace: "default",
				SpectrumX: &config.SpectrumXConfig{
					SinglePlane: &config.SpectrumXInterfaceNamePrefixConfig{
						NetdevPrefix: "eth_r%rail_id%",
						RdmaPrefix:   "roce_r%rail_id%",
					},
				},
				Profile: &config.Profile{
					SpectrumX: &config.ProfileSpectrumX{
						MultiplaneMode: "none",
						NumberOfPlanes: 1,
					},
				},
				ClusterConfig: []config.ClusterConfig{
					{
						Identifier:       identifier,
						MergedIdentifier: identifier,
						PFs: []config.PFConfig{
							{
								PciAddress: "0000:19:00.0",
								Traffic:    "east-west",
							},
						},
					},
				},
			}
			templatePath := filepath.Join(
				"..",
				"..",
				"profiles",
				test.profileDir,
				"80-spectrumxrailpoolconfig.yaml",
			)

			rendered, err := ProcessTemplate(templatePath, cfg, "")
			require.NoError(t, err)
			manifest := rendered["80-spectrumxrailpoolconfig-"+identifier+".yaml"]
			require.NotEmpty(t, manifest)

			var object struct {
				Metadata struct {
					Name string `yaml:"name"`
				} `yaml:"metadata"`
				Spec map[string]any `yaml:"spec"`
			}
			require.NoError(t, yaml.Unmarshal([]byte(manifest), &object))
			require.Equal(t, "rails-"+identifier, object.Metadata.Name)
			require.Less(t, len(object.Metadata.Name), 60,
				"generated values that append the group identifier must stay below 60 bytes")
			require.NotNil(t, object.Spec)
			require.NotContains(t, object.Spec, "withBCM",
				"v1alpha2 SpectrumXRailPoolConfig rejects the removed spec.withBCM field")
			require.Equal(t, false, object.Spec["draEnabled"])
		})
	}
}
