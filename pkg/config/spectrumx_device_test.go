// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEastWestDeviceID(t *testing.T) {
	t.Run("normalizes a unanimous east-west device ID", func(t *testing.T) {
		group := ClusterConfig{
			Identifier: "group-a",
			PFs: []PFConfig{
				{DeviceID: " 0xA2DC ", Traffic: "east-west"},
				{DeviceID: "a2dc", Traffic: "east-west"},
				{DeviceID: "101f", Traffic: "north-south"},
			},
		}

		deviceID, hasEastWest, err := EastWestDeviceID(group)

		require.NoError(t, err)
		assert.True(t, hasEastWest)
		assert.Equal(t, "a2dc", deviceID)
	})

	t.Run("reports no east-west PFs", func(t *testing.T) {
		deviceID, hasEastWest, err := EastWestDeviceID(ClusterConfig{
			Identifier: "north-south-only",
			PFs:        []PFConfig{{DeviceID: "101f", Traffic: "north-south"}},
		})

		require.NoError(t, err)
		assert.False(t, hasEastWest)
		assert.Empty(t, deviceID)
	})

	t.Run("rejects a missing east-west device ID", func(t *testing.T) {
		_, hasEastWest, err := EastWestDeviceID(ClusterConfig{
			Identifier: "group-a",
			PFs:        []PFConfig{{DeviceID: "", Traffic: "east-west"}},
		})

		assert.True(t, hasEastWest)
		require.ErrorContains(t, err, `group "group-a" has an east-west PF without a deviceID`)
	})

	t.Run("rejects mixed east-west device IDs", func(t *testing.T) {
		_, hasEastWest, err := EastWestDeviceID(ClusterConfig{
			Identifier: "group-a",
			PFs: []PFConfig{
				{DeviceID: "1023", Traffic: "east-west"},
				{DeviceID: "a2dc", Traffic: "east-west"},
			},
		})

		assert.True(t, hasEastWest)
		require.ErrorContains(t, err, `group "group-a" east-west PFs have mixed deviceIDs: "1023" vs "a2dc"`)
	})
}

func TestEastWestDeviceIDForGroups(t *testing.T) {
	t.Run("accepts one device ID across groups", func(t *testing.T) {
		deviceID, hasEastWest, err := EastWestDeviceIDForGroups([]ClusterConfig{
			{Identifier: "group-a", PFs: []PFConfig{{DeviceID: "1023", Traffic: "east-west"}}},
			{Identifier: "north-south-only", PFs: []PFConfig{{DeviceID: "101f", Traffic: "north-south"}}},
			{Identifier: "group-b", PFs: []PFConfig{{DeviceID: "0x1023", Traffic: "east-west"}}},
		})

		require.NoError(t, err)
		assert.True(t, hasEastWest)
		assert.Equal(t, "1023", deviceID)
	})

	t.Run("rejects different device IDs across groups", func(t *testing.T) {
		_, hasEastWest, err := EastWestDeviceIDForGroups([]ClusterConfig{
			{Identifier: "group-a", PFs: []PFConfig{{DeviceID: "1023", Traffic: "east-west"}}},
			{Identifier: "group-b", PFs: []PFConfig{{DeviceID: "a2dc", Traffic: "east-west"}}},
		})

		assert.True(t, hasEastWest)
		require.ErrorContains(t, err, `east-west PFs have mixed deviceIDs: "1023" vs "a2dc"`)
	})
}
