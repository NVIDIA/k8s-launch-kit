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

func TestVRRA31EightPlaneGeneration(t *testing.T) {
	for _, ra := range []string{"RA2.4", "RA3.1"} {
		t.Run(ra, func(t *testing.T) {
			cfg, p := dospcxConfigAndProfile(t, ra, "hwplb", 8)
			cfg.ClusterConfig = cfg.ClusterConfig[:1]
			group := &cfg.ClusterConfig[0]
			group.MachineType, group.GPUType = "machine-VR", ""
			group.PFs = nil
			// First NIC is ASIC1 (nic_index_in_rail=0), second is ASIC0.
			// Keep this order even though the PCI addresses sort differently.
			for _, slot := range []string{"0000:92:00", "0000:13:00"} {
				for function := 0; function < 4; function++ {
					rail := 0
					group.PFs = append(group.PFs, config.PFConfig{PciAddress: fmt.Sprintf("%s.%d", slot, function),
						DeviceID: "1023", Traffic: "east-west", Rail: &rail})
				}
			}
			cfg.Profile.SpectrumX.SPCXVersion = ra
			cfg.Profile.SpectrumX.TopologyFile = writeSpectrumXTopology(t, cfg, 8)
			valid, reason := p.Validate(cfg.Profile, config.AggregateCapabilities(cfg.ClusterConfig), cfg.NetworkOperator.SelectedRelease)
			require.True(t, valid, reason)
			rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(p, cfg)
			require.NoError(t, err)
			files := []bundle.File{}
			kinds := map[string]int{}
			for name, content := range rendered {
				files = append(files, bundle.File{Name: name, Content: content})
				var obj struct {
					Kind string `yaml:"kind"`
					Spec struct {
						PfsPerNic        int        `yaml:"pfsPerNic"`
						RailPciAddresses [][]string `yaml:"railPciAddresses"`
						Rails            []struct {
							Selector struct {
								PFNames []string `yaml:"pfNames"`
							} `yaml:"nicSelector"`
						} `yaml:"railTopology"`
					} `yaml:"spec"`
				}
				require.NoError(t, yaml.Unmarshal([]byte(content), &obj))
				kinds[obj.Kind]++
				switch obj.Kind {
				case "NicConfigurationTemplate":
					require.Contains(t, content, `platformType: "vr"`)
					require.Contains(t, content, fmt.Sprintf("version: %q", ra))
					require.Contains(t, content, "numberOfPlanes: 8")
				case "NicInterfaceNameTemplate":
					require.Equal(t, 4, obj.Spec.PfsPerNic)
					require.Equal(t, [][]string{{"0000:92:00.0", "0000:13:00.0"}}, obj.Spec.RailPciAddresses)
				case "SpectrumXRailPoolConfig":
					require.Len(t, obj.Spec.Rails, 1)
					require.Equal(t, []string{"eth_r0_p0", "eth_r0_p1", "eth_r0_p2", "eth_r0_p3", "eth_r0_p4", "eth_r0_p5", "eth_r0_p6", "eth_r0_p7"}, obj.Spec.Rails[0].Selector.PFNames)
				}
			}
			for _, kind := range []string{"NicConfigurationTemplate", "NicInterfaceNameTemplate", "SpectrumXRailPoolConfig", "ConfigMap"} {
				require.Equal(t, 1, kinds[kind], kind)
			}
			_, err = bundle.FromFiles(files)
			require.NoError(t, err)
			// New RA versions must retain strict rail validation.
			group.PFs[0].Rail = nil
			_, err = (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(p, cfg)
			require.ErrorContains(t, err, "requires complete east-west rail metadata")
		})
	}
}

func TestVRNetworkBayRailsRenderEightPlanes(t *testing.T) {
	cfg, profile := dospcxConfigAndProfile(t, "RA3.1", "hwplb", 8)
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
		group.PFs = append(group.PFs, config.PFConfig{PciAddress: nic.pci, DeviceID: "1025", Traffic: "east-west", Rail: &rail})
	}
	cfg.Profile.SpectrumX.TopologyFile = writeSpectrumXTopology(t, cfg, 8)
	rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
	require.NoError(t, err)
	files := []bundle.File{}
	found := false
	for name, content := range rendered {
		files = append(files, bundle.File{Name: name, Content: content})
		var obj struct {
			Kind string `yaml:"kind"`
			Spec struct {
				PFsPerNIC int        `yaml:"pfsPerNic"`
				Rails     [][]string `yaml:"railPciAddresses"`
			} `yaml:"spec"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(content), &obj))
		if obj.Kind == "NicInterfaceNameTemplate" {
			found = true
			require.Equal(t, 4, obj.Spec.PFsPerNIC)
			require.Equal(t, [][]string{
				{"0001:03:00.0", "0006:01:00.0"},
				{"0003:01:00.0", "0004:03:00.0"},
				{"0009:03:00.0", "000e:01:00.0"},
				{"000b:01:00.0", "000c:03:00.0"},
			}, obj.Spec.Rails)
		}
	}
	require.True(t, found)
	_, err = bundle.FromFiles(files)
	require.NoError(t, err)
}
