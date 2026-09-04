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
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/stretchr/testify/require"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	yaml "sigs.k8s.io/yaml"
)

// loadSRIOVProfile resolves an on-disk SR-IOV profile (sriov-ethernet-rdma or
// sriov-ib-rdma) relative to this test file, mirroring loadSpectrumXProfile.
func loadSRIOVProfile(t *testing.T, dir, fabric, networkTemplate string) *profiles.Profile {
	t.Helper()
	profileDir, err := filepath.Abs(filepath.Join("..", "..", "profiles", dir))
	require.NoError(t, err)

	p := &profiles.Profile{
		Name:   dir,
		Plugin: "network-operator",
		ProfileRequirements: profiles.ProfileRequirements{
			Fabric:     fabric,
			Deployment: "sriov",
		},
		Templates: []string{
			"10-nicclusterpolicy.yaml",
			"11-nicnodepolicy.yaml",
			"20-ippool.yaml",
			"30-nicinterfacenametemplate.yaml",
			"40-sriovnetworknodepolicy.yaml",
			networkTemplate,
			"60-example-daemonset.yaml",
		},
	}
	p.UpdateManifestsPaths(profileDir)
	return p
}

// renderSRIOV runs GenerateProfileDeploymentFiles for an SR-IOV profile in
// multirail mode against a grouping testdata config, exactly as the generate
// pipeline does.
func renderSRIOV(t *testing.T, dir, fabric, networkTemplate string) map[string]string {
	return renderSRIOVWithNamespaces(t, dir, fabric, networkTemplate, nil)
}

// renderSRIOVWithNamespaces is renderSRIOV with the network-namespace fan-out
// the CLI's ApplyOptionsToConfig would normally seed: NetworkNamespaces drives
// per-namespace duplication and the renderer selects each current entry.
func renderSRIOVWithNamespaces(t *testing.T, dir, fabric, networkTemplate string, namespaces []string) map[string]string {
	t.Helper()
	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))

	cfg, err := config.LoadFullConfig(
		filepath.Join("testdata", "grouping", "mixed-same-type.yaml"),
		ctrllog.Log,
	)
	require.NoError(t, err)

	cfg.Profile = &config.Profile{
		Fabric:     fabric,
		Deployment: "sriov",
		Multirail:  true,
	}
	if len(namespaces) > 0 {
		cfg.NetworkNamespaces = namespaces
	}

	plugin := &NetworkOperatorPlugin{}
	rendered, err := plugin.GenerateProfileDeploymentFiles(loadSRIOVProfile(t, dir, fabric, networkTemplate), cfg)
	require.NoError(t, err)
	return rendered
}

func metaString(t *testing.T, doc map[string]any, key string) string {
	t.Helper()
	meta, ok := doc["metadata"].(map[string]any)
	require.True(t, ok, "doc has no metadata map")
	v, ok := meta[key].(string)
	require.Truef(t, ok, "metadata.%s missing or not a string in %v", key, meta)
	return v
}

// parseDocs splits a rendered manifest the way `kubectl apply` does (on a
// line that is just `---`) and unmarshals every non-empty document into a
// map, returning the parsed docs. A failure here is exactly the symptom of
// the glued-separator bug: when `---` is appended to the `resourceName:`
// line instead of sitting on its own line, the whole file collapses into a
// single document with duplicate top-level keys, which fails to unmarshal.
func parseDocs(t *testing.T, name, content string) []map[string]any {
	t.Helper()
	var docs []map[string]any
	for i, raw := range splitYAMLDocuments(content) {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var doc map[string]any
		err := yaml.UnmarshalStrict([]byte(raw), &doc)
		require.NoErrorf(t, err, "file %s doc %d is not valid YAML (likely a glued separator):\n%s", name, i, raw)
		docs = append(docs, doc)
	}
	return docs
}

