// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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

package discovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/secondarynetwork"
	"github.com/nvidia/k8s-launch-kit/pkg/nicconfigdaemon"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrNotInstalled reports that no existing NIC Configuration Daemon was found
// for a read-only discovery request.
var ErrNotInstalled = errors.New("nic configuration daemon is not installed")

const (
	readOnlyNICOperatorLabel  = "network.nvidia.com/operator.mlnx-nic"
	readOnlySriovCapableLabel = "feature.node.kubernetes.io/pci-15b3.sriov.capable"
)

// ReadOnlyOption configures read-only discovery without changing the existing
// two-argument DiscoverReadOnly call.
type ReadOnlyOption func(*readOnlyOptions)

type readOnlyPodExec func(context.Context, *rest.Config, string, string, string, []string) (string, error)

type readOnlyOptions struct {
	networkOperatorNamespace string
	restConfig               *rest.Config
	podExec                  readOnlyPodExec
	assumeUnverifiedExcluded bool
	fabricDiscovery          bool
	gpuTopology              bool
	gpuOperatorNamespace     string
}

// WithReadOnlyNetworkOperatorNamespace restricts daemon lookup to the
// configured Network Operator namespace. Supplying it avoids probing fallback
// namespaces and is the least-privilege path for callers with namespace-scoped
// pod permissions.
func WithReadOnlyNetworkOperatorNamespace(namespace string) ReadOnlyOption {
	return func(options *readOnlyOptions) {
		options.networkOperatorNamespace = namespace
	}
}

// WithReadOnlyRESTConfig supplies the credentials needed to inspect an
// existing daemon pod when a Ready daemon node has not published a NicDevice.
// Without it, discovery fails by default rather than treating an uninspected
// node as a non-NIC node. Use WithReadOnlyAssumeUnverifiedExcluded to opt in to
// excluding such nodes with a warning.
func WithReadOnlyRESTConfig(restConfig *rest.Config) ReadOnlyOption {
	return func(options *readOnlyOptions) {
		options.restConfig = restConfig
	}
}

// WithReadOnlyAssumeUnverifiedExcluded allows discovery without a REST config
// to exclude Ready daemon nodes that have not published a NicDevice. Their NIC
// publisher status is unknown, so discovery emits a warning. This option does
// not suppress errors when a supplied REST config cannot inspect a node.
func WithReadOnlyAssumeUnverifiedExcluded() ReadOnlyOption {
	return func(options *readOnlyOptions) {
		options.assumeUnverifiedExcluded = true
	}
}

// WithReadOnlyFabricDiscovery enables optional fabric probing in existing daemon
// pods. Requires WithReadOnlyRESTConfig and pods/exec permission; no resources or
// host configuration are modified. Incomplete observations leave LinkType unset.
func WithReadOnlyFabricDiscovery() ReadOnlyOption {
	return func(options *readOnlyOptions) { options.fabricDiscovery = true }
}

// WithReadOnlyGPUTopology enables optional product/topology observations in
// existing NIC daemon and same-node GPU Operator feature-discovery containers.
// Requires a nonempty GPU Operator namespace, WithReadOnlyRESTConfig, pod and
// DaemonSet read access, and pods/exec permission. No resources are modified.
// Incomplete evidence keeps hardware fallback and emits warnings through the
// context's UI sink; callers must consume that sink to inspect probe failures.
func WithReadOnlyGPUTopology(gpuOperatorNamespace string) ReadOnlyOption {
	return func(options *readOnlyOptions) {
		options.gpuTopology = true
		options.gpuOperatorNamespace = strings.TrimSpace(gpuOperatorNamespace)
	}
}

func withReadOnlyPodExec(exec readOnlyPodExec) ReadOnlyOption {
	return func(options *readOnlyOptions) {
		options.podExec = exec
	}
}

