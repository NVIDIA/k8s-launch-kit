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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
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

func requireMap(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key].(map[string]any)
	require.Truef(t, ok, "%s is missing or is not a map in %v", key, parent)
	return value
}
