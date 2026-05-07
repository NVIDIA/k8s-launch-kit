// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// RenderBucket is the unit of work the rendering pipeline iterates over.
// One bucket per (gpuType, railCount) tuple that survived `--groups` /
// `--gpu-type` filtering.
type RenderBucket struct {
	// Merged is the merged ClusterConfig for this bucket. Used by
	// ScopeBucketed, ScopeAggregate, and ScopeSimpleSelect-in-Mode-A
	// templates. Its `Identifier` and `MergedIdentifier` are equal (the
	// bucket's id). When ModeB is true, `SourceMachineLabels` is
	// populated for ScopeAggregate's `In` selector.
	Merged config.ClusterConfig
	// Sources is the per-source ClusterConfig list for this bucket
	// (post-filter). Each entry's `MergedIdentifier` matches
	// `Merged.MergedIdentifier`, so per-source renders that reference
	// shared resourceName / poolName / etc. all converge on the same
	// stable name. Used by ScopeSimpleSelect-in-Mode-B and
	// ScopePerSource.
	Sources []config.ClusterConfig
	// ModeB is true when the filter selected a strict subset of this
	// bucket's original source groups. ScopeSimpleSelect templates
	// switch from "render once with Merged" to "render N times with
	// Sources" when ModeB is true.
	ModeB bool
	// HadPciConflict mirrors the existing per-bucket flag for
	// NicInterfaceNameTemplate gating.
	HadPciConflict bool
}

// planRender bucketises the filtered source groups by (gpuType,
// railCount), compares each filtered bucket against the original (pre-
// filter) bucket to decide Mode A vs B, and returns one RenderBucket
// per resulting bucket. Buckets and their sources are emitted in the
// same input order as `filteredGroups` for deterministic output.
//
// The second return value is the OR of `HadPciConflict` across all
// buckets — used by the legacy NicInterfaceNameTemplate gating.
func planRender(originalGroups, filteredGroups []config.ClusterConfig, useNameTemplates bool) ([]RenderBucket, bool) {
	originalByKey := bucketize(originalGroups)
	filteredOrder, filteredByKey := bucketizeOrdered(filteredGroups)

	plans := make([]RenderBucket, 0, len(filteredOrder))
	overallHadConflicts := false

	for _, key := range filteredOrder {
		filteredSources := filteredByKey[key]
		originalSources := originalByKey[key]
		modeB := !sourceIdentitySetEqual(filteredSources, originalSources)

		merged, hadConflict := buildBucketMerged(filteredSources, useNameTemplates)
		merged.MergedIdentifier = merged.Identifier
		if modeB {
			// Derive each source's machine label from (machineType,
			// gpuType) — this matches what `l8k discover` writes onto
			// nodes via `applyMachineLabelToGroups`. Reading from
			// `src.NodeSelector[MachineLabelKey]` would miss legacy
			// configs where the differential nodeSelector hasn't been
			// rewritten. When either input is empty,
			// `MachineLabelValue` returns "" and the source contributes
			// no label — empty `SourceMachineLabels` falls back to the
			// merged group's NodeSelector in templates.
			labels := make([]string, 0, len(filteredSources))
			for _, src := range filteredSources {
				if label := config.MachineLabelValue(src.MachineType, src.GPUType); label != "" {
					labels = append(labels, label)
				}
			}
			merged.SourceMachineLabels = labels
		}

		sources := make([]config.ClusterConfig, len(filteredSources))
		for i, src := range filteredSources {
			sources[i] = src
			sources[i].MergedIdentifier = merged.MergedIdentifier
		}

		plans = append(plans, RenderBucket{
			Merged:         merged,
			Sources:        sources,
			ModeB:          modeB,
			HadPciConflict: hadConflict,
		})
		if hadConflict {
			overallHadConflicts = true
		}

		log.Log.V(1).Info("Render plan for bucket",
			"bucket", merged.MergedIdentifier,
			"modeB", modeB,
			"sources", len(filteredSources),
			"sourceMachineLabels", merged.SourceMachineLabels)
	}

	return plans, overallHadConflicts
}