// fileMatching returns the single rendered file whose basename contains substr.
func fileMatching(t *testing.T, rendered map[string]string, substr string) (string, string) {
	t.Helper()
	names := fileNamesMatching(rendered, substr)
	require.Lenf(t, names, 1, "expected exactly one file matching %q, got %v", substr, names)
	return names[0], rendered[names[0]]
}

func specString(t *testing.T, doc map[string]any, key string) string {
	t.Helper()
	spec, ok := doc["spec"].(map[string]any)
	require.True(t, ok, "doc has no spec map")
	v, ok := spec[key].(string)
	require.Truef(t, ok, "spec.%s missing or not a string in %v", key, spec)
	return v
}

func optionalSpecString(t *testing.T, doc map[string]any, key string) (string, bool) {
	t.Helper()
	spec, ok := doc["spec"].(map[string]any)
	require.True(t, ok, "doc has no spec map")
	v, ok := spec[key].(string)
	return v, ok
}

// loadProfileFromDir reads a profile's real on-disk profile.yaml (including its
// template list) so the test renders exactly what `l8k generate` would.
func loadProfileFromDir(t *testing.T, dir string) *profiles.Profile {
	t.Helper()
	profileDir, err := filepath.Abs(filepath.Join("..", "..", "profiles", dir))
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(profileDir, "profile.yaml"))
	require.NoError(t, err)
	p := &profiles.Profile{}
	require.NoError(t, yaml.Unmarshal(data, p))
	p.UpdateManifestsPaths(profileDir)
	return p
}

// renderProfile renders a whole profile (its real template set) against the
// mixed-same-type grouping config in multirail mode.
func renderProfile(t *testing.T, dir, fabric, deployment string) map[string]string {
	return renderProfileWithProfile(t, dir, &config.Profile{Fabric: fabric, Deployment: deployment, Multirail: true})
}

func renderProfileWithProfile(t *testing.T, dir string, profile *config.Profile) map[string]string {
	t.Helper()
	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
	cfg, err := config.LoadFullConfig(
		filepath.Join("testdata", "grouping", "mixed-same-type.yaml"), ctrllog.Log)
	require.NoError(t, err)
	cfg.Profile = profile
	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(loadProfileFromDir(t, dir), cfg)
	require.NoError(t, err)
	return rendered
}

// countKindLines counts `kind:` document headers (lines whose trimmed form
// starts with "kind:") in a rendered manifest.
func countKindLines(content string) int {
	n := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "kind:") {
			n++
		}
	}
	return n
}

// TestProfileManifestsAreValidMultiDocYAML is the broad regression guard for
// the glued-`---`-separator class of bug across EVERY standard profile —
// including the IPPool / Network / NodePolicy templates beyond the four SR-IOV
// files that originally regressed. For each rendered manifest it strict-parses
// every document and asserts the document count equals the number of `kind:`
// headers: a glued separator (e.g. `resourceName: foo---`) merges two docs into
// one, which both drops the count and trips UnmarshalStrict on the duplicate
// top-level keys.
func TestProfileManifestsAreValidMultiDocYAML(t *testing.T) {
	profilesUnderTest := []struct{ dir, fabric, deployment string }{
		{"sriov-ethernet-rdma", "ethernet", "sriov"},
		{"sriov-ib-rdma", "infiniband", "sriov"},
		{"host-device-rdma", "ethernet", "host_device"},
		{"macvlan-rdma-shared", "ethernet", "rdma_shared"},
		{"ipoib-rdma-shared", "infiniband", "rdma_shared"},
	}
	for _, p := range profilesUnderTest {
		t.Run(p.dir, func(t *testing.T) {
			rendered := renderProfile(t, p.dir, p.fabric, p.deployment)
			require.NotEmpty(t, rendered)
			sawIPPool := false
			for name, content := range rendered {
				if !strings.HasSuffix(name, ".yaml") || isHelmValuesTemplate(name) {
					continue
				}
				wantDocs := countKindLines(content)
				if wantDocs == 0 {
					continue // values-like file with no Kind
				}
				docs := parseDocs(t, name, content)
				require.Equalf(t, wantDocs, len(docs),
					"%s: expected %d documents (one per `kind:`), got %d — a glued `---` separator merges documents",
					name, wantDocs, len(docs))
				if strings.Contains(name, "20-ippool") {
					sawIPPool = true
					require.Equal(t, 8, len(docs), "multirail IPPool must emit one valid doc per rail")
				}
			}
			require.True(t, sawIPPool, "expected an IPPool manifest in profile %s", p.dir)
		})
	}
}

