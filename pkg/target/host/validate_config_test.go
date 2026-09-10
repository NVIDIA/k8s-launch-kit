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

package host

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/connectivity"
)

func TestApplyValidationOverrides(t *testing.T) {
	minimumBandwidth := 200.0
	validation := config.NormalizeValidationConfig(&config.ValidationConfig{
		Connectivity: boolPointer(false),
		Checks:       []string{config.ValidationCheckRPing},
		Mode:         config.ValidationModeQuick,
		RDMA: &config.ValidationRDMAConfig{
			RPingIterations:         3,
			IBWriteSize:             4096,
			IBWriteMinBandwidthGbps: &minimumBandwidth,
		},
	})

	err := applyValidationOverrides(ValidateRequest{
		Connectivity:     Explicit[bool]{Value: true, Set: true},
		Mode:             Explicit[string]{Value: " full ", Set: true},
		Checks:           Explicit[[]string]{Value: []string{config.ValidationCheckIBWriteBW}, Set: true},
		RDMAPIterations:  Explicit[int]{Value: 11, Set: true},
		RDMAIBWriteSize:  Explicit[int]{Value: 8192, Set: true},
		RDMAMinBandwidth: Explicit[float64]{Value: 0, Set: true},
	}, validation)

	require.NoError(t, err)
	require.NotNil(t, validation.Connectivity)
	assert.True(t, *validation.Connectivity)
	assert.Equal(t, config.ValidationModeFull, validation.Mode)
	assert.Equal(t, []string{config.ValidationCheckIBWriteBW}, validation.Checks)
	assert.Equal(t, 11, validation.RDMA.RPingIterations)
	assert.Equal(t, 8192, validation.RDMA.IBWriteSize)
	require.NotNil(t, validation.RDMA.IBWriteMinBandwidthGbps)
	assert.Zero(t, *validation.RDMA.IBWriteMinBandwidthGbps)
}

func TestValidationChecksProjection(t *testing.T) {
	validation := config.NormalizeValidationConfig(&config.ValidationConfig{
		Checks: []string{config.ValidationCheckIBWriteBW},
		GPUDirect: config.ValidationGPUDirectConfig{
			Enabled:         true,
			GPUResourceType: "example.com/gpu",
		},
	})
	assert.Equal(t,
		[]connectivity.Check{connectivity.CheckIBWriteBW, connectivity.CheckGPUDirectDMABuf},
		connectivityChecksFromConfig(validation),
	)

	validation.Checks = []string{config.ValidationCheckRPing}
	assert.Equal(t, []connectivity.Check{connectivity.CheckRPing}, connectivityChecksFromConfig(validation))
}

func TestExplicitEmptyValidationChecksDisableConnectivityTests(t *testing.T) {
	validation := config.NormalizeValidationConfig(nil)

	require.NoError(t, applyValidationOverrides(ValidateRequest{
		Checks: Explicit[[]string]{Value: []string{}, Set: true},
	}, validation))

	assert.Empty(t, validation.Checks)
	assert.Empty(t, connectivityChecksFromConfig(validation))
}

func TestApplyValidationOverridesRejectsNilConfig(t *testing.T) {
	assert.ErrorContains(t, applyValidationOverrides(ValidateRequest{}, nil), "must not be nil")
}

func TestValidateRequiredConnectivityConfig(t *testing.T) {
	load := func(t *testing.T, content string) (string, *config.LaunchKitConfig) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "cluster-config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		cfg, err := config.LoadFullConfig(path, logr.Discard())
		require.NoError(t, err)
		return path, cfg
	}

	t.Run("accepts minimal explicit contract", func(t *testing.T) {
		path, cfg := load(t, `profile:
  routing: source-based
validation:
  gpuDirect:
    enabled: false
`)
		assert.NoError(t, validateRequiredConnectivityConfig(path, cfg,
			[]connectivity.Check{connectivity.CheckICMP, connectivity.CheckIBWriteBW}))
	})

	t.Run("requires user-owned path", func(t *testing.T) {
		assert.ErrorContains(t, validateRequiredConnectivityConfig("", &config.LaunchKitConfig{}, nil),
			"user-owned cluster-config.yaml")
	})

	t.Run("requires routing", func(t *testing.T) {
		path, cfg := load(t, `validation:
  gpuDirect:
    enabled: false
`)
		assert.ErrorContains(t, validateRequiredConnectivityConfig(path, cfg, nil), "profile.routing")
	})

	t.Run("rejects unknown routing", func(t *testing.T) {
		path, cfg := load(t, `profile:
  routing: automatic
validation:
  gpuDirect:
    enabled: false
`)
		assert.ErrorContains(t, validateRequiredConnectivityConfig(path, cfg, nil), "source-based")
	})

	t.Run("requires explicit GPUDirect decision", func(t *testing.T) {
		path, cfg := load(t, `profile:
  routing: destination-based
`)
		assert.ErrorContains(t, validateRequiredConnectivityConfig(path, cfg, nil), "validation.gpuDirect.enabled")
	})
}

