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
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/stretchr/testify/require"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// loadSpectrumXProfile resolves the on-disk spectrum-x profile relative to this
// test file. Profile templates live at repo_root/profiles/spectrum-x/, and this
// package sits at repo_root/pkg/networkoperatorplugin/.
func loadSpectrumXProfile(t *testing.T) *profiles.Profile {
	t.Helper()
	profileDir, err := filepath.Abs(filepath.Join("..", "..", "profiles", "spectrum-x"))
	require.NoError(t, err)

	p := &profiles.Profile{
		Name:   "Spectrum-X Multi-Rail",
		Plugin: "network-operator",
		ProfileRequirements: profiles.ProfileRequirements{
			Fabric:     "ethernet",
			Deployment: "sriov",
			SpectrumX: &profiles.ProfileRequirementsSpectrumX{
				SPCXVersion:    "RA2.2",
				MultiplaneMode: []string{"swplb", "hwplb", "uniplane", "none"},
			},
		},
		Templates: []string{
			"10-nicclusterpolicy.yaml",
			"30-nicconfigurationtemplate.yaml",
			"25-nicinterfacenametemplate.yaml",
			"60-cidrpool.yaml",
			"80-spectrumxrailpoolconfig.yaml",
			"90-example-daemonset.yaml",
		},
	}
	p.UpdateManifestsPaths(profileDir)
	return p
}

// renderSpectrumX loads a testdata cluster config, fills in the Spectrum-X
// profile metadata the CLI would normally attach via ApplyOptionsToConfig, and
// runs GenerateProfileDeploymentFiles exactly as the generate pipeline does.
func renderSpectrumX(t *testing.T, configName, multiplaneMode string, numberOfPlanes int) map[string]string {
	t.Helper()
	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))

	cfg, err := config.LoadFullConfig(
		filepath.Join("testdata", "grouping", configName),
		ctrllog.Log,
	)
	require.NoError(t, err)

	// Attach the profile metadata that the CLI would normally inject.
	cfg.Profile = &config.Profile{
		Fabric:     "ethernet",
		Deployment: "sriov",
		Multirail:  true,
		SpectrumX: &config.ProfileSpectrumX{
			Enable:         true,
			SPCXVersion:    "RA2.2",
			MultiplaneMode: multiplaneMode,
			NumberOfPlanes: numberOfPlanes,
		},
	}

	plugin := &NetworkOperatorPlugin{}
	rendered, err := plugin.GenerateProfileDeploymentFiles(loadSpectrumXProfile(t), cfg)
	require.NoError(t, err)
	return rendered
}

