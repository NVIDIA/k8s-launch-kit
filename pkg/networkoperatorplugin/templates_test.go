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
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/assert"
)

func TestReplaceVars(t *testing.T) {
	// Get the function from templateFuncs
	replaceVarsFunc := templateFuncs["replaceVars"].(func(string, int, int, int) string)

	t.Run("replace all three placeholders", func(t *testing.T) {
		template := "nic%nic_id%_p%plane%_r%rail%"
		result := replaceVarsFunc(template, 1, 2, 3)
		assert.Equal(t, "nic1_p2_r3", result)
	})

	t.Run("replace with different values", func(t *testing.T) {
		template := "nic%nic_id%_p%plane%_r%rail%"
		
		result1 := replaceVarsFunc(template, 1, 1, 1)
		assert.Equal(t, "nic1_p1_r1", result1)
		
		result2 := replaceVarsFunc(template, 4, 3, 2)
		assert.Equal(t, "nic4_p3_r2", result2)
		
		result3 := replaceVarsFunc(template, 10, 8, 5)
		assert.Equal(t, "nic10_p8_r5", result3)
	})

	t.Run("template with only plane and rail", func(t *testing.T) {
		template := "eth_p%plane%_r%rail%"
		result := replaceVarsFunc(template, 1, 2, 3)
		assert.Equal(t, "eth_p2_r3", result, "Should replace plane and rail, ignore unused nic_id")
	})

	t.Run("template with only nic_id and rail", func(t *testing.T) {
		template := "nic%nic_id%_rail%rail%"
		result := replaceVarsFunc(template, 5, 2, 7)
		assert.Equal(t, "nic5_rail7", result, "Should replace nic_id and rail, ignore unused plane")
	})

	t.Run("template with only rail", func(t *testing.T) {
		template := "rail_%rail%"
		result := replaceVarsFunc(template, 1, 2, 9)
		assert.Equal(t, "rail_9", result, "Should replace only rail placeholder")
	})

	t.Run("template with no placeholders", func(t *testing.T) {
		template := "static_interface_name"
		result := replaceVarsFunc(template, 1, 2, 3)
		assert.Equal(t, "static_interface_name", result, "Should return unchanged when no placeholders")
	})

	t.Run("template with repeated placeholders", func(t *testing.T) {
		template := "nic%nic_id%_plane%plane%_plane%plane%_rail%rail%"
		result := replaceVarsFunc(template, 2, 3, 4)
		assert.Equal(t, "nic2_plane3_plane3_rail4", result, "Should replace all occurrences")
	})

	t.Run("complex custom template", func(t *testing.T) {
		template := "mlx5_%nic_id%_p%plane%_r%rail%"
		result := replaceVarsFunc(template, 0, 1, 2)
		assert.Equal(t, "mlx5_0_p1_r2", result)
	})

	t.Run("template with underscores and hyphens", func(t *testing.T) {
		template := "nic-%nic_id%-plane-%plane%-rail-%rail%"
		result := replaceVarsFunc(template, 3, 2, 1)
		assert.Equal(t, "nic-3-plane-2-rail-1", result)
	})
}

func TestMultiplaneInterfaceNaming(t *testing.T) {
	replaceVarsFunc := templateFuncs["replaceVars"].(func(string, int, int, int) string)

	t.Run("multiplane with all placeholders", func(t *testing.T) {
		// Standard multiplane template with all three placeholders
		template := "nic%nic_id%_p%plane%_r%rail%"
		
		// Generate names for rail 1, 4 planes
		names := []string{}
		for plane := 1; plane <= 4; plane++ {
			names = append(names, replaceVarsFunc(template, 1, plane, 1))
		}
		
		assert.Equal(t, []string{
			"nic1_p1_r1",
			"nic1_p2_r1",
			"nic1_p3_r1",
			"nic1_p4_r1",
		}, names)
	})

	t.Run("multiplane with only plane and rail required", func(t *testing.T) {
		// Template without nic_id (should still work for multiplane)
		template := "eth_p%plane%_r%rail%"
		
		// Generate names for rail 2, 2 planes
		names := []string{}
		for plane := 1; plane <= 2; plane++ {
			names = append(names, replaceVarsFunc(template, 999, plane, 2)) // nic_id ignored
		}
		
		assert.Equal(t, []string{
			"eth_p1_r2",
			"eth_p2_r2",
		}, names)
	})

	t.Run("multiplane across multiple rails", func(t *testing.T) {
		template := "nic%nic_id%_p%plane%_r%rail%"
		
		// Generate names for 4 rails, 2 planes each
		allNames := [][]string{}
		for rail := 1; rail <= 4; rail++ {
			railNames := []string{}
			for plane := 1; plane <= 2; plane++ {
				railNames = append(railNames, replaceVarsFunc(template, rail, plane, rail))
			}
			allNames = append(allNames, railNames)
		}
		
		// Verify rail 1
		assert.Equal(t, []string{"nic1_p1_r1", "nic1_p2_r1"}, allNames[0])
		// Verify rail 3
		assert.Equal(t, []string{"nic3_p1_r3", "nic3_p2_r3"}, allNames[2])
		// Verify rail 4
		assert.Equal(t, []string{"nic4_p1_r4", "nic4_p2_r4"}, allNames[3])
	})

	t.Run("multiplane with custom separator", func(t *testing.T) {
		template := "sriov-nic%nic_id%-plane%plane%-rail%rail%"
		
		names := []string{}
		for plane := 1; plane <= 4; plane++ {
			names = append(names, replaceVarsFunc(template, 5, plane, 2))
		}
		
		assert.Equal(t, []string{
			"sriov-nic5-plane1-rail2",
			"sriov-nic5-plane2-rail2",
			"sriov-nic5-plane3-rail2",
			"sriov-nic5-plane4-rail2",
		}, names)
	})
}

func TestEdgeCases(t *testing.T) {
	replaceVarsFunc := templateFuncs["replaceVars"].(func(string, int, int, int) string)

	t.Run("empty template", func(t *testing.T) {
		result := replaceVarsFunc("", 1, 2, 3)
		assert.Equal(t, "", result)
	})

	t.Run("template with zero values", func(t *testing.T) {
		template := "nic%nic_id%_p%plane%_r%rail%"
		result := replaceVarsFunc(template, 0, 0, 0)
		assert.Equal(t, "nic0_p0_r0", result)
	})

	t.Run("template with large numbers", func(t *testing.T) {
		template := "nic%nic_id%_p%plane%_r%rail%"
		result := replaceVarsFunc(template, 100, 200, 300)
		assert.Equal(t, "nic100_p200_r300", result)
	})

	t.Run("partial placeholder names should not be replaced", func(t *testing.T) {
		template := "nic%nic_id%extra_p%plane_extra_r%rail%end"
		result := replaceVarsFunc(template, 1, 2, 3)
		// Only exact placeholders should be replaced
		assert.Equal(t, "nic1extra_p%plane_extra_r3end", result)
	})
}

