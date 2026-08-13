// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testLaunchKitVersion = "v26.7.0-test"

func countVersionAnnotations(t *testing.T, stream []byte, version string) int {
	t.Helper()
	return strings.Count(string(stream), launchKitVersionAnnotation+`: "`+version+`"`)
}

func TestAnnotateResources(t *testing.T) {
	input := []byte(`apiVersion: example.io/v1
kind: First
metadata:
  name: first
  annotations:
    existing: keep
    nvidia.kubernetes-launch-kit.version: "old"
spec: {}
---
apiVersion: example.io/v1
kind: Second
metadata:
  name: second
spec: {}
`)

	annotated, err := annotateResources(input, "v26.7.0-rc.1")
	require.NoError(t, err)
	assert.Equal(t, 2, countVersionAnnotations(t, annotated, "v26.7.0-rc.1"))
	assert.Contains(t, string(annotated), "existing: keep")
	assert.NotContains(t, string(annotated), `nvidia.kubernetes-launch-kit.version: "old"`)
}

func TestAnnotateResourcesRejectsMissingMetadata(t *testing.T) {
	_, err := annotateResources([]byte("apiVersion: v1\nkind: ConfigMap\n"), "v1.0.0")
	require.ErrorContains(t, err, "metadata mapping")
}