// fileNamesMatching returns the sorted file names whose basenames contain the
// given substring. Used to assert the grouping outcome by counting per-CRD files.
func fileNamesMatching(rendered map[string]string, substr string) []string {
	var out []string
	for name := range rendered {
		if strings.Contains(name, substr) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func TestSpectrumXGrouping(t *testing.T) {
	tests := []struct {
		name            string
		configFile      string
		planes          int
		multiplaneMode  string
		wantRailPools   []string // expected rail-pool-config filenames (sorted)
		wantNameTmpls   []string // expected nic-interface-name-template filenames (sorted)
		wantCidrPools   []string // expected cidrpool filenames (sorted)
		wantNicCfgTmpls []string // expected nic-configuration-template filenames (sorted)
	}{
		{
			// Two groups that share every east-west PCI address but have different
			// north-south DPU PCIs. Merge keys on (gpuType, east-west rail count)
			// — both match — and hasRailPciConflict only looks at east-west PFs, so
			// no conflict. After merging, every rail's RailPciAddresses has a single
			// address (source groups agreed), so mergedGroupsAgreeOnPci returns true
			// and a single merged NicInterfaceNameTemplate is emitted.
			name:           "same east-west PCIs different north-south: merges fully, single name template",
			configFile:     "same-ew-different-ns.yaml",
			planes:         1,
			multiplaneMode: "none",
			wantRailPools:  []string{"80-spectrumxrailpoolconfig-gpu-model-x.yaml"},
			wantNameTmpls:  []string{"25-nicinterfacenametemplate-gpu-model-x.yaml"},
			wantCidrPools:  []string{"60-cidrpool-gpu-model-x.yaml"},
			wantNicCfgTmpls: []string{
				"30-nicconfigurationtemplate-gpu-model-x.yaml",
			},
		},
		{
			// Three groups with the same gpuType and east-west rail count but
			// DIFFERENT east-west PCI layouts per machine. mergeCompatibleGroups
			// still merges them because name templates are enabled (hadPciConflicts
			// is tolerated). The merged rail pool uses stable renamed pfNames
			// (eth_p*_r*), so the cross-group PCI differences don't break selection.
			// Since source groups disagree on PCI, mergedGroupsAgreeOnPci is false
			// and rendering falls back to per-unmerged-group name templates.
			name:           "mixed same gpuType different PCIs: one merged rail pool, three name templates",
			configFile:     "mixed-same-type.yaml",
			planes:         1,
			multiplaneMode: "none",
			wantRailPools:  []string{"80-spectrumxrailpoolconfig-gpu-model-y.yaml"},
			wantNameTmpls: []string{
				"25-nicinterfacenametemplate-group-0.yaml",
				"25-nicinterfacenametemplate-group-1.yaml",
				"25-nicinterfacenametemplate-group-2.yaml",
			},
			wantCidrPools: []string{"60-cidrpool-gpu-model-y.yaml"},
			wantNicCfgTmpls: []string{
				"30-nicconfigurationtemplate-gpu-model-y.yaml",
			},
		},
		{
			// Two groups of gpu-model-y (8 east-west rails each) + one group of
			// gpu-model-z (4 east-west rails). Merge key is (gpuType, rail count):
			//   - (gpu-model-y, 8) pulls the two y-groups together
			//   - (gpu-model-z, 4) stays alone as group-2
			// Result: two rail pools (one per merged bucket), three name templates
			// (two per-machine for the merged y-pair since their PCIs differ, one
			// for the unmerged z group).
			name:           "true heterogeneous: two gpuTypes, only one pair merges",
			configFile:     "true-heterogeneous.yaml",
			planes:         1,
			multiplaneMode: "none",
			wantRailPools: []string{
				"80-spectrumxrailpoolconfig-gpu-model-y.yaml",
				"80-spectrumxrailpoolconfig-group-2.yaml",
			},
			wantNameTmpls: []string{
				"25-nicinterfacenametemplate-group-0.yaml",
				"25-nicinterfacenametemplate-group-1.yaml",
				"25-nicinterfacenametemplate-group-2.yaml",
			},
			wantCidrPools: []string{
				"60-cidrpool-gpu-model-y.yaml",
				"60-cidrpool-group-2.yaml",
			},
			wantNicCfgTmpls: []string{
				"30-nicconfigurationtemplate-gpu-model-y.yaml",
				"30-nicconfigurationtemplate-group-2.yaml",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rendered := renderSpectrumX(t, tc.configFile, tc.multiplaneMode, tc.planes)

			require.Equal(t, tc.wantRailPools,
				fileNamesMatching(rendered, "80-spectrumxrailpoolconfig"),
				"rail-pool manifests mismatch")
			require.Equal(t, tc.wantNameTmpls,
				fileNamesMatching(rendered, "25-nicinterfacenametemplate"),
				"nic-interface-name-template manifests mismatch")
			require.Equal(t, tc.wantCidrPools,
				fileNamesMatching(rendered, "60-cidrpool"),
				"cidrpool manifests mismatch")
			require.Equal(t, tc.wantNicCfgTmpls,
				fileNamesMatching(rendered, "30-nicconfigurationtemplate"),
				"nic-configuration-template manifests mismatch")

			// NCP is cluster-wide and always produced exactly once.
			require.Equal(t, []string{"10-nicclusterpolicy.yaml"},
				fileNamesMatching(rendered, "10-nicclusterpolicy"),
				"expected exactly one NicClusterPolicy")
		})
	}
}

func TestSpectrumXGrouping_MergedRailPoolContent(t *testing.T) {
	// The merged y-model rail pool must select by gpuType (covers all y-model
	// nodes across machine types) and reference renamed netdev names rather than
	// raw PCI addresses, since source groups disagree on PCI.
	rendered := renderSpectrumX(t, "true-heterogeneous.yaml", "none", 1)

	merged, ok := rendered["80-spectrumxrailpoolconfig-gpu-model-y.yaml"]
	require.True(t, ok, "merged y-model rail pool manifest should exist")

	require.Contains(t, merged, `nvidia.kubernetes-launch-kit.gpu: "gpu-model-y"`,
		"merged rail pool must select by gpuType so it covers all y-model machines")
	require.Contains(t, merged, `pfNames: ["eth_p0_r0"]`,
		"merged rail pool must reference the renamed netdev names, not raw PCI")
	require.NotContains(t, merged, "0000:19:00.0",
		"merged rail pool must not leak any machine-specific PCI address")

	// The unmerged z-model rail pool should preserve the source group's full
	// nodeSelector (not the stripped-down product-type-only selector used for
	// merged groups).
	z, ok := rendered["80-spectrumxrailpoolconfig-group-2.yaml"]
	require.True(t, ok, "unmerged z-model rail pool manifest should exist")
	require.Contains(t, z, `nvidia.com/gpu.machine: "machine-c"`,
		"unmerged z-model rail pool must preserve the source group's full nodeSelector")
}

// loadSpectrumXRA21Profile resolves the on-disk spectrum-x-ra2.1 profile.
// This is the 26.1-only sibling of the spectrum-x profile; it renders the
// SR-IOV-operator CRD chain (SriovNetworkPoolConfig + SriovNetworkNodePolicy
// + OVSNetwork) plus the v1alpha1 SpectrumXRailPoolConfig glue resource.
func loadSpectrumXRA21Profile(t *testing.T) *profiles.Profile {
	t.Helper()
	profileDir, err := filepath.Abs(filepath.Join("..", "..", "profiles", "spectrum-x-ra2.1"))
	require.NoError(t, err)

	p := &profiles.Profile{
		Name:   "Spectrum-X Multi-Rail (RA2.1, Network Operator 26.1)",
		Plugin: "network-operator",
		ProfileRequirements: profiles.ProfileRequirements{
			Fabric:     "ethernet",
			Deployment: "sriov",
			SpectrumX: &profiles.ProfileRequirementsSpectrumX{
				SPCXVersion:    "RA2.1",
				MultiplaneMode: []string{"swplb", "hwplb", "uniplane", "none"},
			},
			MinNetworkOperatorRelease: "26.1",
			MaxNetworkOperatorRelease: "26.1",
		},
		Templates: []string{
			"10-nicclusterpolicy.yaml",
			"25-nicinterfacenametemplate.yaml",
			"30-nicconfigurationtemplate.yaml",
			"40-sriovnetworkpoolconfig.yaml",
			"50-sriovnetworknodepolicy.yaml",
			"55-ovsnetwork.yaml",
			"60-cidrpool.yaml",
			"80-spectrumxrailpoolconfig.yaml",
			"90-example-daemonset.yaml",
		},
	}
	p.UpdateManifestsPaths(profileDir)
	return p
}

// renderSpectrumXRA21 mirrors renderSpectrumX but exercises the RA2.1 profile.
func renderSpectrumXRA21(t *testing.T, configName, multiplaneMode string, numberOfPlanes int) map[string]string {
	t.Helper()
	ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))

	cfg, err := config.LoadFullConfig(
		filepath.Join("testdata", "grouping", configName),
		ctrllog.Log,
	)
	require.NoError(t, err)

	cfg.Profile = &config.Profile{
		Fabric:     "ethernet",
		Deployment: "sriov",
		Multirail:  true,
		SpectrumX: &config.ProfileSpectrumX{
			Enable:         true,
			SPCXVersion:    "RA2.1",
			MultiplaneMode: multiplaneMode,
			NumberOfPlanes: numberOfPlanes,
		},
	}

	plugin := &NetworkOperatorPlugin{}
	rendered, err := plugin.GenerateProfileDeploymentFiles(loadSpectrumXRA21Profile(t), cfg)
	require.NoError(t, err)
	return rendered
}

