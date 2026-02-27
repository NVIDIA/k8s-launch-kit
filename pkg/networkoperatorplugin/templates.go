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
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
)

// templateFuncs provides helper functions for Go templates
var templateFuncs = template.FuncMap{
	"add": func(a, b int) int { return a + b },
	"sub": func(a, b int) int { return a - b },
	"gt":  func(a, b int) bool { return a > b },
	"untilStep": func(start, stop, step int) []int {
		result := []int{}
		for i := start; i < stop; i += step {
			result = append(result, i)
		}
		return result
	},
	"replaceVars": func(template string, nicID, plane, rail int) string {
		// Replace template variables with actual values
		result := template
		result = strings.ReplaceAll(result, "%nic_id%", fmt.Sprintf("%d", nicID))
		result = strings.ReplaceAll(result, "%plane%", fmt.Sprintf("%d", plane))
		result = strings.ReplaceAll(result, "%rail%", fmt.Sprintf("%d", rail))
		return result
	},
	// suffix prepends a "-" if the string is non-empty, producing a name suffix.
	// e.g., suffix("group-0") → "-group-0", suffix("") → ""
	"suffix": func(s string) string {
		if s == "" {
			return ""
		}
		return "-" + s
	},
}

// templateContext wraps the full config but presents a single ClusterConfig group.
// The ClusterConfig field shadows the slice field in LaunchKubernetesConfig,
// so templates can use .ClusterConfig.PFs, .ClusterConfig.NodeSelector, etc.
type templateContext struct {
	*config.LaunchKubernetesConfig
	ClusterConfig *config.ClusterConfig
}

// ProcessTemplate processes a Go template file with the given config.
// Returns a map of filename → rendered content. Templates that reference
// .ClusterConfig are rendered once per group (producing separate files),
// while other templates are rendered once.
func ProcessTemplate(templatePath string, cfg *config.LaunchKubernetesConfig, groupFilter string) (map[string]string, error) {
	// Read the template file
	templateContent, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read template file %s: %w", templatePath, err)
	}

	// Parse the template with helper functions
	tmpl, err := template.New(filepath.Base(templatePath)).Funcs(templateFuncs).Parse(string(templateContent))
	if err != nil {
		return nil, fmt.Errorf("failed to parse template %s: %w", templatePath, err)
	}

	baseName := filepath.Base(templatePath)
	usesClusterConfig := strings.Contains(string(templateContent), ".ClusterConfig")

	if !usesClusterConfig || len(cfg.ClusterConfig) == 0 {
		// Render once — no per-group variation
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, cfg); err != nil {
			return nil, fmt.Errorf("failed to execute template %s: %w", templatePath, err)
		}
		return map[string]string{baseName: buf.String()}, nil
	}

	// Determine which groups to render
	groups := cfg.ClusterConfig
	singleGroupMode := false
	if groupFilter != "" {
		filtered := []config.ClusterConfig{}
		for _, g := range groups {
			if g.Identifier == groupFilter {
				filtered = append(filtered, g)
			}
		}
		if len(filtered) == 0 {
			return nil, fmt.Errorf("group %q not found in ClusterConfig", groupFilter)
		}
		groups = filtered
		singleGroupMode = true
	}

	// Render once per group → separate file per group
	results := map[string]string{}
	ext := filepath.Ext(baseName)
	nameNoExt := strings.TrimSuffix(baseName, ext)

	// Auto-generate per-group subnets if configured (startingSubnet + mask + offset
	// are set, but the explicit subnets list is empty).
	var groupSubnets [][]config.NvIpamSubnetConfig
	autoGenSubnets := cfg.NvIpam != nil &&
		cfg.NvIpam.StartingSubnet != "" &&
		cfg.NvIpam.Mask > 0 &&
		cfg.NvIpam.Offset > 0 &&
		len(cfg.NvIpam.Subnets) == 0

	if autoGenSubnets {
		multirail := cfg.Profile != nil && cfg.Profile.Multirail

		// Calculate per-group subnet counts
		totalSubnets := 0
		groupCounts := make([]int, len(groups))
		for gi := range groups {
			ewPFs := filterEastWestPFs(groups[gi].PFs)
			if multirail && len(ewPFs) > 0 {
				groupCounts[gi] = len(ewPFs)
			} else {
				groupCounts[gi] = 1
			}
			totalSubnets += groupCounts[gi]
		}

		allSubnets, err := config.GenerateSubnets(
			cfg.NvIpam.StartingSubnet, cfg.NvIpam.Mask, cfg.NvIpam.Offset, totalSubnets)
		if err != nil {
			return nil, fmt.Errorf("failed to auto-generate subnets: %w", err)
		}

		groupSubnets = make([][]config.NvIpamSubnetConfig, len(groups))
		subnetOffset := 0
		for gi := range groups {
			groupSubnets[gi] = allSubnets[subnetOffset : subnetOffset+groupCounts[gi]]
			subnetOffset += groupCounts[gi]
		}
	}

	for i := range groups {
		// When --group is used, clear the identifier so CR names and filenames
		// don't carry the group suffix (only one group is being rendered).
		renderGroup := groups[i]
		if singleGroupMode {
			renderGroup.Identifier = ""
		}
		// Filter out north-south PFs so templates only see east-west devices.
		// This keeps indices sequential for naming (a, b, c, d instead of a, d, e, f).
		renderGroup.PFs = filterEastWestPFs(renderGroup.PFs)

		// When auto-generating subnets, give each group its own unique slice
		// so subnets don't overlap between groups.
		cfgForGroup := cfg
		if autoGenSubnets {
			groupCfg := *cfg
			groupNvIpam := *cfg.NvIpam
			groupNvIpam.Subnets = groupSubnets[i]
			groupCfg.NvIpam = &groupNvIpam
			cfgForGroup = &groupCfg
		}

		ctx := &templateContext{
			LaunchKubernetesConfig: cfgForGroup,
			ClusterConfig:          &renderGroup,
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, ctx); err != nil {
			return nil, fmt.Errorf("failed to execute template %s for group %s: %w", templatePath, groups[i].Identifier, err)
		}
		id := renderGroup.Identifier
		fileName := baseName
		if id != "" {
			fileName = fmt.Sprintf("%s-%s%s", nameNoExt, id, ext)
		}
		results[fileName] = buf.String()
	}

	return results, nil
}

// filterEastWestPFs returns only PFs with traffic == "east-west",
// so that template indices stay sequential for naming (a, b, c, d).
func filterEastWestPFs(pfs []config.PFConfig) []config.PFConfig {
	var filtered []config.PFConfig
	for _, pf := range pfs {
		if pf.Traffic == "east-west" {
			filtered = append(filtered, pf)
		}
	}
	return filtered
}

// GenerateProfileDeploymentFiles processes all template files in a profile directory
func (p *NetworkOperatorPlugin) GenerateProfileDeploymentFiles(profile *profiles.Profile, cfg *config.LaunchKubernetesConfig) (map[string]string, error) {
	results := make(map[string]string)

	for _, templatePath := range profile.Templates {
		rendered, err := ProcessTemplate(templatePath, cfg, p.GroupFilter)
		if err != nil {
			return nil, fmt.Errorf("failed to process template %s: %w", templatePath, err)
		}

		for filename, content := range rendered {
			results[filename] = content
		}
	}

	return results, nil
}
