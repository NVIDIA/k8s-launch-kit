// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

func ra24ConfigAndProfile(t *testing.T, mode string, planes int) (*config.LaunchKitConfig, *profiles.Profile) {
	t.Helper()
	return dospcxConfigAndProfile(t, "RA2.4", mode, planes)
}

func dospcxConfigAndProfile(t *testing.T, ra, mode string, planes int) (*config.LaunchKitConfig, *profiles.Profile) {
	t.Helper()
	cfg, err := config.LoadFullConfig(filepath.Join("testdata", "grouping", "mixed-same-type.yaml"), ctrllog.Log)
	require.NoError(t, err)
	cfg.NetworkOperator.SelectedRelease = "26.10"
	cfg.NetworkOperator.Namespace = "operator-override"
	cfg.Profile = &config.Profile{Fabric: "ethernet", Deployment: "sriov", Multirail: true, SpectrumX: &config.ProfileSpectrumX{Enable: true, SPCXVersion: ra, MultiplaneMode: mode, NumberOfPlanes: planes, TopologyType: config.SpectrumXTopology2Tier}}
	raw, err := os.ReadFile(filepath.Join("..", "config", "testdata", "dospcx-configmap.yaml"))
	require.NoError(t, err)
	cfg.Profile.SpectrumX.Profile = string(raw)
	require.NoError(t, config.NormalizeSpectrumXProfileConfig(cfg.Profile.SpectrumX))
	for i := range cfg.ClusterConfig {
		cfg.ClusterConfig[i].GPUType = "NVIDIA-GB300"
	}
	cfg.Profile.SpectrumX.TopologyFile = writeSpectrumXTopology(t, cfg, planes)
	raw, err = os.ReadFile(filepath.Join("..", "..", "profiles", "spectrum-x-"+strings.ToLower(ra), "profile.yaml"))
	require.NoError(t, err)
	p := &profiles.Profile{}
	require.NoError(t, yaml.Unmarshal(raw, p))
	p.UpdateManifestsPaths(filepath.Join("..", "..", "profiles", "spectrum-x-"+strings.ToLower(ra)))
	return cfg, p
}

func TestDospcxProfileBundleModes(t *testing.T) {
	for _, ra := range []string{"RA2.4", "RA3.1"} {
		t.Run(ra, func(t *testing.T) { testDospcxProfileBundleModes(t, ra) })
	}
}

func testDospcxProfileBundleModes(t *testing.T, ra string) {
	for _, tc := range []struct {
		mode   string
		planes int
	}{{"none", 1}, {"swplb", 2}, {"hwplb", 4}} {
		t.Run(tc.mode, func(t *testing.T) {
			cfg, p := dospcxConfigAndProfile(t, ra, tc.mode, tc.planes)
			cfg.SpectrumX.OVSConfig = map[string]string{"max-idle": "0", "custom": "false", "empty": "", "escaped": "quotes\" and \\ slashes"}
			if tc.mode == "hwplb" {
				for i := range cfg.ClusterConfig {
					cfg.ClusterConfig[i].SpectrumX = &config.SpectrumXGroupConfig{SwPlaneByRail: map[int]int{0: 1}}
				}
			}
			rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(p, cfg)
			require.NoError(t, err)
			files := []bundle.File{}
			cmCount, nctCount, poolCount := 0, 0, 0
			for name, content := range rendered {
				files = append(files, bundle.File{Name: name, Content: content})
				if strings.Contains(content, "kind: ConfigMap") {
					cmCount++
					require.Contains(t, content, "namespace: operator-override")
					require.Contains(t, content, "binaryData:")
					require.Contains(t, content, "dospcx-data-commit: test-commit")
				}
				if strings.Contains(content, "kind: NicConfigurationTemplate") {
					nctCount++
					require.Contains(t, content, `platformType: "gb300"`)
					require.Contains(t, content, fmt.Sprintf("version: %q", ra))
				}
				if strings.Contains(content, "kind: SpectrumXRailPoolConfig") {
					poolCount++
					var obj struct {
						Spec struct {
							OVS   map[string]string `yaml:"ovsConfig"`
							Rails []struct {
								SwPlane  *int `yaml:"swPlane"`
								Selector struct {
									PFNames []string `yaml:"pfNames"`
								} `yaml:"nicSelector"`
							} `yaml:"railTopology"`
						} `yaml:"spec"`
					}
					require.NoError(t, yaml.Unmarshal([]byte(content), &obj))
					require.Equal(t, cfg.SpectrumX.OVSConfig, obj.Spec.OVS)
					require.NotEmpty(t, obj.Spec.Rails)
					if tc.mode == "hwplb" {
						require.NotNil(t, obj.Spec.Rails[0].SwPlane)
						require.Equal(t, 1, *obj.Spec.Rails[0].SwPlane)
						require.Len(t, obj.Spec.Rails[0].Selector.PFNames, tc.planes)
					} else {
						require.Nil(t, obj.Spec.Rails[0].SwPlane)
					}
				}
			}
			require.Equal(t, 1, cmCount)
			require.Equal(t, len(cfg.ClusterConfig), nctCount)
			require.Equal(t, 1, poolCount)
			_, err = bundle.FromFiles(files)
			require.NoError(t, err)
			if tc.mode == "hwplb" {
				require.Empty(t, cfg.ClusterConfig[0].SpectrumX.PlatformType)
			} else {
				require.Nil(t, cfg.ClusterConfig[0].SpectrumX)
			}
		})
	}
}