func TestTemplateVariations(t *testing.T) {
	replaceVarsFunc := templateFuncs["replaceVars"].(func(string, int, int, int) string)

	testCases := []struct {
		name           string
		template       string
		nicID          int
		plane          int
		rail           int
		expected       string
		hasPlane       bool
		hasRail        bool
		validMultiplane bool
	}{
		{
			name:           "all three placeholders",
			template:       "nic%nic_id%_p%plane%_r%rail%",
			nicID:          1,
			plane:          2,
			rail:           3,
			expected:       "nic1_p2_r3",
			hasPlane:       true,
			hasRail:        true,
			validMultiplane: true,
		},
		{
			name:           "plane and rail only",
			template:       "eth_p%plane%_r%rail%",
			nicID:          99,
			plane:          1,
			rail:           2,
			expected:       "eth_p1_r2",
			hasPlane:       true,
			hasRail:        true,
			validMultiplane: true,
		},
		{
			name:           "nic_id and rail only",
			template:       "nic%nic_id%_r%rail%",
			nicID:          5,
			plane:          99,
			rail:           3,
			expected:       "nic5_r3",
			hasPlane:       false,
			hasRail:        true,
			validMultiplane: false, // Missing plane
		},
		{
			name:           "nic_id and plane only",
			template:       "nic%nic_id%_p%plane%",
			nicID:          2,
			plane:          4,
			rail:           99,
			expected:       "nic2_p4",
			hasPlane:       true,
			hasRail:        false,
			validMultiplane: false, // Missing rail
		},
		{
			name:           "rail only",
			template:       "rail%rail%",
			nicID:          1,
			plane:          2,
			rail:           7,
			expected:       "rail7",
			hasPlane:       false,
			hasRail:        true,
			validMultiplane: false, // Missing plane
		},
		{
			name:           "nic_id only",
			template:       "nic%nic_id%",
			nicID:          8,
			plane:          1,
			rail:           1,
			expected:       "nic8",
			hasPlane:       false,
			hasRail:        false,
			validMultiplane: false, // Missing both plane and rail
		},
		{
			name:           "custom format with all placeholders",
			template:       "mlnx-nic%nic_id%-plane%plane%-rail%rail%-dev",
			nicID:          3,
			plane:          2,
			rail:           1,
			expected:       "mlnx-nic3-plane2-rail1-dev",
			hasPlane:       true,
			hasRail:        true,
			validMultiplane: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := replaceVarsFunc(tc.template, tc.nicID, tc.plane, tc.rail)
			assert.Equal(t, tc.expected, result)
			
			// Validate multiplane requirements
			if tc.validMultiplane {
				assert.True(t, tc.hasPlane && tc.hasRail, 
					"Valid multiplane template must have both plane and rail placeholders")
			}
		})
	}
}

func TestUntilStep(t *testing.T) {
	untilStepFunc := templateFuncs["untilStep"].(func(int, int, int) []int)

	t.Run("generate sequence 1 to 4 step 1", func(t *testing.T) {
		result := untilStepFunc(1, 5, 1)
		assert.Equal(t, []int{1, 2, 3, 4}, result)
	})

	t.Run("generate sequence 0 to 4 step 1", func(t *testing.T) {
		result := untilStepFunc(0, 4, 1)
		assert.Equal(t, []int{0, 1, 2, 3}, result)
	})

	t.Run("generate sequence 1 to 10 step 2", func(t *testing.T) {
		result := untilStepFunc(1, 10, 2)
		assert.Equal(t, []int{1, 3, 5, 7, 9}, result)
	})

	t.Run("generate empty sequence when start >= stop", func(t *testing.T) {
		result := untilStepFunc(5, 5, 1)
		assert.Equal(t, []int{}, result)
		
		result2 := untilStepFunc(10, 5, 1)
		assert.Equal(t, []int{}, result2)
	})

	t.Run("generate sequence with larger step", func(t *testing.T) {
		result := untilStepFunc(0, 10, 3)
		assert.Equal(t, []int{0, 3, 6, 9}, result)
	})
}

func TestMultiRailNaming(t *testing.T) {
	replaceVarsFunc := templateFuncs["replaceVars"].(func(string, int, int, int) string)

	t.Run("4 rails with 4 planes each using standard template", func(t *testing.T) {
		template := "nic%nic_id%_p%plane%_r%rail%"
		numberOfPlanes := 4
		numberOfRails := 4
		
		// Generate all interface names for 4 rails × 4 planes
		allInterfaces := make([][]string, numberOfRails)
		for rail := 1; rail <= numberOfRails; rail++ {
			railInterfaces := []string{}
			for plane := 1; plane <= numberOfPlanes; plane++ {
				name := replaceVarsFunc(template, rail, plane, rail)
				railInterfaces = append(railInterfaces, name)
			}
			allInterfaces[rail-1] = railInterfaces
		}
		
		// Verify rail 1
		assert.Equal(t, []string{
			"nic1_p1_r1", "nic1_p2_r1", "nic1_p3_r1", "nic1_p4_r1",
		}, allInterfaces[0])
		
		// Verify rail 3
		assert.Equal(t, []string{
			"nic3_p1_r3", "nic3_p2_r3", "nic3_p3_r3", "nic3_p4_r3",
		}, allInterfaces[2])
	})

	t.Run("4 rails with 2 planes each", func(t *testing.T) {
		template := "eth_p%plane%_r%rail%"
		numberOfPlanes := 2
		numberOfRails := 4
		
		allInterfaces := make([][]string, numberOfRails)
		for rail := 1; rail <= numberOfRails; rail++ {
			railInterfaces := []string{}
			for plane := 1; plane <= numberOfPlanes; plane++ {
				name := replaceVarsFunc(template, rail, plane, rail)
				railInterfaces = append(railInterfaces, name)
			}
			allInterfaces[rail-1] = railInterfaces
		}
		
		// Verify rail 1 has 2 planes
		assert.Len(t, allInterfaces[0], 2)
		assert.Equal(t, []string{"eth_p1_r1", "eth_p2_r1"}, allInterfaces[0])
		
		// Verify rail 4 has 2 planes
		assert.Len(t, allInterfaces[3], 2)
		assert.Equal(t, []string{"eth_p1_r4", "eth_p2_r4"}, allInterfaces[3])
	})

	t.Run("8 rails with 1 plane each", func(t *testing.T) {
		template := "sriov_nic%nic_id%_r%rail%"
		numberOfRails := 8
		
		railNames := []string{}
		for rail := 1; rail <= numberOfRails; rail++ {
			// Single plane per rail
			name := replaceVarsFunc(template, rail, 1, rail)
			railNames = append(railNames, name)
		}
		
		assert.Equal(t, []string{
			"sriov_nic1_r1", "sriov_nic2_r2", "sriov_nic3_r3", "sriov_nic4_r4",
			"sriov_nic5_r5", "sriov_nic6_r6", "sriov_nic7_r7", "sriov_nic8_r8",
		}, railNames)
	})
}