func TestSpectrumXIPv6CIDRPoolRendering(t *testing.T) {
	profilesUnderTest := []struct {
		dir            string
		spcxVersion    string
		multiplaneMode string
		numberOfPlanes int
	}{
		{
			dir:            "spectrum-x",
			spcxVersion:    "RA2.3",
			multiplaneMode: "hwplb",
			numberOfPlanes: 4,
		},
		{
			dir:            "spectrum-x-ra2.2",
			spcxVersion:    "RA2.2",
			multiplaneMode: "swplb",
			numberOfPlanes: 2,
		},
		{
			dir:            "spectrum-x-ra2.1",
			spcxVersion:    "RA2.1",
			multiplaneMode: "hwplb",
			numberOfPlanes: 2,
		},
	}

	for _, profile := range profilesUnderTest {
		t.Run(profile.dir, func(t *testing.T) {
			cfg, err := config.LoadFullConfig(
				filepath.Join("testdata", "grouping", "mixed-same-type.yaml"), ctrllog.Log)
			require.NoError(t, err)
			cfg.Profile = &config.Profile{
				Fabric:     "ethernet",
				Deployment: "sriov",
				Multirail:  true,
				SpectrumX: &config.ProfileSpectrumX{
					Enable:         true,
					SPCXVersion:    profile.spcxVersion,
					MultiplaneMode: profile.multiplaneMode,
					NumberOfPlanes: profile.numberOfPlanes,
					TopologyType:   config.SpectrumXTopology2Tier,
					IPVersion:      config.SpectrumXIPVersionIPv6,
					TopologyFile:   writeSpectrumXTopology(t, cfg, profile.numberOfPlanes),
				},
			}

			rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(
				loadProfileFromDir(t, profile.dir), cfg)
			require.NoError(t, err)
			name, content := fileMatching(t, rendered, "60-cidrpool")

			for _, line := range strings.Split(content, "\n") {
				if strings.Contains(line, "---") {
					require.Equal(t, "---", strings.TrimSpace(line),
						"%s must put each YAML document separator on its own line", name)
				}
			}

			docs := parseDocs(t, name, content)
			require.Greater(t, len(docs), 1, "%s must contain multiple CIDRPool documents", name)
			require.Equal(t, countKindLines(content), len(docs),
				"%s must have one YAML document per CIDRPool kind", name)
			for _, doc := range docs {
				require.Equal(t, "CIDRPool", doc["kind"])
				spec, ok := doc["spec"].(map[string]any)
				require.True(t, ok, "CIDRPool has no spec map")
				require.EqualValues(t, 2, spec["gatewayIndex"])
				require.EqualValues(t, 64, spec["perNodeNetworkPrefix"])
				require.Contains(t, spec["cidr"], "/40")

				exclusions, ok := spec["perNodeExclusions"].([]any)
				require.True(t, ok)
				require.Len(t, exclusions, 1)
				exclusion, ok := exclusions[0].(map[string]any)
				require.True(t, ok)
				require.EqualValues(t, 2, exclusion["startIndex"])
				require.EqualValues(t, 2, exclusion["endIndex"])

				routes, ok := spec["routes"].([]any)
				require.True(t, ok)
				require.Len(t, routes, 1)
				route, ok := routes[0].(map[string]any)
				require.True(t, ok)
				require.Equal(t, "fd02::/24", route["dst"])

				allocations, ok := spec["staticAllocations"].([]any)
				require.True(t, ok)
				require.NotEmpty(t, allocations)
				for _, rawAllocation := range allocations {
					allocation, ok := rawAllocation.(map[string]any)
					require.True(t, ok)
					prefix, ok := allocation["prefix"].(string)
					require.True(t, ok)
					require.True(t, strings.HasSuffix(prefix, "/64"))
					gateway, ok := allocation["gateway"].(string)
					require.True(t, ok)
					require.True(t, strings.HasSuffix(gateway, "::2"))
				}
			}
		})
	}
}

