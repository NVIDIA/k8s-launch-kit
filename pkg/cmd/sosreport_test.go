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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSosreportScriptLocationsUseInvokedInstallationPrefix(t *testing.T) {
	t.Parallel()

	invokedExecutablePath := filepath.Join("/opt", "l8k", "bin", "l8k")
	resolvedExecutablePath := filepath.Join("/source", "k8s-launch-kit", "build", "l8k")
	candidates, installPath := sosreportScriptLocations(invokedExecutablePath, resolvedExecutablePath)

	expected := filepath.Join("/opt", "l8k", "share", "l8k", "scripts", sosreportScriptName)
	assert.Equal(t, expected, installPath)
	assert.Contains(t, candidates, expected)
	assert.Contains(t, candidates, filepath.Join("/source", "k8s-launch-kit", "share", "l8k", "scripts", sosreportScriptName))
}

func TestSosreportScriptLocationsFallBackToResolvedExecutable(t *testing.T) {
	t.Parallel()

	resolvedExecutablePath := filepath.Join("/opt", "l8k", "bin", "l8k")
	candidates, installPath := sosreportScriptLocations("", resolvedExecutablePath)

	expected := filepath.Join("/opt", "l8k", "share", "l8k", "scripts", sosreportScriptName)
	assert.Equal(t, expected, installPath)
	assert.Contains(t, candidates, expected)
}

func TestManualSosreportInstallSuggestion(t *testing.T) {
	t.Parallel()

	installPath := filepath.Join("/opt", "l8k", "share", "l8k", "scripts", sosreportScriptName)
	suggestion := manualSosreportInstallSuggestion(installPath)

	assert.Contains(t, suggestion, sosreportScriptURL)
	assert.Contains(t, suggestion, installPath)
	assert.Contains(t, suggestion, "next to the l8k installation")
	assert.NotContains(t, suggestion, "make download-sosreport")
}
