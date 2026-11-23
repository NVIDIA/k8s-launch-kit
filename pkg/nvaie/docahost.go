// SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package nvaie

import (
	"cmp"
	"regexp"
	"slices"
)

// HostPackageObservation retains package-database evidence without claiming
// that the package's kernel modules are loaded. Release is derived from Version.
type HostPackageObservation struct {
	Node    string `json:"node"`
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Release string `json:"release,omitempty"`
}

// DOCAHostEvidence supplies existing host-package observations; concurrent
// live driver containers are still checked for conflicts. Fresh is true only in the discovery run
// that collected the packages; validation does not refresh package databases.
type DOCAHostEvidence struct {
	Observations    []HostPackageObservation
	MembershipKnown bool
	Fresh           bool
}

var hostPackageRelease = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)(?:[-+.]|$)`)

// AssessDOCAHost also retains known package evidence in partial reports when
// initialization fails before live Kubernetes observations can run.
func AssessDOCAHost(version string, evidence DOCAHostEvidence) ComponentResult {
	release, ok := Lookup(version)
	if !ok {
		return ComponentResult{Expected: Component{Name: DOCADriver}, Detection: DetectionUnknown,
			Check: check(DOCADriver, Unknown, "NVAIE release unavailable for DOCA-HOST comparison")}
	}
	for _, component := range release.Components {
		if component.Name == DOCADriver {
			return assessDOCAHost(component, evidence)
		}
	}
	return ComponentResult{Expected: Component{Name: DOCADriver}, Detection: DetectionUnknown,
		Check: check(DOCADriver, Unknown, "DOCA product pin unavailable")}
}

func assessDOCAHost(component Component, evidence DOCAHostEvidence) ComponentResult {
	result := ComponentResult{Expected: component, Detection: NotDetected,
		Method:              "DOCA-HOST package database collected by existing discovery",
		Scope:               "Saved discovery snapshot of networking workers; current packages and loaded modules not verified",
		PackageObservations: slices.Clone(evidence.Observations), PackageEvidenceFresh: evidence.Fresh,
		Check: Check{Name: component.Name, Status: Pass, Detail: "Observed DOCA-HOST product releases match the published release pin"},
	}
	if evidence.Fresh {
		result.Scope = "Package database observed during this discovery; loaded modules not verified"
	}
	slices.SortFunc(result.PackageObservations, func(a, b HostPackageObservation) int { return cmp.Compare(a.Node, b.Node) })
	checks := map[string]Check{}
	for i := range result.PackageObservations {
		observation := &result.PackageObservations[i]
		previous, duplicate := checks[observation.Node]
		ambiguous := observation.Node == "" || duplicate
		if ambiguous && result.Check.Status != Fail {
			result.Check.Status, result.Check.Detail = Unknown, "Package evidence has ambiguous worker identities"
		}

		nodeCheck := hostPackageCheck(component, observation)
		if observation.Status == "installed" {
			result.Detection, result.Check.Required = Detected, true
		}
		if observation.Release == "" {
			result.MissingNodes = append(result.MissingNodes, observation.Node)
		}
		if nodeCheck.Status == Fail {
			result.Check.Status, result.Check.Detail = Fail, "At least one observed DOCA-HOST release conflicts with the published release pin"
		}

		switch {
		case previous.Status == Fail:
			nodeCheck = previous
		case ambiguous && nodeCheck.Status != Fail:
			nodeCheck.Status, nodeCheck.Detail = Unknown, "Package evidence has ambiguous worker identities"
		case !evidence.MembershipKnown && nodeCheck.Status == Pass:
			nodeCheck.Status, nodeCheck.Detail = Unknown, "Discovery worker membership is incomplete"
		}
		checks[observation.Node] = nodeCheck
	}
	if result.Check.Status != Fail && (!evidence.MembershipKnown || len(result.PackageObservations) == 0 || len(result.MissingNodes) > 0) {
		result.Check.Status, result.Check.Detail = Unknown, "DOCA-HOST package version evidence does not cover all discovery workers"
	}
	if !evidence.Fresh {
		result.Check.Detail += "; saved discovery snapshot, not a live package check"
	}
	for node, nodeCheck := range checks {
		nodeCheck.Required = result.Check.Required
		if node == "" {
			result.scopeIssue(nodeCheck.Status, nodeCheck.Detail)
			continue
		}
		result.NodeChecks = append(result.NodeChecks, ComponentNodeCheck{Node: node, Check: nodeCheck})
	}
	slices.SortFunc(result.NodeChecks, compareComponentNodes)
	return result
}

func hostPackageCheck(component Component, observation *HostPackageObservation) Check {
	observation.Release = ""
	if observation.Status != "installed" {
		if observation.Reason != "" {
			return check(component.Name, Unknown, observation.Reason)
		}
		if observation.Status == "not-installed" {
			return check(component.Name, Unknown, "DOCA-HOST package was not detected")
		}
		return check(component.Name, Unknown, "DOCA-HOST package evidence is unavailable")
	}
	match := hostPackageRelease.FindStringSubmatch(observation.Version)
	if len(match) != 2 {
		return check(component.Name, Unknown, "DOCA-HOST package release could not be parsed")
	}
	observation.Release = match[1]
	if observation.Release != component.Version {
		return check(component.Name, Fail, "Observed DOCA-HOST release conflicts with the published release pin")
	}
	return check(component.Name, Pass, "Observed DOCA-HOST release matches the published release pin")
}

func (result *ComponentResult) noteUnavailableLiveDOCAEvidence(detail string) {
	if detail == "" {
		return
	}
	result.scopeIssue(Unknown, detail)
	for i := range result.NodeChecks {
		if node := &result.NodeChecks[i]; node.Check.Status == Pass {
			node.Check.Status, node.Check.Detail = Unknown, detail
		}
	}
}

func mergeDOCADelivery(host, live ComponentResult) ComponentResult {
	const reconciliation = "Concurrent DOCA-HOST package and driver-container evidence requires reconciliation; refresh discovery to establish current delivery"
	host.Observations, host.Workloads = live.Observations, live.Workloads
	host.Detection, host.Check.Required = Detected, true
	if live.Check.Status == Fail {
		host.Check.Status, host.Check.Detail = Fail, live.Check.Detail
	} else if host.Check.Status != Fail {
		host.Check.Status, host.Check.Detail = Unknown, reconciliation
	}

	hostChecks := map[string]int{}
	for i, node := range host.NodeChecks {
		hostChecks[node.Node] = i
	}
	packageStatuses := map[string]string{}
	for _, observation := range host.PackageObservations {
		if _, duplicate := packageStatuses[observation.Node]; duplicate {
			packageStatuses[observation.Node] = ""
		} else {
			packageStatuses[observation.Node] = observation.Status
		}
	}
	liveNodes := map[string]bool{}
	for _, observation := range live.Observations {
		liveNodes[observation.Node] = true
	}
	overlap := false
	for _, node := range live.NodeChecks {
		i, exists := hostChecks[node.Node]
		if !exists {
			host.NodeChecks = append(host.NodeChecks, node)
			continue
		}
		if !liveNodes[node.Node] {
			continue
		}
		host.NodeChecks[i].Workloads = node.Workloads
		target := &host.NodeChecks[i].Check
		switch {
		case node.Check.Status == Fail:
			*target = node.Check
		case target.Status == Fail:
			// Keep a known package mismatch even when the image matches.
		case packageStatuses[node.Node] == "not-installed":
			*target = node.Check
		case packageStatuses[node.Node] == "installed":
			overlap = true
			target.Status, target.Detail = Unknown, reconciliation
		}
	}
	if live.ScopeCheck != nil {
		host.scopeIssue(live.ScopeCheck.Status, live.ScopeCheck.Detail)
	}
	if !overlap && host.Check.Status == Unknown {
		host.scopeIssue(Unknown, reconciliation)
	}
	for i := range host.NodeChecks {
		host.NodeChecks[i].Check.Required = true
	}
	if host.ScopeCheck != nil {
		host.ScopeCheck.Required = true
	}
	slices.SortFunc(host.NodeChecks, compareComponentNodes)
	return host
}
