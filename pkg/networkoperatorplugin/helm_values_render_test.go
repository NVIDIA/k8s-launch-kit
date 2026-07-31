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
	"testing"

	"github.com/go-logr/logr"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

type helmPullSecretValues struct {
	ImagePullSecrets     []string `json:"imagePullSecrets"`
	NodeFeatureDiscovery struct {
		ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets"`
	} `json:"node-feature-discovery"`
	MaintenanceOperatorChart struct {
		ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets"`
	} `json:"maintenance-operator-chart"`
	SriovNetworkOperator struct {
		ImagePullSecrets []string `json:"imagePullSecrets"`
	} `json:"sriov-network-operator"`
}

func TestHelmValuesImagePullSecretShapes(t *testing.T) {
	profiles := []struct {
		name         string
		deploysSriov bool
	}{
		{name: "host-device-rdma", deploysSriov: false},
		{name: "ipoib-rdma-shared", deploysSriov: false},
		{name: "macvlan-rdma-shared", deploysSriov: false},
		{name: "spectrum-x-ra2.1", deploysSriov: true},
		{name: "spectrum-x-ra2.2", deploysSriov: true},
		{name: "spectrum-x", deploysSriov: true},
		{name: "sriov-ethernet-rdma", deploysSriov: true},
		{name: "sriov-ib-rdma", deploysSriov: true},
	}
	secretNames := []string{"registry-secret", "true", "null", "123"}
	secretRefs := []corev1.LocalObjectReference{
		{Name: "registry-secret"},
		{Name: "true"},
		{Name: "null"},
		{Name: "123"},
	}

	for _, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			templatePath, err := filepath.Abs(filepath.Join(
				"..", "..", "profiles", profile.name, helmValuesTemplateName))
			require.NoError(t, err)

			cfg := &config.LaunchKitConfig{
				NetworkOperator: &config.NetworkOperatorConfig{
					Version:            "v26.7.0-beta.5",
					SelectedRelease:    "26.7",
					OperatorRepository: "nvcr.io/nvstaging/mellanox",
					ImagePullSecrets:   secretNames,
				},
				Maintenance: config.DefaultMaintenanceConfig(),
				Profile:     &config.Profile{},
			}

			rendered, err := ProcessTemplate(templatePath, cfg, "")
			require.NoError(t, err)

			var values helmPullSecretValues
			require.NoError(t, yaml.Unmarshal(
				[]byte(rendered[helmValuesTemplateName]), &values))

			assert.Equal(t, secretNames, values.ImagePullSecrets)
			assert.Equal(t, secretRefs, values.NodeFeatureDiscovery.ImagePullSecrets)
			assert.Equal(t, secretRefs, values.MaintenanceOperatorChart.ImagePullSecrets)
			if profile.deploysSriov {
				assert.Equal(t, secretNames, values.SriovNetworkOperator.ImagePullSecrets)
			} else {
				assert.Empty(t, values.SriovNetworkOperator.ImagePullSecrets)
			}
		})
	}
}

func TestProfilePolicyImagePullSecretsRemainStrings(t *testing.T) {
	configPath, err := filepath.Abs(filepath.Join(
		"testdata", "grouping", "mixed-same-type.yaml"))
	require.NoError(t, err)

	cfg, err := config.LoadFullConfig(configPath, logr.Discard())
	require.NoError(t, err)
	cfg.NetworkOperator.SelectedRelease = "26.7"
	cfg.NetworkOperator.ImagePullSecrets = []string{"registry-secret", "true", "null", "123"}
	cfg.Profile = &config.Profile{Multirail: true}

	profiles := []string{
		"host-device-rdma",
		"ipoib-rdma-shared",
		"macvlan-rdma-shared",
		"spectrum-x-ra2.1",
		"spectrum-x-ra2.2",
		"spectrum-x",
		"sriov-ethernet-rdma",
		"sriov-ib-rdma",
	}

	for _, profile := range profiles {
		for _, templateName := range []string{"10-nicclusterpolicy.yaml", "11-nicnodepolicy.yaml"} {
			t.Run(profile+"/"+templateName, func(t *testing.T) {
				templatePath, err := filepath.Abs(filepath.Join(
					"..", "..", "profiles", profile, templateName))
				require.NoError(t, err)
				_, err = os.Stat(templatePath)
				if os.IsNotExist(err) {
					t.Skip("profile does not use this policy kind")
				}
				require.NoError(t, err)

				rendered, err := ProcessTemplate(templatePath, cfg, "")
				require.NoError(t, err)
				for fileName, content := range rendered {
					var document map[string]any
					require.NoError(t, yaml.Unmarshal([]byte(content), &document), fileName)
					assertImagePullSecretItemsAreStrings(t, document, fileName)
				}
			})
		}
	}
}

func assertImagePullSecretItemsAreStrings(t *testing.T, value any, path string) {
	t.Helper()

	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			childPath := path + "." + key
			if key == "imagePullSecrets" {
				items, ok := child.([]any)
				require.True(t, ok, "%s must be a list", childPath)
				for index, item := range items {
					assert.IsType(t, "", item, "%s[%d] must remain a string", childPath, index)
				}
				continue
			}
			assertImagePullSecretItemsAreStrings(t, child, childPath)
		}
	case []any:
		for _, child := range typed {
			assertImagePullSecretItemsAreStrings(t, child, path)
		}
	}
}
