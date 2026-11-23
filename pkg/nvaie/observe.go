// SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package nvaie

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
)

const (
	gpuProductLabel    = "nvidia.com/gpu.product"
	gpuDriverLabel     = "nvidia.com/cuda.driver-version.full"
	observationTimeout = 30 * time.Second
)

var rke2Version = regexp.MustCompile(`^v([0-9]+\.[0-9]+)\.[0-9]+\+rke2r[0-9]+$`)

type Assessment struct {
	Release       string            `json:"release"`
	ServerVersion string            `json:"serverVersion,omitempty"`
	Source        Source            `json:"source"`
	Nodes         []NodeResult      `json:"nodes"`
	Components    []ComponentResult `json:"components"`
	Checks        []Check           `json:"checks"`
	Advisories    []string          `json:"advisories"`
}

// Passed returns whether all required checks passed.
func (a *Assessment) Passed() bool {
	if a == nil || len(a.Checks) == 0 || len(a.Nodes) == 0 || len(a.Components) == 0 {
		return false
	}
	if !checksPassed(a.Checks) {
		return false
	}
	for _, node := range a.Nodes {
		if !checksPassed(node.Checks) {
			return false
		}
	}
	for _, component := range a.Components {
		if !checksPassed([]Check{component.Check}) {
			return false
		}
	}
	return true
}

func checksPassed(checks []Check) bool {
	for _, c := range checks {
		if c.Required && c.Status != Pass && c.Status != NotApplicable {
			return false
		}
	}
	return true
}

func (a Assessment) MarshalJSON() ([]byte, error) {
	type payload Assessment
	return json.Marshal(struct {
		payload
		Passed bool `json:"passed"`
	}{payload(a), a.Passed()})
}

// Assess reads Nodes and Pods to compare fresh observations with a catalog
// release. freshGPUProducts is for products observed during this same discovery
// run. The reader never assumes a Node with no virtualization evidence is
// physically bare metal.
func Assess(ctx context.Context, client kubernetes.Interface, version string, workerNames []string, freshGPUProducts map[string]string, docaHost *DOCAHostEvidence) *Assessment {
	ctx, cancel := context.WithTimeout(ctx, observationTimeout)
	defer cancel()
	catalog, catalogErr := bundledCatalog()
	a := &Assessment{
		Release: version,
		Source:  catalog.Source,
		Advisories: []string{
			"Matrix rows describe published configurations. Verify physical deployment, NVIDIA-certified or qualified hardware, networking hardware, vendor constraints and product-specific platform support separately.",
			"Only components with supported version checks are reported. NOT_DETECTED does not establish absence of alternative installations.",
			"Container artifact tags or existing DOCA-HOST package observations are compared. Saved package observations remain historical; runtime binaries and artifact digests are not verified.",
		},
	}
	if catalogErr != nil {
		a.Checks = append(a.Checks, check("catalog", Unknown, catalogErr.Error()))
		return a
	}
	release, supported := Lookup(version)
	if !supported {
		a.Checks = append(a.Checks, check("catalog", Unknown, "Exact release unavailable in the bundled NVIDIA AI Enterprise catalog"))
		return a
	}
	a.Checks = append(a.Checks, check("catalog", Pass, "Exact release found in the bundled documentation snapshot"))
	if client == nil {
		a.Checks = append(a.Checks, check("cluster-observation", Unknown, "Kubernetes client unavailable"))
		return a
	}
	if err := ctx.Err(); err != nil {
		a.Checks = append(a.Checks, check("cluster-observation", Unknown, err.Error()))
		return a
	}
	nodes, nodeErr := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	pods, podErr := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	serverVersion, serverErr := observeServerVersion(ctx, client)
	a.ServerVersion = serverVersion
	if serverErr != nil {
		a.Checks = append(a.Checks, check("api-server-version", Unknown, serverErr.Error()))
	}
	if err := ctx.Err(); err != nil {
		a.Checks = append(a.Checks, check("cluster-observation", Unknown, err.Error()))
	}
	if nodeErr != nil {
		a.Checks = append(a.Checks, check("nodes", Unknown, fmt.Sprintf("List Nodes: %v", nodeErr)))
	}
	if podErr != nil {
		a.Checks = append(a.Checks, check("pods", Unknown, fmt.Sprintf("List Pods: %v", podErr)))
	}

	inventory := map[string]*corev1.Node{}
	if nodeErr == nil && nodes != nil {
		for i := range nodes.Items {
			node := &nodes.Items[i]
			inventory[node.Name] = node
		}
	}
	workers := selectWorkers(inventory, workerNames, freshGPUProducts)
	if len(workers) == 0 {
		a.Checks = append(a.Checks, check("nodes", Unknown, "No eligible worker Nodes were observed"))
	}
	observations := map[string][]ComponentObservation{}
	if podErr == nil && pods != nil {
		observations = observePods(pods.Items, inventory)
	}
	workloads, workloadFailures := observeWorkloads(ctx, client, observations)
	a.Checks = append(a.Checks, workloadFailures...)
	scopeNodeLocalObservations(observations, workloads, workers, inventory)
	driverNodes := nodesWithDataCenterDriver(observations[GPUDriver])
	platformVersion, platformErr := observeOpenShift(ctx, client, inventory, workers)
	if platformErr != nil {
		a.Checks = append(a.Checks, check("openshift-version", Unknown, platformErr.Error()))
	}
	rke2 := false
	for _, name := range workers {
		facts := NodeFacts{Name: name}
		if node := inventory[name]; node != nil {
			facts = observeNode(node, freshGPUProducts[name], serverVersion, platformVersion, driverNodes[name])
		}
		rke2 = rke2 || facts.Platform == RKE2
		a.Nodes = append(a.Nodes, EvaluateNode(release, facts))
	}
	if rke2 {
		a.Advisories = append(a.Advisories, "NVIDIA AI Enterprise 9.0 and Network Operator 26.7.1 publish differing Rancher RKE2 OS/platform rows. Verify product-specific support against https://docs.nvidia.com/networking/display/kubernetes2671/platform-support.html")
	}
	a.Components = assessComponents(release, observations, workloads, a.Nodes, podErr, workloadFailures, docaHost)
	return a
}