// bucketKey is the merge key — same shape as `mergeCompatibleGroups`.
type bucketKey struct {
	gpuType   string
	railCount int
}

// bucketize maps groups onto (gpuType, railCount) keys. Groups with an
// empty `gpuType` get a unique pseudo-key per index so they never
// merge — same convention as `mergeCompatibleGroups`.
func bucketize(groups []config.ClusterConfig) map[bucketKey][]config.ClusterConfig {
	out := map[bucketKey][]config.ClusterConfig{}
	for i, g := range groups {
		key := keyFor(g, i)
		out[key] = append(out[key], g)
	}
	return out
}

// bucketizeOrdered is like bucketize but also returns the bucket keys in
// first-occurrence order so the renderer's output is deterministic.
func bucketizeOrdered(groups []config.ClusterConfig) ([]bucketKey, map[bucketKey][]config.ClusterConfig) {
	out := map[bucketKey][]config.ClusterConfig{}
	order := []bucketKey{}
	for i, g := range groups {
		key := keyFor(g, i)
		if _, exists := out[key]; !exists {
			order = append(order, key)
		}
		out[key] = append(out[key], g)
	}
	return order, out
}

func keyFor(g config.ClusterConfig, idx int) bucketKey {
	ewCount := len(filterEastWestPFs(g.PFs))
	if g.GPUType == "" {
		return bucketKey{gpuType: fmt.Sprintf("__empty_%d", idx), railCount: ewCount}
	}
	return bucketKey{gpuType: g.GPUType, railCount: ewCount}
}

// sourceIdentitySetEqual returns true when two slices of source groups
// represent the same set of source groups, identified by the
// `Identifier` field. Order doesn't matter.
func sourceIdentitySetEqual(a, b []config.ClusterConfig) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	idsA := make([]string, len(a))
	for i, g := range a {
		idsA[i] = g.Identifier
	}
	idsB := make([]string, len(b))
	for i, g := range b {
		idsB[i] = g.Identifier
	}
	slices.Sort(idsA)
	slices.Sort(idsB)
	return slices.Equal(idsA, idsB)
}

// buildBucketMerged is the same logic as `mergeCompatibleGroups`'s
// per-bucket branch, but always produces a single merged ClusterConfig
// for the bucket regardless of source count. For a single-source
// bucket, the source group itself becomes the "merged" group (with no
// rail-PCI aggregation needed).
func buildBucketMerged(sources []config.ClusterConfig, useNameTemplates bool) (config.ClusterConfig, bool) {
	if len(sources) == 0 {
		return config.ClusterConfig{}, false
	}
	if len(sources) == 1 {
		return sources[0], false
	}

	indices := make([]int, len(sources))
	for i := range sources {
		indices[i] = i
	}
	hadConflict := hasRailPciConflict(sources, indices)
	if hadConflict && !useNameTemplates {
		// PCI conflict + no name-template support → can't merge into a
		// single render group. Caller should treat this bucket as
		// per-source by returning the first source as the merged
		// representative AND signalling the conflict; ScopeSimpleSelect
		// rendering will fan out to per-source via ModeB anyway.
		// We still synthesize a merged record so MergedIdentifier
		// exists — it just won't be used by the ScopeSimple path.
	}
	merged := buildMergedGroup(sources, indices)
	return merged, hadConflict
}

