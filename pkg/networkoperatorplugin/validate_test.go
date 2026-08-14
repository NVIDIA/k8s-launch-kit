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

package networkoperatorplugin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDecodeHelmRelease verifies that the base64-gzip-JSON unwrap matches the
// shape Helm writes into Secret.Data["release"].
func TestDecodeHelmRelease(t *testing.T) {
	// Minimal release JSON shape Helm uses.
	releaseJSON := []byte(`{
		"name": "network-operator",
		"namespace": "nvidia-network-operator",
		"version": 7,
		"info": {"status": "deployed"},
		"chart": {
			"metadata": {
				"name": "network-operator",
				"version": "26.4.0-beta.6",
				"appVersion": "v26.4.0-beta.6"
			}
		}
	}`)

	// gzip → base64 (matches Helm's storage encoding).
	var gzbuf bytes.Buffer
	gw := gzip.NewWriter(&gzbuf)
	_, err := gw.Write(releaseJSON)
	require.NoError(t, err)
	require.NoError(t, gw.Close())
	encoded := base64.StdEncoding.EncodeToString(gzbuf.Bytes())

	info, err := decodeHelmRelease([]byte(encoded))
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, "network-operator", info.Name)
	assert.Equal(t, "nvidia-network-operator", info.Namespace)
	assert.Equal(t, 7, info.Revision)
	assert.Equal(t, "deployed", info.Status)
	assert.Equal(t, "network-operator", info.ChartName)
	assert.Equal(t, "26.4.0-beta.6", info.ChartVersion)
	assert.Equal(t, "v26.4.0-beta.6", info.AppVersion)
}

func TestHelmRevisionFromName(t *testing.T) {
	cases := []struct {
		in  string
		out int
	}{
		{"sh.helm.release.v1.network-operator.v1", 1},
		{"sh.helm.release.v1.network-operator.v17", 17},
		{"sh.helm.release.v1.foo.v123", 123},
		{"not-a-helm-secret", 0},
		{"sh.helm.release.v1.foo.vbad", 0},
	}
	for _, c := range cases {
		assert.Equal(t, c.out, helmRevisionFromName(c.in), c.in)
	}
}

func TestHelmReleaseNameFromSecret(t *testing.T) {
	cases := []struct {
		in  string
		out string
	}{
		{"sh.helm.release.v1.network-operator.v1", "network-operator"},
		{"sh.helm.release.v1.my-release.v17", "my-release"},
		{"sh.helm.release.v1.foo.bar.v3", "foo.bar"},
	}
	for _, c := range cases {
		assert.Equal(t, c.out, helmReleaseNameFromSecret(c.in), c.in)
	}
}

func TestIsExampleManifest(t *testing.T) {
	tests := map[string]bool{
		"50-example-daemonset.yaml":        true,
		"40-example-pod.yaml":              true,
		"30-EXAMPLE-something.yaml":        true,
		"10-nicclusterpolicy.yaml":         false,
		"11-nicnodepolicy-group-0.yaml":    false,
		"30-sriovnetworknodepolicy.yaml":   false,
		"35-nicinterfacenametemplate.yaml": false,
	}
	for in, want := range tests {
		assert.Equalf(t, want, isExampleManifest(in), "isExampleManifest(%q)", in)
	}
}

func TestValidateManifestsSkipsHelmValues(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "values.yaml"), []byte("operator:\n  tag: v26.7.0\n"), 0o600))

	results, err := ValidateManifests(context.Background(), nil, dir)

	require.NoError(t, err)
	assert.Empty(t, results)
}