func selectWorkers(inventory map[string]*corev1.Node, names []string, freshGPUProducts map[string]string) []string {
	workers := append([]string(nil), names...)
	if len(names) == 0 {
		for _, node := range inventory {
			_, controlPlane := node.Labels["node-role.kubernetes.io/control-plane"]
			_, master := node.Labels["node-role.kubernetes.io/master"]
			if (!controlPlane && !master) || hasGPU(node, freshGPUProducts[node.Name]) {
				workers = append(workers, node.Name)
			}
		}
	}
	slices.Sort(workers)
	return slices.Compact(workers)
}

func hasGPU(node *corev1.Node, freshGPU string) bool {
	if node.Labels["nvidia.com/gpu.present"] == "true" || freshGPU != "" || node.Labels[gpuProductLabel] != "" {
		return true
	}
	for resourceName, quantity := range node.Status.Capacity {
		name := string(resourceName)
		if (name == "nvidia.com/gpu" || strings.HasPrefix(name, "nvidia.com/mig-")) && quantity.Sign() > 0 {
			return true
		}
	}
	return false
}

func observeNode(node *corev1.Node, freshGPU, serverVersion, openShiftVersion string, driverContainer bool) NodeFacts {
	info := node.Status.NodeInfo
	facts := NodeFacts{
		Name: node.Name, Architecture: info.Architecture, OS: info.OSImage, KubeletVersion: info.KubeletVersion,
		GPUProduct:  node.Labels[gpuProductLabel],
		MachineType: node.Labels["nvidia.com/gpu.machine"],
	}
	if facts.GPUProduct != "" {
		facts.GPUEvidence = "Node label " + gpuProductLabel
	}
	if freshGPU != "" {
		facts.GPUConflict = facts.GPUProduct != "" && normalizedGPU(freshGPU) != normalizedGPU(facts.GPUProduct)
		facts.GPUProduct = freshGPU
		facts.GPUEvidence = "Current discovery GPU observation"
	}
	switch runtime, _, _ := strings.Cut(info.ContainerRuntimeVersion, "://"); runtime {
	case "containerd":
		facts.Runtime = "Containerd"
	case "cri-o":
		facts.Runtime = "CRI-O"
	default:
		facts.Runtime = runtime
	}
	if match := rke2Version.FindStringSubmatch(info.KubeletVersion); len(match) == 2 {
		facts.Platform, facts.PlatformVersion = RKE2, match[1]
		if serverMatch := rke2Version.FindStringSubmatch(serverVersion); len(serverMatch) == 2 {
			facts.ServerPlatformVersion = serverMatch[1]
		}
	} else if family, _ := operatingSystem(info.OSImage); family == "Red Hat CoreOS" || openShiftVersion != "" {
		facts.Platform, facts.PlatformVersion = OpenShift, openShiftVersion
		if family == "Red Hat CoreOS" && openShiftVersion != "" {
			facts.MatrixOS = "Red Hat CoreOS " + openShiftVersion
		}
	}
	if strings.HasPrefix(node.Spec.ProviderID, "kind://") || strings.HasPrefix(node.Spec.ProviderID, "docker://") ||
		node.Labels["feature.node.kubernetes.io/system-hypervisor"] == "true" {
		facts.Deployment = Simulated
	}
	if node.Labels["nvidia.com/vgpu.present"] == "true" {
		facts.DriverFamily = "vGPU Guest"
		facts.DriverFamilyConflict = driverContainer
	} else if driverContainer {
		facts.DriverFamily = "Data Center"
	}
	return facts
}