func TestSecondaryNetworkMetaPluginsHelper(t *testing.T) {
	t.Run("default empty", func(t *testing.T) {
		require.Empty(t, secondaryNetworkMetaPlugins(&config.Profile{}))
	})

	t.Run("source based routing only", func(t *testing.T) {
		meta := secondaryNetworkMetaPlugins(&config.Profile{Routing: config.RoutingSourceBased})
		require.Contains(t, meta, `"type": "sbr"`)
		require.NotContains(t, meta, `"type": "tuning"`)
	})

	t.Run("ignore arp only", func(t *testing.T) {
		meta := secondaryNetworkMetaPlugins(&config.Profile{IgnoreARP: true})
		require.Contains(t, meta, `"type": "tuning"`)
		require.Contains(t, meta, `"net.ipv4.conf.IFNAME.arp_ignore": "1"`)
		require.NotContains(t, meta, `"type": "sbr"`)
	})

	t.Run("tuning before sbr", func(t *testing.T) {
		meta := secondaryNetworkMetaPlugins(&config.Profile{
			Routing:   config.RoutingSourceBased,
			IgnoreARP: true,
		})
		tuningIdx := strings.Index(meta, `"type": "tuning"`)
		sbrIdx := strings.Index(meta, `"type": "sbr"`)
		require.NotEqual(t, -1, tuningIdx)
		require.NotEqual(t, -1, sbrIdx)
		require.Less(t, tuningIdx, sbrIdx)
	})

	t.Run("spectrum x ignored", func(t *testing.T) {
		meta := secondaryNetworkMetaPlugins(&config.Profile{
			Routing:   config.RoutingSourceBased,
			IgnoreARP: true,
			SpectrumX: &config.ProfileSpectrumX{Enable: true},
		})
		require.Empty(t, meta)
	})
}

func TestNonSpectrumXProfilesRenderMetaPlugins(t *testing.T) {
	profilesUnderTest := []struct {
		dir        string
		fabric     string
		deployment string
		fileSubstr string
	}{
		{"sriov-ethernet-rdma", "ethernet", "sriov", "50-sriovnetwork"},
		{"sriov-ib-rdma", "infiniband", "sriov", "50-sriovibnetwork"},
		{"host-device-rdma", "ethernet", "host_device", "30-hostdevicenetwork"},
		{"macvlan-rdma-shared", "ethernet", "rdma_shared", "30-macvlannetwork"},
		{"ipoib-rdma-shared", "infiniband", "rdma_shared", "30-ipoibnetwork"},
	}

	for _, p := range profilesUnderTest {
		t.Run(p.dir, func(t *testing.T) {
			rendered := renderProfileWithProfile(t, p.dir, &config.Profile{
				Fabric:     p.fabric,
				Deployment: p.deployment,
				Multirail:  true,
				Routing:    config.RoutingSourceBased,
				IgnoreARP:  true,
			})
			name, content := fileMatching(t, rendered, p.fileSubstr)
			docs := parseDocs(t, name, content)
			require.NotEmpty(t, docs)
			for _, doc := range docs {
				metaPlugins := specString(t, doc, "metaPlugins")
				require.Contains(t, metaPlugins, `"type": "tuning"`)
				require.Contains(t, metaPlugins, `"net.ipv4.conf.all.arp_ignore": "1"`)
				require.Contains(t, metaPlugins, `"net.ipv4.conf.IFNAME.arp_announce": "2"`)
				require.Contains(t, metaPlugins, `"type": "sbr"`)
				require.Less(t,
					strings.Index(metaPlugins, `"type": "tuning"`),
					strings.Index(metaPlugins, `"type": "sbr"`),
					"tuning must precede sbr")
			}
		})
	}
}

