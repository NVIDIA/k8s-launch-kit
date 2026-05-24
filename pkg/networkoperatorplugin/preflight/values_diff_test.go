// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeepEqualValues_Equal(t *testing.T) {
	a := map[string]interface{}{
		"nfd":    map[string]interface{}{"enabled": true},
		"images": []interface{}{"a", "b"},
	}
	b := map[string]interface{}{
		"images": []interface{}{"a", "b"},
		"nfd":    map[string]interface{}{"enabled": true},
	}
	diffs := DeepEqualValues(a, b)
	assert.Empty(t, diffs)
}

func TestDeepEqualValues_NilTreatedAsEmpty(t *testing.T) {
	assert.Empty(t, DeepEqualValues(nil, map[string]interface{}{}))
	assert.Empty(t, DeepEqualValues(map[string]interface{}{}, nil))
	assert.Empty(t, DeepEqualValues(nil, nil))
}

func TestDeepEqualValues_ScalarDiff(t *testing.T) {
	a := map[string]interface{}{"sriov": map[string]interface{}{"enabled": true}}
	b := map[string]interface{}{"sriov": map[string]interface{}{"enabled": false}}
	diffs := DeepEqualValues(a, b)
	require.Len(t, diffs, 1)
	assert.Equal(t, "sriov.enabled", diffs[0].Path)
	assert.Equal(t, true, diffs[0].Actual)
	assert.Equal(t, false, diffs[0].Expected)
}

func TestDeepEqualValues_MissingKey(t *testing.T) {
	a := map[string]interface{}{"a": 1}
	b := map[string]interface{}{"a": 1, "b": 2}
	diffs := DeepEqualValues(a, b)
	require.Len(t, diffs, 1)
	assert.Equal(t, "b", diffs[0].Path)
	assert.Nil(t, diffs[0].Actual)
	assert.Equal(t, 2, diffs[0].Expected)
}

func TestDeepEqualValues_SliceLengthDiff(t *testing.T) {
	a := map[string]interface{}{"x": []interface{}{1, 2}}
	b := map[string]interface{}{"x": []interface{}{1, 2, 3}}
	diffs := DeepEqualValues(a, b)
	require.Len(t, diffs, 1)
	assert.Equal(t, "x", diffs[0].Path)
}

func TestDeepEqualValues_TypeMismatch(t *testing.T) {
	// helm.sh-style nuance: string "true" vs bool true.
	a := map[string]interface{}{"enabled": "true"}
	b := map[string]interface{}{"enabled": true}
	diffs := DeepEqualValues(a, b)
	require.Len(t, diffs, 1)
	assert.Equal(t, "enabled", diffs[0].Path)
}

func TestDeepEqualValues_ArgumentOrder(t *testing.T) {
	// Argument order: (actual, expected). The Mismatch.Actual must
	// equal `a`, .Expected must equal `b`. Catches accidental swaps.
	a := map[string]interface{}{"k": "from-cluster"}
	b := map[string]interface{}{"k": "from-l8k"}
	diffs := DeepEqualValues(a, b)
	require.Len(t, diffs, 1)
	assert.Equal(t, "from-cluster", diffs[0].Actual)
	assert.Equal(t, "from-l8k", diffs[0].Expected)
}
