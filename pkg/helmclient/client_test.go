// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package helmclient

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

func TestNewActionConfigUsesExplicitNamespaceAndDriver(t *testing.T) {
	config, err := NewActionConfig(&rest.Config{Host: "https://127.0.0.1"}, "dpf-system", StorageDriver)
	require.NoError(t, err)
	getter, ok := config.RESTClientGetter.(*restClientGetter)
	require.True(t, ok)
	namespace, overridden, err := getter.ToRawKubeConfigLoader().Namespace()
	require.NoError(t, err)
	require.True(t, overridden)
	require.Equal(t, "dpf-system", namespace)
	require.Equal(t, "Secret", config.Releases.Name())
}

func TestNewActionConfigRequiresExplicitInputs(t *testing.T) {
	_, err := NewActionConfig(nil, "dpf-system", StorageDriver)
	require.ErrorContains(t, err, "nil rest config")
	_, err = NewActionConfig(&rest.Config{Host: "https://127.0.0.1"}, "", StorageDriver)
	require.ErrorContains(t, err, "namespace is required")
	_, err = NewActionConfig(&rest.Config{Host: "https://127.0.0.1"}, "dpf-system", "")
	require.ErrorContains(t, err, "storage driver is required")
}