// applyGroupFilter narrows `originalGroups` by `--groups` (identifier
// list) or `--gpu-type` (single value, case-insensitive). At most one
// of the two may be non-empty — the caller already enforces mutual
// exclusivity. An empty filter returns `originalGroups` unchanged.
// Returns an error when the filter matches no group (a typo
// otherwise produces an empty `./deployment/` silently).
func applyGroupFilter(originalGroups []config.ClusterConfig, groupIDs []string, gpuType string) ([]config.ClusterConfig, error) {
	if len(groupIDs) == 0 && gpuType == "" {
		return originalGroups, nil
	}
	if len(groupIDs) > 0 && gpuType != "" {
		// Defence in depth — cobra's MarkFlagsMutuallyExclusive should
		// have caught this earlier.
		return nil, fmt.Errorf("--groups and --gpu-type are mutually exclusive")
	}

	var matched []config.ClusterConfig
	if len(groupIDs) > 0 {
		want := map[string]bool{}
		for _, id := range groupIDs {
			want[id] = true
		}
		seen := map[string]bool{}
		for _, g := range originalGroups {
			if want[g.Identifier] {
				matched = append(matched, g)
				seen[g.Identifier] = true
			}
		}
		var missing []string
		for id := range want {
			if !seen[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			return nil, fmt.Errorf("--groups: no group with identifier(s): %s", strings.Join(missing, ", "))
		}
		return matched, nil
	}

	wantGpu := strings.ToLower(strings.TrimSpace(gpuType))
	for _, g := range originalGroups {
		if strings.ToLower(g.GPUType) == wantGpu {
			matched = append(matched, g)
		}
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("--gpu-type %q: no group matched", gpuType)
	}
	return matched, nil
}

// renderForScope dispatches a single template's rendering across the
// render plans according to the Kind→Scope registry. Per-plan subnets
// (computed by `preallocatePlanSubnets`) are embedded into the per-call
// cfg so ProcessTemplate doesn't auto-allocate from offset 0 each time.
//
// Scopes:
//   - ScopeClusterWide / ScopeUnknown: render once with all merged
//     groups in `cfg.ClusterConfig` (legacy compatible).
//   - ScopeAggregate / ScopeBucketed: render once per plan with that
//     plan's merged group as the sole entry.
//   - ScopeSimpleSelect: Mode A → once per plan with merged; Mode B →
//     once per source group within the plan, each rendering with its
//     own per-source ClusterConfig (shared MergedIdentifier).
//   - ScopePerSource: render once per source group, regardless of mode.
func renderForScope(
	templatePath string,
	cfg *config.LaunchKubernetesConfig,
	plans []RenderBucket,
	planSubnets [][]config.NvIpamSubnetConfig,
) (map[string]string, error) {
	body, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("read template %s: %w", templatePath, err)
	}
	kind, kerr := extractKindFromTemplate(string(body))
	if kerr != nil {
		log.Log.V(1).Info("template kind unrecognized; using legacy render path",
			"template", templatePath, "err", kerr.Error())
	}
	scope := ScopeForKind(kind)

	results := map[string]string{}
	merge := func(rendered map[string]string) {
		for f, c := range rendered {
			results[f] = c
		}
	}

	switch scope {
	case ScopeClusterWide, ScopeUnknown:
		merged := mergedClusterConfigs(plans)
		renderCfg := withClusterConfig(cfg, merged, allSubnets(planSubnets))
		rendered, err := ProcessTemplate(templatePath, renderCfg, "")
		if err != nil {
			return nil, err
		}
		merge(rendered)

	case ScopeAggregate, ScopeBucketed:
		for i, plan := range plans {
			renderCfg := withClusterConfig(cfg, []config.ClusterConfig{plan.Merged}, subnetsAt(planSubnets, i))
			rendered, err := ProcessTemplate(templatePath, renderCfg, "")
			if err != nil {
				return nil, err
			}
			merge(rendered)
		}

	case ScopeSimpleSelect:
		for i, plan := range plans {
			if plan.ModeB {
				for _, src := range plan.Sources {
					renderCfg := withClusterConfig(cfg, []config.ClusterConfig{src}, subnetsAt(planSubnets, i))
					rendered, err := ProcessTemplate(templatePath, renderCfg, "")
					if err != nil {
						return nil, err
					}
					merge(rendered)
				}
			} else {
				renderCfg := withClusterConfig(cfg, []config.ClusterConfig{plan.Merged}, subnetsAt(planSubnets, i))
				rendered, err := ProcessTemplate(templatePath, renderCfg, "")
				if err != nil {
					return nil, err
				}
				merge(rendered)
			}
		}

	case ScopePerSource:
		for i, plan := range plans {
			for _, src := range plan.Sources {
				renderCfg := withClusterConfig(cfg, []config.ClusterConfig{src}, subnetsAt(planSubnets, i))
				rendered, err := ProcessTemplate(templatePath, renderCfg, "")
				if err != nil {
					return nil, err
				}
				merge(rendered)
			}
		}
	}

	return results, nil
}

