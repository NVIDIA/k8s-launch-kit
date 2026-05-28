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
	"os"
	"path/filepath"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sigyaml "sigs.k8s.io/yaml"
)

func intPtr(i int) *int { return &i }

// multirailSriovConfig returns a simple sriov config with 2 east-west PFs for testing.
func multirailSriovConfig() (*config.LaunchKubernetesConfig, *config.ClusterConfig) {
	cfg := &config.LaunchKubernetesConfig{
		PodNamespace: "gpu-workloads",
		Profile: &config.Profile{
			Fabric:     "ethernet",
			Deployment: "sriov",
			Multirail:  true,
		},
		Sriov: &config.SriovConfig{
			ResourceName: "sriov_net",
			NetworkName:  "sriov-network",
		},
	}
	group := &config.ClusterConfig{
		Identifier: "a100",
		PFs: []config.PFConfig{
			{DeviceID: "101b", Traffic: "east-west", Rail: intPtr(0), PciAddress: "0000:04:00.0"},
			{DeviceID: "101b", Traffic: "east-west", Rail: intPtr(1), PciAddress: "0000:05:00.0"},
		},
		NodeSelector: map[string]string{"nvidia.com/gpu.product": "A100"},
	}
	return cfg, group
}

// --- patchWorkloadManifest ---

func TestPatchWorkloadManifest_Pod(t *testing.T) {
	cfg, group := multirailSriovConfig()

	podYAML := `apiVersion: v1
kind: Pod
metadata:
  name: test-pod
spec:
  containers:
    - name: worker
      image: nvcr.io/nvidia/pytorch:latest
`
	dir := t.TempDir()
	path := filepath.Join(dir, "pod.yaml")
	require.NoError(t, os.WriteFile(path, []byte(podYAML), 0644))

	out, err := patchWorkloadManifest(path, cfg, group)
	require.NoError(t, err)

	var obj map[string]interface{}
	require.NoError(t, sigyaml.Unmarshal([]byte(out), &obj))

	// Namespace injected
	meta := obj["metadata"].(map[string]interface{})
	assert.Equal(t, "gpu-workloads", meta["namespace"])

	// Name gets group suffix
	assert.Equal(t, "test-pod-a100", meta["name"])

	// Annotation injected on pod metadata
	annotations := meta["annotations"].(map[string]interface{})
	assert.Equal(t, "sriov-network-rail-0-a100,sriov-network-rail-1-a100", annotations["k8s.v1.cni.cncf.io/networks"])

	// Resources injected on first container
	spec := obj["spec"].(map[string]interface{})
	containers := spec["containers"].([]interface{})
	c0 := containers[0].(map[string]interface{})
	res := c0["resources"].(map[string]interface{})
	requests := res["requests"].(map[string]interface{})
	assert.Equal(t, "1", requests["nvidia.com/sriov_net_rail_0"])
	assert.Equal(t, "1", requests["nvidia.com/sriov_net_rail_1"])

	// Node affinity injected
	affinity := spec["affinity"].(map[string]interface{})
	assert.NotNil(t, affinity["nodeAffinity"])
}

