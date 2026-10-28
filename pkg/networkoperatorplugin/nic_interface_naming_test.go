// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"fmt"
	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"os"
	"path/filepath"
	"sigs.k8s.io/yaml"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/stretchr/testify/require"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

func TestInterfaceNamingUsesOneRailPerPhysicalNIC(t *testing.T) {
	for _, tc := range []struct {
		name, mapping, want string
	}{
		{"split physical NIC", `[["0000:03:00.0"], ["0000:03:00.1"]]`, "physical NIC \"0000:03:00\" appears in multiple rails (0 and 1)"},
		{"same rail", `[["0000:03:00.0", "0000:03:00.1"]]`, ""},
		{"distinct physical NICs", `[["0000:03:00.0"], ["0000:04:00.0"]]`, ""},
		{"distinct PCI domains", `[["0000:03:00.0"], ["0002:03:00.1"]]`, ""},
		{"normalized PCI", `[[" 000A:03:00.0 "], ["000a:03:00.1"]]`, "physical NIC \"000a:03:00\" appears in multiple rails"},
		{"invalid PCI", `[["invalid"]]`, "invalid PCI address"},
		{"invalid YAML", `[[`, "read interface naming layout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := fmt.Sprintf("apiVersion: configuration.net.nvidia.com/v1alpha1\nkind: NicInterfaceNameTemplate\nmetadata:\n  name: naming\nspec:\n  railPciAddresses: %s\n", tc.mapping)
			err := validateInterfaceNamingLayout(config.ClusterConfig{Identifier: "source-a"}, map[string]string{"naming.yaml": content})
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestStandardNamingRejectsSplitPhysicalNIC(t *testing.T) {
	for _, sameInventoryRail := range []bool{false, true} {
		cfg, err := config.LoadFullConfig(filepath.Join("testdata", "grouping", "same-ew-different-ns.yaml"), ctrllog.Log)
		require.NoError(t, err)
		cfg.Profile = &config.Profile{Fabric: "ethernet", Deployment: "sriov", Multirail: true}
		if sameInventoryRail {
			// The standard template still emits one outer rail per PF even
			// when inventory Rail metadata claims those PFs share a rail.
			rail := 0
			cfg.ClusterConfig[0].PFs[1].Rail = &rail
		}
		plugin := &NetworkOperatorPlugin{Groups: []string{cfg.ClusterConfig[0].Identifier}}
		profile := loadProfileFromDir(t, "sriov-ethernet-rdma")
		_, err = plugin.GenerateProfileDeploymentFiles(profile, cfg)
		require.ErrorContains(t, err, `physical NIC "0000:03:00" appears in multiple rails (0 and 1)`)
		require.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(err))

		cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate = false
		rendered, err := plugin.GenerateProfileDeploymentFiles(profile, cfg)
		require.NoError(t, err)
		require.Empty(t, fileNamesMatching(rendered, "nicinterfacenametemplate"))
		_, policy := fileMatching(t, rendered, "40-sriovnetworknodepolicy")
		require.Contains(t, policy, "rootDevices:")
		require.NotContains(t, policy, "pfNames:")
	}
}

func TestStandardNamingRailOrderFromBundledPreset(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "presets", "data", "PowerEdge-XE7745-RTX-PRO-4500", "topology.yaml"))
	require.NoError(t, err)
	var inventory config.ClusterConfig
	require.NoError(t, yaml.Unmarshal(data, &inventory))
	for _, profileName := range []string{"sriov-ethernet-rdma", "sriov-ib-rdma", "host-device-rdma", "macvlan-rdma-shared", "ipoib-rdma-shared"} {
		for _, release := range []string{"26.1", "26.7"} {
			t.Run(profileName+"/"+release, func(t *testing.T) {
				cfg := standardNicConfigurationConfig(t, config.FlavorK8s, "ethernet", "sriov", false)
				cfg.NetworkOperator.SelectedRelease = release
				cfg.ClusterConfig = cfg.ClusterConfig[:1]
				cfg.ClusterConfig[0].PFs = inventory.PFs
				cfg.NicConfigurationOperator.NetdevPrefix = "custom_r%rail_id%"
				cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate = true
				rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(loadProfileFromDir(t, profileName), cfg)
				require.NoError(t, err)
				_, content := fileMatching(t, rendered, "nicinterfacenametemplate")
				var naming nicop.NicInterfaceNameTemplate
				require.NoError(t, yaml.Unmarshal([]byte(content), &naming))
				require.Len(t, naming.Spec.RailPciAddresses, 4)
				require.Equal(t, "custom_r%rail_id%", naming.Spec.NetDevicePrefix)
				for _, pf := range inventory.PFs {
					if pf.Traffic == "east-west" {
						require.Equal(t, []string{pf.PciAddress}, naming.Spec.RailPciAddresses[*pf.Rail])
					}
				}
				if profileName == "host-device-rdma" || (release == "26.1" && (profileName == "macvlan-rdma-shared" || profileName == "ipoib-rdma-shared")) {
					_, policy := fileMatching(t, rendered, "10-nicclusterpolicy")
					for _, pf := range inventory.PFs {
						if pf.Traffic == "east-west" {
							require.Contains(t, policy, fmt.Sprintf("\"custom_r%d\"", *pf.Rail), "singleton selectors must use the names assigned by NCO")
							require.NotContains(t, policy, fmt.Sprintf("\"%s\"", pf.NetworkInterface), "saved names must not survive in singleton selectors")
						}
					}
				}
				require.Equal(t, inventory.PFs, cfg.ClusterConfig[0].PFs, "rendering must preserve saved interface names")
				require.Equal(t, 1, *inventory.PFs[0].Rail, "rendering must preserve source order")
			})
		}
	}
}

func TestStandardNamingRejectsNonContiguousRails(t *testing.T) {
	rail := 2
	err := validateInterfaceNamingLayout(config.ClusterConfig{Identifier: "source-a", PFs: []config.PFConfig{{PciAddress: "0000:03:00.0", Traffic: "east-west", Rail: &rail}}}, map[string]string{"naming.yaml": "spec:\n  railPciAddresses: [[\"0000:03:00.0\"]]\n"})
	require.ErrorContains(t, err, "inventory rail 2 but naming rail 0")
	require.Equal(t, apperrors.ExitValidation, apperrors.ExitCodeFromError(err))
}
