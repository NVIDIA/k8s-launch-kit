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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/releases"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

// helmVersionSecret encodes the same storage envelope as Helm's Secrets driver.
func helmVersionSecret(t *testing.T, name string, revision int, appVersion string) *corev1.Secret {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"name": name, "namespace": "operators", "version": revision,
		"info": map[string]string{"status": "deployed"},
		"chart": map[string]any{"metadata": map[string]string{
			"name": name, "version": "0.0.0-dev", "appVersion": appVersion,
		}},
	})
	require.NoError(t, err)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, err = writer.Write(data)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, revision), Namespace: "operators"},
		Type:       corev1.SecretType(helmReleaseSecretType),
		Data:       map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(compressed.Bytes()))},
	}
}

func TestCheckHelmReleaseVersionSelection(t *testing.T) {
	catalog, ok := releases.LookupRelease("26.7")
	require.True(t, ok)
	expectedVersion := catalog.NetworkOperator.Version
	tests := []struct {
		name     string
		secrets  func(*testing.T) []client.Object
		release  string
		revision int
		match    bool
		reason   string
	}{
		{
			name: "canonical release wins over higher wrapper revision",
			secrets: func(t *testing.T) []client.Object {
				return []client.Object{
					helmVersionSecret(t, "network-operator", 1, "v26.4.0"),
					helmVersionSecret(t, "network-operator", 2, expectedVersion),
					helmVersionSecret(t, "network-operator-post", 4, "26.7.0"),
				}
			}, release: "network-operator", revision: 2, match: true,
		},
		{
			name: "latest canonical mismatch is not hidden by old matching revision",
			secrets: func(t *testing.T) []client.Object {
				return []client.Object{
					helmVersionSecret(t, "network-operator", 1, expectedVersion),
					helmVersionSecret(t, "network-operator", 10, "v26.4.0"),
					helmVersionSecret(t, "network-operator-post", 20, expectedVersion),
				}
			}, release: "network-operator", revision: 10,
		},
		{
			name: "single custom release retains latest revision lookup",
			secrets: func(t *testing.T) []client.Object {
				return []client.Object{
					helmVersionSecret(t, "custom-network-operator", 2, "v26.4.0"),
					helmVersionSecret(t, "custom-network-operator", 10, expectedVersion),
				}
			}, release: "custom-network-operator", revision: 10, match: true,
		},
		{
			name: "multiple custom releases are ambiguous",
			secrets: func(t *testing.T) []client.Object {
				return []client.Object{
					helmVersionSecret(t, "b-network-operator", 20, expectedVersion),
					helmVersionSecret(t, "a-network-operator", 1, "v26.4.0"),
				}
			}, reason: "ambiguous Helm releases in namespace operators: a-network-operator, b-network-operator",
		},
		{
			name: "corrupt canonical revision does not fall back to wrapper",
			secrets: func(t *testing.T) []client.Object {
				broken := helmVersionSecret(t, "network-operator", 2, expectedVersion)
				broken.Data["release"] = []byte("invalid")
				return []client.Object{
					helmVersionSecret(t, "network-operator", 1, expectedVersion),
					broken,
					helmVersionSecret(t, "network-operator-post", 4, expectedVersion),
				}
			}, reason: "decode sh.helm.release.v1.network-operator.v2",
		},
		{
			name: "ignore other namespaces types and names",
			secrets: func(t *testing.T) []client.Object {
				otherNamespace := helmVersionSecret(t, "network-operator", 10, expectedVersion)
				otherNamespace.Namespace = "elsewhere"
				wrongType := helmVersionSecret(t, "network-operator", 2, expectedVersion)
				wrongType.Type = corev1.SecretTypeOpaque
				wrongPrefix := helmVersionSecret(t, "network-operator", 3, expectedVersion)
				wrongPrefix.Name = "network-operator"
				return []client.Object{otherNamespace, wrongType, wrongPrefix, helmVersionSecret(t, "unrelated", 5, expectedVersion)}
			}, reason: "no Helm release Secret",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.secrets(t)...).Build()
			result, err := CheckHelmReleaseVersion(context.Background(), c, "operators", "26.7")
			require.NoError(t, err)
			if tt.reason != "" {
				require.True(t, result.Skipped)
				assert.False(t, result.Match)
				assert.Contains(t, result.Reason, tt.reason)
				assert.Nil(t, result.DeployedRelease)
				return
			}
			require.False(t, result.Skipped, result.Reason)
			require.NotNil(t, result.DeployedRelease)
			assert.Equal(t, tt.release, result.DeployedRelease.Name)
			assert.Equal(t, tt.revision, result.DeployedRelease.Revision)
			assert.Equal(t, tt.match, result.Match)
		})
	}
}
