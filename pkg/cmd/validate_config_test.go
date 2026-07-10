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

package cmd

import (
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/connectivity"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newValidateOverrideTestCmd() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().BoolVar(&validateConnectivity, "connectivity", true, "")
	cmd.Flags().StringVar(&validateMode, "validation-mode", "", "")
	cmd.Flags().StringSliceVar(&validateChecks, "validation-checks", nil, "")
	cmd.Flags().IntVar(&validateRDMAIterations, "rdma-rping-iterations", 0, "")
	cmd.Flags().IntVar(&validateRDMAIBWriteSize, "rdma-ib-write-size", 0, "")
	cmd.Flags().Float64Var(&validateRDMAIBWriteMinGbps, "rdma-ib-write-min-bandwidth-gbps", 0, "")
	return cmd
}

func resetValidateOverrideGlobals(t *testing.T) {
	t.Helper()
	validateConnectivity = true
	validateMode = ""
	validateChecks = nil
	validateRDMAIterations = 0
	validateRDMAIBWriteSize = 0
	validateRDMAIBWriteMinGbps = 0
}

func TestApplyValidateFlagOverrides(t *testing.T) {
	t.Cleanup(func() { resetValidateOverrideGlobals(t) })

	t.Run("cli overrides config checks and rdma parameters", func(t *testing.T) {
		resetValidateOverrideGlobals(t)
		cmd := newValidateOverrideTestCmd()
		minBandwidthGbps := 200.0
		cfg := config.NormalizeValidationConfig(&config.ValidationConfig{
			Checks: []string{config.ValidationCheckRPing},
			Mode:   config.ValidationModeQuick,
			RDMA: &config.ValidationRDMAConfig{
				RPingIterations:         3,
				IBWriteSize:             4096,
				IBWriteMinBandwidthGbps: &minBandwidthGbps,
			},
		})

		require.NoError(t, cmd.Flags().Set("validation-mode", "full"))
		require.NoError(t, cmd.Flags().Set("validation-checks", "ib_write_bw"))
		require.NoError(t, cmd.Flags().Set("rdma-rping-iterations", "11"))
		require.NoError(t, cmd.Flags().Set("rdma-ib-write-size", "8192"))
		require.NoError(t, cmd.Flags().Set("rdma-ib-write-min-bandwidth-gbps", "800"))
		require.NoError(t, applyValidateFlagOverrides(cmd, cfg))

		assert.Equal(t, config.ValidationModeFull, cfg.Mode)
		assert.Equal(t, []string{config.ValidationCheckIBWriteBW}, cfg.Checks)
		assert.Equal(t, 11, cfg.RDMA.RPingIterations)
		assert.Equal(t, 8192, cfg.RDMA.IBWriteSize)
		require.NotNil(t, cfg.RDMA.IBWriteMinBandwidthGbps)
		assert.Equal(t, 800.0, *cfg.RDMA.IBWriteMinBandwidthGbps)
		assert.Equal(t, []connectivity.Check{connectivity.CheckIBWriteBW}, connectivityChecksFromConfig(cfg))
	})

	t.Run("cli can select icmp check", func(t *testing.T) {
		resetValidateOverrideGlobals(t)
		cmd := newValidateOverrideTestCmd()
		cfg := config.NormalizeValidationConfig(nil)

		require.NoError(t, cmd.Flags().Set("validation-checks", "icmp"))
		require.NoError(t, applyValidateFlagOverrides(cmd, cfg))

		assert.Equal(t, []string{config.ValidationCheckICMP}, cfg.Checks)
		assert.Equal(t, []connectivity.Check{connectivity.CheckICMP}, connectivityChecksFromConfig(cfg))
	})

	t.Run("zero minimum bandwidth override disables bandwidth gating", func(t *testing.T) {
		resetValidateOverrideGlobals(t)
		cmd := newValidateOverrideTestCmd()
		cfg := config.NormalizeValidationConfig(nil)

		require.NoError(t, cmd.Flags().Set("rdma-ib-write-min-bandwidth-gbps", "0"))
		require.NoError(t, applyValidateFlagOverrides(cmd, cfg))

		require.NotNil(t, cfg.RDMA.IBWriteMinBandwidthGbps)
		assert.Equal(t, 0.0, *cfg.RDMA.IBWriteMinBandwidthGbps)
	})

	t.Run("explicit empty check list disables connectivity tests", func(t *testing.T) {
		resetValidateOverrideGlobals(t)
		cmd := newValidateOverrideTestCmd()
		cfg := config.NormalizeValidationConfig(nil)

		require.NoError(t, cmd.Flags().Set("validation-checks", ""))
		require.NoError(t, applyValidateFlagOverrides(cmd, cfg))

		assert.Empty(t, cfg.Checks)
		assert.Empty(t, connectivityChecksFromConfig(cfg))
	})

	t.Run("connectivity flag can override config false", func(t *testing.T) {
		resetValidateOverrideGlobals(t)
		cmd := newValidateOverrideTestCmd()
		disabled := false
		cfg := config.NormalizeValidationConfig(&config.ValidationConfig{Connectivity: &disabled})

		require.NoError(t, cmd.Flags().Set("connectivity", "true"))
		require.NoError(t, applyValidateFlagOverrides(cmd, cfg))

		require.NotNil(t, cfg.Connectivity)
		assert.True(t, *cfg.Connectivity)
	})
}
