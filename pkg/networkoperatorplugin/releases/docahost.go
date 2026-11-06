// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package releases

import (
	"fmt"
	"regexp"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

var docaHostReleasePattern = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)(?:[-+.]|$)`)
var docaDriverReleasePattern = regexp.MustCompile(`^doca([0-9]+\.[0-9]+\.[0-9]+)-`)

// DOCAHostIssues compares the discovered DOCA release to the selected Network
// Operator release SBOM, synchronized into the catalog from Mofed.version in
// hack/release.yaml. Host package and OFED/container build suffixes describe
// different builds and cannot be compared as equivalent package coordinates.
// This checks discovery-time evidence; it does not query the current hosts.
func DOCAHostIssues(cfg *config.LaunchKitConfig) []string {
	if cfg == nil {
		return nil
	}
	var installed []config.DOCAHostObservation
	for _, observation := range config.SelectedDOCAHost(cfg) {
		if observation.Status == config.DOCAHostInstalled {
			installed = append(installed, observation)
		}
	}
	if len(installed) == 0 {
		return nil
	}
	if cfg.NetworkOperator == nil || cfg.NetworkOperator.SelectedRelease == "" {
		return []string{"Cannot verify discovered host DOCA-HOST: networkOperator.selectedRelease is required to select the release SBOM"}
	}
	release := cfg.NetworkOperator.SelectedRelease
	entry, found := LookupRelease(release)
	if !found {
		return []string{fmt.Sprintf("Cannot verify discovered host DOCA-HOST: release %q is not in the SBOM-derived catalog", release)}
	}
	expected := docaDriverReleasePattern.FindStringSubmatch(entry.DOCADriver.Version)
	if len(expected) != 2 {
		return []string{fmt.Sprintf("Cannot determine DOCA-HOST release from release %s SBOM driver %q", release, entry.DOCADriver.Version)}
	}
	var issues []string
	for _, observation := range installed {
		actual := docaHostReleasePattern.FindStringSubmatch(observation.Version)
		if len(actual) != 2 {
			issues = append(issues, fmt.Sprintf("Node %s: cannot determine DOCA release from discovered DOCA-HOST package %q", observation.Node, observation.Version))
		} else if actual[1] != expected[1] {
			issues = append(issues, fmt.Sprintf("Node %s: discovered DOCA-HOST %s (DOCA %s) does not match Network Operator %s release SBOM DOCA %s (%s)",
				observation.Node, observation.Version, actual[1], release, expected[1], entry.DOCADriver.Version))
		}
	}
	return issues
}