func TestSpectrumXRA21Grouping_HwplbProducesFullSriovChain(t *testing.T) {
	// On 26.1 the per-rail wiring spans 4 CRDs instead of the v1alpha2
	// SpectrumXRailPoolConfig: a SriovNetworkNodePolicy + OVSNetwork + CIDRPool
	// + glue v1alpha1 SpectrumXRailPoolConfig per rail. The cluster-scoped
	// SriovNetworkPoolConfig must render exactly once.
	rendered := renderSpectrumXRA21(t, "mixed-same-type.yaml", "hwplb", 2)

	require.Equal(t, []string{"40-sriovnetworkpoolconfig-gpu-model-y.yaml"},
		fileNamesMatching(rendered, "40-sriovnetworkpoolconfig"),
		"SriovNetworkPoolConfig is per merged group so its nodeSelector lines up with the SriovNetworkNodePolicies in 50-")

	require.Equal(t, []string{"50-sriovnetworknodepolicy-gpu-model-y.yaml"},
		fileNamesMatching(rendered, "50-sriovnetworknodepolicy"),
		"SriovNetworkNodePolicy is per-merged-group")
	require.Equal(t, []string{"55-ovsnetwork-gpu-model-y.yaml"},
		fileNamesMatching(rendered, "55-ovsnetwork"))
	require.Equal(t, []string{"80-spectrumxrailpoolconfig-gpu-model-y.yaml"},
		fileNamesMatching(rendered, "80-spectrumxrailpoolconfig"))

	nodePolicy := rendered["50-sriovnetworknodepolicy-gpu-model-y.yaml"]
	require.NotEmpty(t, nodePolicy)
	// HWPLB grouping is "all" with devlinkParams.esw_multiport set.
	require.Contains(t, nodePolicy, "groupingPolicy: all",
		"hwplb must use groupingPolicy=all")
	require.Contains(t, nodePolicy, "esw_multiport",
		"hwplb must enable esw_multiport via devlinkParams")
	require.NotContains(t, nodePolicy, "groupingPolicy: perPF",
		"hwplb must not use groupingPolicy=perPF (that's swplb)")

	// OVSNetwork must include both rdma and rail meta-plugins per the design.
	ovs := rendered["55-ovsnetwork-gpu-model-y.yaml"]
	require.Contains(t, ovs, `"type": "rdma"`)
	require.Contains(t, ovs, `"type": "rail"`)

	// SpectrumXRailPoolConfig is the v1alpha1 glue resource referencing the
	// SriovNetworkNodePolicy by k8s name and the CIDRPool by name.
	glue := rendered["80-spectrumxrailpoolconfig-gpu-model-y.yaml"]
	require.Contains(t, glue, "apiVersion: spectrumx.nvidia.com/v1alpha1")
	require.Contains(t, glue, "sriovNetworkNodePolicyRef: rail-0-gpu-model-y")
	require.Contains(t, glue, "cidrPoolRef: rail-0")
}