func TestRA24PlaneMergeAndSelectedPlatformValidation(t *testing.T) {
	cfg, p := ra24ConfigAndProfile(t, "hwplb", 4)
	for i := range cfg.ClusterConfig {
		cfg.ClusterConfig[i].SpectrumX = &config.SpectrumXGroupConfig{SwPlaneByRail: map[int]int{0: 1}}
	}
	cfg.ClusterConfig[1].SpectrumX.SwPlaneByRail[0] = 2
	_, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(p, cfg)
	require.ErrorContains(t, err, "cannot merge")
	cfg.ClusterConfig[1].GPUType = "unmapped-product"
	_, err = (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(p, cfg)
	require.ErrorContains(t, err, "unresolved doSPCX platform")
	rendered, err := (&NetworkOperatorPlugin{Groups: []string{cfg.ClusterConfig[0].Identifier}}).GenerateProfileDeploymentFiles(p, cfg)
	require.NoError(t, err)
	require.NotEmpty(t, rendered)
}

func TestRA24PlaneMergeTreatsOmittedAndExplicitZeroEqually(t *testing.T) {
	for _, explicitFirst := range []bool{false, true} {
		name := "omitted representative"
		if explicitFirst {
			name = "explicit zero representative"
		}
		t.Run(name, func(t *testing.T) {
			cfg, p := ra24ConfigAndProfile(t, "hwplb", 4)
			index := 1
			if explicitFirst {
				index = 0
			}
			cfg.ClusterConfig[index].SpectrumX = &config.SpectrumXGroupConfig{SwPlaneByRail: map[int]int{0: 0}}
			rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(p, cfg)
			require.NoError(t, err)
			poolCount := 0
			for _, content := range rendered {
				if !strings.Contains(content, "kind: SpectrumXRailPoolConfig") {
					continue
				}
				poolCount++
				var pool struct {
					Spec struct {
						Rails []struct {
							SwPlane *int `yaml:"swPlane"`
						} `yaml:"railTopology"`
					} `yaml:"spec"`
				}
				require.NoError(t, yaml.Unmarshal([]byte(content), &pool))
				require.NotEmpty(t, pool.Spec.Rails)
				for _, rail := range pool.Spec.Rails {
					require.NotNil(t, rail.SwPlane)
					require.Zero(t, *rail.SwPlane)
				}
			}
			require.Equal(t, 1, poolCount, "compatible groups must retain merged rendering")
		})
	}
}

func TestRA24ReportsUnknownPlatformsWithoutRailMetadata(t *testing.T) {
	cfg, profile := ra24ConfigAndProfile(t, "hwplb", 4)
	for i := range cfg.ClusterConfig {
		group := &cfg.ClusterConfig[i]
		group.GPUType = fmt.Sprintf("unknown-product-%d", i)
		for j := range group.PFs {
			group.PFs[j].Rail = nil
		}
	}
	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
	require.ErrorContains(t, err, "unresolved doSPCX platform")
	for _, group := range cfg.ClusterConfig {
		require.ErrorContains(t, err, group.Identifier)
		require.ErrorContains(t, err, group.GPUType)
	}
	require.Nil(t, rendered)
}

func TestRA24RejectsDifferentRailLayoutsBeforeRendering(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pairedRails bool
		change      func(*config.ClusterConfig)
	}{
		{
			name: "same PF count but two versus eight rails",
			change: func(group *config.ClusterConfig) {
				for i := range group.PFs {
					if group.PFs[i].Traffic == "east-west" {
						group.PFs[i].Rail = intPtr(*group.PFs[i].Rail / 4)
					}
				}
			},
		},
		{
			name:        "same PF and rail counts but different master NIC counts",
			pairedRails: true,
			change: func(group *config.ClusterConfig) {
				// Two PFs of the same NIC count as one master, unlike
				// two independent NICs with one function each.
				for i := range group.PFs {
					if group.PFs[i].Traffic == "east-west" {
						index := *group.PFs[i].Rail
						group.PFs[i].Rail = intPtr(index / 2)
						group.PFs[i].PciAddress = fmt.Sprintf("0001:%02x:00.%d", index/2, index%2)
					}
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, profile := ra24ConfigAndProfile(t, "hwplb", 4)
			if tc.pairedRails {
				for g := 1; g < len(cfg.ClusterConfig); g++ {
					for i := range cfg.ClusterConfig[g].PFs {
						pf := &cfg.ClusterConfig[g].PFs[i]
						if pf.Traffic == "east-west" {
							pf.Rail = intPtr(*pf.Rail / 2)
						}
					}
				}
			}
			tc.change(&cfg.ClusterConfig[0])
			// Supply topology covering all eight rails for every worker;
			// this must fail layout validation, not rely on missing links.
			rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
			require.ErrorContains(t, err, "east-west rail layouts differ")
			require.ErrorContains(t, err, "group-0")
			require.ErrorContains(t, err, "group-1")
			require.Nil(t, rendered)
		})
	}
}

func TestRA24RequiresRailMetadataWithoutPlaneOverrides(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*config.ClusterConfig)
		want   string
	}{
		{"all missing", func(group *config.ClusterConfig) {
			for i := range group.PFs {
				group.PFs[i].Rail = nil
			}
		}, "complete east-west rail metadata"},
		{"partially missing", func(group *config.ClusterConfig) { group.PFs[0].Rail = nil }, "complete east-west rail metadata"},
		{"sparse", func(group *config.ClusterConfig) { group.PFs[0].Rail = intPtr(8) }, "dense zero-based rail IDs"},
		{"negative", func(group *config.ClusterConfig) { group.PFs[0].Rail = intPtr(-1) }, "dense zero-based rail IDs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, profile := ra24ConfigAndProfile(t, "hwplb", 4)
			tc.change(&cfg.ClusterConfig[0])
			require.Nil(t, cfg.ClusterConfig[0].SpectrumX)
			rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
			require.ErrorContains(t, err, tc.want)
			require.Nil(t, rendered)
			// Invalid excluded groups do not constrain a valid selection.
			rendered, err = (&NetworkOperatorPlugin{Groups: []string{cfg.ClusterConfig[1].Identifier}}).GenerateProfileDeploymentFiles(profile, cfg)
			require.NoError(t, err)
			require.NotEmpty(t, rendered)
		})
	}
}