func TestPatchWorkloadManifest_Deployment(t *testing.T) {
	cfg, group := multirailSriovConfig()

	deployYAML := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-deploy
spec:
  replicas: 1
  template:
    metadata:
      labels:
        app: test
    spec:
      containers:
        - name: worker
          image: nvcr.io/nvidia/pytorch:latest
`
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(deployYAML), 0644))

	out, err := patchWorkloadManifest(path, cfg, group)
	require.NoError(t, err)

	var obj map[string]interface{}
	require.NoError(t, sigyaml.Unmarshal([]byte(out), &obj))

	// Annotation is on .spec.template.metadata, not .metadata
	spec := obj["spec"].(map[string]interface{})
	template := spec["template"].(map[string]interface{})
	tmplMeta := template["metadata"].(map[string]interface{})
	annotations := tmplMeta["annotations"].(map[string]interface{})
	assert.Equal(t, "sriov-network-rail-0-a100,sriov-network-rail-1-a100", annotations["k8s.v1.cni.cncf.io/networks"])

	// Resources on .spec.template.spec.containers[0]
	tmplSpec := template["spec"].(map[string]interface{})
	containers := tmplSpec["containers"].([]interface{})
	c0 := containers[0].(map[string]interface{})
	res := c0["resources"].(map[string]interface{})
	limits := res["limits"].(map[string]interface{})
	assert.Equal(t, "1", limits["nvidia.com/sriov_net_rail_0"])
	assert.Equal(t, "1", limits["nvidia.com/sriov_net_rail_1"])
}

func TestPatchWorkloadManifest_DaemonSet(t *testing.T) {
	cfg, group := multirailSriovConfig()

	dsYAML := `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: test-ds
spec:
  template:
    spec:
      containers:
        - name: worker
          image: nvcr.io/nvidia/pytorch:latest
`
	dir := t.TempDir()
	path := filepath.Join(dir, "ds.yaml")
	require.NoError(t, os.WriteFile(path, []byte(dsYAML), 0644))

	out, err := patchWorkloadManifest(path, cfg, group)
	require.NoError(t, err)

	var obj map[string]interface{}
	require.NoError(t, sigyaml.Unmarshal([]byte(out), &obj))

	// Verify patching goes through template path
	spec := obj["spec"].(map[string]interface{})
	template := spec["template"].(map[string]interface{})
	tmplMeta := template["metadata"].(map[string]interface{})
	annotations := tmplMeta["annotations"].(map[string]interface{})
	assert.Contains(t, annotations, "k8s.v1.cni.cncf.io/networks")
}

func TestPatchWorkloadManifest_UnsupportedKind(t *testing.T) {
	cfg, group := multirailSriovConfig()

	cmYAML := `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-cm
data:
  key: value
`
	dir := t.TempDir()
	path := filepath.Join(dir, "cm.yaml")
	require.NoError(t, os.WriteFile(path, []byte(cmYAML), 0644))

	_, err := patchWorkloadManifest(path, cfg, group)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported workload kind")
}

// --- buildNetworkAnnotation ---

func TestBuildNetworkAnnotation(t *testing.T) {
	t.Run("sriov multirail", func(t *testing.T) {
		cfg, group := multirailSriovConfig()
		got := buildNetworkAnnotation(cfg, group)
		assert.Equal(t, "sriov-network-rail-0-a100,sriov-network-rail-1-a100", got)
	})

	t.Run("sriov single-rail", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{Deployment: "sriov", Multirail: false},
			Sriov:   &config.SriovConfig{NetworkName: "sriov-net"},
		}
		group := &config.ClusterConfig{
			Identifier: "h100",
			PFs:        []config.PFConfig{{Traffic: "east-west", Rail: intPtr(0)}},
		}
		got := buildNetworkAnnotation(cfg, group)
		assert.Equal(t, "sriov-net-h100", got)
	})

	t.Run("hostdev", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{Deployment: "host_device", Multirail: false},
			Hostdev: &config.HostdevConfig{NetworkName: "hostdev-net"},
		}
		group := &config.ClusterConfig{
			PFs: []config.PFConfig{{Traffic: "east-west", Rail: intPtr(0)}},
		}
		got := buildNetworkAnnotation(cfg, group)
		assert.Equal(t, "hostdev-net", got)
	})

	t.Run("rdma_shared ipoib", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{Deployment: "rdma_shared", Fabric: "infiniband", Multirail: false},
			Ipoib:   &config.IpoibConfig{NetworkName: "ipoib-net"},
		}
		group := &config.ClusterConfig{
			PFs: []config.PFConfig{{Traffic: "east-west", Rail: intPtr(0)}},
		}
		got := buildNetworkAnnotation(cfg, group)
		assert.Equal(t, "ipoib-net", got)
	})

	t.Run("spectrum-x swplb", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{
				SpectrumX: &config.ProfileSpectrumX{
					Enable:         true,
					MultiplaneMode: "swplb",
					NumberOfPlanes: 2,
				},
			},
		}
		group := &config.ClusterConfig{
			PFs: []config.PFConfig{
				{Traffic: "east-west", Rail: intPtr(0)},
				{Traffic: "east-west", Rail: intPtr(1)},
			},
		}
		got := buildNetworkAnnotation(cfg, group)
		assert.Equal(t, "rail-0-plane-0,rail-0-plane-1,rail-1-plane-0,rail-1-plane-1", got)
	})

	t.Run("spectrum-x uniplane", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{
				SpectrumX: &config.ProfileSpectrumX{
					Enable:         true,
					MultiplaneMode: "uniplane",
				},
			},
		}
		group := &config.ClusterConfig{
			PFs: []config.PFConfig{
				{Traffic: "east-west", Rail: intPtr(0)},
				{Traffic: "east-west", Rail: intPtr(1)},
			},
		}
		got := buildNetworkAnnotation(cfg, group)
		assert.Equal(t, "rail-0,rail-1", got)
	})

	t.Run("nil profile returns empty", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{}
		group := &config.ClusterConfig{}
		got := buildNetworkAnnotation(cfg, group)
		assert.Equal(t, "", got)
	})
}

// --- buildNetworkResources ---

func TestBuildNetworkResources(t *testing.T) {
	t.Run("sriov multirail", func(t *testing.T) {
		cfg, group := multirailSriovConfig()
		got := buildNetworkResources(cfg, group)
		assert.Equal(t, map[string]string{
			"nvidia.com/sriov_net_rail_0": "1",
			"nvidia.com/sriov_net_rail_1": "1",
		}, got)
	})

	t.Run("sriov single-rail", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{Deployment: "sriov", Multirail: false},
			Sriov:   &config.SriovConfig{ResourceName: "sriov_res"},
		}
		group := &config.ClusterConfig{
			PFs: []config.PFConfig{{Traffic: "east-west", Rail: intPtr(0)}},
		}
		got := buildNetworkResources(cfg, group)
		assert.Equal(t, map[string]string{"nvidia.com/sriov_res": "1"}, got)
	})

	t.Run("rdma_shared", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile:    &config.Profile{Deployment: "rdma_shared", Multirail: false},
			RdmaShared: &config.RdmaSharedConfig{ResourceName: "shared_rdma"},
		}
		group := &config.ClusterConfig{
			PFs: []config.PFConfig{{Traffic: "east-west", Rail: intPtr(0)}},
		}
		got := buildNetworkResources(cfg, group)
		assert.Equal(t, map[string]string{"rdma/shared_rdma": "1"}, got)
	})

	t.Run("spectrum-x swplb", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{
				SpectrumX: &config.ProfileSpectrumX{
					Enable:         true,
					MultiplaneMode: "swplb",
					NumberOfPlanes: 2,
				},
			},
		}
		group := &config.ClusterConfig{
			PFs: []config.PFConfig{
				{Traffic: "east-west", Rail: intPtr(0)},
				{Traffic: "east-west", Rail: intPtr(1)},
			},
		}
		got := buildNetworkResources(cfg, group)
		assert.Equal(t, map[string]string{
			"nvidia.com/rail_0_plane_0": "1",
			"nvidia.com/rail_0_plane_1": "1",
			"nvidia.com/rail_1_plane_0": "1",
			"nvidia.com/rail_1_plane_1": "1",
		}, got)
	})

	t.Run("spectrum-x uniplane", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{
				SpectrumX: &config.ProfileSpectrumX{
					Enable:         true,
					MultiplaneMode: "uniplane",
				},
			},
		}
		group := &config.ClusterConfig{
			PFs: []config.PFConfig{
				{Traffic: "east-west", Rail: intPtr(0)},
				{Traffic: "east-west", Rail: intPtr(1)},
			},
		}
		got := buildNetworkResources(cfg, group)
		assert.Equal(t, map[string]string{
			"nvidia.com/rail_0": "1",
			"nvidia.com/rail_1": "1",
		}, got)
	})

	t.Run("nil profile returns nil", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{}
		group := &config.ClusterConfig{}
		got := buildNetworkResources(cfg, group)
		assert.Nil(t, got)
	})
}

// --- isWorkloadTemplate ---

func TestIsWorkloadTemplate(t *testing.T) {
	assert.True(t, isWorkloadTemplate("example-daemonset.yaml"))
	assert.True(t, isWorkloadTemplate("/path/to/example-daemonset-v2.yaml"))
	assert.False(t, isWorkloadTemplate("my-deployment.yaml"))
	assert.False(t, isWorkloadTemplate("daemonset.yaml"))
}
