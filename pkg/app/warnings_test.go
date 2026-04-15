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
	"fmt"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/assert"
)

// testOutput captures Warning calls for assertion. It implements ui.Output.
type testOutput struct {
	warnings []string
}

func (t *testOutput) Info(format string, args ...interface{})    {}
func (t *testOutput) Success(format string, args ...interface{}) {}
func (t *testOutput) Warning(format string, args ...interface{}) {
	t.warnings = append(t.warnings, fmt.Sprintf(format, args...))
}
func (t *testOutput) Error(format string, args ...interface{})       {}
func (t *testOutput) StartProgress(message string) ui.Progress      { return &noopProgress{} }
func (t *testOutput) Header(text string)                             {}
func (t *testOutput) Section(text string)                            {}
func (t *testOutput) Confirm(prompt string) (bool, error)            { return false, nil }

type noopProgress struct{}

func (p *noopProgress) Update(message string)  {}
func (p *noopProgress) Success(message string) {}
func (p *noopProgress) Fail(message string)    {}

func TestWarnThirdPartyRDMAModules_DriverDisabled(t *testing.T) {
	out := &testOutput{}
	cfg := &config.LaunchKubernetesConfig{
		DOCADriver: &config.DOCADriverConfig{Enable: false, UnloadThirdPartyRDMAModules: true},
		ClusterConfig: []config.ClusterConfig{
			{Identifier: "g1", ThirdPartyRDMAModules: []string{"rdma_rxe"}},
		},
	}
	warnThirdPartyRDMAModules(cfg, "discover", out)
	assert.Empty(t, out.warnings)
}

func TestWarnThirdPartyRDMAModules_NilDriver(t *testing.T) {
	out := &testOutput{}
	cfg := &config.LaunchKubernetesConfig{
		DOCADriver: nil,
		ClusterConfig: []config.ClusterConfig{
			{Identifier: "g1", ThirdPartyRDMAModules: []string{"rdma_rxe"}},
		},
	}
	warnThirdPartyRDMAModules(cfg, "discover", out)
	assert.Empty(t, out.warnings)
}

func TestWarnThirdPartyRDMAModules_FlagFalse_NoWarning(t *testing.T) {
	out := &testOutput{}
	cfg := &config.LaunchKubernetesConfig{
		DOCADriver: &config.DOCADriverConfig{Enable: true, UnloadThirdPartyRDMAModules: false},
		ClusterConfig: []config.ClusterConfig{
			{Identifier: "g1", ThirdPartyRDMAModules: []string{"rdma_rxe"}},
		},
	}
	warnThirdPartyRDMAModules(cfg, "discover", out)
	assert.Empty(t, out.warnings, "no warning when flag is false — discovery auto-enables it")
}

func TestWarnThirdPartyRDMAModules_AutoEnabled_Discover(t *testing.T) {
	out := &testOutput{}
	cfg := &config.LaunchKubernetesConfig{
		DOCADriver: &config.DOCADriverConfig{Enable: true, UnloadThirdPartyRDMAModules: true},
		ClusterConfig: []config.ClusterConfig{
			{Identifier: "g1", ThirdPartyRDMAModules: []string{"rdma_rxe"}},
		},
	}
	warnThirdPartyRDMAModules(cfg, "discover", out)
	assert.Len(t, out.warnings, 1)
	assert.Contains(t, out.warnings[0], "unloadThirdPartyRDMAModules is enabled")
	assert.Contains(t, out.warnings[0], "rdma_rxe")
	assert.Contains(t, out.warnings[0], "g1")
	assert.Contains(t, out.warnings[0], "Verify it is safe")
}

func TestWarnThirdPartyRDMAModules_AutoEnabled_Generate(t *testing.T) {
	out := &testOutput{}
	cfg := &config.LaunchKubernetesConfig{
		DOCADriver: &config.DOCADriverConfig{Enable: true, UnloadThirdPartyRDMAModules: true},
		ClusterConfig: []config.ClusterConfig{
			{Identifier: "g1", ThirdPartyRDMAModules: []string{"rdma_rxe"}},
		},
	}
	warnThirdPartyRDMAModules(cfg, "generate", out)
	assert.Len(t, out.warnings, 1)
	assert.Contains(t, out.warnings[0], "unloadThirdPartyRDMAModules is enabled")
	assert.Contains(t, out.warnings[0], "Verify it is safe")
}

