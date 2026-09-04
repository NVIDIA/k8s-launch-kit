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
	yaml "gopkg.in/yaml.v2"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

type renderedOFEDPolicy struct {
	Kind string `yaml:"kind"`
	Spec struct {
		OFEDDriver *struct {
			Env []config.DOCADriverEnvVar `yaml:"env"`
		} `yaml:"ofedDriver"`
	} `yaml:"spec"`
}

var defaultMOFEDEnv = []mofedEnvVar{
	{Name: "UNLOAD_STORAGE_MODULES", Value: "true"},
	{Name: "ENABLE_NFSRDMA", Value: "false"},
	{Name: "UNLOAD_THIRD_PARTY_RDMA_MODULES", Value: "true"},
	{Name: "SKIP_PREFLIGHT_CHECKS", Value: "false"},
}

func TestMOFEDEnvMerge(t *testing.T) {
	t.Run("unset custom env preserves generated values", func(t *testing.T) {
		driver := &config.DOCADriverConfig{
			UnloadStorageModules:        true,
			UnloadThirdPartyRDMAModules: true,
		}

		require.Equal(t, defaultMOFEDEnv, mofedEnv(driver))
	})

	t.Run("custom env overrides generated and earlier custom values", func(t *testing.T) {
		driver := &config.DOCADriverConfig{
			UnloadStorageModules:        true,
			UnloadThirdPartyRDMAModules: true,
			Env: []config.DOCADriverEnvVar{
				{Name: "UNLOAD_STORAGE_MODULES", Value: "false"},
				{Name: "THIRD_PARTY_RDMA_MODULES", Value: "nvidia_peermem"},
				{Name: "true", Value: "yaml-significant"},
				{Name: "CUSTOM_OPTION", Value: "first"},
				{Name: "CUSTOM_OPTION", Value: "last"},
			},
		}

		require.Equal(t, []mofedEnvVar{
			{Name: "UNLOAD_STORAGE_MODULES", Value: "false", Custom: true},
			{Name: "ENABLE_NFSRDMA", Value: "false"},
			{Name: "UNLOAD_THIRD_PARTY_RDMA_MODULES", Value: "true"},
			{Name: "SKIP_PREFLIGHT_CHECKS", Value: "false"},
			{Name: "THIRD_PARTY_RDMA_MODULES", Value: "nvidia_peermem", Custom: true},
			{Name: "true", Value: "yaml-significant", Custom: true},
			{Name: "CUSTOM_OPTION", Value: "last", Custom: true},
		}, mofedEnv(driver))
	})
}

func TestCustomMOFEDEnvRendersAcrossProfilesAndPolicyKinds(t *testing.T) {
	profiles := []struct {
		dir        string
		fabric     string
		deployment string
	}{
		{dir: "host-device-rdma", fabric: "ethernet", deployment: "host_device"},
		{dir: "ipoib-rdma-shared", fabric: "infiniband", deployment: "rdma_shared"},
		{dir: "macvlan-rdma-shared", fabric: "ethernet", deployment: "rdma_shared"},
		{dir: "sriov-ethernet-rdma", fabric: "ethernet", deployment: "sriov"},
		{dir: "sriov-ib-rdma", fabric: "infiniband", deployment: "sriov"},
	}

	for _, profile := range profiles {
		for _, release := range []string{"26.1", "26.4"} {
			t.Run(profile.dir+"/"+release, func(t *testing.T) {
				ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
				cfg, err := config.LoadFullConfig(
					filepath.Join("testdata", "grouping", "mixed-same-type.yaml"), ctrllog.Log)
				require.NoError(t, err)
				cfg.NetworkOperator.SelectedRelease = release
				cfg.Profile = &config.Profile{
					Fabric:     profile.fabric,
					Deployment: profile.deployment,
					Multirail:  true,
				}
				cfg.DOCADriver.Env = []config.DOCADriverEnvVar{
					{Name: "UNLOAD_STORAGE_MODULES", Value: "false"},
					{Name: "SKIP_PREFLIGHT_CHECKS", Value: "true"},
					{Name: "THIRD_PARTY_RDMA_MODULES", Value: "nvidia_peermem"},
					{Name: "true", Value: "yaml-significant"},
				}

				rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
					loadProfileFromDir(t, profile.dir), cfg)
				require.NoError(t, err)

				wantKind := "NicNodePolicy"
				if profile.dir == "host-device-rdma" || release == "26.1" {
					wantKind = "NicClusterPolicy"
				}
				assertRenderedMOFEDEnv(t, rendered, wantKind, []config.DOCADriverEnvVar{
					{Name: "UNLOAD_STORAGE_MODULES", Value: "false"},
					{Name: "ENABLE_NFSRDMA", Value: "false"},
					{Name: "UNLOAD_THIRD_PARTY_RDMA_MODULES", Value: "true"},
					{Name: "SKIP_PREFLIGHT_CHECKS", Value: "true"},
					{Name: "THIRD_PARTY_RDMA_MODULES", Value: "nvidia_peermem"},
					{Name: "true", Value: "yaml-significant"},
				})
			})
		}
	}
}

func assertRenderedMOFEDEnv(
	t *testing.T,
	rendered map[string]string,
	wantKind string,
	want []config.DOCADriverEnvVar,
) {
	t.Helper()
	found := 0
	for name, content := range rendered {
		var policy renderedOFEDPolicy
		if err := yaml.Unmarshal([]byte(content), &policy); err != nil || policy.Kind != wantKind || policy.Spec.OFEDDriver == nil {
			continue
		}
		found++
		require.Contains(t, content, `- name: "true"`, name)
		require.Equal(t, want, policy.Spec.OFEDDriver.Env, name)
		seen := make(map[string]struct{}, len(policy.Spec.OFEDDriver.Env))
		for _, env := range policy.Spec.OFEDDriver.Env {
			_, duplicate := seen[env.Name]
			require.Falsef(t, duplicate, "duplicate env %q in %s", env.Name, name)
			seen[env.Name] = struct{}{}
		}
	}
	require.Positive(t, found, "no %s with ofedDriver rendered", wantKind)
}
