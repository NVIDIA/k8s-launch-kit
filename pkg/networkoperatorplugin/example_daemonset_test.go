// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func TestExampleDaemonSetTemplatesDeclareICMPHelper(t *testing.T) {
	templates, err := filepath.Glob(filepath.Join("..", "..", "profiles", "*", "*-example-daemonset.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, templates)

	const (
		helperName    = "- name: netshoot"
		helperImage   = "image: nicolaka/netshoot:latest"
		helperCommand = "trap 'exit 0' TERM INT; while true; do while wait -n 2>/dev/null; do :; done; sleep 1 & wait $! || true; done"
	)

	for _, templatePath := range templates {
		t.Run(filepath.Base(filepath.Dir(templatePath)), func(t *testing.T) {
			content, err := os.ReadFile(templatePath)
			require.NoError(t, err)
			body := string(content)
			daemonSetBranches := strings.Count(body, "kind: DaemonSet")
			require.Greater(t, daemonSetBranches, 0)
			require.Equal(t, daemonSetBranches, strings.Count(body, helperName),
				"every rendered DaemonSet branch must declare the ICMP helper")
			require.Equal(t, daemonSetBranches, strings.Count(body, helperImage))
			require.Equal(t, daemonSetBranches, strings.Count(body, helperCommand))
		})
	}
}

func TestExampleDaemonSetTemplatesDeclareReleaseImagePullSecretsAndGPURequests(t *testing.T) {
	templates, err := filepath.Glob(filepath.Join("..", "..", "profiles", "*", "*-example-daemonset.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, templates)

	for _, templatePath := range templates {
		t.Run(filepath.Base(filepath.Dir(templatePath)), func(t *testing.T) {
			content, err := os.ReadFile(templatePath)
			require.NoError(t, err)
			body := string(content)
			branches := strings.Count(body, "kind: DaemonSet")
			require.Equal(t, branches, strings.Count(body, "image: {{validationImage $.NetworkOperator}}"))
			require.Equal(t, branches, strings.Count(body, "imagePullSecrets:"))
			require.GreaterOrEqual(t, strings.Count(body, "{{$.Validation.GPUDirect.GPUResourceType}}"), branches*2,
				"every primary container branch must request and limit the configured GPU resource")
		})
	}
}

func TestRenderedExampleDaemonSetGPUDirectResources(t *testing.T) {
	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
	cfg, err := config.LoadFullConfig(filepath.Join("testdata", "grouping", "mixed-same-type.yaml"), ctrllog.Log)
	require.NoError(t, err)
	cfg.Profile = &config.Profile{Fabric: "ethernet", Deployment: "sriov", Multirail: true}
	cfg.NetworkOperator.SelectedRelease = "26.4"
	cfg.NetworkOperator.ImagePullSecrets = []string{"yes"}
	cfg.Validation.GPUDirect.Enabled = true
	cfg.Validation.GPUDirect.GPUResourceType = "example.com/gpu"
	require.GreaterOrEqual(t, len(cfg.ClusterConfig), 2)
	for groupIndex := range cfg.ClusterConfig {
		gpuIndex := 1
		if groupIndex > 0 {
			gpuIndex = 7
		}
		for pfIndex := range cfg.ClusterConfig[groupIndex].PFs {
			cfg.ClusterConfig[groupIndex].PFs[pfIndex].ConnectedGPU = fmt.Sprintf("GPU%d", gpuIndex)
		}
	}
	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(loadProfileFromDir(t, "sriov-ethernet-rdma"), cfg)
	require.NoError(t, err)
	manifestName, manifest := fileMatching(t, rendered, "example-daemonset")
	docs := parseDocs(t, manifestName, manifest)
	require.Len(t, docs, 1)
	podSpec := requireMap(t, requireMap(t, requireMap(t, docs[0], "spec"), "template"), "spec")
	pullSecrets, ok := podSpec["imagePullSecrets"].([]any)
	require.True(t, ok)
	require.Equal(t, map[string]any{"name": "yes"}, pullSecrets[0], "pull secret names must remain YAML strings")
	containers, ok := podSpec["containers"].([]any)
	require.True(t, ok)
	primary := containers[0].(map[string]any)
	require.Equal(t, "nvcr.io/nvidia/doca/doca:full-rt-cuda13.0.0-3.4.0-runtime-host", primary["image"])
	resources := requireMap(t, primary, "resources")
	requests := requireMap(t, resources, "requests")
	limits := requireMap(t, resources, "limits")
	require.Equal(t, "8", requests["example.com/gpu"], "merged DaemonSet must cover the highest source-group GPU index")
	require.Equal(t, "8", limits["example.com/gpu"])
	helper := containers[1].(map[string]any)
	_, hasHelperResources := helper["resources"]
	require.False(t, hasHelperResources)

	cfg.Validation.GPUDirect.Enabled = false
	rendered, err = (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(loadProfileFromDir(t, "sriov-ethernet-rdma"), cfg)
	require.NoError(t, err)
	manifestName, manifest = fileMatching(t, rendered, "example-daemonset")
	docs = parseDocs(t, manifestName, manifest)
	podSpec = requireMap(t, requireMap(t, requireMap(t, docs[0], "spec"), "template"), "spec")
	containers = podSpec["containers"].([]any)
	primary = containers[0].(map[string]any)
	requests = requireMap(t, requireMap(t, primary, "resources"), "requests")
	_, hasGPURequest := requests["example.com/gpu"]
	require.False(t, hasGPURequest, "disabled GPUDirect must not request a GPU")
}

func TestRenderedSpectrumXDRAExampleDaemonSetGPUDirectResources(t *testing.T) {
	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
	cfg, err := config.LoadFullConfig(filepath.Join("testdata", "grouping", "mixed-same-type.yaml"), ctrllog.Log)
	require.NoError(t, err)
	topologyFile := writeSpectrumXTopology(t, cfg, 2)
	cfg.Profile = &config.Profile{
		Fabric: "ethernet", Deployment: "sriov", Multirail: true,
		SpectrumX: &config.ProfileSpectrumX{
			Enable: true, SPCXVersion: "RA2.3", MultiplaneMode: "swplb",
			NumberOfPlanes: 2, TopologyType: config.SpectrumXTopology2Tier,
			TopologyFile: topologyFile, UseDRA: true,
		},
	}
	cfg.NetworkOperator.SelectedRelease = "26.7"
	cfg.Validation.GPUDirect.Enabled = true
	for groupIndex := range cfg.ClusterConfig {
		for pfIndex := range cfg.ClusterConfig[groupIndex].PFs {
			cfg.ClusterConfig[groupIndex].PFs[pfIndex].ConnectedGPU = "GPU7"
		}
	}
	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(loadProfileFromDir(t, "spectrum-x"), cfg)
	require.NoError(t, err)
	manifestName, manifest := fileMatching(t, rendered, "example-daemonset")
	docs := parseDocs(t, manifestName, manifest)
	require.Len(t, docs, 1)
	podSpec := requireMap(t, requireMap(t, requireMap(t, docs[0], "spec"), "template"), "spec")
	containers := podSpec["containers"].([]any)
	primary := containers[0].(map[string]any)
	require.Equal(t, "nvcr.io/nvstaging/doca/doca:full-rt-cuda13.0.0-3.5.0-runtime-host-dev", primary["image"])
	resources := requireMap(t, primary, "resources")
	require.NotEmpty(t, resources["claims"])
	require.Equal(t, "8", requireMap(t, resources, "requests")["nvidia.com/gpu"])
	require.Equal(t, "8", requireMap(t, resources, "limits")["nvidia.com/gpu"])
}

func TestRenderedExampleDaemonSetsDeclareICMPHelper(t *testing.T) {
	profilesUnderTest := []struct {
		dir        string
		fabric     string
		deployment string
	}{
		{dir: "sriov-ethernet-rdma", fabric: "ethernet", deployment: "sriov"},
		{dir: "sriov-ib-rdma", fabric: "infiniband", deployment: "sriov"},
		{dir: "host-device-rdma", fabric: "ethernet", deployment: "host_device"},
		{dir: "macvlan-rdma-shared", fabric: "ethernet", deployment: "rdma_shared"},
		{dir: "ipoib-rdma-shared", fabric: "infiniband", deployment: "rdma_shared"},
	}

	for _, profile := range profilesUnderTest {
		for _, multirail := range []bool{false, true} {
			name := profile.dir + "/single-rail"
			if multirail {
				name = profile.dir + "/multirail"
			}
			t.Run(name, func(t *testing.T) {
				rendered := renderProfileWithProfile(t, profile.dir, &config.Profile{
					Fabric:     profile.fabric,
					Deployment: profile.deployment,
					Multirail:  multirail,
				})
				manifestName, manifest := fileMatching(t, rendered, "example-daemonset")
				docs := parseDocs(t, manifestName, manifest)
				require.Len(t, docs, 1)

				spec := requireMap(t, docs[0], "spec")
				template := requireMap(t, spec, "template")
				podSpec := requireMap(t, template, "spec")
				containers, ok := podSpec["containers"].([]any)
				require.True(t, ok)
				require.Len(t, containers, 2)
				helper, ok := containers[1].(map[string]any)
				require.True(t, ok)
				require.Equal(t, "netshoot", helper["name"])
				require.Equal(t, "nicolaka/netshoot:latest", helper["image"])
			})
		}
	}
}

func TestRenderedSpectrumXExampleDaemonSetsDeclareICMPHelper(t *testing.T) {
	testCases := []struct {
		name           string
		dir            string
		spcxVersion    string
		multiplaneMode string
		numberOfPlanes int
	}{
		{name: "ra2.3-none", dir: "spectrum-x", spcxVersion: "RA2.3", multiplaneMode: "none", numberOfPlanes: 1},
		{name: "ra2.3-uniplane", dir: "spectrum-x", spcxVersion: "RA2.3", multiplaneMode: "uniplane", numberOfPlanes: 1},
		{name: "ra2.3-hwplb", dir: "spectrum-x", spcxVersion: "RA2.3", multiplaneMode: "hwplb", numberOfPlanes: 4},
		{name: "ra2.3-swplb", dir: "spectrum-x", spcxVersion: "RA2.3", multiplaneMode: "swplb", numberOfPlanes: 2},
		{name: "ra2.2-swplb", dir: "spectrum-x-ra2.2", spcxVersion: "RA2.2", multiplaneMode: "swplb", numberOfPlanes: 2},
		{name: "ra2.1-none", dir: "spectrum-x-ra2.1", spcxVersion: "RA2.1", multiplaneMode: "none", numberOfPlanes: 1},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg, err := config.LoadFullConfig(
				filepath.Join("testdata", "grouping", "mixed-same-type.yaml"), ctrllog.Log)
			require.NoError(t, err)
			topologyFile := writeSpectrumXTopology(t, cfg, testCase.numberOfPlanes)
			rendered := renderProfileWithProfile(t, testCase.dir, &config.Profile{
				Fabric:     "ethernet",
				Deployment: "sriov",
				Multirail:  true,
				SpectrumX: &config.ProfileSpectrumX{
					Enable:         true,
					SPCXVersion:    testCase.spcxVersion,
					MultiplaneMode: testCase.multiplaneMode,
					NumberOfPlanes: testCase.numberOfPlanes,
					TopologyType:   config.SpectrumXTopology2Tier,
					TopologyFile:   topologyFile,
				},
			})

			manifestName, manifest := fileMatching(t, rendered, "example-daemonset")
			docs := parseDocs(t, manifestName, manifest)
			require.Len(t, docs, 1)
			spec := requireMap(t, docs[0], "spec")
			template := requireMap(t, spec, "template")
			podSpec := requireMap(t, template, "spec")
			containers, ok := podSpec["containers"].([]any)
			require.True(t, ok)
			require.Len(t, containers, 2)
			helper, ok := containers[1].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "netshoot", helper["name"])
			require.Equal(t, "nicolaka/netshoot:latest", helper["image"])
		})
	}
}

func TestRenderedGPUDirectResourcesAcrossEveryProfileBranch(t *testing.T) {
	standardProfiles := []struct {
		dir        string
		fabric     string
		deployment string
	}{
		{dir: "sriov-ethernet-rdma", fabric: "ethernet", deployment: "sriov"},
		{dir: "sriov-ib-rdma", fabric: "infiniband", deployment: "sriov"},
		{dir: "host-device-rdma", fabric: "ethernet", deployment: "host_device"},
		{dir: "macvlan-rdma-shared", fabric: "ethernet", deployment: "rdma_shared"},
		{dir: "ipoib-rdma-shared", fabric: "infiniband", deployment: "rdma_shared"},
	}
	for _, profile := range standardProfiles {
		for _, multirail := range []bool{false, true} {
			name := fmt.Sprintf("%s/multirail=%t", profile.dir, multirail)
			t.Run(name, func(t *testing.T) {
				cfg := gpudirectRenderConfig(t, &config.Profile{
					Fabric: profile.fabric, Deployment: profile.deployment, Multirail: multirail,
				}, "26.4", 2)
				assertRenderedGPUDirectExample(t, profile.dir, cfg,
					"nvcr.io/nvidia/doca/doca:full-rt-cuda13.0.0-3.4.0-runtime-host", "3")
			})
		}
	}

	spectrumProfiles := []struct {
		name, dir, version, release, mode string
		planes                            int
	}{
		{name: "ra2.1-none", dir: "spectrum-x-ra2.1", version: "RA2.1", release: "26.1", mode: "none", planes: 1},
		{name: "ra2.1-uniplane", dir: "spectrum-x-ra2.1", version: "RA2.1", release: "26.1", mode: "uniplane", planes: 1},
		{name: "ra2.1-hwplb", dir: "spectrum-x-ra2.1", version: "RA2.1", release: "26.1", mode: "hwplb", planes: 2},
		{name: "ra2.1-swplb", dir: "spectrum-x-ra2.1", version: "RA2.1", release: "26.1", mode: "swplb", planes: 2},
		{name: "ra2.2-none", dir: "spectrum-x-ra2.2", version: "RA2.2", release: "26.4", mode: "none", planes: 1},
		{name: "ra2.2-uniplane", dir: "spectrum-x-ra2.2", version: "RA2.2", release: "26.4", mode: "uniplane", planes: 1},
		{name: "ra2.2-hwplb", dir: "spectrum-x-ra2.2", version: "RA2.2", release: "26.4", mode: "hwplb", planes: 4},
		{name: "ra2.2-swplb", dir: "spectrum-x-ra2.2", version: "RA2.2", release: "26.4", mode: "swplb", planes: 2},
		{name: "ra2.3-none", dir: "spectrum-x", version: "RA2.3", release: "26.7", mode: "none", planes: 1},
		{name: "ra2.3-uniplane", dir: "spectrum-x", version: "RA2.3", release: "26.7", mode: "uniplane", planes: 1},
		{name: "ra2.3-hwplb", dir: "spectrum-x", version: "RA2.3", release: "26.7", mode: "hwplb", planes: 4},
		{name: "ra2.3-swplb", dir: "spectrum-x", version: "RA2.3", release: "26.7", mode: "swplb", planes: 2},
	}
	for _, profile := range spectrumProfiles {
		t.Run(profile.name, func(t *testing.T) {
			cfg := gpudirectRenderConfig(t, nil, profile.release, 2)
			topologyFile := writeSpectrumXTopology(t, cfg, profile.planes)
			cfg.Profile = &config.Profile{
				Fabric: "ethernet", Deployment: "sriov", Multirail: true,
				SpectrumX: &config.ProfileSpectrumX{
					Enable: true, SPCXVersion: profile.version, MultiplaneMode: profile.mode,
					NumberOfPlanes: profile.planes, TopologyType: config.SpectrumXTopology2Tier,
					TopologyFile: topologyFile,
				},
			}
			image := "nvcr.io/nvidia/doca/doca:full-rt-cuda13.0.0-3.4.0-runtime-host"
			if profile.release == "26.1" {
				image = "nvcr.io/nvidia/doca/doca:full-rt-cuda13.0.0-3.2.3-runtime-host"
			}
			if profile.release == "26.7" {
				image = "nvcr.io/nvstaging/doca/doca:full-rt-cuda13.0.0-3.5.0-runtime-host-dev"
			}
			assertRenderedGPUDirectExample(t, profile.dir, cfg, image, "3")
		})
	}
}

func gpudirectRenderConfig(t *testing.T, profile *config.Profile, release string, gpuIndex int) *config.LaunchKitConfig {
	t.Helper()
	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
	cfg, err := config.LoadFullConfig(filepath.Join("testdata", "grouping", "mixed-same-type.yaml"), ctrllog.Log)
	require.NoError(t, err)
	cfg.Profile = profile
	cfg.NetworkOperator.SelectedRelease = release
	cfg.NetworkOperator.ImagePullSecrets = []string{"yes"}
	cfg.Validation.GPUDirect.Enabled = true
	cfg.Validation.GPUDirect.GPUResourceType = "example.com/gpu"
	for groupIndex := range cfg.ClusterConfig {
		for pfIndex := range cfg.ClusterConfig[groupIndex].PFs {
			cfg.ClusterConfig[groupIndex].PFs[pfIndex].ConnectedGPU = fmt.Sprintf("GPU%d", gpuIndex)
		}
	}
	return cfg
}

func assertRenderedGPUDirectExample(t *testing.T, profileDir string, cfg *config.LaunchKitConfig, image, count string) {
	t.Helper()
	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(loadProfileFromDir(t, profileDir), cfg)
	require.NoError(t, err)
	manifestName, manifest := fileMatching(t, rendered, "example-daemonset")
	docs := parseDocs(t, manifestName, manifest)
	require.Len(t, docs, 1)
	podSpec := requireMap(t, requireMap(t, requireMap(t, docs[0], "spec"), "template"), "spec")
	pullSecrets, ok := podSpec["imagePullSecrets"].([]any)
	require.True(t, ok)
	require.Equal(t, map[string]any{"name": "yes"}, pullSecrets[0])
	containers, ok := podSpec["containers"].([]any)
	require.True(t, ok)
	require.Len(t, containers, 2)
	primary := containers[0].(map[string]any)
	require.Equal(t, image, primary["image"])
	resources := requireMap(t, primary, "resources")
	require.Equal(t, count, requireMap(t, resources, "requests")["example.com/gpu"])
	require.Equal(t, count, requireMap(t, resources, "limits")["example.com/gpu"])
	_, helperHasResources := containers[1].(map[string]any)["resources"]
	require.False(t, helperHasResources)
}

func requireMap(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key].(map[string]any)
	require.Truef(t, ok, "%s is missing or is not a map in %v", key, parent)
	return value
}
