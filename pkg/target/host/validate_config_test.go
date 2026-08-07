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
	"testing"

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

func boolPointer(value bool) *bool {
	return &value
}