func TestTemplateValidation(t *testing.T) {
	t.Run("validate multiplane templates have required placeholders", func(t *testing.T) {
		validTemplates := []string{
			"nic%nic_id%_p%plane%_r%rail%",  // All three
			"eth_p%plane%_r%rail%",            // Plane and rail (valid for multiplane)
			"nic%nic_id%_plane%plane%_rail%rail%", // Alternative naming
		}
		
		for _, template := range validTemplates {
			hasPlane := containsPlaceholder(template, "%plane%")
			hasRail := containsPlaceholder(template, "%rail%")
			assert.True(t, hasPlane && hasRail, 
				"Multiplane template %s should have both plane and rail placeholders", template)
		}
	})

	t.Run("identify invalid multiplane templates", func(t *testing.T) {
		invalidTemplates := []string{
			"nic%nic_id%",                    // Missing plane and rail
			"nic%nic_id%_r%rail%",            // Missing plane
			"nic%nic_id%_p%plane%",           // Missing rail
			"static_name",                     // No placeholders
		}
		
		for _, template := range invalidTemplates {
			hasPlane := containsPlaceholder(template, "%plane%")
			hasRail := containsPlaceholder(template, "%rail%")
			assert.False(t, hasPlane && hasRail,
				"Template %s should not be valid for multiplane (missing required placeholders)", template)
		}
	})
}

// helper to create a PFConfig with east-west traffic and a rail number
func ewPF(pciAddr string, rail int) config.PFConfig {
	r := rail
	return config.PFConfig{
		DeviceID:   "a2dc",
		PciAddress: pciAddr,
		Traffic:    "east-west",
		Rail:       &r,
	}
}

func TestSanitizeIdentifier(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"NVIDIA-H200", "nvidia-h200"},
		{"NVIDIA A100 80GB PCIe", "nvidia-a100-80gb-pcie"},
		{"already-lowercase", "already-lowercase"},
		{"MixedCase GPU", "mixedcase-gpu"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.expected, sanitizeIdentifier(tc.input))
		})
	}
}

// labelledGroup builds a source group fixture in the post-Unit-6 shape:
// MachineType + GPUType resolved, Identifier follows resource-name
// conventions (lowercase via sanitizeIdentifier), NodeSelector keyed by
// config.MachineLabelKey with the raw `<machineType>-<gpuType>` value.
func labelledGroup(machineType, gpuType string, pfs []config.PFConfig, nodes []string,
	caps *config.ClusterCapabilities) config.ClusterConfig {
	labelValue := config.MachineLabelValue(machineType, gpuType)
	return config.ClusterConfig{
		Identifier:   sanitizeIdentifier(labelValue),
		MachineType:  machineType,
		GPUType:      gpuType,
		PFs:          pfs,
		WorkerNodes:  nodes,
		Capabilities: caps,
		NodeSelector: map[string]string{config.MachineLabelKey: labelValue},
	}
}

