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

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type fakeGitHub struct {
	branches map[string]bool
	files    map[string]string
	requests []string
}

func (f *fakeGitHub) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	f.requests = append(f.requests, request.URL.RequestURI())

	const branchPrefix = "/repos/Mellanox/network-operator/branches/"
	if strings.HasPrefix(request.URL.Path, branchPrefix) {
		branch, err := url.PathUnescape(strings.TrimPrefix(request.URL.Path, branchPrefix))
		if err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		if !f.branches[branch] {
			http.NotFound(response, request)
			return
		}
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(`{}`))
		return
	}

	const contentsPath = "/repos/Mellanox/network-operator/contents/hack/release.yaml"
	if request.URL.Path == contentsPath {
		ref := request.URL.Query().Get("ref")
		releaseFile, ok := f.files[ref]
		if !ok {
			http.NotFound(response, request)
			return
		}
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(releaseFile))
		return
	}

	http.NotFound(response, request)
}

type catalogReleaseValues struct {
	NetworkOperator struct {
		Version            string `yaml:"version"`
		ComponentVersion   string `yaml:"componentVersion"`
		Repository         string `yaml:"repository"`
		OperatorRepository string `yaml:"operatorRepository"`
		HelmRepoURL        string `yaml:"helmRepoURL"`
	} `yaml:"networkOperator"`
	DOCADriver struct {
		Version string `yaml:"version"`
	} `yaml:"docaDriver"`
}

