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
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/yaml"
)

func TestSpectrumXPCIAddresses(t *testing.T) {
	group := &config.ClusterConfig{
		Identifier: "machine-a",
		PFs: []config.PFConfig{
			{PciAddress: " 000A:19:00.0 ", Traffic: "east-west"},
			{PciAddress: "000a:19:00.0", Traffic: "east-west"},
			{PciAddress: "0000:03:00.0", Traffic: "north-south"},
		},
	}

	addresses, err := spectrumXPCIAddresses(group)
	require.NoError(t, err)
	require.Equal(t, []string{"000a:19:00.0"}, addresses,
		"the selector must normalize, deduplicate, and exclude north-south PCI addresses")

	group.PFs[0].PciAddress = ""
	group.PFs = group.PFs[:1]
	_, err = spectrumXPCIAddresses(group)
	require.ErrorContains(t, err, `group "machine-a" has an east-west PF without a pciAddress`)
}

func TestSpectrumXNicConfigurationTemplateExcludesSameTypeNorthSouthDevices(t *testing.T) {
	tests := []struct {
		name        string
		profileDir  string
		spcxVersion string
	}{
		{name: "RA2.1", profileDir: "spectrum-x-ra2.1", spcxVersion: "RA2.1"},
		{name: "RA2.2", profileDir: "spectrum-x-ra2.2", spcxVersion: "RA2.2"},
		{name: "RA2.3", profileDir: "spectrum-x", spcxVersion: "RA2.3"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctrllog.SetLogger(zap.New(zap.UseDevMode(true)))
			cfg, err := config.LoadFullConfig(
				filepath.Join("testdata", "grouping", "same-ew-different-ns.yaml"),
				ctrllog.Log,
			)
			require.NoError(t, err)

			for groupIndex := range cfg.ClusterConfig {
				machineLabel := "machine-a-gpu-model-x"
				if groupIndex == 1 {
					machineLabel = "machine-b-gpu-model-x"
				}
				cfg.ClusterConfig[groupIndex].NodeSelector = map[string]string{
					config.MachineLabelKey: machineLabel,
				}
				for pfIndex := range cfg.ClusterConfig[groupIndex].PFs {
					// Model the reported failure: both the east-west SuperNIC and
					// north-south DPU expose the same BlueField-3 device ID.
					cfg.ClusterConfig[groupIndex].PFs[pfIndex].DeviceID = "a2dc"
				}
			}
			cfg.Profile = &config.Profile{
				Fabric:     "ethernet",
				Deployment: "sriov",
				Multirail:  true,
				SpectrumX: &config.ProfileSpectrumX{
					Enable:         true,
					SPCXVersion:    test.spcxVersion,
					MultiplaneMode: "none",
					NumberOfPlanes: 1,
					ConfigMapName:  "site-ra23-profile",
				},
			}

			profile := loadProfileFromDir(t, test.profileDir)
			var nicConfigurationTemplate string
			for _, templatePath := range profile.Templates {
				if filepath.Base(templatePath) == "30-nicconfigurationtemplate.yaml" {
					nicConfigurationTemplate = templatePath
					break
				}
			}
			require.NotEmpty(t, nicConfigurationTemplate)
			profile.Templates = []string{nicConfigurationTemplate}

			rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
			require.NoError(t, err)
			require.Equal(t, []string{
				"30-nicconfigurationtemplate-group-0.yaml",
				"30-nicconfigurationtemplate-group-1.yaml",
			}, fileNamesMatching(rendered, "30-nicconfigurationtemplate"),
				"a merged hardware bucket must still emit one PCI-scoped template per source group")

			for groupIndex, group := range cfg.ClusterConfig {
				fileName := fmt.Sprintf("30-nicconfigurationtemplate-group-%d.yaml", groupIndex)
				manifest := rendered[fileName]
				require.NotEmpty(t, manifest)

				var object struct {
					Spec struct {
						NodeSelector map[string]string `yaml:"nodeSelector"`
						NicSelector  struct {
							NicType      string   `yaml:"nicType"`
							PCIAddresses []string `yaml:"pciAddresses"`
						} `yaml:"nicSelector"`
					} `yaml:"spec"`
				}
				require.NoError(t, yaml.Unmarshal([]byte(manifest), &object))
				require.Equal(t, "a2dc", object.Spec.NicSelector.NicType)
				require.Equal(t, group.NodeSelector, object.Spec.NodeSelector)

				var eastWestPCIs []string
				var northSouthPCIs []string
				for _, pf := range group.PFs {
					switch pf.Traffic {
					case "east-west":
						eastWestPCIs = append(eastWestPCIs, pf.PciAddress)
					case "north-south":
						northSouthPCIs = append(northSouthPCIs, pf.PciAddress)
					}
				}
				require.Equal(t, eastWestPCIs, object.Spec.NicSelector.PCIAddresses)
				for _, pciAddress := range northSouthPCIs {
					require.NotContains(t, object.Spec.NicSelector.PCIAddresses, pciAddress,
						"north-south PCI address %s must not be selected", pciAddress)
				}
			}
		})
	}
}