func TestMergeCompatibleGroups(t *testing.T) {
	t.Run("all groups same gpuType and rail count", func(t *testing.T) {
		groups := []config.ClusterConfig{
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:19:00.0", 0), ewPF("0000:2a:00.0", 1)},
				[]string{"node-b", "node-a"},
				&config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Sriov: true, Rdma: true}},
			),
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:1a:00.0", 0), ewPF("0000:3c:00.0", 1)},
				[]string{"node-d", "node-c"},
				&config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Sriov: true, Rdma: true, Ib: true}},
			),
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:09:00.0", 0), ewPF("0000:23:00.0", 1)},
				[]string{"node-e"},
				&config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Rdma: true}},
			),
		}

		result, _ := mergeCompatibleGroups(groups, false)

		assert.Len(t, result, 1)
		merged := result[0]
		// Merged groups can span machineTypes, so identifier follows
		// resource-name conventions (lowercase via sanitizeIdentifier) and
		// nodeSelector keys on the GPULabelKey written by `l8k discover`
		// — its value matches the gpuType verbatim.
		assert.Equal(t, "nvidia-h200", merged.Identifier)
		assert.Equal(t, "NVIDIA-H200", merged.GPUType)
		assert.Equal(t, "DGX-B200", merged.MachineType)
		assert.Equal(t, map[string]string{config.GPULabelKey: "NVIDIA-H200"}, merged.NodeSelector)
		assert.Equal(t, []string{"node-a", "node-b", "node-c", "node-d", "node-e"}, merged.WorkerNodes)

		// Capabilities should be aggregated
		assert.True(t, merged.Capabilities.Nodes.Sriov)
		assert.True(t, merged.Capabilities.Nodes.Rdma)
		assert.True(t, merged.Capabilities.Nodes.Ib)

		// RailPciAddresses: 2 rails, 3 addresses each
		assert.Len(t, merged.RailPciAddresses, 2)
		assert.Equal(t, []string{"0000:19:00.0", "0000:1a:00.0", "0000:09:00.0"}, merged.RailPciAddresses[0])
		assert.Equal(t, []string{"0000:2a:00.0", "0000:3c:00.0", "0000:23:00.0"}, merged.RailPciAddresses[1])
	})

	t.Run("different gpuTypes no merge", func(t *testing.T) {
		groups := []config.ClusterConfig{
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:19:00.0", 0)}, []string{"node-a"}, nil),
			labelledGroup("DGX-B200", "NVIDIA-A100",
				[]config.PFConfig{ewPF("0000:1a:00.0", 0)}, []string{"node-b"}, nil),
		}

		result, _ := mergeCompatibleGroups(groups, false)

		assert.Len(t, result, 2)
		// Single-source buckets keep the source group's machine-label identifier.
		assert.Equal(t, sanitizeIdentifier(config.MachineLabelValue("DGX-B200", "NVIDIA-H200")), result[0].Identifier)
		assert.Equal(t, sanitizeIdentifier(config.MachineLabelValue("DGX-B200", "NVIDIA-A100")), result[1].Identifier)
		assert.Nil(t, result[0].RailPciAddresses)
		assert.Nil(t, result[1].RailPciAddresses)
	})

	t.Run("different machineTypes same gpuType auto-merge", func(t *testing.T) {
		// Merge keys on (gpuType, railCount), so two vendor SKUs sharing a
		// GPU type auto-merge. The merged group's identifier is a
		// lowercase resource-name form of the gpuType, and the
		// nodeSelector keys on GPULabelKey (l8k state, written by
		// discover) since the source machine labels differ.
		groups := []config.ClusterConfig{
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:19:00.0", 0)}, []string{"node-a"}, nil),
			labelledGroup("PowerEdge-XE9680", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:1a:00.0", 0)}, []string{"node-b"}, nil),
		}

		result, _ := mergeCompatibleGroups(groups, false)

		assert.Len(t, result, 1)
		assert.Equal(t, "nvidia-h200", result[0].Identifier)
		assert.Equal(t, map[string]string{config.GPULabelKey: "NVIDIA-H200"}, result[0].NodeSelector)
	})

	t.Run("same gpuType different rail count no merge", func(t *testing.T) {
		groups := []config.ClusterConfig{
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:19:00.0", 0), ewPF("0000:2a:00.0", 1)},
				[]string{"node-a"}, nil),
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:1a:00.0", 0)},
				[]string{"node-b"}, nil),
		}

		result, _ := mergeCompatibleGroups(groups, false)

		// Same gpuType but different rail counts → kept separate.
		assert.Len(t, result, 2)
	})

	t.Run("mixed some mergeable some not", func(t *testing.T) {
		groups := []config.ClusterConfig{
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:19:00.0", 0)},
				[]string{"node-a"},
				&config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Rdma: true}}),
			labelledGroup("DGX-A100", "NVIDIA-A100",
				[]config.PFConfig{ewPF("0000:1a:00.0", 0)},
				[]string{"node-b"}, nil),
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:09:00.0", 0)},
				[]string{"node-c"},
				&config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Rdma: true}}),
		}

		result, _ := mergeCompatibleGroups(groups, false)

		assert.Len(t, result, 2)
		// First: merged H200 group (group-0 + group-2) — gpuType-based identifier
		assert.Equal(t, "nvidia-h200", result[0].Identifier)
		assert.Equal(t, []string{"node-a", "node-c"}, result[0].WorkerNodes)
		assert.Len(t, result[0].RailPciAddresses, 1)
		assert.Equal(t, []string{"0000:19:00.0", "0000:09:00.0"}, result[0].RailPciAddresses[0])
		// Second: unmerged DGX-A100/A100 group keeps its machine-label identifier
		assert.Equal(t, sanitizeIdentifier(config.MachineLabelValue("DGX-A100", "NVIDIA-A100")), result[1].Identifier)
		assert.Nil(t, result[1].RailPciAddresses)
	})

	t.Run("single group no merge", func(t *testing.T) {
		groups := []config.ClusterConfig{
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:19:00.0", 0)},
				[]string{"node-a"}, nil),
		}

		result, _ := mergeCompatibleGroups(groups, false)

		assert.Len(t, result, 1)
		assert.Equal(t, sanitizeIdentifier(config.MachineLabelValue("DGX-B200", "NVIDIA-H200")), result[0].Identifier)
		assert.Nil(t, result[0].RailPciAddresses)
	})

	t.Run("empty machineType or gpuType no merge", func(t *testing.T) {
		// When the machine label can't be computed (probe failed), each
		// group lands in its own bucket and the fallback "group-N"
		// identifier carries through.
		groups := []config.ClusterConfig{
			{
				Identifier:  "group-0",
				GPUType:     "",
				PFs:         []config.PFConfig{ewPF("0000:19:00.0", 0)},
				WorkerNodes: []string{"node-a"},
			},
			{
				Identifier:  "group-1",
				GPUType:     "",
				PFs:         []config.PFConfig{ewPF("0000:1a:00.0", 0)},
				WorkerNodes: []string{"node-b"},
			},
		}

		result, _ := mergeCompatibleGroups(groups, false)

		assert.Len(t, result, 2)
		assert.Equal(t, "group-0", result[0].Identifier)
		assert.Equal(t, "group-1", result[1].Identifier)
	})

	t.Run("north-south PFs excluded from rail count", func(t *testing.T) {
		nsPF := config.PFConfig{DeviceID: "101f", PciAddress: "0000:5a:00.0", Traffic: "north-south"}
		groups := []config.ClusterConfig{
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:19:00.0", 0), nsPF},
				[]string{"node-a"},
				&config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Rdma: true}}),
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:1a:00.0", 0), nsPF},
				[]string{"node-b"},
				&config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Rdma: true}}),
		}

		result, _ := mergeCompatibleGroups(groups, false)

		// Should merge: both have 1 east-west rail (north-south excluded from count)
		assert.Len(t, result, 1)
		assert.Equal(t, "nvidia-h200", result[0].Identifier)
		assert.Len(t, result[0].RailPciAddresses, 1)
		assert.Equal(t, []string{"0000:19:00.0", "0000:1a:00.0"}, result[0].RailPciAddresses[0])
	})

	t.Run("no PCI conflict reports hadPciConflicts false", func(t *testing.T) {
		groups := []config.ClusterConfig{
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:19:00.0", 0)}, []string{"node-a"}, nil),
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:1a:00.0", 0)}, []string{"node-b"}, nil),
		}

		result, hadPciConflicts := mergeCompatibleGroups(groups, true)

		assert.Len(t, result, 1)
		assert.False(t, hadPciConflicts, "no PCI conflicts, name templates not needed")
	})

	t.Run("PCI conflict with name templates merges and reports hadPciConflicts true", func(t *testing.T) {
		groups := []config.ClusterConfig{
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:19:00.0", 0), ewPF("0000:9c:00.0", 1)},
				[]string{"node-a"}, nil),
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:9c:00.0", 0), ewPF("0000:cd:00.0", 1)},
				[]string{"node-b"}, nil),
		}

		result, hadPciConflicts := mergeCompatibleGroups(groups, true)

		// With name templates enabled, should merge despite PCI conflict
		assert.Len(t, result, 1)
		assert.True(t, hadPciConflicts, "PCI conflicts exist, name templates needed")
	})

	t.Run("duplicate PCI addresses across groups are deduplicated per rail", func(t *testing.T) {
		// Two nodes with identical east-west PCI layouts — merged RailPciAddresses
		// should contain each address once per rail, preserving first-occurrence order.
		groups := []config.ClusterConfig{
			labelledGroup("DGX-GB300", "NVIDIA-GB300",
				[]config.PFConfig{ewPF("0000:03:00.0", 0), ewPF("0000:03:00.1", 1)},
				[]string{"node-a"}, nil),
			labelledGroup("DGX-GB300", "NVIDIA-GB300",
				[]config.PFConfig{ewPF("0000:03:00.0", 0), ewPF("0000:03:00.1", 1)},
				[]string{"node-b"}, nil),
		}

		result, _ := mergeCompatibleGroups(groups, false)

		assert.Len(t, result, 1)
		assert.Len(t, result[0].RailPciAddresses, 2)
		assert.Equal(t, []string{"0000:03:00.0"}, result[0].RailPciAddresses[0])
		assert.Equal(t, []string{"0000:03:00.1"}, result[0].RailPciAddresses[1])
	})

	t.Run("partial PCI overlap across groups deduplicated per rail", func(t *testing.T) {
		// Three nodes: two share rail 0 PCI, one differs. Merged result keeps both unique
		// addresses on rail 0 in first-occurrence order.
		groups := []config.ClusterConfig{
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:19:00.0", 0)}, []string{"node-a"}, nil),
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:19:00.0", 0)}, []string{"node-b"}, nil),
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:1a:00.0", 0)}, []string{"node-c"}, nil),
		}

		result, _ := mergeCompatibleGroups(groups, false)

		assert.Len(t, result, 1)
		assert.Equal(t, []string{"0000:19:00.0", "0000:1a:00.0"}, result[0].RailPciAddresses[0])
	})

	t.Run("cross-rail PCI address conflict prevents merge", func(t *testing.T) {
		// Same PCI address 0000:9c:00.0 at rail 4 in group-1 and rail 5 in group-2.
		// Merging would cause the device plugin to claim it for the wrong rail.
		expectedID := sanitizeIdentifier(config.MachineLabelValue("DGX-B200", "NVIDIA-H200"))
		groups := []config.ClusterConfig{
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:19:00.0", 0), ewPF("0000:9b:00.0", 1)},
				[]string{"node-a"}, nil),
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:1a:00.0", 0), ewPF("0000:9c:00.0", 1)},
				[]string{"node-b"}, nil),
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{ewPF("0000:9c:00.0", 0), ewPF("0000:cd:00.0", 1)},
				[]string{"node-c"}, nil),
		}

		result, _ := mergeCompatibleGroups(groups, false)

		// Should NOT merge: PCI conflict on 0000:9c:00.0 (rail 1 vs rail 0).
		// All three groups share the same machine label, so the bucket
		// contains all of them; the PCI-conflict check then returns each
		// source group individually.
		assert.Len(t, result, 3)
		for _, g := range result {
			assert.Equal(t, expectedID, g.Identifier)
		}
		assert.Nil(t, result[0].RailPciAddresses)
	})
}

