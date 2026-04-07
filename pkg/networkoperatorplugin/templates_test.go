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

func TestMergeCompatibleGroups(t *testing.T) {
	t.Run("all groups same productType and rail count", func(t *testing.T) {
		groups := []config.ClusterConfig{
			{
				Identifier:  "group-0",
				ProductType: "NVIDIA-H200",
				PFs: []config.PFConfig{
					ewPF("0000:19:00.0", 0),
					ewPF("0000:2a:00.0", 1),
				},
				WorkerNodes:  []string{"node-b", "node-a"},
				Capabilities: &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Sriov: true, Rdma: true}},
			},
			{
				Identifier:  "group-1",
				ProductType: "NVIDIA-H200",
				PFs: []config.PFConfig{
					ewPF("0000:1a:00.0", 0),
					ewPF("0000:3c:00.0", 1),
				},
				WorkerNodes:  []string{"node-d", "node-c"},
				Capabilities: &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Sriov: true, Rdma: true, Ib: true}},
			},
			{
				Identifier:  "group-2",
				ProductType: "NVIDIA-H200",
				PFs: []config.PFConfig{
					ewPF("0000:09:00.0", 0),
					ewPF("0000:23:00.0", 1),
				},
				WorkerNodes:  []string{"node-e"},
				Capabilities: &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Rdma: true}},
			},
		}

		result, _ := mergeCompatibleGroups(groups, false)

		assert.Len(t, result, 1)
		merged := result[0]
		assert.Equal(t, "nvidia-h200", merged.Identifier)
		assert.Equal(t, "NVIDIA-H200", merged.ProductType)
		assert.Equal(t, map[string]string{"nvidia.com/gpu.product": "NVIDIA-H200"}, merged.NodeSelector)
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

	t.Run("different productTypes no merge", func(t *testing.T) {
		groups := []config.ClusterConfig{
			{
				Identifier:  "group-0",
				ProductType: "NVIDIA-H200",
				PFs:         []config.PFConfig{ewPF("0000:19:00.0", 0)},
				WorkerNodes: []string{"node-a"},
			},
			{
				Identifier:  "group-1",
				ProductType: "NVIDIA-A100",
				PFs:         []config.PFConfig{ewPF("0000:1a:00.0", 0)},
				WorkerNodes: []string{"node-b"},
			},
		}

		result, _ := mergeCompatibleGroups(groups, false)

		assert.Len(t, result, 2)
		assert.Equal(t, "group-0", result[0].Identifier)
		assert.Equal(t, "group-1", result[1].Identifier)
		assert.Nil(t, result[0].RailPciAddresses)
		assert.Nil(t, result[1].RailPciAddresses)
	})

	t.Run("same productType different rail count no merge", func(t *testing.T) {
		groups := []config.ClusterConfig{
			{
				Identifier:  "group-0",
				ProductType: "NVIDIA-H200",
				PFs: []config.PFConfig{
					ewPF("0000:19:00.0", 0),
					ewPF("0000:2a:00.0", 1),
				},
				WorkerNodes: []string{"node-a"},
			},
			{
				Identifier:  "group-1",
				ProductType: "NVIDIA-H200",
				PFs:         []config.PFConfig{ewPF("0000:1a:00.0", 0)},
				WorkerNodes: []string{"node-b"},
			},
		}

		result, _ := mergeCompatibleGroups(groups, false)

		assert.Len(t, result, 2)
		assert.Equal(t, "group-0", result[0].Identifier)
		assert.Equal(t, "group-1", result[1].Identifier)
	})

	t.Run("mixed some mergeable some not", func(t *testing.T) {
		groups := []config.ClusterConfig{
			{
				Identifier:  "group-0",
				ProductType: "NVIDIA-H200",
				PFs:         []config.PFConfig{ewPF("0000:19:00.0", 0)},
				WorkerNodes: []string{"node-a"},
				Capabilities: &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Rdma: true}},
			},
			{
				Identifier:  "group-1",
				ProductType: "NVIDIA-A100",
				PFs:         []config.PFConfig{ewPF("0000:1a:00.0", 0)},
				WorkerNodes: []string{"node-b"},
			},
			{
				Identifier:  "group-2",
				ProductType: "NVIDIA-H200",
				PFs:         []config.PFConfig{ewPF("0000:09:00.0", 0)},
				WorkerNodes: []string{"node-c"},
				Capabilities: &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Rdma: true}},
			},
		}

		result, _ := mergeCompatibleGroups(groups, false)

		assert.Len(t, result, 2)
		// First: merged H200 group (group-0 + group-2)
		assert.Equal(t, "nvidia-h200", result[0].Identifier)
		assert.Equal(t, []string{"node-a", "node-c"}, result[0].WorkerNodes)
		assert.Len(t, result[0].RailPciAddresses, 1)
		assert.Equal(t, []string{"0000:19:00.0", "0000:09:00.0"}, result[0].RailPciAddresses[0])
		// Second: unmerged A100 group
		assert.Equal(t, "group-1", result[1].Identifier)
		assert.Nil(t, result[1].RailPciAddresses)
	})

	t.Run("single group no merge", func(t *testing.T) {
		groups := []config.ClusterConfig{
			{
				Identifier:  "group-0",
				ProductType: "NVIDIA-H200",
				PFs:         []config.PFConfig{ewPF("0000:19:00.0", 0)},
				WorkerNodes: []string{"node-a"},
			},
		}

		result, _ := mergeCompatibleGroups(groups, false)

		assert.Len(t, result, 1)
		assert.Equal(t, "group-0", result[0].Identifier)
		assert.Nil(t, result[0].RailPciAddresses)
	})

	t.Run("empty productType no merge", func(t *testing.T) {
		groups := []config.ClusterConfig{
			{
				Identifier:  "group-0",
				ProductType: "",
				PFs:         []config.PFConfig{ewPF("0000:19:00.0", 0)},
				WorkerNodes: []string{"node-a"},
			},
			{
				Identifier:  "group-1",
				ProductType: "",
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
			{
				Identifier:  "group-0",
				ProductType: "NVIDIA-H200",
				PFs:         []config.PFConfig{ewPF("0000:19:00.0", 0), nsPF},
				WorkerNodes: []string{"node-a"},
				Capabilities: &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Rdma: true}},
			},
			{
				Identifier:  "group-1",
				ProductType: "NVIDIA-H200",
				PFs:         []config.PFConfig{ewPF("0000:1a:00.0", 0), nsPF},
				WorkerNodes: []string{"node-b"},
				Capabilities: &config.ClusterCapabilities{Nodes: &config.NodesCapabilities{Rdma: true}},
			},
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
			{
				Identifier:  "group-0",
				ProductType: "NVIDIA-H200",
				PFs:         []config.PFConfig{ewPF("0000:19:00.0", 0)},
				WorkerNodes: []string{"node-a"},
			},
			{
				Identifier:  "group-1",
				ProductType: "NVIDIA-H200",
				PFs:         []config.PFConfig{ewPF("0000:1a:00.0", 0)},
				WorkerNodes: []string{"node-b"},
			},
		}

		result, hadPciConflicts := mergeCompatibleGroups(groups, true)

		assert.Len(t, result, 1)
		assert.False(t, hadPciConflicts, "no PCI conflicts, name templates not needed")
	})

	t.Run("PCI conflict with name templates merges and reports hadPciConflicts true", func(t *testing.T) {
		groups := []config.ClusterConfig{
			{
				Identifier:  "group-0",
				ProductType: "NVIDIA-H200",
				PFs: []config.PFConfig{
					ewPF("0000:19:00.0", 0),
					ewPF("0000:9c:00.0", 1),
				},
				WorkerNodes: []string{"node-a"},
			},
			{
				Identifier:  "group-1",
				ProductType: "NVIDIA-H200",
				PFs: []config.PFConfig{
					ewPF("0000:9c:00.0", 0), // same PCI at different rail
					ewPF("0000:cd:00.0", 1),
				},
				WorkerNodes: []string{"node-b"},
			},
		}

		result, hadPciConflicts := mergeCompatibleGroups(groups, true)

		// With name templates enabled, should merge despite PCI conflict
		assert.Len(t, result, 1)
		assert.True(t, hadPciConflicts, "PCI conflicts exist, name templates needed")
	})

	t.Run("cross-rail PCI address conflict prevents merge", func(t *testing.T) {
		// Same PCI address 0000:9c:00.0 at rail 4 in group-1 and rail 5 in group-2.
		// Merging would cause the device plugin to claim it for the wrong rail.
		groups := []config.ClusterConfig{
			{
				Identifier:  "group-0",
				ProductType: "NVIDIA-H200",
				PFs: []config.PFConfig{
					ewPF("0000:19:00.0", 0),
					ewPF("0000:9b:00.0", 1),
				},
				WorkerNodes: []string{"node-a"},
			},
			{
				Identifier:  "group-1",
				ProductType: "NVIDIA-H200",
				PFs: []config.PFConfig{
					ewPF("0000:1a:00.0", 0),
					ewPF("0000:9c:00.0", 1), // this address...
				},
				WorkerNodes: []string{"node-b"},
			},
			{
				Identifier:  "group-2",
				ProductType: "NVIDIA-H200",
				PFs: []config.PFConfig{
					ewPF("0000:9c:00.0", 0), // ...appears at a different rail here
					ewPF("0000:cd:00.0", 1),
				},
				WorkerNodes: []string{"node-c"},
			},
		}

		result, _ := mergeCompatibleGroups(groups, false)

		// Should NOT merge: PCI conflict on 0000:9c:00.0 (rail 1 vs rail 0)
		assert.Len(t, result, 3)
		assert.Equal(t, "group-0", result[0].Identifier)
		assert.Equal(t, "group-1", result[1].Identifier)
		assert.Equal(t, "group-2", result[2].Identifier)
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
		groups := []config.ClusterConfig{
			{
				Identifier:  "group-0",
				ProductType: "NVIDIA-H200",
				PFs: []config.PFConfig{
					{DeviceID: "a2dc", PciAddress: "0000:19:00.0", Traffic: "east-west", Rail: &rail0},
					{DeviceID: "a2dc", PciAddress: "0000:2a:00.0", Traffic: "east-west", Rail: &rail1},
				},
				WorkerNodes:          []string{"node-1"},
				ThirdPartyRDMAModules: []string{"iw_cm", "nfsrdma"},
			},
			{
				Identifier:  "group-1",
				ProductType: "NVIDIA-H200",
				PFs: []config.PFConfig{
					{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", Traffic: "east-west", Rail: &rail0},
					{DeviceID: "a2dc", PciAddress: "0000:3c:00.0", Traffic: "east-west", Rail: &rail1},
				},
				WorkerNodes:          []string{"node-2"},
				ThirdPartyRDMAModules: []string{"iw_cm", "xprtrdma"},
			},
		}

		merged, _ := mergeCompatibleGroups(groups, false)
		assert.Len(t, merged, 1)
		assert.Equal(t, []string{"iw_cm", "nfsrdma", "xprtrdma"}, merged[0].ThirdPartyRDMAModules)
	})

	t.Run("no modules when source groups have none", func(t *testing.T) {
		groups := []config.ClusterConfig{
			{
				Identifier:  "group-0",
				ProductType: "NVIDIA-H200",
				PFs: []config.PFConfig{
					{DeviceID: "a2dc", PciAddress: "0000:19:00.0", Traffic: "east-west", Rail: &rail0},
				},
				WorkerNodes: []string{"node-1"},
			},
			{
				Identifier:  "group-1",
				ProductType: "NVIDIA-H200",
				PFs: []config.PFConfig{
					{DeviceID: "a2dc", PciAddress: "0000:1a:00.0", Traffic: "east-west", Rail: &rail0},
				},
				WorkerNodes: []string{"node-2"},
			},
		}

		merged, _ := mergeCompatibleGroups(groups, false)
		assert.Len(t, merged, 1)
		assert.Nil(t, merged[0].ThirdPartyRDMAModules)
	})

	t.Run("unmerged groups keep their own modules", func(t *testing.T) {
		groups := []config.ClusterConfig{
			{
				Identifier:           "group-0",
				ProductType:          "NVIDIA-H200",
				PFs:                  []config.PFConfig{{DeviceID: "a2dc", PciAddress: "0000:19:00.0", Traffic: "east-west", Rail: &rail0}},
				WorkerNodes:          []string{"node-1"},
				ThirdPartyRDMAModules: []string{"iw_cm"},
			},
			{
				Identifier:           "group-1",
				ProductType:          "NVIDIA-A100", // different product
				PFs:                  []config.PFConfig{{DeviceID: "1017", PciAddress: "0000:1a:00.0", Traffic: "east-west", Rail: &rail0}},
				WorkerNodes:          []string{"node-2"},
				ThirdPartyRDMAModules: []string{"xprtrdma"},
			},
		}

		merged, _ := mergeCompatibleGroups(groups, false)
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
