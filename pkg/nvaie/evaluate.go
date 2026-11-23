// SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package nvaie

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type Status string

const (
	Pass          Status = "PASS"
	Fail          Status = "FAIL"
	Unknown       Status = "UNKNOWN"
	NotApplicable Status = "NOT_APPLICABLE"
)

type Check struct {
	Name     string `json:"name"`
	Status   Status `json:"status"`
	Detail   string `json:"detail"`
	Required bool   `json:"required"`
}

type Deployment string

const (
	BareMetal Deployment = "bare-metal"
	Simulated Deployment = "simulated"
)

const (
	RKE2      = "SUSE Rancher RKE2"
	OpenShift = "Red Hat OpenShift"
)

// NodeFacts captures the independently observed facts used by the matrix.
type NodeFacts struct {
	Name                  string     `json:"name"`
	Architecture          string     `json:"architecture,omitempty"`
	MachineType           string     `json:"machineType,omitempty"`
	GPUProduct            string     `json:"gpuProduct,omitempty"`
	GPUEvidence           string     `json:"gpuEvidence,omitempty"`
	GPUConflict           bool       `json:"gpuConflict,omitempty"`
	OS                    string     `json:"os,omitempty"`
	MatrixOS              string     `json:"matrixOS,omitempty"`
	KubeletVersion        string     `json:"kubeletVersion,omitempty"`
	Platform              string     `json:"platform,omitempty"`
	PlatformVersion       string     `json:"platformVersion,omitempty"`
	ServerPlatformVersion string     `json:"serverPlatformVersion,omitempty"`
	Runtime               string     `json:"runtime,omitempty"`
	Deployment            Deployment `json:"deployment,omitempty"`
	DriverFamily          string     `json:"driverFamily,omitempty"`
	DriverFamilyConflict  bool       `json:"driverFamilyConflict,omitempty"`
}

type NodeResult struct {
	MatrixGPU          string        `json:"matrixGPU,omitempty"`
	SourceURL          string        `json:"sourceURL,omitempty"`
	BareMetalCandidate bool          `json:"bareMetalCandidate,omitempty"`
	Facts              NodeFacts     `json:"facts"`
	Candidates         []PlatformRow `json:"candidates,omitempty"`
	Checks             []Check       `json:"checks"`
}

