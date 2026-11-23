// SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package nvaie

import (
	"cmp"
	"regexp"
	"slices"
	"strings"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

const (
	nimOperator = "NIM Operator"
	runAI       = "Run:ai"
)

// Detection describes supported installation coverage independently of version checks.
type Detection string

const (
	Detected         Detection = "DETECTED"
	NotDetected      Detection = "NOT_DETECTED"
	DetectionUnknown Detection = "UNKNOWN"
)

type componentDetector struct {
	component, repository, container, kind, method, scope string
	nodeLocal                                             bool
}

// Catalog membership does not enable a detector. These installation forms
// use source-backed workload identities and declared artifact tags. Repository aliases,
// mirrored images and other installers require their own evidence before inclusion.
var componentDetectors = []componentDetector{
	{
		component: NetworkOperator, repository: "nvcr.io/nvidia/cloud-native/network-operator",
		container: "network-operator", kind: "Deployment",
		method: "Official Network Operator Deployment; ready owned container image tag",
		scope:  "Identified Deployments across the cluster",
	},
	{
		component: GPUOperator, repository: "nvcr.io/nvidia/gpu-operator",
		container: "gpu-operator", kind: "Deployment",
		method: "Official GPU Operator Deployment; ready owned container image tag",
		scope:  "Identified Deployments across the cluster",
	},
	{
		component: nimOperator, repository: "nvcr.io/nvidia/cloud-native/k8s-nim-operator",
		container: "manager", kind: "Deployment",
		method: "Official NIM Operator Deployment; ready owned manager container image tag",
		scope:  "Identified operator Deployments; NIM model services are not version evidence",
	},
	{
		component: runAI, repository: "nvcr.io/nvidia/runai-oss/go-operator",
		container: "runai-go-operator", kind: "Deployment",
		method: "Official Run:ai cluster operator Deployment; ready owned container image tag",
		scope:  "Cluster operator artifact; minor release pins include stable hotfixes; control plane and operands not verified",
	},
	{
		component: DOCADriver, repository: "nvcr.io/nvidia/mellanox/doca-driver",
		container: "mofed-container", kind: "DaemonSet", nodeLocal: true,
		method: "Network Operator DOCA driver DaemonSet; ready owned container image tag",
		scope:  "Selected networking workers; declared DOCA product release, not loaded host modules",
	},
	{
		component: GPUDriver, repository: "nvcr.io/nvidia/driver",
		container: "nvidia-driver-ctr", kind: "DaemonSet", nodeLocal: true,
		method: "GPU Operator driver DaemonSet; declared driver container artifact image tag",
		scope:  "Selected GPU workers; driver container artifact only, host installation/binary not verified",
	},
	{
		component: ContainerToolkit, repository: "nvcr.io/nvidia/k8s/container-toolkit",
		container: "nvidia-container-toolkit-ctr", kind: "DaemonSet", nodeLocal: true,
		method: "GPU Operator toolkit DaemonSet; declared toolkit container artifact image tag",
		scope:  "Selected GPU workers; toolkit container artifact only, host installation/binary not verified",
	},
}

func detectorFor(component string) (componentDetector, bool) {
	for _, detector := range componentDetectors {
		if detector.component == component {
			return detector, true
		}
	}
	return componentDetector{}, false
}

type ComponentObservation struct {
	Node            string `json:"node,omitempty"`
	Architecture    string `json:"architecture,omitempty"`
	Pod             string `json:"pod,omitempty"`
	Container       string `json:"container,omitempty"`
	Image           string `json:"image,omitempty"`
	StatusImage     string `json:"statusImage,omitempty"`
	ImageStatus     Status `json:"imageStatus,omitempty"`
	ImageDetail     string `json:"imageDetail,omitempty"`
	Version         string `json:"version,omitempty"`
	StatusVersion   string `json:"statusVersion,omitempty"`
	Source          string `json:"source"`
	Ready           *bool  `json:"ready,omitempty"`
	owner           workloadIdentity
	podUID          types.UID
	normalizedImage string
}

// ComponentNodeCheck records the outcome from evidence located on one worker.
type ComponentNodeCheck struct {
	Node      string                `json:"node"`
	Check     Check                 `json:"check"`
	Workloads []WorkloadObservation `json:"workloads,omitempty"`
}

type ComponentResult struct {
	Expected             Component                `json:"expected"`
	Detection            Detection                `json:"detection"`
	Method               string                   `json:"method"`
	Scope                string                   `json:"scope"`
	MissingNodes         []string                 `json:"missingNodes,omitempty"`
	Observations         []ComponentObservation   `json:"observations,omitempty"`
	Workloads            []WorkloadObservation    `json:"workloads,omitempty"`
	Check                Check                    `json:"check"`
	NodeChecks           []ComponentNodeCheck     `json:"nodeChecks,omitempty"`
	ScopeCheck           *Check                   `json:"scopeCheck,omitempty"` // Unassigned non-passing evidence.
	PackageObservations  []HostPackageObservation `json:"packageObservations,omitempty"`
	PackageEvidenceFresh bool                     `json:"packageEvidenceFresh"`
}

var (
	exactPatchTag = regexp.MustCompile(`^v?([0-9]+\.[0-9]+\.[0-9]+)$`)
	docaTag       = regexp.MustCompile(`^doca([0-9]+\.[0-9]+\.[0-9]+)-[0-9]{2}\.[0-9]{2}-[0-9]+(?:\.[0-9]+)*-[0-9]+(?:-(?:[0-9][a-z0-9._+-]*-)?(?:ubuntu|rhel|rhcos|sles|debian)[0-9]+(?:\.[0-9]+)*-(?:amd64|arm64))?$`)
	osVariantTag  = regexp.MustCompile(`^v?([0-9]+\.[0-9]+\.[0-9]+)(?:-(?:ubuntu|ubi|rhel|rhcos|debian|sles)[0-9][a-z0-9.-]*)?$`)
)

func observePods(pods []corev1.Pod, nodes map[string]*corev1.Node) map[string][]ComponentObservation {
	observations := map[string][]ComponentObservation{}
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		podReady := pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning && slices.ContainsFunc(pod.Status.Conditions, func(c corev1.PodCondition) bool { return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue })
		for _, container := range pod.Spec.Containers {
			component, version, image := identifyImage(container.Image, container.Name)
			status := findContainerStatus(pod.Status.ContainerStatuses, container.Name)
			statusComponent, statusVersion, statusImage := identifyImage(status.Image, container.Name)
			if component == "" && statusComponent == "" {
				continue
			}
			observation := ComponentObservation{
				Node: pod.Spec.NodeName, Pod: pod.Namespace + "/" + pod.Name, Container: container.Name,
				Image: container.Image, Version: version, Source: "Ready controller-owned Pod image tag",
				Ready:       new(podReady && status.Ready && status.State.Running != nil),
				StatusImage: status.Image, StatusVersion: statusVersion,
				owner: controllingWorkload(&pod), podUID: pod.UID, normalizedImage: normalizedImage(image),
			}
			observation.ImageStatus, observation.ImageDetail = comparePodImages(image, statusImage)
			if node := nodes[pod.Spec.NodeName]; node != nil {
				observation.Architecture = node.Status.NodeInfo.Architecture
			}
			if component != "" {
				observations[component] = append(observations[component], observation)
			}
			if statusComponent != "" && statusComponent != component {
				observations[statusComponent] = append(observations[statusComponent], observation)
			}
		}
	}
	return observations
}

