// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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

package app

import (
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

// warnThirdPartyRDMAModules warns about third-party RDMA modules that will be
// unloaded. Discovery auto-enables the flag when modules are found, so this only
// emits the "verify safety" warning when the flag is true and modules are present.
func warnThirdPartyRDMAModules(cfg *config.LaunchKitConfig, _ string, output ui.Output) {
	if cfg.DOCADriver == nil || !cfg.DOCADriver.Enable || !cfg.DOCADriver.UnloadThirdPartyRDMAModules {
		return
	}

	for _, group := range cfg.ClusterConfig {
		if len(group.ThirdPartyRDMAModules) == 0 {
			continue
		}
		moduleList := strings.Join(group.ThirdPartyRDMAModules, ", ")
		output.Warning(
			"unloadThirdPartyRDMAModules is enabled. The DOCA driver will unload known third-party RDMA modules "+
				"(including [%s] found on group %s) before driver installation. "+
				"Verify it is safe to unload these modules before deploying.",
			moduleList, group.Identifier)
	}
}

// warnStorageModules warns about storage-over-RDMA modules that will be unloaded.
// Discovery auto-enables the flag when modules are found, so this only emits the
// "verify safety" warning when the flag is true and modules are present.
func warnStorageModules(cfg *config.LaunchKitConfig, _ string, output ui.Output) {
	if cfg.DOCADriver == nil || !cfg.DOCADriver.Enable || !cfg.DOCADriver.UnloadStorageModules {
		return
	}

	for _, group := range cfg.ClusterConfig {
		if len(group.StorageModules) == 0 {
			continue
		}
		moduleList := strings.Join(group.StorageModules, ", ")
		output.Warning(
			"unloadStorageModules is enabled. The DOCA driver will unload known storage-over-RDMA modules "+
				"(including [%s] found on group %s) before driver installation. "+
				"Verify that no running workloads depend on these modules before deploying.",
			moduleList, group.Identifier)
	}
}
