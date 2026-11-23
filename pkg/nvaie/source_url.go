// SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package nvaie

import (
	"net/url"
	"slices"
	"strings"
)

// matrixSourceURL links to the explorer using a reviewed catalog release and
// observed node facts. An unknown deployment can show a bare-metal candidate
// when a platform or OS filter is available. The caller labels that link as
// an unverified deployment.
func matrixSourceURL(release Release, facts NodeFacts) (sourceURL string, bareMetalCandidate bool) {
	gpus := gpuMatches(release, facts.GPUProduct, facts.MachineType)
	if facts.GPUConflict || len(gpus) != 1 {
		return "", false
	}
	catalog, err := bundledCatalog()
	if err != nil {
		return "", false
	}
	source, err := url.Parse(catalog.Source.MatrixURL)
	if err != nil || source.Scheme != "https" || source.Host != "docs.nvidia.com" || source.User != nil {
		return "", false
	}
	query := url.Values{
		"sm_v": {"1"},
		"view": {"landing"},
		"mode": {"gpu"},
		"gpu":  {gpus[0].Name},
		"grel": {release.Version},
	}
	switch facts.Deployment {
	case BareMetal:
		query.Set("gdep", "bm-k8s")
		addBareMetalFilters(query, release, facts)
	case "":
		if addBareMetalFilters(query, release, facts) {
			query.Set("gdep", "bm-k8s")
			bareMetalCandidate = true
		}
	}
	source.RawQuery = query.Encode()
	return source.String(), bareMetalCandidate
}

func addBareMetalFilters(query url.Values, release Release, facts NodeFacts) bool {
	platformKnown := facts.Platform != "" && slices.ContainsFunc(release.BareMetalK8s, func(row PlatformRow) bool {
		return row.Platform == facts.Platform
	})
	if platformKnown {
		query.Set("gplat", facts.Platform)
		if versionPattern.MatchString(facts.PlatformVersion) {
			query.Set("gpver", minorVersion(facts.PlatformVersion))
		}
	}
	image := facts.OS
	if facts.MatrixOS != "" {
		image = facts.MatrixOS
	}
	family, version := operatingSystem(image)
	if family == "" {
		family = image
	}
	osKnown := family != "" && slices.ContainsFunc(release.BareMetalK8s, func(row PlatformRow) bool {
		rowFamily, _ := operatingSystem(row.OS)
		return rowFamily == family
	})
	if osKnown {
		query.Set("gos", family)
		if minor := minorVersion(version); minor != "" {
			// Explorer filters use the published option, e.g. "24.04 LTS",
			// rather than the host patch version or a shortened "24.04".
			for _, row := range release.BareMetalK8s {
				rowFamily, versions := operatingSystem(row.OS)
				if rowFamily != family || (platformKnown && row.Platform != facts.Platform) {
					continue
				}
				for option := range strings.SplitSeq(versions, ",") {
					option = strings.TrimSpace(option)
					if strings.TrimSuffix(option, " LTS") == minor {
						query.Set("gosver", option)
					} else if strings.Contains(option, " - ") {
						if matched, valid := matchesVersion(minor, option); matched && valid {
							query.Set("gosver", minor)
						}
					}
				}
			}
		}
	}
	return platformKnown || osKnown
}