func findContainerStatus(statuses []corev1.ContainerStatus, name string) corev1.ContainerStatus {
	for _, status := range statuses {
		if status.Name == name {
			return status
		}
	}
	return corev1.ContainerStatus{}
}

func identifyImage(raw, containerName string) (component, version string, image reference.Named) {
	image, err := reference.ParseNormalizedNamed(raw)
	if err != nil {
		return "", "", nil
	}
	for _, detector := range componentDetectors {
		if reference.TrimNamed(image).String() != detector.repository || containerName != detector.container {
			continue
		}
		component = detector.component
		pattern := exactPatchTag
		if detector.nodeLocal {
			pattern = osVariantTag
		}
		if component == DOCADriver {
			pattern = docaTag
		}
		if tagged, ok := image.(reference.Tagged); ok {
			if match := pattern.FindStringSubmatch(tagged.Tag()); len(match) == 2 {
				version = match[1]
			}
		}

		return component, version, image
	}
	return "", "", image
}

func comparePodImages(specImage, statusImage reference.Named) (Status, string) {
	if specImage == nil {
		return Unknown, "Pod spec image is absent or could not be parsed"
	}
	if hasDigest(specImage) {
		return Unknown, "The Pod spec image includes an unverified digest"
	}
	if statusImage == nil {
		return Unknown, "Container status image is absent or could not be parsed"
	}
	if hasDigest(statusImage) {
		return Unknown, "The container status image includes an unverified digest"
	}
	if normalizedImage(specImage) != normalizedImage(statusImage) {
		return Fail, "The Pod spec and container status image references differ"
	}
	return Pass, ""
}
func normalizedImage(image reference.Named) string {
	if image == nil {
		return ""
	}
	return reference.TagNameOnly(image).String()
}
func hasDigest(image reference.Named) bool {
	_, digested := image.(reference.Digested)
	return digested
}