func TestStandardNicConfigurationProfiles(t *testing.T) {
	for _, flavor := range []string{config.FlavorK8s, config.FlavorOCP} {
		for _, tc := range []struct{ profile, fabric, deployment string }{
			{"sriov-ethernet-rdma", "ethernet", "sriov"},
			{"sriov-ib-rdma", "infiniband", "sriov"},
			{"macvlan-rdma-shared", "ethernet", "rdma_shared"},
			{"ipoib-rdma-shared", "infiniband", "rdma_shared"},
			{"host-device-rdma", "ethernet", "host_device"},
			{"host-device-rdma", "infiniband", "host_device"},
		} {
			for _, enabled := range []bool{false, true} {
				for _, multirail := range []bool{false, true} {
					name := fmt.Sprintf("%s/%s/%s/enabled=%t/multirail=%t", flavor, tc.profile, tc.fabric, enabled, multirail)
					t.Run(name, func(t *testing.T) {
						cfg := standardNicConfigurationConfig(t, flavor, tc.fabric, tc.deployment, enabled)
						cfg.Profile.Multirail = multirail
						profileName := tc.profile
						if flavor == config.FlavorOCP {
							profileName += "-ocp"
						}
						profile := loadProfileFromDir(t, profileName)
						localTemplate, err := filepath.Abs(filepath.Join("..", "..", "profiles", profileName, "30-nicconfigurationtemplate.yaml"))
						require.NoError(t, err)
						require.Contains(t, profile.Templates, localTemplate, "each profile must own its NIC configuration template")
						rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
						require.NoError(t, err)
						files := make([]bundle.File, 0, len(rendered))
						for name, content := range rendered {
							files = append(files, bundle.File{Name: name, Content: content})
						}
						artifacts, err := bundle.FromFiles(files)
						require.NoError(t, err)
						templates := 0
						for _, doc := range artifacts.Documents() {
							obj := doc.ObjectCopy()
							if obj.GetKind() == "NicClusterPolicy" {
								_, found, err := unstructured.NestedMap(obj.Object, "spec", "nicConfigurationOperator")
								require.NoError(t, err)
								require.Equal(t, enabled, found, "NCO enablement must be independent of naming and firmware flags")
							}
							if obj.GetKind() == "NicConfigurationTemplate" {
								templates++
								selector, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "nodeSelector")
								var group *config.ClusterConfig
								for i := range cfg.ClusterConfig {
									if reflect.DeepEqual(cfg.ClusterConfig[i].NodeSelector, selector) {
										group = &cfg.ClusterConfig[i]
									}
								}
								require.NotNil(t, group, "template must preserve a source selector")
								pcis, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "nicSelector", "pciAddresses")
								expected := []string{}
								for _, pf := range group.PFs {
									if pf.Traffic == "east-west" {
										expected = append(expected, pf.PciAddress)
									}
								}
								require.Equal(t, expected, pcis)
								nicType, _, _ := unstructured.NestedString(obj.Object, "spec", "nicSelector", "nicType")
								require.Equal(t, "a2dc", nicType)
								payload, _, _ := unstructured.NestedMap(obj.Object, "spec", "template")
								linkType := "Ethernet"
								if tc.fabric == "infiniband" {
									linkType = "Infiniband"
								}
								want := map[string]interface{}{
									"numVfs": int64(16), "linkType": linkType,
									"pciPerformanceOptimized": map[string]interface{}{"enabled": true, "maxReadRequest": int64(4096)},
									"gpuDirectOptimized":      map[string]interface{}{"enabled": true, "env": "Baremetal"},
								}
								if tc.fabric == "ethernet" {
									want["roceOptimized"] = map[string]interface{}{"enabled": true, "qos": map[string]interface{}{"trust": "dscp", "pfc": "0,0,0,1,0,0,0,0"}}
								}
								require.Equal(t, want, payload)
							}
						}
						if enabled {
							require.Equal(t, len(cfg.ClusterConfig), templates)
						} else {
							require.Zero(t, templates)
							require.Empty(t, fileNamesMatching(rendered, "30-nicconfigurationtemplate"))
						}
						pluginDisabled := false
						for _, content := range rendered {
							pluginDisabled = pluginDisabled || strings.Contains(content, "disablePlugins:")
						}
						require.Equal(t, enabled && tc.deployment == "sriov", pluginDisabled)
						if flavor == config.FlavorK8s && tc.deployment == "sriov" {
							_, values := fileMatching(t, rendered, "values.yaml")
							var helm map[string]interface{}
							require.NoError(t, yaml.Unmarshal([]byte(values), &helm))
							disabled, found, err := unstructured.NestedStringSlice(helm, "sriov-network-operator", "sriovOperatorConfig", "disablePlugins")
							require.NoError(t, err)
							require.Equal(t, enabled, found)
							if enabled {
								require.Equal(t, []string{"mellanox"}, disabled)
							}
							_, found, err = unstructured.NestedStringSlice(helm, "maintenance-operator-chart", "operatorConfig", "disablePlugins")
							require.NoError(t, err)
							require.False(t, found)
						}
					})
				}
			}
		}
	}
}