func TestApplyPrefix(t *testing.T) {
	tests := []struct {
		prefix   string
		nicID    int
		plane    int
		rail     int
		expected string
	}{
		{"eth_r%rail%", 0, 0, 0, "eth_r0"},
		{"eth_r%rail%", 0, 0, 3, "eth_r3"},
		{"rdma_r%rail%", 0, 0, 7, "rdma_r7"},
		{"roce_p%plane%_r%rail%", 1, 2, 3, "roce_p2_r3"},
		{"nic%nic_id%_p%plane%_r%rail%", 5, 1, 2, "nic5_p1_r2"},
		{"static_name", 0, 0, 0, "static_name"},
	}
	for _, tc := range tests {
		t.Run(tc.expected, func(t *testing.T) {
			assert.Equal(t, tc.expected, applyPrefix(tc.prefix, tc.nicID, tc.plane, tc.rail))
		})
	}
}

func TestPfsPerNic(t *testing.T) {
	t.Run("single-port NICs", func(t *testing.T) {
		pfs := []config.PFConfig{
			ewPF("0000:19:00.0", 0),
			ewPF("0000:2a:00.0", 1),
			ewPF("0000:3b:00.0", 2),
			ewPF("0000:4c:00.0", 3),
		}
		assert.Equal(t, 1, pfsPerNic(pfs))
	})

	t.Run("dual-port NICs", func(t *testing.T) {
		pfs := []config.PFConfig{
			ewPF("0000:19:00.0", 0),
			ewPF("0000:19:00.1", 1),
			ewPF("0000:2a:00.0", 2),
			ewPF("0000:2a:00.1", 3),
		}
		assert.Equal(t, 2, pfsPerNic(pfs))
	})

	t.Run("empty PFs", func(t *testing.T) {
		assert.Equal(t, 1, pfsPerNic(nil))
		assert.Equal(t, 1, pfsPerNic([]config.PFConfig{}))
	})

	t.Run("north-south PFs excluded", func(t *testing.T) {
		pfs := []config.PFConfig{
			ewPF("0000:19:00.0", 0),
			ewPF("0000:2a:00.0", 1),
			{DeviceID: "101f", PciAddress: "0000:5a:00.0", Traffic: "north-south"},
			{DeviceID: "101f", PciAddress: "0000:5a:00.1", Traffic: "north-south"},
		}
		assert.Equal(t, 1, pfsPerNic(pfs))
	})
}

func TestHasEmptyNetworkInterfaceNames(t *testing.T) {
	t.Run("all PFs have interface names", func(t *testing.T) {
		groups := []config.ClusterConfig{
			{
				PFs: []config.PFConfig{
					{PciAddress: "0000:19:00.0", Traffic: "east-west", NetworkInterface: "eth0"},
					{PciAddress: "0000:2a:00.0", Traffic: "east-west", NetworkInterface: "eth1"},
				},
			},
		}
		assert.False(t, hasEmptyNetworkInterfaceNames(groups))
	})

	t.Run("some PFs have empty interface names", func(t *testing.T) {
		groups := []config.ClusterConfig{
			{
				PFs: []config.PFConfig{
					{PciAddress: "0000:19:00.0", Traffic: "east-west", NetworkInterface: "eth0"},
					{PciAddress: "0000:2a:00.0", Traffic: "east-west", NetworkInterface: ""},
				},
			},
		}
		assert.True(t, hasEmptyNetworkInterfaceNames(groups))
	})

	t.Run("north-south PFs excluded", func(t *testing.T) {
		groups := []config.ClusterConfig{
			{
				PFs: []config.PFConfig{
					{PciAddress: "0000:19:00.0", Traffic: "east-west", NetworkInterface: "eth0"},
					{PciAddress: "0000:5a:00.0", Traffic: "north-south", NetworkInterface: ""},
				},
			},
		}
		assert.False(t, hasEmptyNetworkInterfaceNames(groups))
	})

	t.Run("empty interface in second group", func(t *testing.T) {
		groups := []config.ClusterConfig{
			{
				PFs: []config.PFConfig{
					{PciAddress: "0000:19:00.0", Traffic: "east-west", NetworkInterface: "eth0"},
				},
			},
			{
				PFs: []config.PFConfig{
					{PciAddress: "0000:1a:00.0", Traffic: "east-west", NetworkInterface: ""},
				},
			},
		}
		assert.True(t, hasEmptyNetworkInterfaceNames(groups))
	})
}

func TestIsRdmaShared(t *testing.T) {
	t.Run("rdma_shared deployment", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{Deployment: "rdma_shared"},
		}
		assert.True(t, isRdmaShared(cfg))
	})

	t.Run("sriov deployment", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{Deployment: "sriov"},
		}
		assert.False(t, isRdmaShared(cfg))
	})

	t.Run("nil profile", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{}
		assert.False(t, isRdmaShared(cfg))
	})
}