func TestRA24PhysicalNICFunctionsMustShareOneRail(t *testing.T) {
	for _, sameRail := range []bool{false, true} {
		name := "different rails rejected"
		if sameRail {
			name = "same rail accepted"
		}
		t.Run(name, func(t *testing.T) {
			cfg, profile := ra24ConfigAndProfile(t, "hwplb", 4)
			cfg.ClusterConfig = cfg.ClusterConfig[:1]
			group := &cfg.ClusterConfig[0]
			first := group.PFs[0]
			first.PciAddress = "0000:08:00.0"
			first.Rail = intPtr(0)
			second := first
			second.PciAddress = "0000:08:00.1"
			if !sameRail {
				second.Rail = intPtr(1)
			}
			group.PFs = []config.PFConfig{first, second}
			cfg.Profile.SpectrumX.TopologyFile = writeSpectrumXTopology(t, cfg, 4)
			rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
			if !sameRail {
				require.ErrorContains(t, err, "assigns physical NIC")
				require.ErrorContains(t, err, "multiple rails (0 and 1)")
				require.Nil(t, rendered)
				return
			}
			require.NoError(t, err)
			foundNaming := false
			for _, content := range rendered {
				if strings.Contains(content, "kind: NicInterfaceNameTemplate") {
					foundNaming = true
					var naming struct {
						Spec struct {
							PFsPerNIC int        `yaml:"pfsPerNic"`
							Rails     [][]string `yaml:"railPciAddresses"`
						} `yaml:"spec"`
					}
					require.NoError(t, yaml.Unmarshal([]byte(content), &naming))
					require.Equal(t, 4, naming.Spec.PFsPerNIC)
					require.Equal(t, [][]string{{"0000:08:00.0"}}, naming.Spec.Rails)
				}
			}
			require.True(t, foundNaming)
		})
	}
}