func TestDefaultProfilesDoNotRenderMetaPlugins(t *testing.T) {
	rendered := renderProfile(t, "sriov-ethernet-rdma", "ethernet", "sriov")
	name, content := fileMatching(t, rendered, "50-sriovnetwork")
	for _, doc := range parseDocs(t, name, content) {
		_, ok := optionalSpecString(t, doc, "metaPlugins")
		require.False(t, ok, "default destination-based routing with ignoreARP=false must not render metaPlugins")
	}
}

// TestSRIOVMultiDocSeparators is the regression test for the broken-formatting
// bug: the multirail SR-IOV templates used to glue the `---` document
// separator (and, for SriovIBNetwork, the `linkState: enable` key) directly
// onto the `resourceName:` line, producing manifests that `kubectl apply`
// rejected. Each multi-doc network file must now split into one valid document
// per east-west rail with the expected, un-glued field values.
func TestSRIOVMultiDocSeparators(t *testing.T) {
	const wantRails = 8 // mixed-same-type.yaml merges to one 8-rail bucket

	t.Run("ethernet sriovnetworknodepolicy", func(t *testing.T) {
		rendered := renderSRIOV(t, "sriov-ethernet-rdma", "ethernet", "50-sriovnetwork.yaml")
		name, content := fileMatching(t, rendered, "40-sriovnetworknodepolicy")
		require.NotContains(t, content, "---apiVersion", "separator must sit on its own line")
		docs := parseDocs(t, name, content)
		require.Len(t, docs, wantRails)
		for i, doc := range docs {
			require.Equal(t, "SriovNetworkNodePolicy", doc["kind"])
			require.Equal(t, fmt.Sprintf("sriov_resource_rail_%d", i), specString(t, doc, "resourceName"))
		}
	})

	t.Run("ethernet sriovnetwork", func(t *testing.T) {
		rendered := renderSRIOV(t, "sriov-ethernet-rdma", "ethernet", "50-sriovnetwork.yaml")
		name, content := fileMatching(t, rendered, "50-sriovnetwork")
		require.NotContains(t, content, "---apiVersion", "separator must sit on its own line")
		docs := parseDocs(t, name, content)
		require.Len(t, docs, wantRails)
		for i, doc := range docs {
			require.Equal(t, "SriovNetwork", doc["kind"])
			require.Equal(t, fmt.Sprintf("sriov_resource_rail_%d", i), specString(t, doc, "resourceName"))
		}
	})

	t.Run("infiniband sriovnetworknodepolicy", func(t *testing.T) {
		rendered := renderSRIOV(t, "sriov-ib-rdma", "infiniband", "50-sriovibnetwork.yaml")
		name, content := fileMatching(t, rendered, "40-sriovnetworknodepolicy")
		require.NotContains(t, content, "---apiVersion", "separator must sit on its own line")
		docs := parseDocs(t, name, content)
		require.Len(t, docs, wantRails)
		for i, doc := range docs {
			require.Equal(t, "SriovNetworkNodePolicy", doc["kind"])
			require.Equal(t, fmt.Sprintf("sriov_resource_rail_%d", i), specString(t, doc, "resourceName"))
		}
	})

	t.Run("infiniband sriovibnetwork keeps resourceName and linkState separate", func(t *testing.T) {
		rendered := renderSRIOV(t, "sriov-ib-rdma", "infiniband", "50-sriovibnetwork.yaml")
		name, content := fileMatching(t, rendered, "50-sriovibnetwork")
		require.NotContains(t, content, "---apiVersion", "separator must sit on its own line")
		docs := parseDocs(t, name, content)
		require.Len(t, docs, wantRails)
		for i, doc := range docs {
			require.Equal(t, "SriovIBNetwork", doc["kind"])
			// The glued bug produced `resourceName: <name>  linkState: enable`,
			// which parsed (if at all) into a single scalar. Strict parsing plus
			// these per-key assertions lock both keys to their own lines.
			require.Equal(t, fmt.Sprintf("sriov_resource_rail_%d", i), specString(t, doc, "resourceName"))
			require.Equal(t, "enable", specString(t, doc, "linkState"))
		}
	})
}

