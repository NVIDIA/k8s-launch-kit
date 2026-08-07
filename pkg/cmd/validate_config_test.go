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

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newValidateRequestTestCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().BoolVar(&validateConnectivity, "connectivity", true, "")
	cmd.Flags().StringVar(&validateMode, "validation-mode", "", "")
	cmd.Flags().StringSliceVar(&validateChecks, "validation-checks", nil, "")
	cmd.Flags().IntVar(&validateRDMAIterations, "rdma-rping-iterations", 0, "")
	cmd.Flags().IntVar(&validateRDMAIBWriteSize, "rdma-ib-write-size", 0, "")
	cmd.Flags().Float64Var(&validateRDMAIBWriteMinGbps, "rdma-ib-write-min-bandwidth-gbps", 0, "")
	return cmd
}

func resetValidateRequestGlobals(t *testing.T) {
	t.Helper()
	validateConnectivity = true
	validateMode = ""
	validateChecks = nil
	validateRDMAIterations = 0
	validateRDMAIBWriteSize = 0
	validateRDMAIBWriteMinGbps = 0
}

func TestNewHostValidateRequestCapturesExplicitValues(t *testing.T) {
	t.Cleanup(func() { resetValidateRequestGlobals(t) })
	resetValidateRequestGlobals(t)
	cmd := newValidateRequestTestCommand()

	require.NoError(t, cmd.Flags().Set("connectivity", "false"))
	require.NoError(t, cmd.Flags().Set("validation-mode", "full"))
	require.NoError(t, cmd.Flags().Set("validation-checks", ""))
	require.NoError(t, cmd.Flags().Set("rdma-rping-iterations", "11"))
	require.NoError(t, cmd.Flags().Set("rdma-ib-write-size", "8192"))
	require.NoError(t, cmd.Flags().Set("rdma-ib-write-min-bandwidth-gbps", "0"))

	request := newHostValidateRequest(cmd)
	assert.True(t, request.Connectivity.Set)
	assert.False(t, request.Connectivity.Value)
	assert.True(t, request.Mode.Set)
	assert.Equal(t, "full", request.Mode.Value)
	assert.True(t, request.Checks.Set)
	assert.Empty(t, request.Checks.Value)
	assert.Equal(t, 11, request.RDMAPIterations.Value)
	assert.True(t, request.RDMAPIterations.Set)
	assert.Equal(t, 8192, request.RDMAIBWriteSize.Value)
	assert.True(t, request.RDMAIBWriteSize.Set)
	assert.Zero(t, request.RDMAMinBandwidth.Value)
	assert.True(t, request.RDMAMinBandwidth.Set)
}

func TestNewHostValidateRequestPreservesOmission(t *testing.T) {
	t.Cleanup(func() { resetValidateRequestGlobals(t) })
	resetValidateRequestGlobals(t)

	request := newHostValidateRequest(newValidateRequestTestCommand())
	assert.False(t, request.Connectivity.Set)
	assert.False(t, request.Mode.Set)
	assert.False(t, request.Checks.Set)
	assert.False(t, request.RDMAPIterations.Set)
	assert.False(t, request.RDMAIBWriteSize.Set)
	assert.False(t, request.RDMAMinBandwidth.Set)
}