func TestSpectrumXRA21Grouping_SwplbExplodesPerPlane(t *testing.T) {
	rendered := renderSpectrumXRA21(t, "mixed-same-type.yaml", "swplb", 2)

	nodePolicy := rendered["50-sriovnetworknodepolicy-gpu-model-y.yaml"]
	require.NotEmpty(t, nodePolicy)
	// SWPLB uses perPF and does NOT add devlinkParams.
	require.Contains(t, nodePolicy, "groupingPolicy: perPF",
		"swplb must use groupingPolicy=perPF")
	require.NotContains(t, nodePolicy, "esw_multiport",
		"swplb must not set devlinkParams.esw_multiport")

	// Each rail explodes into per-plane policies. mixed-same-type.yaml has 8
	// east-west PFs each on its own rail (rails 0..7), so the merged group
	// has 8 rails × 2 planes = 16 SriovNetworkNodePolicy entries. The
	// firmware splits each NIC's single master PF into 2 logical planes;
	// railPciAddresses lists the master only.
	require.Equal(t, 16, strings.Count(nodePolicy, "kind: SriovNetworkNodePolicy"),
		"swplb expected 16 SriovNetworkNodePolicy entries (8 rails × 2 planes)")

	// SpectrumXRailPoolConfig glue must reference plane-suffixed names.
	glue := rendered["80-spectrumxrailpoolconfig-gpu-model-y.yaml"]
	require.Contains(t, glue, "name: rail-0-plane-0-gpu-model-y")
	require.Contains(t, glue, "sriovNetworkNodePolicyRef: rail-0-plane-0-gpu-model-y")
	require.Contains(t, glue, "cidrPoolRef: rail-0-plane-0")
}

func TestSpectrumXGrouping_PerGroupNameTemplatesHaveDistinctPciLists(t *testing.T) {
	// mixed-same-type has three machines with distinct east-west PCI layouts.
	// Each per-group NicInterfaceNameTemplate must carry that machine's own PCI
	// list and must not leak another machine's PCIs.
	rendered := renderSpectrumX(t, "mixed-same-type.yaml", "none", 1)

	g0 := rendered["25-nicinterfacenametemplate-group-0.yaml"]
	g1 := rendered["25-nicinterfacenametemplate-group-1.yaml"]
	g2 := rendered["25-nicinterfacenametemplate-group-2.yaml"]
	require.NotEmpty(t, g0, "group-0 nic-rename manifest expected")
	require.NotEmpty(t, g1, "group-1 nic-rename manifest expected")
	require.NotEmpty(t, g2, "group-2 nic-rename manifest expected")

	require.Contains(t, g0, `"0000:19:00.0"`, "group-0 must list its own PCIs")
	require.Contains(t, g1, `"0000:1a:00.0"`, "group-1 must list its own PCIs")
	require.Contains(t, g2, `"0000:09:00.0"`, "group-2 must list its own PCIs")

	require.NotContains(t, g0, `"0000:1a:00.0"`, "group-0 must not leak group-1 PCIs")
	require.NotContains(t, g1, `"0000:09:00.0"`, "group-1 must not leak group-2 PCIs")
	require.NotContains(t, g2, `"0000:19:00.0"`, "group-2 must not leak group-0 PCIs")
}
