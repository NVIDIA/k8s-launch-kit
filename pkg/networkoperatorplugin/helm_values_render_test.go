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
	secretNames := []string{"registry-secret", "backup-secret"}
	secretRefs := []corev1.LocalObjectReference{
		{Name: "registry-secret"},
		{Name: "backup-secret"},
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