// DiscoverReadOnly reads hardware discovered by an existing NIC Configuration
// Daemon and returns it as a LaunchKitConfig. It never creates, patches, or
// deletes Kubernetes resources. The daemon must already be running; callers
// that need Launch Kit to bootstrap a temporary daemon should use Discover.
//
// The read-only path consumes NicDevice resources already published by the
// operator. If a Ready daemon node has not published a device, it probes that
// node's existing daemon pod to distinguish an eligible publisher from a node
// with no discoverable NIC or only restricted BlueFields. That fallback needs
// a REST config and pods/exec permission. Without a REST config, discovery
// fails unless WithReadOnlyAssumeUnverifiedExcluded is set; that option excludes
// the unverified nodes and warns that the returned inventory may be incomplete.
// Probe errors remain errors. The function never creates bootstrap resources.
func DiscoverReadOnly(ctx context.Context, kubeClient client.Client, opts ...ReadOnlyOption) (*config.LaunchKitConfig, error) {
	if kubeClient == nil {
		return nil, errors.New("DiscoverReadOnly: kubeClient must not be nil")
	}

	options := readOnlyOptions{}
	for _, opt := range opts {
		opt(&options)
	}
	if options.fabricDiscovery && options.restConfig == nil {
		return nil, errors.New("DiscoverReadOnly: WithReadOnlyFabricDiscovery requires WithReadOnlyRESTConfig")
	}
	if options.gpuTopology && (options.restConfig == nil || options.gpuOperatorNamespace == "") {
		return nil, errors.New("DiscoverReadOnly: WithReadOnlyGPUTopology requires a GPU Operator namespace and WithReadOnlyRESTConfig")
	}
	namespace, err := findDaemonSetNamespace(ctx, kubeClient, nicconfigdaemon.DaemonSetName, options.networkOperatorNamespace)
	if err != nil {
		if errors.Is(err, ErrNotInstalled) {
			return nil, err
		}
		return nil, fmt.Errorf("DiscoverReadOnly: inspect existing daemon: %w", err)
	}

	var expectedNodes []string
	var daemonPods []corev1.Pod
	if options.networkOperatorNamespace != "" {
		expectedNodes, daemonPods, namespace, err = waitForDaemonSetPodsInNamespace(ctx, kubeClient, ui.FromContext(ctx),
			namespace, nicconfigdaemon.DaemonSetName, 5*time.Minute)
	} else {
		expectedNodes, daemonPods, namespace, err = waitForDaemonSetPods(ctx, kubeClient, ui.FromContext(ctx),
			namespace, nicconfigdaemon.DaemonSetName, 5*time.Minute)
	}
	if err != nil {
		return nil, fmt.Errorf("DiscoverReadOnly: wait for existing daemon: %w", err)
	}

	nodeLabels, err := fetchNodeLabels(ctx, kubeClient)
	if err != nil {
		return nil, fmt.Errorf("DiscoverReadOnly: list nodes: %w", err)
	}
	devices := &nicop.NicDeviceList{}
	if err := kubeClient.List(ctx, devices); err != nil {
		return nil, fmt.Errorf("DiscoverReadOnly: list NicDevice resources: %w", err)
	}
	readyNodes := make(map[string]bool, len(expectedNodes))
	for _, node := range expectedNodes {
		readyNodes[node] = true
	}
	filtered := devices.Items[:0]
	deviceNodes := make(map[string]bool, len(devices.Items))
	for _, device := range devices.Items {
		if readyNodes[device.Status.Node] {
			filtered = append(filtered, device)
			deviceNodes[device.Status.Node] = true
		}
	}
	devices.Items = filtered
	if len(devices.Items) == 0 {
		return nil, fmt.Errorf("DiscoverReadOnly: no NicDevice resources found on ready daemon nodes")
	}
	probeResults, err := probeMissingReadOnlyPublishers(ctx, options.restConfig, namespace,
		expectedNodes, daemonPods, deviceNodes, options.podExec, options.assumeUnverifiedExcluded)
	if err != nil {
		return nil, fmt.Errorf("DiscoverReadOnly: determine NIC publisher eligibility: %w", err)
	}
	selection, err := readOnlyPublisherNodes(expectedNodes, deviceNodes, probeResults)
	if err != nil {
		return nil, fmt.Errorf("DiscoverReadOnly: determine NIC publisher eligibility: %w", err)
	}
	if len(selection.excludedNodes) > 0 {
		ui.FromContext(ctx).Warning("Excluding %d ready daemon node(s) with no discoverable or only restricted NVIDIA NICs", len(selection.excludedNodes))
	}
	if len(selection.unverifiedNodes) > 0 {
		ui.FromContext(ctx).Warning(
			"Excluding %d ready daemon node(s) with unverified NicDevice publisher status; "+
				"pass WithReadOnlyRESTConfig to inspect them (inventory may be incomplete)",
			len(selection.unverifiedNodes),
		)
	}
	var missingNodes []string
	for _, node := range selection.publisherNodes {
		if !deviceNodes[node] {
			missingNodes = append(missingNodes, node)
		}
	}
	if len(missingNodes) > 0 {
		return nil, fmt.Errorf("DiscoverReadOnly: NicDevice resources are missing for eligible publisher nodes: %v", missingNodes)
	}
	clusterConfig, warnings, members := buildClusterConfigWithMembers(devices.Items, nodeLabels, nil, true)
	if options.gpuTopology {
		observations, identities, err := discoverGPUObservations(ctx, kubeClient, options.restConfig, options.gpuOperatorNamespace,
			clusterConfig, nodeLabels, devices.Items, members, daemonPods, options.podExec)
		if err != nil {
			return nil, fmt.Errorf("DiscoverReadOnly: discover GPU topology: %w", err)
		}
		applyGPUObservations(ctx, clusterConfig, identities, observations)
	}
	if err := assignVRRails(clusterConfig, devices.Items); err != nil {
		return nil, fmt.Errorf("DiscoverReadOnly: %w", err)
	}
	if err := ensureReadOnlyNodeSelectors(clusterConfig, nodeLabels); err != nil {
		return nil, fmt.Errorf("DiscoverReadOnly: %w", err)
	}
	for _, warning := range warnings {
		ui.FromContext(ctx).Warning("%s", warning)
	}

	if options.fabricDiscovery {
		if err := discoverFabric(ctx, options.restConfig, namespace, clusterConfig, devices.Items, members, daemonPods, options.podExec); err != nil {
			return nil, fmt.Errorf("DiscoverReadOnly: discover fabric: %w", err)
		}
	}
	cfg, err := config.DefaultLaunchKitConfig()
	if err != nil {
		return nil, fmt.Errorf("DiscoverReadOnly: load default config: %w", err)
	}
	cfg.ClusterConfig = clusterConfig
	cfg.DiscoveredSecondaryNetwork, err = secondarynetwork.DiscoverEvidence(ctx, kubeClient)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func findDaemonSetNamespace(ctx context.Context, c client.Client, daemonSetName, configuredNamespace string) (string, error) {
	namespaces := []string{configuredNamespace}
	if configuredNamespace == "" {
		namespaces = []string{"nvidia-network-operator", "network-operator", nicconfigdaemon.Namespace}
	}
	var lastErr error
	var firstWithPods string
	for _, namespace := range namespaces {
		readiness, err := checkDaemonSetPodsReady(ctx, c, namespace, daemonSetName)
		if err == nil && readiness.ready > 0 {
			return namespace, nil
		}
		if err == nil {
			if firstWithPods == "" {
				firstWithPods = namespace
			}
		} else if !errors.Is(err, ErrNotInstalled) && lastErr == nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	if firstWithPods != "" {
		return firstWithPods, nil
	}
	return "", fmt.Errorf("%w: DaemonSet %q was not found in usable operator namespaces", ErrNotInstalled, daemonSetName)
}

func probeMissingReadOnlyPublishers(ctx context.Context, restConfig *rest.Config, namespace string,
	expectedNodes []string, daemonPods []corev1.Pod, deviceNodes map[string]bool, podExec readOnlyPodExec,
	assumeUnverifiedExcluded bool,
) (map[string]mellanoxNICProbeResult, error) {
	results := make(map[string]mellanoxNICProbeResult)
	for _, node := range expectedNodes {
		if deviceNodes[node] {
			continue
		}
		if restConfig == nil {
			if assumeUnverifiedExcluded {
				results[node] = mellanoxNICProbeUnverified
				continue
			}
			return nil, fmt.Errorf("cannot verify whether daemon node %q can publish NicDevice without a REST config; pass WithReadOnlyRESTConfig", node)
		}
		pod := findDaemonPod([]string{node}, daemonPods)
		if pod == nil {
			return nil, fmt.Errorf("no ready daemon pod is available to inspect NIC publisher eligibility on node %q", node)
		}
		containerName := ""
		if len(pod.Spec.Containers) > 0 {
			containerName = pod.Spec.Containers[0].Name
		}
		if podExec == nil {
			podExec = execInPod
		}
		output, err := podExec(ctx, restConfig, namespace, pod.Name, containerName,
			[]string{"/bin/sh", "-c", sysfsMellanoxNICPresentCmd})
		if err != nil {
			return nil, fmt.Errorf("inspect NIC publisher eligibility on node %q: %w", node, err)
		}
		result := parseMellanoxNICProbeResult(output)
		if result == mellanoxNICProbeNone && strings.TrimSpace(output) != "" {
			return nil, fmt.Errorf("unexpected NIC probe output on node %q", node)
		}
		results[node] = result
	}
	return results, nil
}

type readOnlyPublisherSelection struct {
	publisherNodes  []string
	excludedNodes   []string
	unverifiedNodes []string
}

func readOnlyPublisherNodes(expectedNodes []string, deviceNodes map[string]bool,
	probeResults map[string]mellanoxNICProbeResult,
) (readOnlyPublisherSelection, error) {
	selection := readOnlyPublisherSelection{
		publisherNodes:  make([]string, 0, len(expectedNodes)),
		excludedNodes:   make([]string, 0),
		unverifiedNodes: make([]string, 0),
	}
	for _, node := range expectedNodes {
		if deviceNodes[node] {
			selection.publisherNodes = append(selection.publisherNodes, node)
			continue
		}
		result, probed := probeResults[node]
		if !probed {
			return readOnlyPublisherSelection{}, fmt.Errorf("NIC publisher eligibility on node %q was not verified", node)
		}
		switch result {
		case mellanoxNICProbePresent:
			selection.publisherNodes = append(selection.publisherNodes, node)
		case mellanoxNICProbeNone, mellanoxNICProbeRestricted:
			selection.excludedNodes = append(selection.excludedNodes, node)
		case mellanoxNICProbeUnverified:
			selection.unverifiedNodes = append(selection.unverifiedNodes, node)
		default:
			return readOnlyPublisherSelection{}, fmt.Errorf("unknown NIC publisher probe result %q on node %q", result, node)
		}
	}
	return selection, nil
}

func ensureReadOnlyNodeSelectors(groups []config.ClusterConfig, nodeLabels map[string]map[string]string) error {
	for i := range groups {
		if len(groups[i].NodeSelector) > 0 &&
			selectorMatchesOnlyNodes(groups[i].NodeSelector, groups[i].WorkerNodes, nodeLabels) {
			continue
		}

		var selector map[string]string
		for _, candidate := range readOnlyNodeSelectors(groups[i].WorkerNodes, nodeLabels) {
			if selectorMatchesOnlyNodes(candidate, groups[i].WorkerNodes, nodeLabels) {
				selector = candidate
				break
			}
		}
		if len(selector) == 0 {
			return fmt.Errorf("could not derive a safe node selector for discovered group %q", groups[i].Identifier)
		}
		groups[i].NodeSelector = selector
	}
	return nil
}

func readOnlyNodeSelectors(nodes []string, nodeLabels map[string]map[string]string) []map[string]string {
	common := computeCommonLabels(nodes, nodeLabels)
	selectors := make([]map[string]string, 0, 8)
	for _, key := range []string{config.MachineLabelKey, config.GPULabelKey,
		"nvidia.com/gpu.product", "nvidia.com/gpu.machine",
		readOnlyNICOperatorLabel, readOnlySriovCapableLabel} {
		if value := common[key]; value != "" {
			selectors = append(selectors, map[string]string{key: value})
		}
	}
	if len(common) > 0 {
		combined := make(map[string]string, len(common))
		for key, value := range common {
			combined[key] = value
		}
		selectors = append(selectors, combined)
	}
	if len(nodes) == 1 {
		selectors = append(selectors, map[string]string{"kubernetes.io/hostname": nodes[0]})
	}
	return selectors
}

func selectorMatchesOnlyNodes(selector map[string]string, groupNodes []string, nodeLabels map[string]map[string]string) bool {
	wanted := make(map[string]bool, len(groupNodes))
	for _, node := range groupNodes {
		if _, exists := nodeLabels[node]; !exists {
			return false
		}
		wanted[node] = true
	}
	matcher := labels.SelectorFromSet(selector)
	for node, values := range nodeLabels {
		if matcher.Matches(labels.Set(values)) != wanted[node] {
			return false
		}
	}
	return true
}
