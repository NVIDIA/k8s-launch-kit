// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/options"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverRegistersProfileFlags(t *testing.T) {
	for _, name := range []string{
		"fabric",
		"deployment-type",
		"multirail",
		"spectrum-x",
		"multiplane-mode",
		"number-of-planes",
	} {
		assert.NotNilf(t, discoverCmd.Flags().Lookup(name), "missing --%s", name)
	}
}

func TestValidateProfileFlagValues(t *testing.T) {
	t.Run("rejects invalid fabric", func(t *testing.T) {
		err := validateProfileFlagValues(&options.Options{Fabric: "roce"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--fabric")
	})

	t.Run("rejects invalid deployment", func(t *testing.T) {
		err := validateProfileFlagValues(&options.Options{DeploymentType: "macvlan"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--deployment-type")
	})

	t.Run("allows partial profile", func(t *testing.T) {
		assert.NoError(t, validateProfileFlagValues(&options.Options{Fabric: "ethernet"}))
		assert.NoError(t, validateProfileFlagValues(&options.Options{DeploymentType: "sriov"}))
	})
}
