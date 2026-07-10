// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
)

func TestNetworkNamespacesAreTheOnlyPersistedNamespaceSetting(t *testing.T) {
	cfg := LaunchKitConfig{
		NetworkNamespaces:       []string{"ns1", "ns2"},
		CurrentNetworkNamespace: "ns1",
	}

	data, err := yaml.Marshal(&cfg)
	require.NoError(t, err)
	assert.Contains(t, string(data), "networkNamespaces:")
	assert.NotContains(t, string(data), "podNamespace")
	assert.NotContains(t, string(data), "currentNetworkNamespace")
}

func TestLegacyPodNamespaceIsIgnored(t *testing.T) {
	var cfg LaunchKitConfig
	require.NoError(t, yaml.Unmarshal([]byte("podNamespace: legacy\n"), &cfg))
	assert.Empty(t, cfg.NetworkNamespaces)
	assert.Empty(t, cfg.CurrentNetworkNamespace)
}
