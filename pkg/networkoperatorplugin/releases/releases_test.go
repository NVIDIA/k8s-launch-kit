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
	"testing"

	"github.com/Masterminds/semver/v3"
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
		assert.NotEmpty(t, r.Validation.Image, "key %s: validation.image", key)
	}
}

func TestLookupRelease_ArtifactDestinationsMatchVersion(t *testing.T) {
	for _, key := range SupportedReleases() {
		t.Run(key, func(t *testing.T) {
			r, ok := LookupRelease(key)
			require.True(t, ok, "expected catalog entry for %q", key)

			version, err := semver.NewVersion(r.NetworkOperator.Version)
			require.NoError(t, err, "release %s has an invalid Network Operator version", key)
			assert.Equal(t, "network-operator-"+r.NetworkOperator.Version, r.NetworkOperator.ComponentVersion)
			assert.NotEmpty(t, r.DOCADriver.Version)

			if version.Prerelease() != "" {
				assert.Equal(t, "nvcr.io/nvstaging/mellanox", r.NetworkOperator.Repository)
				assert.Equal(t, "nvcr.io/nvstaging/mellanox", r.NetworkOperator.OperatorRepository)
				assert.Equal(t, "https://helm.ngc.nvidia.com/nvstaging/mellanox", r.NetworkOperator.HelmRepoURL)
				return
			}

			assert.Equal(t, "nvcr.io/nvidia/mellanox", r.NetworkOperator.Repository)
			assert.Equal(t, "nvcr.io/nvidia/cloud-native", r.NetworkOperator.OperatorRepository)
			assert.Equal(t, "https://helm.ngc.nvidia.com/nvidia", r.NetworkOperator.HelmRepoURL)
		})
	}
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

func TestLookupRelease_267UsesPublicXPlaneImage(t *testing.T) {
	r, ok := LookupRelease("26.7")
	require.True(t, ok)
	assert.Equal(t, "nvcr.io/nvidia/doca", r.XPlane.Repository)
	assert.Equal(t, "doca-3.5.0", r.XPlane.Version)
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