func TestValidateRequiredConnectivityConfigGPUDirectTopology(t *testing.T) {
	rail := 0
	validGroups := []config.ClusterConfig{{
		Identifier:  "workers",
		WorkerNodes: []string{"worker-0"},
		PFs: []config.PFConfig{{
			Traffic:      "east-west",
			Rail:         &rail,
			ConnectedGPU: "GPU3",
		}},
	}}
	checks := []connectivity.Check{connectivity.CheckIBWriteBW, connectivity.CheckGPUDirectDMABuf}

	write := func(t *testing.T, groups []config.ClusterConfig) (string, *config.LaunchKitConfig) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "cluster-config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(`profile:
  routing: source-based
validation:
  gpuDirect:
    enabled: true
`), 0o600))
		cfg, err := config.LoadFullConfig(path, logr.Discard())
		require.NoError(t, err)
		cfg.ClusterConfig = groups
		return path, cfg
	}

	t.Run("accepts complete topology", func(t *testing.T) {
		path, cfg := write(t, validGroups)
		assert.NoError(t, validateRequiredConnectivityConfig(path, cfg, checks))
	})

	t.Run("requires worker groups", func(t *testing.T) {
		path, cfg := write(t, nil)
		assert.ErrorContains(t, validateRequiredConnectivityConfig(path, cfg, checks), "at least one worker group")
	})

	t.Run("requires workers", func(t *testing.T) {
		groups := append([]config.ClusterConfig(nil), validGroups...)
		groups[0].WorkerNodes = nil
		path, cfg := write(t, groups)
		assert.ErrorContains(t, validateRequiredConnectivityConfig(path, cfg, checks), "workerNodes")
	})

	t.Run("requires rail", func(t *testing.T) {
		groups := append([]config.ClusterConfig(nil), validGroups...)
		groups[0].PFs = append([]config.PFConfig(nil), groups[0].PFs...)
		groups[0].PFs[0].Rail = nil
		path, cfg := write(t, groups)
		assert.ErrorContains(t, validateRequiredConnectivityConfig(path, cfg, checks), "non-negative rail")
	})

	t.Run("requires GPU index", func(t *testing.T) {
		groups := append([]config.ClusterConfig(nil), validGroups...)
		groups[0].PFs = append([]config.PFConfig(nil), groups[0].PFs...)
		groups[0].PFs[0].ConnectedGPU = "3"
		path, cfg := write(t, groups)
		assert.ErrorContains(t, validateRequiredConnectivityConfig(path, cfg, checks), "GPU<N>")
	})

	t.Run("rejects workers shared by groups", func(t *testing.T) {
		groups := append([]config.ClusterConfig(nil), validGroups...)
		groups = append(groups, config.ClusterConfig{
			Identifier:  "other-workers",
			WorkerNodes: []string{"worker-0"},
			PFs: []config.PFConfig{{
				Traffic:      "east-west",
				Rail:         &rail,
				ConnectedGPU: "GPU4",
			}},
		})
		path, cfg := write(t, groups)
		assert.ErrorContains(t, validateRequiredConnectivityConfig(path, cfg, checks), "belongs to both")
	})

	t.Run("does not require topology when GPUDirect check is absent", func(t *testing.T) {
		path, cfg := write(t, nil)
		assert.NoError(t, validateRequiredConnectivityConfig(path, cfg,
			[]connectivity.Check{connectivity.CheckIBWriteBW}))
	})
}

func boolPointer(value bool) *bool {
	return &value
}