func TestSyncCatalogUpdatesAllReleasesAndFallsBackForLatest(t *testing.T) {
	catalogPath := writeTestCatalog(t, testCatalogYAML())
	fake := &fakeGitHub{
		branches: map[string]bool{
			"v26.1.x": true,
			"v26.7.x": false,
			"master":  true,
		},
		files: map[string]string{
			"v26.1.x": upstreamReleaseYAML(
				"v26.1.2",
				stableOperatorRepository,
				stableComponentRepository,
				"doca3.3.0-26.01-1.0.0.0-6",
			),
			"master": upstreamReleaseYAML(
				"v26.7.0-beta.5",
				stagingOperatorRepository,
				stagingComponentRepository,
				"doca3.5.0-26.07-0.4.3.0-0",
			),
		},
		requests: nil,
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	result, err := syncCatalog(context.Background(), syncOptions{
		CatalogPath: catalogPath,
		APIBaseURL:  server.URL,
		Token:       "test-token",
		HTTPClient:  server.Client(),
	})
	require.NoError(t, err)
	require.True(t, result.Changed)
	require.Len(t, result.Updates, 2)
	assert.Equal(t, "v26.1.x", result.Updates[0].Ref)
	assert.Equal(t, fallbackBranch, result.Updates[1].Ref)

	updated := readTestCatalog(t, catalogPath)
	assert.Contains(t, string(updated), "# Test release catalog")

	var catalog struct {
		Releases map[string]catalogReleaseValues `yaml:"releases"`
	}
	require.NoError(t, yaml.Unmarshal(updated, &catalog))
	require.Len(t, catalog.Releases, 2)

	stable := catalog.Releases["26.1"]
	assert.Equal(t, "v26.1.2", stable.NetworkOperator.Version)
	assert.Equal(t, "network-operator-v26.1.2", stable.NetworkOperator.ComponentVersion)
	assert.Equal(t, stableComponentRepository, stable.NetworkOperator.Repository)
	assert.Equal(t, stableOperatorRepository, stable.NetworkOperator.OperatorRepository)
	assert.Equal(t, stableHelmRepoURL, stable.NetworkOperator.HelmRepoURL)
	assert.Equal(t, "doca3.3.0-26.01-1.0.0.0-6", stable.DOCADriver.Version)

	latest := catalog.Releases["26.7"]
	assert.Equal(t, "v26.7.0-beta.5", latest.NetworkOperator.Version)
	assert.Equal(t, "network-operator-v26.7.0-beta.5", latest.NetworkOperator.ComponentVersion)
	assert.Equal(t, stagingComponentRepository, latest.NetworkOperator.Repository)
	assert.Equal(t, stagingOperatorRepository, latest.NetworkOperator.OperatorRepository)
	assert.Equal(t, stagingHelmRepoURL, latest.NetworkOperator.HelmRepoURL)
	assert.Equal(t, "doca3.5.0-26.07-0.4.3.0-0", latest.DOCADriver.Version)

	beforeSecondRun := append([]byte(nil), updated...)
	secondResult, err := syncCatalog(context.Background(), syncOptions{
		CatalogPath: catalogPath,
		APIBaseURL:  server.URL,
		Token:       "test-token",
		HTTPClient:  server.Client(),
	})
	require.NoError(t, err)
	assert.False(t, secondResult.Changed)
	assert.Empty(t, secondResult.Updates)
	assert.Equal(t, beforeSecondRun, readTestCatalog(t, catalogPath))
}

func TestSyncCatalogDoesNotFallbackForOlderRelease(t *testing.T) {
	catalogPath := writeTestCatalog(t, testCatalogYAML())
	fake := &fakeGitHub{
		branches: map[string]bool{
			"v26.1.x": false,
			"v26.7.x": true,
			"master":  true,
		},
		files:    map[string]string{},
		requests: nil,
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	_, err := syncCatalog(context.Background(), syncOptions{
		CatalogPath: catalogPath,
		APIBaseURL:  server.URL,
		Token:       "",
		HTTPClient:  server.Client(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `release branch "v26.1.x" does not exist`)
	assert.NotContains(t, strings.Join(fake.requests, "\n"), "/branches/master")
}

func TestSyncCatalogRejectsMismatchedMasterWithoutWriting(t *testing.T) {
	original := testCatalogYAML()
	catalogPath := writeTestCatalog(t, original)
	fake := &fakeGitHub{
		branches: map[string]bool{
			"v26.1.x": true,
			"v26.7.x": false,
			"master":  true,
		},
		files: map[string]string{
			"v26.1.x": upstreamReleaseYAML(
				"v26.1.2",
				stableOperatorRepository,
				stableComponentRepository,
				"doca3.3.0-26.01-1.0.0.0-6",
			),
			"master": upstreamReleaseYAML(
				"v26.10.0-beta.1",
				stagingOperatorRepository,
				stagingComponentRepository,
				"doca3.6.0-26.10-0.1.0.0-0",
			),
		},
		requests: nil,
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	_, err := syncCatalog(context.Background(), syncOptions{
		CatalogPath: catalogPath,
		APIBaseURL:  server.URL,
		Token:       "",
		HTTPClient:  server.Client(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not belong to release line 26.7")
	assert.Equal(t, []byte(original), readTestCatalog(t, catalogPath))
}

func TestDesiredFromUpstreamRejectsInvalidArtifactMetadata(t *testing.T) {
	tests := []struct {
		name          string
		operatorRepo  string
		componentRepo string
		version       string
		expectedError string
	}{
		{
			name:          "unsupported operator repository",
			operatorRepo:  "registry.example.com/network-operator",
			componentRepo: stableComponentRepository,
			version:       "v26.4.1",
			expectedError: "unsupported NetworkOperator.repository",
		},
		{
			name:          "component version mismatch",
			operatorRepo:  stableOperatorRepository,
			componentRepo: stableComponentRepository,
			version:       "v26.7.0",
			expectedError: "does not belong to release line 26.4",
		},
		{
			name:          "component repository mismatch",
			operatorRepo:  stableOperatorRepository,
			componentRepo: stagingComponentRepository,
			version:       "v26.4.1",
			expectedError: "NetworkOperatorInitContainer.repository",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := desiredFromUpstream(
				"26.4",
				[]byte(upstreamReleaseYAML(
					test.version,
					test.operatorRepo,
					test.componentRepo,
					"doca3.4.1-26.04-1.1.0.0-1",
				)),
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.expectedError)
		})
	}
}

func TestCatalogReleasesRequiresMajorMinorKeys(t *testing.T) {
	_, err := catalogReleases([]byte(`
releases:
  "26.01":
    networkOperator: {}
    docaDriver: {}
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must use MAJOR.MINOR format")
}

func TestLatestCatalogTag(t *testing.T) {
	tag, err := latestCatalogTag([]byte(testCatalogYAML()))
	require.NoError(t, err)
	assert.Equal(t, "v26.7.0-beta.4", tag)
}

func TestRequireNewerTag(t *testing.T) {
	require.NoError(t, requireNewerTag("v26.7.0-beta.5", "v26.7.0-beta.4"))
	require.NoError(t, requireNewerTag("v26.7.0", "v26.7.0-rc.1"))

	err := requireNewerTag("v26.7.0-beta.4", "v26.7.0-beta.5")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not newer")
}

func writeTestCatalog(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "releases.yaml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o644))
	return path
}

func readTestCatalog(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return contents
}

func testCatalogYAML() string {
	return `# Test release catalog
releases:
  "26.1":
    networkOperator:
      version: v26.1.1
      componentVersion: network-operator-v26.1.1
      repository: nvcr.io/nvidia/mellanox
      operatorRepository: nvcr.io/nvidia/cloud-native
      helmRepoURL: https://helm.ngc.nvidia.com/nvidia
    docaDriver:
      version: doca3.3.0-26.01-1.0.0.0-0
  "26.7":
    networkOperator:
      version: v26.7.0-beta.4
      componentVersion: network-operator-v26.7.0-beta.4
      repository: nvcr.io/nvstaging/mellanox
      operatorRepository: nvcr.io/nvstaging/mellanox
      helmRepoURL: https://helm.ngc.nvidia.com/nvstaging/mellanox
    docaDriver:
      version: doca3.5.0-26.07-0.4.2.0-0
`
}

func upstreamReleaseYAML(
	version string,
	operatorRepository string,
	componentRepository string,
	docaVersion string,
) string {
	return fmt.Sprintf(`
NetworkOperator:
  repository: %s
  version: %s
NetworkOperatorInitContainer:
  repository: %s
  version: network-operator-%s
Mofed:
  repository: %s
  version: %s
`,
		operatorRepository,
		version,
		componentRepository,
		version,
		componentRepository,
		docaVersion,
	)
}