func nodesWithDataCenterDriver(observations []ComponentObservation) map[string]bool {
	result := map[string]bool{}
	for _, observation := range observations {
		if observation.Ready != nil && *observation.Ready && observation.ImageStatus == Pass && observation.Version != "" {
			result[observation.Node] = true
		}
	}
	return result
}

func assessComponents(release Release, observations map[string][]ComponentObservation, workloads map[string][]WorkloadObservation, nodes []NodeResult, podErr error, workloadFailures []Check, docaHost *DOCAHostEvidence) []ComponentResult {
	results := make([]ComponentResult, 0, len(release.Components))
	for _, component := range release.Components {
		detector, supported := detectorFor(component.Name)
		if !supported {
			continue
		}
		componentPods, componentWorkloads := observations[component.Name], workloads[component.Name]
		liveEvidence := len(componentPods) > 0 || len(componentWorkloads) > 0
		hasHostEvidence := component.Name == DOCADriver && docaHost != nil
		readDetail := componentReadDetail(detector, podErr, workloadFailures)
		if hasHostEvidence && !liveEvidence {
			result := assessDOCAHost(component, *docaHost)
			result.noteUnavailableLiveDOCAEvidence(readDetail)
			results = append(results, result)
			continue
		}

		result := ComponentResult{
			Expected: component, Method: detector.method, Scope: detector.scope,
			Detection: NotDetected, Observations: componentPods, Workloads: componentWorkloads,
		}
		slices.SortFunc(result.Observations, compareObservations)
		slices.SortFunc(result.Workloads, func(a, b WorkloadObservation) int {
			return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Image, b.Image))
		})
		if liveEvidence {
			result.Detection = Detected
		}
		if readDetail != "" {
			result.Detection = DetectionUnknown
		}
		result.Check, result.MissingNodes = componentCheck(component, detector, result.Observations, result.Workloads, nodes, readDetail)
		if detector.nodeLocal {
			assessComponentNodes(&result, detector, nodes, readDetail)
		}
		if hasHostEvidence {
			result = mergeDOCADelivery(assessDOCAHost(component, *docaHost), result)
			result.noteUnavailableLiveDOCAEvidence(readDetail)
		}
		if detector.nodeLocal {
			result.retainUnassignedCheck()
		}
		results = append(results, result)
	}
	return results
}

func componentReadDetail(detector componentDetector, podErr error, workloadFailures []Check) string {
	detail := ""
	if podErr != nil {
		detail = "Pod evidence unavailable"
	}
	for _, failure := range workloadFailures {
		if (failure.Name == "deployments" && detector.kind == "Deployment") || (failure.Name == "daemonsets" && detector.kind == "DaemonSet") || (failure.Name == "replicasets" && detector.kind == "Deployment") {
			detail = failure.Detail
		}
	}
	return detail
}

func compareObservations(a, b ComponentObservation) int {
	return cmp.Or(cmp.Compare(a.Node, b.Node), cmp.Compare(a.Pod, b.Pod), cmp.Compare(a.Container, b.Container))
}