func TestRA24RejectsRTXThinkSystemPresetWithNICSpanningRails(t *testing.T) {
	cfg, profile := ra24ConfigAndProfile(t, "none", 1)
	cfg.ClusterConfig = cfg.ClusterConfig[:1]
	raw, err := os.ReadFile(filepath.Join("..", "presets", "data", "ThinkSystem-SR650-V4-RTX-PRO-6000", "topology.yaml"))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(raw, &cfg.ClusterConfig[0]))
	cfg.Profile.SpectrumX.TopologyFile = writeSpectrumXTopology(t, cfg, 1)
	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
	require.ErrorContains(t, err, "assigns physical NIC")
	require.ErrorContains(t, err, "0000:46:00")
	require.ErrorContains(t, err, "multiple rails (0 and 1)")
	require.Nil(t, rendered)
}

func TestRA24RejectsUnrepresentableNICNamingLayouts(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mode          string
		planes        int
		mastersByRail []int
		want          string
	}{
		{"nonuniform", "hwplb", 4, []int{2, 1}, "requires uniform master NIC counts"},
		{"three NICs with four planes", "hwplb", 4, []int{3, 3}, "positive multiple"},
		{"more NICs than planes", "swplb", 2, []int{4, 4}, "positive multiple"},
		{"multiple NICs in single-plane mode", "none", 1, []int{2, 2}, "positive multiple"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, profile := ra24ConfigAndProfile(t, tc.mode, tc.planes)
			group := &cfg.ClusterConfig[0]
			pfTemplate := group.PFs[0]
			group.PFs = nil
			index := 0
			for rail, count := range tc.mastersByRail {
				for range count {
					pf := pfTemplate
					pf.Rail = intPtr(rail)
					pf.PciAddress = fmt.Sprintf("0000:%02x:00.0", index)
					group.PFs = append(group.PFs, pf)
					index++
				}
			}
			// A single selected source still needs a representable NIT:
			// pfsPerNic is one scalar shared by all of its rails.
			rendered, err := (&NetworkOperatorPlugin{Groups: []string{group.Identifier}}).GenerateProfileDeploymentFiles(profile, cfg)
			require.ErrorContains(t, err, tc.want)
			require.Nil(t, rendered)
		})
	}
}

func TestRA24MergesEquivalentRailLayoutsWithDifferentPCIAddresses(t *testing.T) {
	for _, mastersPerRail := range []int{2, 4} {
		t.Run(fmt.Sprintf("%d NICs per rail", mastersPerRail), func(t *testing.T) {
			cfg, profile := ra24ConfigAndProfile(t, "hwplb", 4)
			for groupIndex := range cfg.ClusterConfig {
				group := &cfg.ClusterConfig[groupIndex]
				for i := range group.PFs {
					pf := &group.PFs[i]
					if pf.Traffic != "east-west" {
						continue
					}
					pf.PciAddress = fmt.Sprintf("%04x:%02x:00.0", groupIndex, *pf.Rail)
					pf.Rail = intPtr(*pf.Rail / mastersPerRail)
				}
			}
			cfg.Profile.SpectrumX.TopologyFile = writeSpectrumXTopology(t, cfg, 4)
			rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
			require.NoError(t, err)
			poolCount, namingCount := 0, 0
			var files []bundle.File
			for name, content := range rendered {
				files = append(files, bundle.File{Name: name, Content: content})
				if strings.Contains(content, "kind: SpectrumXRailPoolConfig") {
					poolCount++
					var pool struct {
						Spec struct {
							Rails []struct {
								Name string `yaml:"name"`
							} `yaml:"railTopology"`
						} `yaml:"spec"`
					}
					require.NoError(t, yaml.Unmarshal([]byte(content), &pool))
					require.Len(t, pool.Spec.Rails, 8/mastersPerRail)
					for i, rail := range pool.Spec.Rails {
						require.Equal(t, fmt.Sprintf("rail%d", i), rail.Name)
					}
				}
				if strings.Contains(content, "kind: NicInterfaceNameTemplate") {
					namingCount++
					var naming struct {
						Spec struct {
							PFsPerNIC int        `yaml:"pfsPerNic"`
							Rails     [][]string `yaml:"railPciAddresses"`
						} `yaml:"spec"`
					}
					require.NoError(t, yaml.Unmarshal([]byte(content), &naming))
					require.Equal(t, 4/mastersPerRail, naming.Spec.PFsPerNIC)
					require.Len(t, naming.Spec.Rails, 8/mastersPerRail)
					for _, rail := range naming.Spec.Rails {
						require.Len(t, rail, mastersPerRail)
					}
				}
			}
			require.Equal(t, 1, poolCount)
			require.Equal(t, len(cfg.ClusterConfig), namingCount)
			_, err = bundle.FromFiles(files)
			require.NoError(t, err)
		})
	}
}

