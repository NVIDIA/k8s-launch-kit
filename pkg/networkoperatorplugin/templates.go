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
	"slices"
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
	"joinModules": func(modules []string) string {
		return strings.Join(modules, " ")
	},
	"untilStep": func(start, stop, step int) []int {
		result := []int{}
		for i := start; i < stop; i += step {
			result = append(result, i)
		}
		return result
	},
	"replaceVars": func(template string, nicID, plane, rail int) string {
		// Replace template variables with actual values
		// Supports both old (%rail%) and new (%rail_id%) placeholder formats
		result := template
		result = strings.ReplaceAll(result, "%nic_id%", fmt.Sprintf("%d", nicID))
		result = strings.ReplaceAll(result, "%plane_id%", fmt.Sprintf("%d", plane))
		result = strings.ReplaceAll(result, "%plane%", fmt.Sprintf("%d", plane))
		result = strings.ReplaceAll(result, "%rail_id%", fmt.Sprintf("%d", rail))
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
	// resourceSuffix prepends a "_" and replaces all "-" with "_", producing a
	// suffix suitable for K8s extended resource names (which use underscores).
	// e.g., resourceSuffix("group-0") → "_group_0", resourceSuffix("") → ""
	"resourceSuffix": func(s string) string {
		if s == "" {
			return ""
		}
		return "_" + strings.ReplaceAll(s, "-", "_")
	},
	// pfsPerNic computes how many PFs share the same physical NIC by grouping
	// east-west PFs by PCI bus:device prefix (everything before the last ".").
	// E.g., 8 PFs across 8 NICs → 1; 8 PFs across 4 NICs → 2.
	"pfsPerNic": func(pfs []config.PFConfig) int {
		return pfsPerNic(pfs)
	},
}

// applyPrefix substitutes %nic_id%, %plane%, %rail% placeholders in a prefix template.
func applyPrefix(prefix string, nicID, plane, rail int) string {
	result := prefix
	// Supports both old (%rail%) and new (%rail_id%) placeholder formats
	result = strings.ReplaceAll(result, "%nic_id%", fmt.Sprintf("%d", nicID))
	result = strings.ReplaceAll(result, "%plane_id%", fmt.Sprintf("%d", plane))
	result = strings.ReplaceAll(result, "%plane%", fmt.Sprintf("%d", plane))
	result = strings.ReplaceAll(result, "%rail_id%", fmt.Sprintf("%d", rail))
	result = strings.ReplaceAll(result, "%rail%", fmt.Sprintf("%d", rail))
	return result
}