func standardNicConfigurationConfig(t *testing.T, flavor, fabric, deployment string, enabled bool) *config.LaunchKitConfig {
	t.Helper()
	cfg, err := config.LoadFullConfig(filepath.Join("testdata", "grouping", "same-ew-different-ns.yaml"), ctrllog.Log)
	require.NoError(t, err)
	cfg.Flavor = flavor
	require.NoError(t, config.ApplyFlavorDefaults(cfg))
	cfg.Profile = &config.Profile{Fabric: fabric, Deployment: deployment, Multirail: true}
	cfg.NetworkOperator.SelectedRelease = "26.7"
	cfg.NicConfigurationOperator = &config.NicConfigurationOperatorConfig{DeployNicConfigurationTemplate: enabled, DeployNicInterfaceNameTemplate: false, UpdateFW: false, NetdevPrefix: "eth_r%rail_id%", RdmaPrefix: "rdma_r%rail_id%"}
	cfg.Sriov.NumVfs = 16
	cfg.NetworkNamespaces = []string{"workload-a", "workload-b"}
	for i := range cfg.ClusterConfig {
		cfg.ClusterConfig[i].NodeSelector = map[string]string{config.MachineLabelKey: fmt.Sprintf("machine-%d", i)}
		cfg.ClusterConfig[i].WorkerNodes = []string{fmt.Sprintf("worker-%d", i)}
		for j := range cfg.ClusterConfig[i].PFs {
			cfg.ClusterConfig[i].PFs[j].DeviceID = "a2dc"
		}
	}
	return cfg
}

func TestStandardNicConfigurationInvalidSelection(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		mutate     func(*config.ClusterConfig)
	}{
		{"missing node selector", "requires a source group nodeSelector", func(g *config.ClusterConfig) { g.NodeSelector = nil }},
		{"missing PCI", "without a pciAddress", func(g *config.ClusterConfig) { g.PFs[0].PciAddress = "" }},
		{"missing type", "without a deviceID", func(g *config.ClusterConfig) { g.PFs[0].DeviceID = "" }},
		{"mixed types", "mixed deviceIDs", func(g *config.ClusterConfig) { g.PFs[1].DeviceID = "1021" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := standardNicConfigurationConfig(t, config.FlavorK8s, "ethernet", "sriov", true)
			tc.mutate(&cfg.ClusterConfig[0])
			profile := loadProfileFromDir(t, "sriov-ethernet-rdma")
			profile.Templates = profile.Templates[:1]
			_, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestStandardNicConfigurationMixedTrafficNIC(t *testing.T) {
	for _, flavor := range []string{config.FlavorK8s, config.FlavorOCP} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enabled=%t", flavor, enabled), func(t *testing.T) {
				cfg := standardNicConfigurationConfig(t, flavor, "ethernet", "sriov", enabled)
				// Keep both functions in the source inventory. Per-template PF
				// filtering must not hide the excluded sibling from validation.
				cfg.ClusterConfig[0].PFs[0].PciAddress = "0000:AB:00.0"
				cfg.ClusterConfig[0].PFs = append(cfg.ClusterConfig[0].PFs, config.PFConfig{
					PciAddress: " 0000:ab:00.1 ", Traffic: "north-south", DeviceID: "a2dc",
				})
				profileName := "sriov-ethernet-rdma"
				if flavor == config.FlavorOCP {
					profileName += "-ocp"
				}
				profile := loadProfileFromDir(t, profileName)
				_, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
				if enabled {
					require.ErrorContains(t, err, "whole-NIC tuning requires all ports to be in scope")
				} else {
					require.NoError(t, err)
				}
				// A source group outside the requested subset does not block it.
				_, err = (&NetworkOperatorPlugin{Groups: []string{cfg.ClusterConfig[1].Identifier}}).GenerateProfileDeploymentFiles(profile, cfg)
				require.NoError(t, err)
			})
		}
	}
}