func TestRA24MergedGPUSelectorCannotBroadenSelectedBucket(t *testing.T) {
	for _, excludedGPU := range []string{"NVIDIA-GB300", "NVIDIA-B300"} {
		t.Run(excludedGPU, func(t *testing.T) {
			cfg, profile := ra24ConfigAndProfile(t, "hwplb", 4)
			// The third source shares the GPU but has only four rails,
			// so selecting the first two sources completes their bucket.
			excluded := &cfg.ClusterConfig[2]
			excluded.GPUType = excludedGPU
			var fourRails []config.PFConfig
			for _, pf := range excluded.PFs {
				if pf.Traffic == "east-west" && *pf.Rail < 4 {
					fourRails = append(fourRails, pf)
				}
			}
			excluded.PFs = fourRails
			plugin := &NetworkOperatorPlugin{Groups: []string{cfg.ClusterConfig[0].Identifier, cfg.ClusterConfig[1].Identifier}}
			rendered, err := plugin.GenerateProfileDeploymentFiles(profile, cfg)
			if excludedGPU == "NVIDIA-GB300" {
				require.ErrorContains(t, err, "merged GPU selector")
				require.ErrorContains(t, err, "would include excluded group")
				require.ErrorContains(t, err, excluded.Identifier)
				require.Nil(t, rendered)
			} else {
				require.NoError(t, err)
				require.NotEmpty(t, rendered)
			}
			// One selected source renders its original source selector,
			// so a same-GPU source in another bucket remains excluded.
			plugin.Groups = plugin.Groups[:1]
			rendered, err = plugin.GenerateProfileDeploymentFiles(profile, cfg)
			require.NoError(t, err)
			poolCount := 0
			for _, content := range rendered {
				if !strings.Contains(content, "kind: SpectrumXRailPoolConfig") {
					continue
				}
				poolCount++
				var pool struct {
					Spec struct {
						NodeSelector map[string]string `yaml:"nodeSelector"`
					} `yaml:"spec"`
				}
				require.NoError(t, yaml.Unmarshal([]byte(content), &pool))
				require.Equal(t, cfg.ClusterConfig[0].NodeSelector, pool.Spec.NodeSelector)
			}
			require.Equal(t, 1, poolCount)
		})
	}
}

func TestRA24RejectsMultipleRailPoolParents(t *testing.T) {
	for _, mode := range []struct {
		name   string
		planes int
	}{{"none", 1}, {"swplb", 2}, {"hwplb", 4}} {
		t.Run(mode.name, func(t *testing.T) {
			t.Run("strict subset with two sources", func(t *testing.T) {
				cfg, p := ra24ConfigAndProfile(t, mode.name, mode.planes)
				plugin := &NetworkOperatorPlugin{Groups: []string{cfg.ClusterConfig[0].Identifier, cfg.ClusterConfig[1].Identifier}}
				rendered, err := plugin.GenerateProfileDeploymentFiles(p, cfg)
				require.ErrorContains(t, err, "2 SpectrumXRailPoolConfig parents")
				require.ErrorContains(t, err, "child-name collisions")
				require.ErrorContains(t, err, "full source cohort")
				require.ErrorContains(t, err, "single source with --groups")
				require.Nil(t, rendered)

				// Both alternatives in the diagnostic are safe to render.
				plugin.Groups = nil
				rendered, err = plugin.GenerateProfileDeploymentFiles(p, cfg)
				require.NoError(t, err)
				require.NotEmpty(t, rendered)
				plugin.Groups = []string{cfg.ClusterConfig[0].Identifier}
				rendered, err = plugin.GenerateProfileDeploymentFiles(p, cfg)
				require.NoError(t, err)
				require.NotEmpty(t, rendered)
			})
			t.Run("heterogeneous GPU buckets", func(t *testing.T) {
				cfg, p := ra24ConfigAndProfile(t, mode.name, mode.planes)
				cfg.ClusterConfig[1].GPUType = "NVIDIA-B300"
				rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(p, cfg)
				require.ErrorContains(t, err, "2 SpectrumXRailPoolConfig parents")
				require.ErrorContains(t, err, "child-name collisions")
				require.Nil(t, rendered)
			})
		})
	}
}