func TestMaybeDisableInterfaceNameTemplate(t *testing.T) {
	mkCfg := func(deployment string, deploy bool, spectrumX bool) *config.LaunchKubernetesConfig {
		c := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{Deployment: deployment},
			NicConfigurationOperator: &config.NicConfigurationOperatorConfig{
				DeployNicInterfaceNameTemplate: deploy,
			},
		}
		if spectrumX {
			c.Profile.SpectrumX = &config.ProfileSpectrumX{Enable: true}
		}
		return c
	}
	oneGroup := []config.ClusterConfig{{Identifier: "g1"}}
	twoGroups := []config.ClusterConfig{{Identifier: "g1"}, {Identifier: "g2"}}

	t.Run("single source group + sriov → disabled", func(t *testing.T) {
		cfg := mkCfg("sriov", true, false)
		out := maybeDisableInterfaceNameTemplate(cfg, oneGroup)
		assert.False(t, out.NicConfigurationOperator.DeployNicInterfaceNameTemplate)
		// original cfg must NOT be mutated
		assert.True(t, cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate)
	})

	t.Run("multiple source groups → left enabled", func(t *testing.T) {
		cfg := mkCfg("sriov", true, false)
		out := maybeDisableInterfaceNameTemplate(cfg, twoGroups)
		assert.True(t, out.NicConfigurationOperator.DeployNicInterfaceNameTemplate)
		assert.Same(t, cfg, out)
	})

	t.Run("flag already false → left as-is", func(t *testing.T) {
		cfg := mkCfg("sriov", false, false)
		out := maybeDisableInterfaceNameTemplate(cfg, oneGroup)
		assert.False(t, out.NicConfigurationOperator.DeployNicInterfaceNameTemplate)
		assert.Same(t, cfg, out)
	})

	t.Run("spectrum-x single group → left enabled (distinct render path)", func(t *testing.T) {
		cfg := mkCfg("sriov", true, true)
		out := maybeDisableInterfaceNameTemplate(cfg, oneGroup)
		assert.True(t, out.NicConfigurationOperator.DeployNicInterfaceNameTemplate)
		assert.Same(t, cfg, out)
	})

	t.Run("rdma_shared with empty interface names → left enabled (no PCI fallback)", func(t *testing.T) {
		cfg := mkCfg("rdma_shared", true, false)
		// One source group with one east-west PF whose NetworkInterface
		// is empty — discovery left it blank for multi-node safety.
		ewRail := 0
		groups := []config.ClusterConfig{
			{
				Identifier: "g1",
				PFs: []config.PFConfig{{
					PciAddress: "0000:08:00.0",
					Traffic:    "east-west",
					Rail:       &ewRail,
				}},
			},
		}
		out := maybeDisableInterfaceNameTemplate(cfg, groups)
		assert.True(t, out.NicConfigurationOperator.DeployNicInterfaceNameTemplate)
		assert.Same(t, cfg, out)
	})

	t.Run("rdma_shared with populated names → disabled (single-node case)", func(t *testing.T) {
		cfg := mkCfg("rdma_shared", true, false)
		ewRail := 0
		groups := []config.ClusterConfig{
			{
				Identifier: "g1",
				PFs: []config.PFConfig{{
					PciAddress:       "0000:08:00.0",
					Traffic:          "east-west",
					Rail:             &ewRail,
					NetworkInterface: "ens1f0",
				}},
			},
		}
		out := maybeDisableInterfaceNameTemplate(cfg, groups)
		assert.False(t, out.NicConfigurationOperator.DeployNicInterfaceNameTemplate)
	})

	t.Run("nil NicConfigurationOperator → no-op", func(t *testing.T) {
		cfg := &config.LaunchKubernetesConfig{
			Profile: &config.Profile{Deployment: "sriov"},
		}
		out := maybeDisableInterfaceNameTemplate(cfg, oneGroup)
		assert.Same(t, cfg, out)
	})
}

// Helper function to check if a template contains a placeholder
func containsPlaceholder(template, placeholder string) bool {
	return len(template) > 0 && len(placeholder) > 0 && 
		template != "" && placeholder != "" &&
		len(template) >= len(placeholder) &&
		indexOf(template, placeholder) >= 0
}

// Simple string contains check
func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}