func componentCheck(component Component, detector componentDetector, observations []ComponentObservation, workloads []WorkloadObservation, nodes []NodeResult, readDetail string) (Check, []string) {
	detected := len(observations) > 0 || len(workloads) > 0
	result := Check{Name: component.Name, Required: component.Name == NetworkOperator || detected, Status: Unknown, Detail: "NOT_DETECTED: no supported installation form observed; host or alternative installations remain unchecked"}

	if !detected {
		if readDetail != "" {
			result.Detail = readDetail
			return result, nil
		}
		if result.Required {
			result.Detail = "Required Network Operator installation/version evidence unavailable"
		}
		return result, nil
	}
	result.Status, result.Detail = Pass, "Performed version checks match the published release pin; coverage is limited to the stated method"
	if readDetail != "" {
		result.Status, result.Detail = Unknown, readDetail
	}
	if len(observations) == 0 {
		result.Status, result.Detail = Unknown, "No eligible ready version observations in the assessment scope"
	}
	coveredNodes := map[string]bool{}
	for _, observation := range observations {
		archStatus, archDetail := architectureSupported(component, observation.Architecture)
		switch {
		case observation.ImageStatus == Fail:
			result.Status, result.Detail = Fail, observation.ImageDetail
		case observation.ImageStatus == Pass && observation.Version != "" && !componentVersionMatches(component, observation.Version):
			result.Status, result.Detail = Fail, "At least one observed version conflicts with the published release pin"
		case archStatus == Fail:
			result.Status, result.Detail = Fail, archDetail
		case result.Status != Fail && observation.ImageStatus == Unknown:
			result.Status, result.Detail = Unknown, observation.ImageDetail
		case result.Status != Fail && (observation.Version == "" || observation.Ready == nil || !*observation.Ready || archStatus == Unknown):
			result.Status, result.Detail = Unknown, "At least one identified instance lacks ready version or architecture evidence"
		}
		if observation.Version != "" && observation.ImageStatus == Pass && observation.Ready != nil && *observation.Ready && archStatus == Pass {
			coveredNodes[observation.Node] = true
		}
	}
	for _, workload := range workloads {
		status, detail := componentWorkloadCheck(component, detector, workload)
		if status == Fail || (status == Unknown && result.Status != Fail) {
			result.Status, result.Detail = status, detail
		}
	}
	if result.Status == Pass {
		for _, node := range nodes {
			archStatus, archDetail := architectureSupported(component, node.Facts.Architecture)
			if archStatus != Pass {
				result.Status, result.Detail = archStatus, archDetail
				break
			}
		}
	}

	var missing []string
	if detector.nodeLocal {
		for _, node := range nodes {
			if (component.Name == DOCADriver || node.Facts.GPUProduct != "") && !coveredNodes[node.Facts.Name] {
				missing = append(missing, node.Facts.Name)
			}
		}
		if len(missing) > 0 && result.Status != Fail {
			result.Status, result.Detail = Unknown, "Missing declared artifact version evidence on one or more selected workers"
		}
	}
	if component.Name == GPUDriver {
		for _, node := range nodes {
			if node.Facts.DriverFamilyConflict {
				result.Status, result.Detail = Fail, "At least one Node has conflicting vGPU and Data Center driver evidence"
			}
		}
	}
	return result, missing
}

func componentWorkloadCheck(component Component, detector componentDetector, workload WorkloadObservation) (Status, string) {
	switch {
	case workload.ImageStatus == Pass && workload.Version != "" && !componentVersionMatches(component, workload.Version):
		return Fail, "At least one workload template declares a different version"
	case workload.ImageStatus != Pass || (!detector.nodeLocal && !workload.Complete):
		return Unknown, "At least one identified workload lacks complete rollout or image evidence"
	case !detector.nodeLocal && workload.OwnedReadyPods != workload.Desired:
		return Unknown, "Current owned, ready Pods do not cover the identified workload's desired replicas"
	default:
		return Pass, ""
	}
}

func assessComponentNodes(result *ComponentResult, detector componentDetector, nodes []NodeResult, readDetail string) {
	observations := map[string][]ComponentObservation{}
	for _, observation := range result.Observations {
		observations[observation.Node] = append(observations[observation.Node], observation)
	}
	workloads := map[workloadIdentity][]WorkloadObservation{}
	for _, workload := range result.Workloads {
		workloads[workload.identity] = append(workloads[workload.identity], workload)
	}
	assigned := map[workloadIdentity]bool{}
	if readDetail != "" {
		result.scopeIssue(Unknown, readDetail)
	}
	for _, node := range nodes {
		name := node.Facts.Name
		local := observations[name]
		if name == "" || (detector.component != DOCADriver && node.Facts.GPUProduct == "" && len(local) == 0) {
			continue
		}
		var owned []WorkloadObservation
		owners := map[workloadIdentity]bool{}
		// The observation pipeline has already verified each Pod's controller UID.
		for _, observation := range local {
			owner := observation.owner
			if owner.uid != "" && !owners[owner] {
				owned = append(owned, workloads[owner]...)
				owners[owner], assigned[owner] = true, true
			}
		}
		detail := readDetail
		if len(local) == 0 && result.Check.Required && detail == "" {
			detail = "Missing declared artifact version evidence on the selected worker"
		}
		check, _ := componentCheck(result.Expected, detector, local, owned, []NodeResult{node}, detail)
		check.Required = result.Check.Required
		result.NodeChecks = append(result.NodeChecks, ComponentNodeCheck{Node: name, Check: check, Workloads: owned})
	}
	slices.SortFunc(result.NodeChecks, compareComponentNodes)
	for _, workload := range result.Workloads {
		if !assigned[workload.identity] {
			status, detail := componentWorkloadCheck(result.Expected, detector, workload)
			if status != Pass {
				result.scopeIssue(status, detail)
			}
		}
	}
}