func TestMultipleRailPoolParentGuardDoesNotChangeOlderRAs(t *testing.T) {
	for _, version := range []string{"RA2.1", "RA2.2", "RA2.3"} {
		t.Run(version, func(t *testing.T) {
			cfg := &config.LaunchKitConfig{Profile: &config.Profile{SpectrumX: &config.ProfileSpectrumX{Enable: true, SPCXVersion: version}}}
			plans := []RenderBucket{{ModeB: true, Sources: []config.ClusterConfig{{Identifier: "source-a"}, {Identifier: "source-b"}}}}
			require.NoError(t, validateSpectrumXPlaneBuckets(cfg, plans))
		})
	}
}

func TestRA24SwPlaneValidation(t *testing.T) {
	for _, tc := range []struct {
		name, mode  string
		rail, value int
		metadata    *int
		want        string
	}{
		{"negative", "hwplb", 0, -1, intPtr(0), "nonnegative"},
		{"unknown rail", "hwplb", 1, 1, intPtr(0), "unknown east-west rail"},
		{"missing metadata", "hwplb", 0, 0, nil, "complete"},
		{"sparse rails", "hwplb", 1, 1, intPtr(1), "dense"},
		{"single PF mode", "swplb", 0, 1, intPtr(0), "requires hwplb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := config.ClusterConfig{Identifier: "machine-a", SpectrumX: &config.SpectrumXGroupConfig{SwPlaneByRail: map[int]int{tc.rail: tc.value}}, PFs: []config.PFConfig{{Traffic: "east-west", Rail: tc.metadata}}}
			require.ErrorContains(t, validateSpectrumXSwPlanes(group, tc.mode), tc.want)
		})
	}
}

func TestRA24OVSDefaultAndLegacySettingsRejected(t *testing.T) {
	cfg, p := ra24ConfigAndProfile(t, "hwplb", 4)
	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(p, cfg)
	require.NoError(t, err)
	for _, content := range rendered {
		require.NotContains(t, content, "ovsConfig:")
	}
	cfg.Profile.SpectrumX.SPCXVersion = "RA2.3"
	cfg.SpectrumX.OVSConfig = map[string]string{"custom": "value"}
	require.ErrorContains(t, validateSpectrumXGeneration(cfg, cfg.ClusterConfig), "ovsConfig requires RA2.4")
	cfg.SpectrumX.OVSConfig = nil
	cfg.ClusterConfig[0].SpectrumX = &config.SpectrumXGroupConfig{SwPlaneByRail: map[int]int{0: 0}}
	require.ErrorContains(t, validateSpectrumXGeneration(cfg, cfg.ClusterConfig), "swPlaneByRail requires RA2.4")
}

func vrConfigAndProfile(t *testing.T, ra string, split bool) (*config.LaunchKitConfig, *profiles.Profile) {
	t.Helper()
	cfg, profile := dospcxConfigAndProfile(t, ra, "hwplb", 8)
	cfg.ClusterConfig = cfg.ClusterConfig[:1]
	group := &cfg.ClusterConfig[0]
	group.MachineType, group.GPUType = "machine-VR", ""
	group.PFs = nil
	// Discovery preserves PCI order; rail membership crosses adjacent NetworkBays.
	for _, nic := range []struct {
		pci  string
		rail int
	}{
		{"0001:03:00.0", 0}, {"0003:01:00.0", 1},
		{"0004:03:00.0", 1}, {"0006:01:00.0", 0},
		{"0009:03:00.0", 2}, {"000b:01:00.0", 3},
		{"000c:03:00.0", 3}, {"000e:01:00.0", 2},
	} {
		rail := nic.rail
		functions := 1
		if split {
			functions = 4
		}
		for function := 0; function < functions; function++ {
			pci := strings.TrimSuffix(nic.pci, "0") + fmt.Sprint(function)
			group.PFs = append(group.PFs, config.PFConfig{PciAddress: pci, DeviceID: "1025", Traffic: "east-west", Rail: &rail})
		}
	}
	cfg.Profile.SpectrumX.TopologyFile = writeSpectrumXTopology(t, cfg, 8)
	return cfg, profile
}