func TestMergeCompatibleGroups_MergesThirdPartyRDMAModules(t *testing.T) {
	rail0 := 0
	rail1 := 1

	t.Run("merges modules from all groups deduplicated and sorted", func(t *testing.T) {
		g1 := labelledGroup("DGX-B200", "NVIDIA-H200",
			[]config.PFConfig{
				{DeviceID: "a2dc", PciAddress: "0000:19:00.0", Traffic: "east-west", Rail: &rail0},
				{DeviceID: "a2dc", PciAddress: "0000:2a:00.0", Traffic: "east-west", Rail: &rail1},
			},
			[]string{"node-1"}, nil)
		g1.ThirdPartyRDMAModules = []string{"iw_cm", "nfsrdma"}
		g2 := labelledGroup("DGX-B200", "NVIDIA-H200",
			[]config.PFConfig{
				{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", Traffic: "east-west", Rail: &rail0},
				{DeviceID: "a2dc", PciAddress: "0000:3c:00.0", Traffic: "east-west", Rail: &rail1},
			},
			[]string{"node-2"}, nil)
		g2.ThirdPartyRDMAModules = []string{"iw_cm", "xprtrdma"}

		merged, _ := mergeCompatibleGroups([]config.ClusterConfig{g1, g2}, false)
		assert.Len(t, merged, 1)
		assert.Equal(t, []string{"iw_cm", "nfsrdma", "xprtrdma"}, merged[0].ThirdPartyRDMAModules)
	})

	t.Run("no modules when source groups have none", func(t *testing.T) {
		groups := []config.ClusterConfig{
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{{DeviceID: "a2dc", PciAddress: "0000:19:00.0", Traffic: "east-west", Rail: &rail0}},
				[]string{"node-1"}, nil),
			labelledGroup("DGX-B200", "NVIDIA-H200",
				[]config.PFConfig{{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", Traffic: "east-west", Rail: &rail0}},
				[]string{"node-2"}, nil),
		}

		merged, _ := mergeCompatibleGroups(groups, false)
		assert.Len(t, merged, 1)
		assert.Nil(t, merged[0].ThirdPartyRDMAModules)
	})

	t.Run("unmerged groups keep their own modules", func(t *testing.T) {
		g1 := labelledGroup("DGX-B200", "NVIDIA-H200",
			[]config.PFConfig{{DeviceID: "a2dc", PciAddress: "0000:19:00.0", Traffic: "east-west", Rail: &rail0}},
			[]string{"node-1"}, nil)
		g1.ThirdPartyRDMAModules = []string{"iw_cm"}
		g2 := labelledGroup("DGX-A100", "NVIDIA-A100",
			[]config.PFConfig{{DeviceID: "1017", PciAddress: "0000:1a:00.0", Traffic: "east-west", Rail: &rail0}},
			[]string{"node-2"}, nil)
		g2.ThirdPartyRDMAModules = []string{"xprtrdma"}

		merged, _ := mergeCompatibleGroups([]config.ClusterConfig{g1, g2}, false)
		assert.Len(t, merged, 2)
		assert.Equal(t, []string{"iw_cm"}, merged[0].ThirdPartyRDMAModules)
		assert.Equal(t, []string{"xprtrdma"}, merged[1].ThirdPartyRDMAModules)
	})
}

func TestParseModuleList(t *testing.T) {
	exclude := []string{"mlx5_core", "mlx5_ib", "ib_umad", "ib_uverbs", "ib_ipoib", "rdma_cm", "rdma_ucm", "ib_core", "ib_cm"}

	t.Run("filters out target modules", func(t *testing.T) {
		output := "iw_cm\nmlx5_core\nib_core\nnfsrdma\n"
		result := parseModuleList(output, exclude)
		assert.Equal(t, []string{"iw_cm", "nfsrdma"}, result)
	})

	t.Run("empty output returns nil", func(t *testing.T) {
		result := parseModuleList("", exclude)
		assert.Nil(t, result)
	})

	t.Run("only target modules returns nil", func(t *testing.T) {
		result := parseModuleList("mlx5_core\nib_core\n", exclude)
		assert.Nil(t, result)
	})

	t.Run("handles whitespace and blank lines", func(t *testing.T) {
		output := "  iw_cm  \n\n  xprtrdma\n  \n"
		result := parseModuleList(output, exclude)
		assert.Equal(t, []string{"iw_cm", "xprtrdma"}, result)
	})

	t.Run("deduplicates", func(t *testing.T) {
		output := "iw_cm\niw_cm\niw_cm\n"
		result := parseModuleList(output, exclude)
		assert.Equal(t, []string{"iw_cm"}, result)
	})
}

func TestNnpName(t *testing.T) {
	t.Run("empty identifier returns l8k", func(t *testing.T) {
		assert.Equal(t, "l8k", nnpName(""))
	})

	t.Run("normal identifier pass-through", func(t *testing.T) {
		assert.Equal(t, "nvidia-h200", nnpName("nvidia-h200"))
	})

	t.Run("truncates to 30 chars", func(t *testing.T) {
		longName := "this-is-a-very-long-identifier-that-exceeds-limit"
		result := nnpName(longName)
		assert.Len(t, result, 30)
		assert.Equal(t, "this-is-a-very-long-identifier", result)
	})

	t.Run("exactly 30 chars is untouched", func(t *testing.T) {
		exact := "abcdefghijklmnopqrstuvwxyz1234"
		assert.Len(t, exact, 30)
		assert.Equal(t, exact, nnpName(exact))
	})

	t.Run("template function matches direct call", func(t *testing.T) {
		nnpNameFunc := templateFuncs["nnpName"].(func(string) string)
		assert.Equal(t, "l8k", nnpNameFunc(""))
		assert.Equal(t, "nvidia-h200", nnpNameFunc("nvidia-h200"))
	})
}

func TestProcessTemplate_NicNodePolicy(t *testing.T) {
	// Create a minimal NNP template in a temp dir
	tmpDir := t.TempDir()

	rail0 := 0
	rail1 := 1

	baseCfg := &config.LaunchKubernetesConfig{
		NetworkOperator: &config.NetworkOperatorConfig{
			Repository:       "nvcr.io/nvidia/mellanox",
			ComponentVersion: "v1.0.0",
		},
		DOCADriver: &config.DOCADriverConfig{
			Enable:  true,
			Version: "24.10",
		},
		Profile: &config.Profile{
			Fabric:     "ethernet",
			Deployment: "host_device",
			Multirail:  false,
		},
		Hostdev: &config.HostdevConfig{
			ResourceName: "rdma_host",
		},
	}

	t.Run("single group renders one NNP file", func(t *testing.T) {
		tmplContent := `apiVersion: mellanox.com/v1alpha1
kind: NicNodePolicy
metadata:
  name: {{.ClusterConfig.Identifier | nnpName}}
spec:
  {{- if .ClusterConfig.NodeSelector }}
  nodeSelector:
    {{- range $k, $v := .ClusterConfig.NodeSelector }}
    {{ $k }}: "{{ $v }}"
    {{- end }}
  {{- end }}
  {{- if .DOCADriver.Enable }}
  ofedDriver:
    image: doca-driver
    version: {{.DOCADriver.Version}}
  {{- end }}`

		tmplPath := tmpDir + "/11-nicnodepolicy.yaml"
		err := os.WriteFile(tmplPath, []byte(tmplContent), 0644)
		assert.NoError(t, err)

		cfg := *baseCfg
		cfg.ClusterConfig = []config.ClusterConfig{
			{
				Identifier: "nvidia-h200",
				NodeSelector: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H200",
				},
				PFs: []config.PFConfig{
					{DeviceID: "a2dc", PciAddress: "0000:19:00.0", Traffic: "east-west", Rail: &rail0},
				},
			},
		}

		results, err := ProcessTemplate(tmplPath, &cfg, "")
		assert.NoError(t, err)
		assert.Len(t, results, 1)

		content, ok := results["11-nicnodepolicy-nvidia-h200.yaml"]
		assert.True(t, ok, "expected file with group suffix")
		assert.Contains(t, content, "kind: NicNodePolicy")
		assert.Contains(t, content, "name: nvidia-h200")
		assert.Contains(t, content, `nvidia.com/gpu.product: "NVIDIA-H200"`)
		assert.Contains(t, content, "version: 24.10")
	})

	t.Run("multiple groups render separate NNP files", func(t *testing.T) {
		tmplContent := `apiVersion: mellanox.com/v1alpha1
kind: NicNodePolicy
metadata:
  name: {{.ClusterConfig.Identifier | nnpName}}
spec:
  {{- if .DOCADriver.Enable }}
  ofedDriver:
    version: {{.DOCADriver.Version}}
  {{- end }}`

		tmplPath := tmpDir + "/11-nnp-multi.yaml"
		err := os.WriteFile(tmplPath, []byte(tmplContent), 0644)
		assert.NoError(t, err)

		cfg := *baseCfg
		cfg.ClusterConfig = []config.ClusterConfig{
			{
				Identifier: "nvidia-h200",
				PFs:        []config.PFConfig{ewPF("0000:19:00.0", 0)},
			},
			{
				Identifier: "nvidia-a100",
				PFs:        []config.PFConfig{ewPF("0000:1a:00.0", 0)},
			},
		}

		results, err := ProcessTemplate(tmplPath, &cfg, "")
		assert.NoError(t, err)
		assert.Len(t, results, 2)

		_, ok1 := results["11-nnp-multi-nvidia-h200.yaml"]
		_, ok2 := results["11-nnp-multi-nvidia-a100.yaml"]
		assert.True(t, ok1)
		assert.True(t, ok2)
		assert.Contains(t, results["11-nnp-multi-nvidia-h200.yaml"], "name: nvidia-h200")
		assert.Contains(t, results["11-nnp-multi-nvidia-a100.yaml"], "name: nvidia-a100")
	})

	t.Run("empty identifier uses l8k default name", func(t *testing.T) {
		tmplContent := `apiVersion: mellanox.com/v1alpha1
kind: NicNodePolicy
metadata:
  name: {{.ClusterConfig.Identifier | nnpName}}`

		tmplPath := tmpDir + "/11-nnp-single.yaml"
		err := os.WriteFile(tmplPath, []byte(tmplContent), 0644)
		assert.NoError(t, err)

		cfg := *baseCfg
		cfg.ClusterConfig = []config.ClusterConfig{
			{
				Identifier: "",
				PFs:        []config.PFConfig{ewPF("0000:19:00.0", 0)},
			},
		}

		// Single group with empty identifier — renders as base filename (no suffix)
		results, err := ProcessTemplate(tmplPath, &cfg, "")
		assert.NoError(t, err)
		assert.Len(t, results, 1)

		content, ok := results["11-nnp-single.yaml"]
		assert.True(t, ok)
		assert.Contains(t, content, "name: l8k")
	})

	t.Run("NNP with nodeSelector renders correctly", func(t *testing.T) {
		tmplContent := `apiVersion: mellanox.com/v1alpha1
kind: NicNodePolicy
metadata:
  name: {{.ClusterConfig.Identifier | nnpName}}
spec:
  {{- if .ClusterConfig.NodeSelector }}
  nodeSelector:
    {{- range $k, $v := .ClusterConfig.NodeSelector }}
    {{ $k }}: "{{ $v }}"
    {{- end }}
  {{- end }}`

		tmplPath := tmpDir + "/11-nnp-selector.yaml"
		err := os.WriteFile(tmplPath, []byte(tmplContent), 0644)
		assert.NoError(t, err)

		cfg := *baseCfg
		cfg.ClusterConfig = []config.ClusterConfig{
			{
				Identifier: "pool-a",
				NodeSelector: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H200",
				},
				PFs: []config.PFConfig{
					{DeviceID: "a2dc", PciAddress: "0000:19:00.0", Traffic: "east-west", Rail: &rail0},
					{DeviceID: "a2dc", PciAddress: "0000:2a:00.0", Traffic: "east-west", Rail: &rail1},
				},
			},
		}

		results, err := ProcessTemplate(tmplPath, &cfg, "")
		assert.NoError(t, err)
		assert.Len(t, results, 1)

		content := results["11-nnp-selector-pool-a.yaml"]
		assert.Contains(t, content, "name: pool-a")
		assert.Contains(t, content, `nvidia.com/gpu.product: "NVIDIA-H200"`)
	})

	t.Run("NNP without nodeSelector omits section", func(t *testing.T) {
		tmplContent := `apiVersion: mellanox.com/v1alpha1
kind: NicNodePolicy
metadata:
  name: {{.ClusterConfig.Identifier | nnpName}}
spec:
  {{- if .ClusterConfig.NodeSelector }}
  nodeSelector:
    {{- range $k, $v := .ClusterConfig.NodeSelector }}
    {{ $k }}: "{{ $v }}"
    {{- end }}
  {{- end }}
  ofedDriver:
    version: {{.DOCADriver.Version}}`

		tmplPath := tmpDir + "/11-nnp-noselector.yaml"
		err := os.WriteFile(tmplPath, []byte(tmplContent), 0644)
		assert.NoError(t, err)

		cfg := *baseCfg
		cfg.ClusterConfig = []config.ClusterConfig{
			{
				Identifier: "single",
				PFs:        []config.PFConfig{ewPF("0000:19:00.0", 0)},
			},
		}

		results, err := ProcessTemplate(tmplPath, &cfg, "")
		assert.NoError(t, err)

		content := results["11-nnp-noselector-single.yaml"]
		assert.NotContains(t, content, "nodeSelector")
		assert.Contains(t, content, "ofedDriver")
	})
}

