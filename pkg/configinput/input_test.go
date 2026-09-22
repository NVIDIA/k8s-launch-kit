// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package configinput

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValuesCloneOwnsSliceValues(t *testing.T) {
	original := Values{Overrides: []Override{{Value: []string{"default"}}}}
	clone := original.Clone()
	clone.Overrides[0].Value.([]string)[0] = "changed"

	assert.Equal(t, []string{"default"}, original.Overrides[0].Value)
}
