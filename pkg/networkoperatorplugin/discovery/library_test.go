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

package discovery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestParseClusterConfig_RoundTrip checks the bytes-to-struct contract that
// library callers loading a cluster-config.yaml from a ConfigMap (or any
// in-memory source) rely on. Anchoring on a fixed YAML rather than reusing
// the production cluster-config.yaml in the repo keeps the test independent
// of any future field rename in that file.
func TestParseClusterConfig_RoundTrip(t *testing.T) {
	src := `
networkOperator:
  version: v26.4.0-beta.3
  componentVersion: network-operator-v26.4.0-beta.3
  repository: nvcr.io/nvstaging/mellanox
  namespace: network-operator
docaDriver:
  enable: true
  version: doca3.3.0-26.01-1.0.0.0-0
clusterConfig:
- identifier: group-0
  machineType: GB300-NVL
  gpuType: NVIDIA-GB300
  capabilities:
    nodes:
      sriov: true
      rdma: true
      ib: true
`

	cfg, err := ParseClusterConfig(strings.NewReader(src))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.NetworkOperator)
	assert.Equal(t, "v26.4.0-beta.3", cfg.NetworkOperator.Version)
	require.NotNil(t, cfg.DOCADriver)
	assert.True(t, cfg.DOCADriver.Enable)

	require.Len(t, cfg.ClusterConfig, 1)
	g := cfg.ClusterConfig[0]
	assert.Equal(t, "group-0", g.Identifier)
	assert.Equal(t, "GB300-NVL", g.MachineType)
	assert.Equal(t, "NVIDIA-GB300", g.GPUType)
	require.NotNil(t, g.Capabilities)
	require.NotNil(t, g.Capabilities.Nodes)
	assert.True(t, g.Capabilities.Nodes.Sriov)
	assert.True(t, g.Capabilities.Nodes.Rdma)
	assert.True(t, g.Capabilities.Nodes.Ib)
}

func TestParseClusterConfig_NilReader(t *testing.T) {
	_, err := ParseClusterConfig(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reader must not be nil")
}

func TestParseClusterConfig_InvalidYAML(t *testing.T) {
	_, err := ParseClusterConfig(strings.NewReader("networkOperator: : :"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "yaml unmarshal failed")
}

// TestDiscover_NilClientRejected anchors the input-validation contract.
// Library callers that pass nil get an error before any state is touched.
func TestDiscover_NilClientRejected(t *testing.T) {
	cfg, err := config.DefaultLaunchKitConfig()
	require.NoError(t, err)
	_, err = Discover(context.Background(), nil, nil, cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kubeClient must not be nil")
}

// TestDiscover_NilCfgRejected pins the cfg-required contract introduced
// when WithBaseConfig was replaced by the positional cfg argument.
func TestDiscover_NilCfgRejected(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	_, err := Discover(context.Background(), c, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cfg must not be nil")
}

// TestDiscover_PreservesUserFields checks that Discover hands the
// caller's cfg to the underlying discovery flow instead of substituting
// the embedded default. We can't run the full discovery loop without a
// real cluster, so we exercise the entry point only as far as the
// pre-discovery wiring; DiscoverClusterConfig short-circuits cleanly when
// the supplied cfg lacks a NetworkOperator section.
func TestDiscover_PreservesUserFields(t *testing.T) {
	base := &config.LaunchKitConfig{
		PodNamespace: "library-test-ns",
		// Deliberately no NetworkOperator: DiscoverClusterConfig requires
		// it and will return early with a descriptive error. That gets
		// us past the wiring step (which is what this test pins down)
		// without needing a fake DaemonSet rollout.
	}
	c := fake.NewClientBuilder().Build()

	_, err := Discover(context.Background(), c, nil, base)
	require.Error(t, err, "expected DiscoverClusterConfig to surface its missing-NetworkOperator error")
	assert.Contains(t, err.Error(), "networkOperator section is required")
}

// TestDiscover_DefaultConfigPath checks the converse: when the caller
// passes a freshly-loaded DefaultLaunchKitConfig (the canonical "I want
// l8k's defaults" entry point), the embedded NetworkOperator section is
// present and the missing-section error from the prior test does NOT
// fire. Instead Discover progresses past the wiring step into the
// cluster-contact phase, which we bound with a tight context timeout to
// keep the test cheap. Any non-nil error other than "networkOperator
// section is required" is acceptable for this wiring check; we never
// want the test to actually wait on DaemonSet rollout.
func TestDiscover_DefaultConfigPath(t *testing.T) {
	cfg, err := config.DefaultLaunchKitConfig()
	require.NoError(t, err)

	c := fake.NewClientBuilder().Build()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err = Discover(ctx, c, nil, cfg)
	require.Error(t, err, "expected a downstream error since the fake client has no NIC daemons")
	assert.NotContains(t, err.Error(), "networkOperator section is required",
		"DefaultLaunchKitConfig must populate NetworkOperator")
}