// withClusterConfig returns a shallow copy of cfg with ClusterConfig
// overridden and (when subnets is non-nil) NvIpam.Subnets replaced.
func withClusterConfig(cfg *config.LaunchKubernetesConfig, groups []config.ClusterConfig, subnets []config.NvIpamSubnetConfig) *config.LaunchKubernetesConfig {
	out := *cfg
	out.ClusterConfig = groups
	if subnets != nil && cfg.NvIpam != nil {
		nv := *cfg.NvIpam
		nv.Subnets = subnets
		out.NvIpam = &nv
	}
	return &out
}

// preallocatePlanSubnets computes per-plan subnet slices when the
// auto-gen criteria are met (StartingSubnet+Mask+Offset set, no
// explicit Subnets). Returns nil to leave cfg.NvIpam untouched in the
// non-auto-gen case.
func preallocatePlanSubnets(cfg *config.LaunchKubernetesConfig, plans []RenderBucket) ([][]config.NvIpamSubnetConfig, error) {
	if cfg.NvIpam == nil ||
		cfg.NvIpam.StartingSubnet == "" ||
		cfg.NvIpam.Mask == 0 ||
		cfg.NvIpam.Offset == 0 ||
		len(cfg.NvIpam.Subnets) > 0 {
		return nil, nil
	}
	multirail := cfg.Profile != nil && cfg.Profile.Multirail
	counts := make([]int, len(plans))
	total := 0
	for i, plan := range plans {
		ewPFs := filterEastWestPFs(plan.Merged.PFs)
		if multirail && len(ewPFs) > 0 {
			counts[i] = len(ewPFs)
		} else {
			counts[i] = 1
		}
		total += counts[i]
	}
	if total == 0 {
		return nil, nil
	}
	all, err := config.GenerateSubnets(cfg.NvIpam.StartingSubnet, cfg.NvIpam.Mask, cfg.NvIpam.Offset, total)
	if err != nil {
		return nil, fmt.Errorf("auto-generate subnets: %w", err)
	}
	out := make([][]config.NvIpamSubnetConfig, len(plans))
	off := 0
	for i, n := range counts {
		out[i] = all[off : off+n]
		off += n
	}
	return out, nil
}

func mergedClusterConfigs(plans []RenderBucket) []config.ClusterConfig {
	out := make([]config.ClusterConfig, len(plans))
	for i, plan := range plans {
		out[i] = plan.Merged
	}
	return out
}

func allSubnets(planSubnets [][]config.NvIpamSubnetConfig) []config.NvIpamSubnetConfig {
	if planSubnets == nil {
		return nil
	}
	var out []config.NvIpamSubnetConfig
	for _, s := range planSubnets {
		out = append(out, s...)
	}
	return out
}

func subnetsAt(planSubnets [][]config.NvIpamSubnetConfig, i int) []config.NvIpamSubnetConfig {
	if planSubnets == nil || i >= len(planSubnets) {
		return nil
	}
	return planSubnets[i]
}

// plansHaveEmptyNetworkInterfaceNames is the per-plan equivalent of
// `hasEmptyNetworkInterfaceNames`: returns true when any east-west PF
// across any plan's source groups has an empty NetworkInterface field.
// Drives the NicInterfaceNameTemplate-required gating in the
// rdma_shared deployment path.
func plansHaveEmptyNetworkInterfaceNames(plans []RenderBucket) bool {
	for _, plan := range plans {
		for _, src := range plan.Sources {
			for _, pf := range filterEastWestPFs(src.PFs) {
				if pf.NetworkInterface == "" {
					return true
				}
			}
		}
	}
	return false
}
