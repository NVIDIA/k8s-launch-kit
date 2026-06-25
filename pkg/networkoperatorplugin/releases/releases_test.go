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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupRelease_Known(t *testing.T) {
	for _, key := range []string{"26.1", "26.4"} {
		r, ok := LookupRelease(key)
		require.True(t, ok, "expected catalog entry for %q", key)
		assert.NotEmpty(t, r.NetworkOperator.Version, "key %s: networkOperator.version", key)
		assert.NotEmpty(t, r.NetworkOperator.ComponentVersion, "key %s: networkOperator.componentVersion", key)
		assert.NotEmpty(t, r.NetworkOperator.Repository, "key %s: networkOperator.repository", key)
		assert.NotEmpty(t, r.DOCADriver.Version, "key %s: docaDriver.version", key)
	}
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

	// Ensure ascending order.
	for i := 1; i < len(got); i++ {
		assert.Less(t, got[i-1], got[i], "expected sorted output")
	}
}