func TestVRNetworkBayRailsRenderEightPlanes(t *testing.T) {
	for _, ra := range []string{"RA2.4", "RA3.1"} {
		for _, split := range []bool{false, true} {
			for _, dra := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/split=%v/dra=%v", ra, split, dra), func(t *testing.T) {
					testVRNetworkBayManifestRails(t, ra, split, dra)
				})
			}
		}
	}
}

func testVRNetworkBayManifestRails(t *testing.T, ra string, split, dra bool) {
	t.Helper()
	cfg, profile := vrConfigAndProfile(t, ra, split)
	cfg.Profile.SpectrumX.UseDRA = dra
	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
	require.NoError(t, err)
	files := []bundle.File{}
	for name, content := range rendered {
		files = append(files, bundle.File{Name: name, Content: content})
	}
	b, err := bundle.FromFiles(files)
	require.NoError(t, err)
	kinds := map[string]int{}
	poolNames, poolRefs := []string{}, []string{}
	claimNames, claimRefs := []string{}, []string{}
	for _, document := range b.Documents() {
		obj := document.ObjectCopy()
		kinds[obj.GetKind()]++
		switch obj.GetKind() {
		case "NicConfigurationTemplate":
			require.Equal(t, "vr", vrManifestField(t, obj.Object, "spec", "template", "spectrumXOptimized", "platformType"))
			require.Equal(t, ra, vrManifestField(t, obj.Object, "spec", "template", "spectrumXOptimized", "version"))
			require.EqualValues(t, 8, vrManifestField(t, obj.Object, "spec", "template", "spectrumXOptimized", "numberOfPlanes"))
		case "NicInterfaceNameTemplate":
			require.EqualValues(t, 4, vrManifestField(t, obj.Object, "spec", "pfsPerNic"))
			rail := kinds["NicInterfaceNameTemplate"] - 1
			expected := [][]any{
				{"0001:03:00.0", "0006:01:00.0"},
				{"0003:01:00.0", "0004:03:00.0"},
				{"0009:03:00.0", "000e:01:00.0"},
				{"000b:01:00.0", "000c:03:00.0"},
			}
			require.Equal(t, []any{expected[rail]}, vrManifestField(t, obj.Object, "spec", "railPciAddresses"))
			require.Equal(t, fmt.Sprintf("roce_r%d_n%%nic_id%%", rail), vrManifestField(t, obj.Object, "spec", "rdmaDevicePrefix"))
			require.Equal(t, fmt.Sprintf("eth_r%d_p%%plane_id%%", rail), vrManifestField(t, obj.Object, "spec", "netDevicePrefix"))
		case "SpectrumXRailPoolConfig":
			rails := vrManifestField(t, obj.Object, "spec", "railTopology").([]any)
			require.Len(t, rails, 8)
			for index, raw := range rails {
				rail, nic := index/2, index%2
				entry := raw.(map[string]any)
				require.Equal(t, fmt.Sprintf("rail%dnic%d", rail, nic), entry["name"])
				poolRefs = append(poolRefs, entry["cidrPoolRef"].(string))
				want := []any{}
				require.EqualValues(t, nic, entry["swPlane"])
				for plane := nic * 4; plane < (nic+1)*4; plane++ {
					want = append(want, fmt.Sprintf("eth_r%d_p%d", rail, plane))
				}
				require.Equal(t, want, vrManifestField(t, entry, "nicSelector", "pfNames"))
			}
		case "CIDRPool":
			poolNames = append(poolNames, obj.GetName())
		case "ResourceClaimTemplate":
			claimNames = append(claimNames, obj.GetName())
			requests := vrManifestField(t, obj.Object, "spec", "spec", "devices", "requests").([]any)
			require.Len(t, requests, 3)
			for i, raw := range requests {
				request := raw.(map[string]any)
				require.EqualValues(t, 1, vrManifestField(t, request, "exactly", "count"))
				if i == 0 {
					require.Equal(t, "gpu", request["name"])
				} else {
					require.Equal(t, fmt.Sprintf("vf%d", i-1), request["name"])
					rail := kinds["ResourceClaimTemplate"] - 1
					roots := [][]string{{"pci0001:00", "pci0006:00"}, {"pci0003:00", "pci0004:00"}, {"pci0009:00", "pci000e:00"}, {"pci000b:00", "pci000c:00"}}
					selectors := vrManifestField(t, request, "exactly", "selectors").([]any)
					require.Len(t, selectors, 2)
					require.Equal(t, fmt.Sprintf(`device.attributes["k8s.cni.cncf.io"].resourceName == "nvidia.com/rail%dnic%d"`, rail, i-1), vrManifestField(t, selectors[0].(map[string]any), "cel", "expression"))
					require.Equal(t, fmt.Sprintf(`device.attributes["resource.kubernetes.io"].pcieRoot == %q`, roots[rail][i-1]), vrManifestField(t, selectors[1].(map[string]any), "cel", "expression"))
				}
			}
		case "DaemonSet":
			require.Equal(t, "rail0nic0,rail0nic1,rail1nic0,rail1nic1,rail2nic0,rail2nic1,rail3nic0,rail3nic1", vrManifestField(t, obj.Object,
				"spec", "template", "metadata", "annotations", "k8s.v1.cni.cncf.io/networks"))
			if dra {
				claims := vrManifestField(t, obj.Object, "spec", "template", "spec", "resourceClaims").([]any)
				require.Len(t, claims, 4)
				for _, claim := range claims {
					claimRefs = append(claimRefs, claim.(map[string]any)["resourceClaimTemplateName"].(string))
				}
			} else {
				containers := vrManifestField(t, obj.Object, "spec", "template", "spec", "containers").([]any)
				limits := vrManifestField(t, containers[0].(map[string]any), "resources", "limits").(map[string]any)
				require.Len(t, limits, 8)
				for rail := 0; rail < 4; rail++ {
					for nic := 0; nic < 2; nic++ {
						require.Equal(t, "1", limits[fmt.Sprintf("nvidia.com/rail%dnic%d", rail, nic)])
					}
				}
			}
		}
	}
	for _, kind := range []string{"NicConfigurationTemplate", "SpectrumXRailPoolConfig", "ConfigMap", "DaemonSet"} {
		require.Equal(t, 1, kinds[kind], kind)
	}
	require.Equal(t, 4, kinds["NicInterfaceNameTemplate"])
	require.Len(t, poolNames, 8)
	require.ElementsMatch(t, poolNames, poolRefs)
	require.ElementsMatch(t, claimNames, claimRefs)
	if dra {
		require.Len(t, claimNames, 4)
	} else {
		require.Empty(t, claimNames)
	}
}