var (
	versionPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){1,2}$`)
	gpuSeparator   = regexp.MustCompile(`[ _-]+`)
	osVersion      = regexp.MustCompile(`^([0-9]+\.[0-9]+)(?:\.[0-9]+)?(?:\s|$)`)
)

func normalizedGPU(product string) string {
	return strings.TrimPrefix(gpuSeparator.ReplaceAllString(strings.ToUpper(strings.TrimSpace(product)), " "), "NVIDIA ")
}

// These aliases require both observed labels. A family name alone cannot
// distinguish rack, desktop, SXM, NVL, or server-edition variants.
// Source evidence and the complete preset audit are in engineering/reference/nvaie-catalog.md.
var gpuModelAliases = map[[2]string]string{
	{"GB300", "GB300 NVL"}:                         "GB300 NVL72",
	{"GB300", "DGX GB300 COMPUTE TRAY"}:            "DGX GB300 NVL72",
	{"H200", "POWEREDGE XE9680"}:                   "H200 SXM",
	{"H200", "THINKSYSTEM SR680A V3"}:              "H200 SXM",
	{"H200", "UCSC 885A M8 H22"}:                   "H200 SXM",
	{"RTX PRO 4500 BLACKWELL", "POWEREDGE XE7745"}: "RTX PRO 4500 Blackwell Server Edition",
}

func gpuMatches(release Release, product, machine string) []GPU {
	normalized := normalizedGPU(product)
	if alias := gpuModelAliases[[2]string{normalized, normalizedGPU(machine)}]; alias != "" {
		normalized = normalizedGPU(alias)
	}
	var matches []GPU
	for _, gpu := range release.GPUs {
		if normalizedGPU(gpu.Name) == normalized {
			matches = append(matches, gpu)
		}
	}
	// Inventory often reports a family name that is also a catalog name
	// (e.g. H100). A short name cannot distinguish explicitly listed variants.
	if len(matches) == 1 && !strings.Contains(normalized, " ") {
		for _, gpu := range release.GPUs {
			if strings.HasPrefix(normalizedGPU(gpu.Name), normalized+" ") {
				return nil
			}
		}
	}
	return matches
}

func check(name string, status Status, detail string) Check {
	return Check{Name: name, Status: status, Detail: detail, Required: true}
}

// EvaluateNode evaluates a tuple. A bare-metal fact supplied by a caller is
// only an input to this comparison and does not certify the server.
func EvaluateNode(release Release, facts NodeFacts) NodeResult {
	result := NodeResult{Facts: facts}
	result.SourceURL, result.BareMetalCandidate = matrixSourceURL(release, facts)
	gpus := gpuMatches(release, facts.GPUProduct, facts.MachineType)
	switch {
	case facts.GPUConflict:
		result.Checks = append(result.Checks, check("gpu-model", Unknown, "Fresh GPU observations disagree"))
	case facts.GPUProduct == "":
		result.Checks = append(result.Checks, check("gpu-model", Unknown, "GPU model is unavailable"))
	case len(gpus) != 1:
		result.Checks = append(result.Checks, check("gpu-model", Unknown, "GPU product does not identify a unique catalog model"))
	default:
		result.MatrixGPU = gpus[0].Name
		result.Checks = append(result.Checks, check("gpu-model", Pass, gpus[0].Name))
	}

	switch facts.Deployment {
	case BareMetal:
		result.Checks = append(result.Checks, check("deployment", Pass, "Bare-metal fact supplied"))
	case Simulated:
		result.Checks = append(result.Checks, check("deployment", Fail, "Node is in a simulated or containerized environment"))
	default:
		result.Checks = append(result.Checks, Check{Name: "deployment", Status: Unknown, Detail: "Physical bare-metal deployment requires independent qualification"})
	}

	switch {
	case facts.DriverFamilyConflict:
		result.Checks = append(result.Checks, check("gpu-driver-family", Fail, "Node advertises vGPU and also runs an identified Data Center driver container"))
	case facts.DriverFamily == "vGPU Guest":
		result.Checks = append(result.Checks, check("gpu-driver-family", Unknown, "vGPU Guest is outside the Data Center GPU Driver assessment"))
	}

	status, detail := platformMatch(release, facts, gpus, &result.Candidates)
	result.Checks = append(result.Checks, check("matrix-row", status, detail))
	return result
}

func platformMatch(release Release, facts NodeFacts, gpus []GPU, candidates *[]PlatformRow) (Status, string) {
	missing := facts.Platform == "" || facts.OS == "" || facts.Runtime == "" || facts.PlatformVersion == ""
	if missing {
		return Unknown, "Platform, platform version, OS or container runtime evidence is unavailable"
	}
	matrixOS := facts.OS
	if facts.MatrixOS != "" {
		matrixOS = facts.MatrixOS
	}
	osFamily, osVersion := operatingSystem(matrixOS)
	osMinor := minorVersion(osVersion)
	if osFamily == "" || osMinor == "" || !versionPattern.MatchString(facts.PlatformVersion) {
		return Unknown, "Platform or OS version could not be normalized"
	}
	if facts.Platform == RKE2 {
		if facts.ServerPlatformVersion == "" {
			return Unknown, "The API server version does not establish a matching RKE2 distribution"
		}
		nodeVersion, nodeOK := versionParts(facts.PlatformVersion)
		serverVersion, serverOK := versionParts(facts.ServerPlatformVersion)
		if !nodeOK || !serverOK {
			return Unknown, "RKE2 node or API server version could not be normalized"
		}
		if nodeVersion[0] != serverVersion[0] || serverVersion[1] < nodeVersion[1] || serverVersion[1]-nodeVersion[1] > 3 {
			return Fail, "RKE2 node and API server versions are incompatible"
		}
	}
	var pending, matching bool
	for _, row := range release.BareMetalK8s {
		if row.Platform != facts.Platform || !strings.EqualFold(row.Engine, facts.Runtime) {
			continue
		}
		rowFamily, rowVersions := operatingSystem(row.OS)
		if rowFamily != osFamily {
			continue
		}
		platformOK, platformKnown := matchesVersion(facts.PlatformVersion, row.Version)
		if facts.Platform == RKE2 {
			serverOK, serverKnown := matchesVersion(facts.ServerPlatformVersion, row.Version)
			platformOK, platformKnown = platformOK && serverOK, platformKnown && serverKnown
		}
		osOK, osKnown := matchesVersion(osMinor, rowVersions)
		if !platformKnown || !osKnown {
			pending = true
			continue
		}
		if !platformOK || !osOK {
			continue
		}
		*candidates = append(*candidates, row)
		if len(gpus) != 1 || facts.GPUConflict {
			pending = true
			continue
		}
		if slices.Contains(gpus[0].DeploymentExclusions, "bm-k8s") || row.GPUOperator == "-" || row.NetworkOperator == "-" {
			continue
		}
		if row.GPUOperator != "Supported" || row.NetworkOperator != "Supported" {
			pending = true
			continue
		}
		if gpus[0].Type == "Workstation" && !strings.Contains(strings.ToLower(row.GPUDriver), "vgpu") {
			continue
		}
		if gpus[0].Type == "Workstation" && facts.DriverFamily == "Data Center" {
			continue
		}
		if gpus[0].Type == "Workstation" && facts.DriverFamily == "" {
			pending = true
			continue
		}
		if facts.DriverFamily != "" && !strings.Contains(strings.ToLower(row.GPUDriver), strings.ToLower(facts.DriverFamily)) {
			continue
		}
		if len(row.Footnotes) > 0 || len(row.NetOpFootnotes) > 0 {
			pending = true
			continue
		}
		matching = true
	}
	switch {
	case matching:
		return Pass, "Facts match a published bare-metal Kubernetes row"
	case pending:
		return Unknown, "Candidate matrix row has unresolved GPU, driver, version or footnote conditions"
	default:
		return Fail, "No matrix row supports the GPU Operator and Network Operator for this tuple"
	}
}

func operatingSystem(image string) (string, string) {
	if version, found := strings.CutPrefix(image, "Red Hat Enterprise Linux CoreOS "); found {
		return "Red Hat CoreOS", strings.TrimSpace(version)
	}
	for _, system := range []string{"Red Hat Enterprise Linux", "Red Hat CoreOS", "Ubuntu", "Rocky Linux", "SLES", "Debian GNU/Linux"} {
		if version, found := strings.CutPrefix(image, system+" "); found {
			return system, strings.TrimSpace(version)
		}
	}
	return "", ""
}

func minorVersion(value string) string {
	match := osVersion.FindStringSubmatch(strings.TrimSpace(value))
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func matchesVersion(version, specification string) (matched, valid bool) {
	actual, ok := versionParts(version)
	if !ok {
		return false, false
	}
	for offer := range strings.SplitSeq(specification, ",") {
		offer = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(offer), " LTS"))
		start, end, isRange := strings.Cut(offer, " - ")
		startParts, startOK := versionParts(start)
		if !startOK {
			return false, false
		}
		if !isRange {
			matched = matched || actual == startParts
			continue
		}
		endParts, endOK := versionParts(end)
		if !endOK || compareVersions(startParts, endParts) > 0 {
			return false, false
		}
		matched = matched || (compareVersions(actual, startParts) >= 0 && compareVersions(actual, endParts) <= 0)
	}
	return matched, true
}

func versionParts(version string) (parts [2]int, valid bool) {
	if !versionPattern.MatchString(version) {
		return parts, false
	}
	segments := strings.Split(version, ".")
	for i := range parts {
		value, err := strconv.Atoi(segments[i])
		if err != nil {
			return parts, false
		}
		parts[i] = value
	}
	return parts, true
}

func compareVersions(a, b [2]int) int {
	return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
}

func architectureSupported(component Component, arch string) (Status, string) {
	switch arch {
	case "amd64", "x86_64":
		if component.X86 {
			return Pass, ""
		}
	case "arm64", "aarch64":
		if component.ARM {
			return Pass, ""
		}
	default:
		return Unknown, fmt.Sprintf("Host architecture %q has no catalog mapping", arch)
	}
	return Fail, fmt.Sprintf("%s has no published %s support", component.Name, arch)
}