func TestWarnThirdPartyRDMAModules_NoModules(t *testing.T) {
	out := &testOutput{}
	cfg := &config.LaunchKubernetesConfig{
		DOCADriver: &config.DOCADriverConfig{Enable: true, UnloadThirdPartyRDMAModules: true},
		ClusterConfig: []config.ClusterConfig{
			{Identifier: "g1", ThirdPartyRDMAModules: nil},
			{Identifier: "g2", ThirdPartyRDMAModules: []string{}},
		},
	}
	warnThirdPartyRDMAModules(cfg, "generate", out)
	assert.Empty(t, out.warnings)
}

func TestWarnThirdPartyRDMAModules_MultipleGroups(t *testing.T) {
	out := &testOutput{}
	cfg := &config.LaunchKubernetesConfig{
		DOCADriver: &config.DOCADriverConfig{Enable: true, UnloadThirdPartyRDMAModules: true},
		ClusterConfig: []config.ClusterConfig{
			{Identifier: "group-a", ThirdPartyRDMAModules: []string{"rdma_rxe"}},
			{Identifier: "group-b", ThirdPartyRDMAModules: []string{"qedr", "siw"}},
		},
	}
	warnThirdPartyRDMAModules(cfg, "discover", out)
	assert.Len(t, out.warnings, 2)
	assert.Contains(t, out.warnings[0], "group-a")
	assert.Contains(t, out.warnings[0], "rdma_rxe")
	assert.Contains(t, out.warnings[1], "group-b")
	assert.Contains(t, out.warnings[1], "qedr, siw")
}

func TestWarnStorageModules_FlagFalse_NoWarning(t *testing.T) {
	out := &testOutput{}
	cfg := &config.LaunchKubernetesConfig{
		DOCADriver: &config.DOCADriverConfig{Enable: true, UnloadStorageModules: false},
		ClusterConfig: []config.ClusterConfig{
			{Identifier: "g1", StorageModules: []string{"nvme_rdma", "ib_isert"}},
		},
	}
	warnStorageModules(cfg, "discover", out)
	assert.Empty(t, out.warnings, "no warning when flag is false — discovery auto-enables it")
}

func TestWarnStorageModules_AutoEnabled_Discover(t *testing.T) {
	out := &testOutput{}
	cfg := &config.LaunchKubernetesConfig{
		DOCADriver: &config.DOCADriverConfig{Enable: true, UnloadStorageModules: true},
		ClusterConfig: []config.ClusterConfig{
			{Identifier: "g1", StorageModules: []string{"nvme_rdma"}},
		},
	}
	warnStorageModules(cfg, "discover", out)
	assert.Len(t, out.warnings, 1)
	assert.Contains(t, out.warnings[0], "unloadStorageModules is enabled")
	assert.Contains(t, out.warnings[0], "nvme_rdma")
	assert.Contains(t, out.warnings[0], "Verify")
}

func TestWarnStorageModules_AutoEnabled_Generate(t *testing.T) {
	out := &testOutput{}
	cfg := &config.LaunchKubernetesConfig{
		DOCADriver: &config.DOCADriverConfig{Enable: true, UnloadStorageModules: true},
		ClusterConfig: []config.ClusterConfig{
			{Identifier: "g1", StorageModules: []string{"nvme_rdma"}},
		},
	}
	warnStorageModules(cfg, "generate", out)
	assert.Len(t, out.warnings, 1)
	assert.Contains(t, out.warnings[0], "unloadStorageModules is enabled")
	assert.Contains(t, out.warnings[0], "Verify")
}

func TestWarnStorageModules_NoModules(t *testing.T) {
	out := &testOutput{}
	cfg := &config.LaunchKubernetesConfig{
		DOCADriver: &config.DOCADriverConfig{Enable: true, UnloadStorageModules: true},
		ClusterConfig: []config.ClusterConfig{
			{Identifier: "g1", StorageModules: nil},
		},
	}
	warnStorageModules(cfg, "generate", out)
	assert.Empty(t, out.warnings)
}

func TestWarnStorageModules_DriverDisabled(t *testing.T) {
	out := &testOutput{}
	cfg := &config.LaunchKubernetesConfig{
		DOCADriver: &config.DOCADriverConfig{Enable: false, UnloadStorageModules: true},
		ClusterConfig: []config.ClusterConfig{
			{Identifier: "g1", StorageModules: []string{"nvme_rdma"}},
		},
	}
	warnStorageModules(cfg, "discover", out)
	assert.Empty(t, out.warnings)
}