func TestVersionGE(t *testing.T) {
	cases := []struct {
		have, target string
		want         bool
	}{
		{"", "26.4", true},        // empty = latest, always passes
		{"26.4", "26.4", true},    // equal
		{"26.4", "26.1", true},    // higher minor
		{"26.1", "26.4", false},   // lower minor
		{"v26.4", "26.4", true},   // v-prefix tolerated
		{"26.4", "v26.4", true},   // v-prefix on target
		{"26.4.0", "26.4", true},  // full semver compares to MAJOR.MINOR
		{"26.4.5", "26.4", true},  // patch >= base
		{"27.0", "26.4", true},    // higher major
		{"26.4", "27.0", false},   // lower major
		{"garbage", "26.4", false}, // unparseable have
		{"26.4", "garbage", false}, // unparseable target
	}
	for _, c := range cases {
		got := versionGE(c.have, c.target)
		assert.Equalf(t, c.want, got, "versionGE(%q, %q)", c.have, c.target)
	}
}

func TestVersionLT(t *testing.T) {
	cases := []struct {
		have, target string
		want         bool
	}{
		{"", "26.4", false},      // empty = latest, never less than
		{"26.1", "26.4", true},
		{"26.4", "26.4", false},
		{"26.4", "26.1", false},
		{"v26.1", "26.4", true},
		{"26.4.0", "26.4", false},
		{"26.3.99", "26.4", true},
		{"garbage", "26.4", false},
	}
	for _, c := range cases {
		got := versionLT(c.have, c.target)
		assert.Equalf(t, c.want, got, "versionLT(%q, %q)", c.have, c.target)
	}
}

func TestVersionEQ(t *testing.T) {
	cases := []struct {
		have, target string
		want         bool
	}{
		{"", "26.4", false},      // empty is not equal to anything
		{"26.4", "26.4", true},
		{"v26.4", "26.4", true},
		{"26.4.0", "26.4", true},
		{"26.4.1", "26.4", false},
		{"26.1", "26.4", false},
	}
	for _, c := range cases {
		got := versionEQ(c.have, c.target)
		assert.Equalf(t, c.want, got, "versionEQ(%q, %q)", c.have, c.target)
	}
}

// --- groupFabric tests ---

func TestGroupFabric_Set(t *testing.T) {
	g := config.ClusterConfig{LinkType: "Ethernet"}
	verdict, ok := groupFabric(g)
	assert.True(t, ok)
	assert.Equal(t, "Ethernet", verdict)
}

func TestGroupFabric_Unset(t *testing.T) {
	// When discovery couldn't confirm a fabric, group.LinkType is empty.
	g := config.ClusterConfig{}
	verdict, ok := groupFabric(g)
	assert.False(t, ok)
	assert.Equal(t, "", verdict)
}