// pfsPerNic computes PFs-per-NIC by grouping east-west PFs by PCI bus:device prefix.
func pfsPerNic(pfs []config.PFConfig) int {
	ewPFs := filterEastWestPFs(pfs)
	nicDevices := map[string]bool{}
	for _, pf := range ewPFs {
		if idx := strings.LastIndex(pf.PciAddress, "."); idx > 0 {
			nicDevices[pf.PciAddress[:idx]] = true
		}
	}
	if len(nicDevices) == 0 {
		return 1
	}
	return len(ewPFs) / len(nicDevices)
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
		// Skip empty output (e.g., template guarded by a false condition)
		if strings.TrimSpace(buf.String()) == "" {
			return map[string]string{}, nil
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

		// For non-Spectrum-X profiles with NicInterfaceNameTemplate enabled,
		// pre-compute NetworkInterface and RdmaDevice names on each PF.
		// Templates use $pf.NetworkInterface for pfNames selectors.
		if cfg.NicConfigurationOperator != nil &&
			cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate &&
			!isSpectrumX(cfg) {
			for j := range renderGroup.PFs {
				if renderGroup.PFs[j].Rail != nil {
					rail := *renderGroup.PFs[j].Rail
					renderGroup.PFs[j].NetworkInterface = applyPrefix(
						cfg.NicConfigurationOperator.NetdevPrefix, 0, 0, rail)
					renderGroup.PFs[j].RdmaDevice = applyPrefix(
						cfg.NicConfigurationOperator.RdmaPrefix, 0, 0, rail)
				}
			}
		}

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
		// Skip empty output (e.g., template guarded by a false condition)
		if strings.TrimSpace(buf.String()) != "" {
			results[fileName] = buf.String()
		}
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
	// Keep unmerged config for NicInterfaceNameTemplate (needs per-machine-type PCI addresses)
	unmrgCfg := cfg

	// Merge compatible groups when: multiple groups, no --group filter, not spectrum-x
	useNameTemplates := cfg.NicConfigurationOperator != nil &&
		cfg.NicConfigurationOperator.DeployNicInterfaceNameTemplate
	hadPciConflicts := false

	if p.GroupFilter == "" && cfg.Profile != nil &&
		len(cfg.ClusterConfig) > 1 && !isSpectrumX(cfg) {
		mergedCfg := *cfg
		mergedCfg.ClusterConfig, hadPciConflicts = mergeCompatibleGroups(cfg.ClusterConfig, useNameTemplates)
		cfg = &mergedCfg
	}

	// NicInterfaceNameTemplate is only needed when:
	// 1. Groups were merged AND PCI addresses conflict across rails, OR
	// 2. Deployment is rdma_shared AND PFs have empty NetworkInterface names
	//    (rdmaSharedDevicePlugin uses ifNames selectors that require them).
	// When neither condition holds, disable name templates so the device
	// plugin uses PCI addresses directly.
	if useNameTemplates && !isSpectrumX(cfg) {
		needsNameTemplates := hadPciConflicts ||
			(isRdmaShared(cfg) && hasEmptyNetworkInterfaceNames(cfg.ClusterConfig))
		if !needsNameTemplates {
			overrideNicCfg := *cfg.NicConfigurationOperator
			overrideNicCfg.DeployNicInterfaceNameTemplate = false
			overrideCfg := *cfg
			overrideCfg.NicConfigurationOperator = &overrideNicCfg
			cfg = &overrideCfg
			// Also update unmerged config so nicinterfacenametemplate files are skipped.
			unmrgOverride := *unmrgCfg
			unmrgOverride.NicConfigurationOperator = &overrideNicCfg
			unmrgCfg = &unmrgOverride
		}
	}

	results := make(map[string]string)

	for _, templatePath := range profile.Templates {
		// NicInterfaceNameTemplate must be rendered per original group (not merged)
		// because each machine type has different PCI addresses for railPciAddresses.
		renderCfg := cfg
		if strings.Contains(filepath.Base(templatePath), "nicinterfacenametemplate") {
			renderCfg = unmrgCfg
		}
		rendered, err := ProcessTemplate(templatePath, renderCfg, p.GroupFilter)
		if err != nil {
			return nil, fmt.Errorf("failed to process template %s: %w", templatePath, err)
		}

		for filename, content := range rendered {
			results[filename] = content
		}
	}

	return results, nil
}

// hasEmptyNetworkInterfaceNames returns true if any east-west PF across all
// groups has an empty NetworkInterface. This happens when discovery finds
// multiple nodes per group and omits device names for safety. In rdma_shared
// deployments the rdmaSharedDevicePlugin needs interface names (ifNames
// selector), so NicInterfaceNameTemplate must be enabled to provide them.
func hasEmptyNetworkInterfaceNames(groups []config.ClusterConfig) bool {
	for i := range groups {
		for _, pf := range filterEastWestPFs(groups[i].PFs) {
			if pf.NetworkInterface == "" {
				return true
			}
		}
	}
	return false
}

// isRdmaShared returns true if the config targets an rdma_shared deployment.
func isRdmaShared(cfg *config.LaunchKubernetesConfig) bool {
	return cfg.Profile != nil && cfg.Profile.Deployment == "rdma_shared"
}

// isSpectrumX returns true if the config targets a Spectrum-X profile.
func isSpectrumX(cfg *config.LaunchKubernetesConfig) bool {
	return cfg.Profile != nil && cfg.Profile.SpectrumX != nil && cfg.Profile.SpectrumX.Enable
}

// sanitizeIdentifier converts a product type string to a valid K8s name component.
// Lowercases the string and replaces spaces with hyphens.
func sanitizeIdentifier(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// mergeCompatibleGroups merges ClusterConfig groups that share the same productType
// and number of east-west rails into a single group per productType.
// For each merged group:
//   - Identifier = sanitized productType (lowercase, spaces→hyphens)
//   - NodeSelector = {"nvidia.com/gpu.product": productType}
//   - RailPciAddresses = per-rail list of all PCI addresses from source groups
//   - WorkerNodes = union of all worker nodes
//   - Capabilities = aggregated (union)
//
// Groups with empty productType are never merged.
// Single-entry buckets are returned as-is.
func mergeCompatibleGroups(groups []config.ClusterConfig, useNameTemplates bool) ([]config.ClusterConfig, bool) {
	type mergeKey struct {
		productType string
		railCount   int
	}

	// Group indices by (productType, railCount)
	bucketOrder := []mergeKey{}
	buckets := map[mergeKey][]int{}

	for i, g := range groups {
		ewCount := len(filterEastWestPFs(g.PFs))
		key := mergeKey{productType: g.ProductType, railCount: ewCount}
		if g.ProductType == "" {
			// Never merge groups without a productType — use a unique key per group
			key = mergeKey{productType: fmt.Sprintf("__empty_%d", i), railCount: ewCount}
		}
		if _, exists := buckets[key]; !exists {
			bucketOrder = append(bucketOrder, key)
		}
		buckets[key] = append(buckets[key], i)
	}

	var result []config.ClusterConfig
	hadPciConflicts := false
	for _, key := range bucketOrder {
		indices := buckets[key]

		if len(indices) == 1 || groups[indices[0]].ProductType == "" {
			// No merge: single group or empty productType
			result = append(result, groups[indices[0]])
			continue
		}

		// Check for cross-rail PCI address conflicts before merging.
		// When NicInterfaceNameTemplate is enabled, merge despite conflicts
		// (renamed pfNames avoid the conflict) and track that it happened.
		if hasRailPciConflict(groups, indices) {
			if !useNameTemplates {
				for _, idx := range indices {
					result = append(result, groups[idx])
				}
				continue
			}
			hadPciConflicts = true
		}

		// Merge all groups in this bucket
		result = append(result, buildMergedGroup(groups, indices))
	}

	return result, hadPciConflicts
}

// hasRailPciConflict returns true if any PCI address appears at different rail
// positions across the groups identified by indices. This would cause the device
// plugin to claim a device for the wrong rail on some nodes.
func hasRailPciConflict(groups []config.ClusterConfig, indices []int) bool {
	// Map each PCI address to the rail index where it first appears
	addrToRail := map[string]int{}
	for _, idx := range indices {
		for _, pf := range filterEastWestPFs(groups[idx].PFs) {
			if pf.Rail == nil {
				continue
			}
			if prevRail, seen := addrToRail[pf.PciAddress]; seen {
				if prevRail != *pf.Rail {
					return true
				}
			} else {
				addrToRail[pf.PciAddress] = *pf.Rail
			}
		}
	}
	return false
}

// buildMergedGroup creates a single ClusterConfig from multiple groups that share
// the same productType and east-west rail count.
func buildMergedGroup(groups []config.ClusterConfig, indices []int) config.ClusterConfig {
	first := groups[indices[0]]
	productType := first.ProductType

	// Collect east-west PFs per group (all have the same count)
	ewPFsByGroup := make([][]config.PFConfig, len(indices))
	for i, idx := range indices {
		ewPFsByGroup[i] = filterEastWestPFs(groups[idx].PFs)
	}
	railCount := len(ewPFsByGroup[0])

	// Build RailPciAddresses: for each rail, collect PCI addresses from all groups
	railPciAddresses := make([][]string, railCount)
	for rail := 0; rail < railCount; rail++ {
		addrs := make([]string, 0, len(indices))
		for _, pfs := range ewPFsByGroup {
			addrs = append(addrs, pfs[rail].PciAddress)
		}
		railPciAddresses[rail] = addrs
	}

	// Collect all worker nodes
	var allNodes []string
	for _, idx := range indices {
		allNodes = append(allNodes, groups[idx].WorkerNodes...)
	}
	slices.Sort(allNodes)

	// Aggregate capabilities
	caps := &config.ClusterCapabilities{
		Nodes: &config.NodesCapabilities{},
	}
	for _, idx := range indices {
		g := groups[idx]
		if g.Capabilities != nil && g.Capabilities.Nodes != nil {
			caps.Nodes.Sriov = caps.Nodes.Sriov || g.Capabilities.Nodes.Sriov
			caps.Nodes.Rdma = caps.Nodes.Rdma || g.Capabilities.Nodes.Rdma
			caps.Nodes.Ib = caps.Nodes.Ib || g.Capabilities.Nodes.Ib
		}
	}

	// Merge OFED-dependent modules (union across all groups, deduplicated and sorted)
	depModSet := map[string]bool{}
	for _, idx := range indices {
		for _, mod := range groups[idx].OfedDependentModules {
			depModSet[mod] = true
		}
	}
	var mergedDepMods []string
	if len(depModSet) > 0 {
		mergedDepMods = make([]string, 0, len(depModSet))
		for mod := range depModSet {
			mergedDepMods = append(mergedDepMods, mod)
		}
		slices.Sort(mergedDepMods)
	}

	return config.ClusterConfig{
		Identifier:           sanitizeIdentifier(productType),
		ProductType:          productType,
		Capabilities:         caps,
		PFs:                  first.PFs, // Representative PFs from first group
		WorkerNodes:          allNodes,
		OfedDependentModules: mergedDepMods,
		NodeSelector: map[string]string{
			"nvidia.com/gpu.product": productType,
		},
		RailPciAddresses: railPciAddresses,
	}
}
