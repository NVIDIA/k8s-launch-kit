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
