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
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/releases"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpectrumXProfilesRenderCatalogXPlaneArtifacts(t *testing.T) {
	tests := []struct {
		name       string
		profileDir string
		release    string
	}{
		{
			name:       "RA2.3",
			profileDir: "spectrum-x",
			release:    "26.7",
		},
		{
			name:       "RA2.2",
			profileDir: "spectrum-x-ra2.2",
			release:    "26.4",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			release, ok := releases.LookupRelease(test.release)
			require.True(t, ok)
			require.NotEmpty(t, release.XPlane.Repository)
			require.NotEmpty(t, release.XPlane.Version)

			cfg := &config.LaunchKitConfig{
				NetworkOperator: &config.NetworkOperatorConfig{
					SelectedRelease:  test.release,
					Repository:       "generic-component-repository",
					ComponentVersion: "generic-component-version",
				},
				NicConfigurationOperator: &config.NicConfigurationOperatorConfig{},
			}
			templatePath := filepath.Join(
				"..",
				"..",
				"profiles",
				test.profileDir,
				"10-nicclusterpolicy.yaml",
			)

			rendered, err := ProcessTemplate(templatePath, cfg, "")
			require.NoError(t, err)
			manifest := rendered["10-nicclusterpolicy.yaml"]
			expected := fmt.Sprintf(
				"xPlane:\n      image: xplane\n      repository: %q\n      version: %q",
				release.XPlane.Repository,
				release.XPlane.Version,
			)
			assert.Contains(t, manifest, expected)
		})
	}
}

func TestXPlaneArtifactsFallBackForUnpinnedConfig(t *testing.T) {
	networkOperator := &config.NetworkOperatorConfig{
		Repository:       "custom.example.com/components",
		ComponentVersion: "custom-version",
	}

	assert.Equal(t, networkOperator.Repository, xPlaneRepository(networkOperator))
	assert.Equal(t, networkOperator.ComponentVersion, xPlaneVersion(networkOperator))
	assert.Empty(t, xPlaneRepository(nil))
	assert.Empty(t, xPlaneVersion(nil))
}
