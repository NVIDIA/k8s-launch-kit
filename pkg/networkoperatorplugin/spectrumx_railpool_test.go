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
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestSpectrumXRailPoolTemplatesOmitRemovedWithBCMField(t *testing.T) {
	tests := []struct {
		name       string
		profileDir string
	}{
		{name: "RA2.3", profileDir: "spectrum-x"},
		{name: "RA2.2", profileDir: "spectrum-x-ra2.2"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
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
						Identifier:       "test",
						MergedIdentifier: "test",
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
			manifest := rendered["80-spectrumxrailpoolconfig-test.yaml"]
			require.NotEmpty(t, manifest)

			var object struct {
				Spec map[string]any `yaml:"spec"`
			}
			require.NoError(t, yaml.Unmarshal([]byte(manifest), &object))
			require.NotNil(t, object.Spec)
			require.NotContains(t, object.Spec, "withBCM",
				"v1alpha2 SpectrumXRailPoolConfig rejects the removed spec.withBCM field")
			require.Equal(t, false, object.Spec["draEnabled"])
		})
	}
}
