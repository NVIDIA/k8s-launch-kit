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
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindOrDownloadSosreportScriptUsesExistingScript(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	existingPath := filepath.Join(dir, sosreportScriptName)
	require.NoError(t, os.WriteFile(existingPath, []byte("existing"), 0o755))

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("downloaded"))
	}))
	t.Cleanup(server.Close)

	installPath := filepath.Join(dir, "install", sosreportScriptName)
	path, err := findOrDownloadSosreportScript(
		context.Background(),
		server.Client(),
		server.URL,
		[]string{existingPath},
		installPath,
	)

	require.NoError(t, err)
	assert.Equal(t, existingPath, path)
	assert.Equal(t, int32(0), requests.Load())
	_, err = os.Stat(installPath)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestFindOrDownloadSosreportScriptDownloadsMissingScript(t *testing.T) {
	t.Parallel()

	const script = "#!/bin/bash\necho sosreport\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/kubectl-netop_sosreport", r.URL.Path)
		_, _ = w.Write([]byte(script))
	}))
	t.Cleanup(server.Close)

	installPath := filepath.Join(t.TempDir(), "share", "l8k", "scripts", sosreportScriptName)
	path, err := findOrDownloadSosreportScript(
		context.Background(),
		server.Client(),
		server.URL+"/kubectl-netop_sosreport",
		[]string{filepath.Join(t.TempDir(), "missing")},
		installPath,
	)

	require.NoError(t, err)
	assert.Equal(t, installPath, path)
	contents, err := os.ReadFile(installPath)
	require.NoError(t, err)
	assert.Equal(t, script, string(contents))
	info, err := os.Stat(installPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestFindOrDownloadSosreportScriptReportsDownloadLocation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	installPath := filepath.Join(t.TempDir(), "share", "l8k", "scripts", sosreportScriptName)
	_, err := findOrDownloadSosreportScript(
		context.Background(),
		server.Client(),
		server.URL,
		[]string{filepath.Join(t.TempDir(), "missing")},
		installPath,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), server.URL)
	assert.Contains(t, err.Error(), installPath)
	assert.Contains(t, err.Error(), "503 Service Unavailable")
	_, statErr := os.Stat(installPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestDownloadSosreportScriptRejectsEmptyResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(server.Close)

	installPath := filepath.Join(t.TempDir(), sosreportScriptName)
	err := downloadSosreportScript(context.Background(), server.Client(), server.URL, installPath)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "downloaded script is empty")
	_, statErr := os.Stat(installPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestManualSosreportInstallSuggestion(t *testing.T) {
	t.Parallel()

	installPath := filepath.Join("opt", "l8k", "share", "l8k", "scripts", sosreportScriptName)
	suggestion := manualSosreportInstallSuggestion(installPath)

	assert.Contains(t, suggestion, sosreportScriptURL)
	assert.Contains(t, suggestion, installPath)
	assert.Contains(t, suggestion, "next to the l8k installation")
	assert.NotContains(t, suggestion, "make download-sosreport")
}

func TestSosreportScriptLocationsUseInstallationPrefix(t *testing.T) {
	t.Parallel()

	candidates, installPath := sosreportScriptLocations(filepath.Join("opt", "l8k", "bin", "l8k"))

	expected := filepath.Join("opt", "l8k", "share", "l8k", "scripts", sosreportScriptName)
	assert.Equal(t, expected, installPath)
	assert.Equal(t, expected, candidates[len(candidates)-1])
}
