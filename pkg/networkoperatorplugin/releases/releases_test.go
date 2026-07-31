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

package releases

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupRelease_Known(t *testing.T) {
	for _, key := range []string{"26.1", "26.4", "26.7"} {
		r, ok := LookupRelease(key)
		require.True(t, ok, "expected catalog entry for %q", key)
		assert.NotEmpty(t, r.NetworkOperator.Version, "key %s: networkOperator.version", key)
		assert.NotEmpty(t, r.NetworkOperator.ComponentVersion, "key %s: networkOperator.componentVersion", key)
		assert.NotEmpty(t, r.NetworkOperator.Repository, "key %s: networkOperator.repository", key)
		assert.NotEmpty(t, r.DOCADriver.Version, "key %s: docaDriver.version", key)
	}
}

func TestLookupRelease_BetaReleasesUseStagingArtifacts(t *testing.T) {
	checkedBeta := false
	for _, key := range SupportedReleases() {
		r, ok := LookupRelease(key)
		require.True(t, ok, "expected catalog entry for %q", key)
		if !strings.Contains(r.NetworkOperator.Version, "-beta.") {
			continue
		}
		checkedBeta = true

		assert.Equal(t, "network-operator-"+r.NetworkOperator.Version, r.NetworkOperator.ComponentVersion)
		assert.Equal(t, "nvcr.io/nvstaging/mellanox", r.NetworkOperator.Repository)
		assert.Equal(t, "nvcr.io/nvstaging/mellanox", r.NetworkOperator.OperatorRepository)
		assert.Equal(t, "https://helm.ngc.nvidia.com/nvstaging/mellanox", r.NetworkOperator.HelmRepoURL)
		assert.NotEmpty(t, r.DOCADriver.Version)
	}
	require.True(t, checkedBeta, "expected at least one beta release catalog entry")
}

func TestLookupRelease_XPlaneArtifactsAreIndependent(t *testing.T) {
	for _, key := range []string{"26.4", "26.7"} {
		r, ok := LookupRelease(key)
		require.True(t, ok, "expected catalog entry for %q", key)
		assert.NotEmpty(t, r.XPlane.Repository, "key %s: xPlane.repository", key)
		assert.NotEmpty(t, r.XPlane.Version, "key %s: xPlane.version", key)
		assert.NotEqual(t, r.NetworkOperator.Repository, r.XPlane.Repository)
		assert.NotEqual(t, r.NetworkOperator.ComponentVersion, r.XPlane.Version)
	}

	r, ok := LookupRelease("26.1")
	require.True(t, ok)
	assert.Empty(t, r.XPlane.Repository)
	assert.Empty(t, r.XPlane.Version)
}

func TestLookupRelease_Unknown(t *testing.T) {
	_, ok := LookupRelease("99.0")
	assert.False(t, ok)

	_, ok = LookupRelease("")
	assert.False(t, ok)
}

func TestSupportedReleases_SortedAndContainsKnownKeys(t *testing.T) {
	got := SupportedReleases()
	require.NotEmpty(t, got)
	assert.Contains(t, got, "26.1")
	assert.Contains(t, got, "26.4")
	assert.Contains(t, got, "26.7")

	// Ensure ascending order.
	for i := 1; i < len(got); i++ {
		assert.Less(t, got[i-1], got[i], "expected sorted output")
	}
}