func vrManifestField(t *testing.T, object map[string]any, fields ...string) any {
	t.Helper()
	value, found, err := unstructured.NestedFieldNoCopy(object, fields...)
	require.NoError(t, err)
	require.True(t, found, "missing %v", fields)
	return value
}

func TestVRGenerationRejectsStaleSingleNICRails(t *testing.T) {
	for _, ra := range []string{"RA2.4", "RA3.1"} {
		t.Run(ra, func(t *testing.T) {
			cfg, profile := vrConfigAndProfile(t, ra, false)
			for i := range cfg.ClusterConfig[0].PFs {
				cfg.ClusterConfig[0].PFs[i].Rail = intPtr(i)
			}
			rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
			require.ErrorContains(t, err, "requires four rails with two physical NICs per rail")
			require.Empty(t, rendered)
		})
	}
}

func TestVRGenerationPreservesNICOrderWithinRails(t *testing.T) {
	cfg, profile := vrConfigAndProfile(t, "RA3.1", false)
	pfs := cfg.ClusterConfig[0].PFs
	pfs[0], pfs[3] = pfs[3], pfs[0]
	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
	require.NoError(t, err)
	for name, content := range rendered {
		if strings.Contains(name, "nicinterfacenametemplate") {
			require.Contains(t, content, `["0006:01:00.0", "0001:03:00.0"]`)
			return
		}
	}
	t.Fatal("missing NicInterfaceNameTemplate")
}

func TestVRCustomInterfacePrefixes(t *testing.T) {
	cfg, profile := vrConfigAndProfile(t, "RA3.1", false)
	cfg.SpectrumX.HWPLB = &config.SpectrumXInterfaceNamePrefixConfig{NetdevPrefix: "net_r%rail_id%_n%nic_id%_p%plane_id%", RdmaPrefix: "rdma_r%rail_id%_n%nic_id%"}
	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
	require.NoError(t, err)
	for name, content := range rendered {
		if strings.Contains(name, "railpool") {
			require.Contains(t, content, "net_r0_n1_p4")
		}
	}
	cfg.SpectrumX.HWPLB.RdmaPrefix = "rdma_r%rail_id%_p%plane_id%"
	_, err = (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
	require.ErrorContains(t, err, "requires a %nic_id% placeholder and no plane placeholder")
}