// TestNetworkNamespacesFanOut covers --network-namespaces: the secondary-network
// CRs and the example test DaemonSet are duplicated once per namespace (each
// pointed at its namespace, with namespace-suffixed names so the copies don't
// collide), while shared resources — IPPool, SriovNetworkNodePolicy,
// NicClusterPolicy — are rendered exactly once.
func TestNetworkNamespacesFanOut(t *testing.T) {
	t.Run("two namespaces duplicate networks + DS but not shared CRs", func(t *testing.T) {
		rendered := renderSRIOVWithNamespaces(t, "sriov-ethernet-rdma", "ethernet", "50-sriovnetwork.yaml", []string{"ns1", "ns2"})

		// Network CRs + example DS: one file per namespace.
		require.Len(t, fileNamesMatching(rendered, "50-sriovnetwork"), 2)
		require.Len(t, fileNamesMatching(rendered, "60-example-daemonset"), 2)

		// Shared resources: never duplicated.
		require.Len(t, fileNamesMatching(rendered, "20-ippool"), 1)
		require.Len(t, fileNamesMatching(rendered, "40-sriovnetworknodepolicy"), 1)
		require.Len(t, fileNamesMatching(rendered, "10-nicclusterpolicy"), 1)

		for _, ns := range []string{"ns1", "ns2"} {
			netName := "50-sriovnetwork-gpu-model-y-" + ns + ".yaml"
			content, ok := rendered[netName]
			require.Truef(t, ok, "expected per-namespace file %s; got %v", netName, fileNamesMatching(rendered, "50-sriovnetwork"))
			for i, doc := range parseDocs(t, netName, content) {
				require.Equal(t, fmt.Sprintf("sriov-network-rail-%d-gpu-model-y-%s", i, ns), metaString(t, doc, "name"))
				require.Equal(t, ns, specString(t, doc, "networkNamespace"))
				// resourceName is the SHARED device-plugin resource — it must
				// NOT carry the namespace suffix, since the single
				// SriovNetworkNodePolicy registers it once for all namespaces.
				require.Equal(t, fmt.Sprintf("sriov_resource_rail_%d", i), specString(t, doc, "resourceName"))
			}

			dsName := "60-example-daemonset-gpu-model-y-" + ns + ".yaml"
			dsContent, ok := rendered[dsName]
			require.Truef(t, ok, "expected per-namespace DS file %s", dsName)
			dsDocs := parseDocs(t, dsName, dsContent)
			require.Len(t, dsDocs, 1)
			require.Equal(t, ns, metaString(t, dsDocs[0], "namespace"))
			// The DS must reference the namespace-suffixed network names.
			require.Contains(t, dsContent, "sriov-network-rail-0-gpu-model-y-"+ns)
			require.NotContains(t, dsContent, "sriov-network-rail-0-gpu-model-y,")
		}
	})

	t.Run("single namespace keeps unsuffixed names", func(t *testing.T) {
		rendered := renderSRIOVWithNamespaces(t, "sriov-ethernet-rdma", "ethernet", "50-sriovnetwork.yaml", []string{"default"})

		name, content := fileMatching(t, rendered, "50-sriovnetwork")
		require.Equal(t, "50-sriovnetwork-gpu-model-y.yaml", name, "single namespace must not suffix the filename")
		for i, doc := range parseDocs(t, name, content) {
			require.Equal(t, fmt.Sprintf("sriov-network-rail-%d-gpu-model-y", i), metaString(t, doc, "name"))
			require.Equal(t, "default", specString(t, doc, "networkNamespace"))
		}
	})
}