// Observe the version through the authenticated Kubernetes discovery client.
func observeServerVersion(ctx context.Context, client kubernetes.Interface) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	api, ok := client.Discovery().(discovery.ServerVersionInterfaceWithContext)
	if !ok {
		return "", fmt.Errorf("context-aware API server version observation is unavailable")
	}
	info, err := api.ServerVersionWithContext(ctx)
	if err != nil {
		return "", fmt.Errorf("read API server version: %w", err)
	}
	if info == nil || info.GitVersion == "" {
		return "", fmt.Errorf("API server GitVersion is unavailable")
	}
	return info.GitVersion, nil
}

// The Kubernetes discovery REST client shares the kubeconfig and transport.
// Call the ClusterVersion API only when the OS gives a strong OpenShift cue.
func observeOpenShift(ctx context.Context, client kubernetes.Interface, inventory map[string]*corev1.Node, names []string) (string, error) {
	for _, name := range names {
		node := inventory[name]
		if node == nil {
			continue
		}
		if family, _ := operatingSystem(node.Status.NodeInfo.OSImage); family != "Red Hat CoreOS" {
			continue
		}
		api := client.Discovery().RESTClient()
		if api == nil {
			return "", fmt.Errorf("OpenShift ClusterVersion API unavailable")
		}
		data, err := api.Get().AbsPath("/apis/config.openshift.io/v1/clusterversions/version").Do(ctx).Raw()
		if err != nil {
			return "", fmt.Errorf("read OpenShift ClusterVersion: %w", err)
		}
		var cv struct {
			Status struct {
				History []struct {
					Version string
					State   string
				}
			}
		}
		if err := json.Unmarshal(data, &cv); err != nil {
			return "", fmt.Errorf("decode OpenShift ClusterVersion: %w", err)
		}
		if len(cv.Status.History) == 0 || cv.Status.History[0].State != "Completed" {
			return "", fmt.Errorf("OpenShift ClusterVersion installation or update has not completed")
		}
		version := cv.Status.History[0].Version
		if !versionPattern.MatchString(version) {
			return "", fmt.Errorf("OpenShift ClusterVersion has unrecognized version %q", version)
		}
		return minorVersion(version), nil
	}
	return "", nil
}