func TestStandardNicConfigurationSubsetScope(t *testing.T) {
	cfg := standardNicConfigurationConfig(t, config.FlavorK8s, "ethernet", "sriov", true)
	profile := loadProfileFromDir(t, "sriov-ethernet-rdma")
	full, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(profile, cfg)
	require.NoError(t, err)
	selected := cfg.ClusterConfig[0].Identifier
	subset, err := (&NetworkOperatorPlugin{Groups: []string{selected}}).GenerateProfileDeploymentFiles(profile, cfg)
	require.NoError(t, err)
	names := fileNamesMatching(subset, "30-nicconfigurationtemplate")
	require.Len(t, names, 1)
	require.Equal(t, full[names[0]], subset[names[0]], "selected source keeps its identity and selectors")
}

func TestStandardNicConfigurationDeployValidateConvergence(t *testing.T) {
	for _, flavor := range []string{config.FlavorK8s, config.FlavorOCP} {
		for _, tc := range []struct {
			name, reason, status string
			stale                bool
			want                 crstate.CRState
		}{
			{"success", "UpdateSuccessful", "False", false, crstate.StateSuccess},
			{"inconsistent success", "UpdateSuccessful", "True", false, crstate.StateInProgress},
			{"pending reboot", "PendingReboot", "True", false, crstate.StateInProgress},
			{"runtime failure", "RuntimeConfigUpdateFailed", "False", false, crstate.StateError},
			{"stale success", "UpdateSuccessful", "False", true, crstate.StateInProgress},
		} {
			t.Run(flavor+"/"+tc.name, func(t *testing.T) {
				cfg := standardNicConfigurationConfig(t, flavor, "ethernet", "sriov", true)
				profile := loadProfileFromDir(t, "sriov-ethernet-rdma")
				profile.Templates = profile.Templates[:1]
				rendered, err := (&NetworkOperatorPlugin{Groups: []string{cfg.ClusterConfig[0].Identifier}}).GenerateProfileDeploymentFiles(profile, cfg)
				require.NoError(t, err)
				_, data := fileMatching(t, rendered, "30-nicconfigurationtemplate")
				artifacts, err := bundle.FromFiles([]bundle.File{{Name: "30-config.yaml", Content: data}})
				require.NoError(t, err)
				desired := artifacts.Documents()[0].ObjectCopy()
				live := desired.DeepCopy()
				require.NoError(t, unstructured.SetNestedStringSlice(live.Object, []string{"nic-a"}, "status", "nicDevices"))
				payload, _, _ := unstructured.NestedMap(desired.Object, "spec", "template")
				generation := int64(2)
				observed := generation
				if tc.stale {
					observed--
				}
				device := ocpTestObject("configuration.net.nvidia.com/v1alpha1", "NicDevice", desired.GetNamespace(), "nic-a", map[string]interface{}{
					"spec": map[string]interface{}{"configuration": map[string]interface{}{"resetToDefault": false, "template": payload}},
					"status": map[string]interface{}{
						"node": "worker-0", "type": "a2dc",
						"ports":      []interface{}{map[string]interface{}{"pci": cfg.ClusterConfig[0].PFs[0].PciAddress}},
						"conditions": []interface{}{map[string]interface{}{"type": "ConfigUpdateInProgress", "reason": tc.reason, "status": tc.status, "observedGeneration": observed}},
					},
				})
				device.SetGeneration(generation)
				node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Labels: cfg.ClusterConfig[0].NodeSelector}}
				scheme := runtime.NewScheme()
				require.NoError(t, corev1.AddToScheme(scheme))
				client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live, device, node).Build()
				results, err := ValidateBundle(context.Background(), client, artifacts, flavor)
				require.NoError(t, err)
				require.Len(t, results, 1)
				require.Equal(t, tc.want, results[0].State)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
				defer cancel()
				err = pollUntilTerminalWithRetryableErrorTimeout(ctx, client, crstate.NewDefault(), desired, "NIC configuration", "", 0, time.Millisecond)
				switch tc.want {
				case crstate.StateSuccess:
					require.NoError(t, err)
				case crstate.StateInProgress:
					require.ErrorIs(t, err, context.DeadlineExceeded)
				case crstate.StateError:
					require.ErrorContains(t, err, "RuntimeConfigUpdateFailed")
				case crstate.StateNotDeployed:
					t.Fatal("unexpected test state")
				}
			})
		}
	}
}