func compareComponentNodes(a, b ComponentNodeCheck) int {
	return cmp.Compare(a.Node, b.Node)
}

func (result *ComponentResult) scopeIssue(status Status, detail string) {
	if status != Fail && status != Unknown {
		return
	}
	if result.ScopeCheck == nil || (status == Fail && result.ScopeCheck.Status != Fail) {
		result.ScopeCheck = &Check{Name: result.Expected.Name, Status: status, Detail: detail, Required: result.Check.Required}
	}
}

// Preserve aggregate outcomes that were not attributable to individual nodes.
func (result *ComponentResult) retainUnassignedCheck() {
	if len(result.NodeChecks) == 0 || result.Check.Status == Pass {
		return
	}
	if !slices.ContainsFunc(result.NodeChecks, func(node ComponentNodeCheck) bool {
		return node.Check.Status == result.Check.Status
	}) {
		result.scopeIssue(result.Check.Status, result.Check.Detail)
	}
}

// NVIDIA's Run:ai pin names a minor release whose official hotfixes have
// three-part versions. Other component pins, including exact Run:ai patches,
// remain exact. The image parser excludes prereleases and unrecognized tags.
func componentVersionMatches(component Component, observed string) bool {
	if component.Name == runAI && len(strings.Split(component.Version, ".")) == 2 {
		return exactPatchTag.MatchString(observed) && strings.HasPrefix(observed, component.Version+".")
	}
	return observed == component.Version
}

// Scope node-local artifacts to the selected workers. Their absence does not
// establish anything about packages installed directly on the host.
func scopeNodeLocalObservations(observations map[string][]ComponentObservation, workloads map[string][]WorkloadObservation, workers []string, nodes map[string]*corev1.Node) {
	selected := map[string]bool{}
	for _, worker := range workers {
		selected[worker] = true
	}
	for _, detector := range componentDetectors {
		if !detector.nodeLocal {
			continue
		}
		component := detector.component
		placedOwners, unplacedOwners := map[workloadIdentity]bool{}, map[workloadIdentity]bool{}
		for _, observation := range observations[component] {
			if observation.Node == "" {
				unplacedOwners[observation.owner] = true
			} else {
				placedOwners[observation.owner] = true
			}
		}
		observations[component] = slices.DeleteFunc(observations[component], func(observation ComponentObservation) bool { return !selected[observation.Node] })
		owners := map[workloadIdentity]bool{}
		for i := range observations[component] {
			owners[observations[component][i].owner] = true
			observations[component][i].Source = "Declared container artifact image tag; host installation/binary not verified"
		}
		kept := workloads[component][:0]
		for _, workload := range workloads[component] {
			if owners[workload.identity] {
				kept = append(kept, workload)
				continue
			}
			if (!placedOwners[workload.identity] || unplacedOwners[workload.identity]) && mayTargetSelectedNodes(workload, workers, nodes) {
				// Without a scheduled Pod, selectors are only prospective scope.
				// Retain incomplete intent without treating its tag as live evidence.
				workload.ImageStatus = Unknown
				kept = append(kept, workload)
			}
		}
		workloads[component] = kept
	}
}

// Exclude only definite node-name/selector mismatches. Affinity and scheduling
// are not reconstructed here: an unscheduled controller remains UNKNOWN.
func mayTargetSelectedNodes(workload WorkloadObservation, workers []string, nodes map[string]*corev1.Node) bool {
	for _, name := range workers {
		if workload.nodeName != "" && workload.nodeName != name {
			continue
		}
		node := nodes[name]
		if node == nil || labels.SelectorFromSet(workload.nodeSelector).Matches(labels.Set(node.Labels)) {
			return true
		}
	}
	return false
}
