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
	"time"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/nicconfigdaemon"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrNotInstalled reports that no existing NIC Configuration Daemon was found
// for a read-only discovery request.
var ErrNotInstalled = errors.New("nic configuration daemon is not installed")

// ReadOnlyOption configures read-only discovery without changing the existing
// two-argument DiscoverReadOnly call.
type ReadOnlyOption func(*readOnlyOptions)

type readOnlyOptions struct {
	networkOperatorNamespace string
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

// DiscoverReadOnly reads hardware discovered by an existing NIC Configuration
// Daemon and returns it as a LaunchKitConfig. It never creates, patches, or
// deletes Kubernetes resources. The daemon must already be running; callers
// that need Launch Kit to bootstrap a temporary daemon should use Discover.
//
// The read-only path intentionally consumes the NicDevice resources already
// published by the operator. Pod execution is not required because the
// operator has already performed the privileged hardware inspection. This
// keeps the API usable with read-only RBAC and avoids granting a consumer
// permission to create bootstrap resources.
func DiscoverReadOnly(ctx context.Context, kubeClient client.Client, opts ...ReadOnlyOption) (*config.LaunchKitConfig, error) {
	if kubeClient == nil {
		return nil, errors.New("DiscoverReadOnly: kubeClient must not be nil")
	}

	options := readOnlyOptions{}
	for _, opt := range opts {
		opt(&options)
	}
	namespace, err := findDaemonSetNamespace(ctx, kubeClient, nicconfigdaemon.DaemonSetName, options.networkOperatorNamespace)
	if err != nil {
		if errors.Is(err, ErrNotInstalled) {
			return nil, err
		}
		return nil, fmt.Errorf("DiscoverReadOnly: inspect existing daemon: %w", err)
	}

	expectedNodes, _, _, err := waitForDaemonSetPods(ctx, kubeClient, ui.FromContext(ctx),
		namespace, nicconfigdaemon.DaemonSetName, 5*time.Minute)
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
	var missingNodes []string
	for _, node := range expectedNodes {
		if !deviceNodes[node] {
			missingNodes = append(missingNodes, node)
		}
	}
	if len(missingNodes) > 0 {
		return nil, fmt.Errorf("DiscoverReadOnly: NicDevice resources are missing for ready daemon nodes: %v", missingNodes)
	}
	clusterConfig, warnings := buildClusterConfig(devices.Items, nodeLabels, nil, true)
	if len(clusterConfig) == 1 && len(clusterConfig[0].NodeSelector) == 0 {
		var selector map[string]string
		for _, candidate := range readOnlyNodeSelectors(clusterConfig[0].WorkerNodes, nodeLabels) {
			if selectorMatchesOnlyNodes(candidate, clusterConfig[0].WorkerNodes, nodeLabels) {
				selector = candidate
				break
			}
		}
		if len(selector) == 0 {
			return nil, fmt.Errorf("DiscoverReadOnly: derived node selector matches nodes outside the discovered group")
		}
		clusterConfig[0].NodeSelector = selector
	}
	for _, warning := range warnings {
		ui.FromContext(ctx).Warning("%s", warning)
	}

	cfg, err := config.DefaultLaunchKitConfig()
	if err != nil {
		return nil, fmt.Errorf("DiscoverReadOnly: load default config: %w", err)
	}
	cfg.ClusterConfig = clusterConfig
	return cfg, nil
}

func findDaemonSetNamespace(ctx context.Context, c client.Client, daemonSetName, configuredNamespace string) (string, error) {
	namespaces := []string{configuredNamespace}
	if configuredNamespace == "" {
		namespaces = []string{"nvidia-network-operator", "network-operator", nicconfigdaemon.Namespace}
	}
	var lastErr error
	for _, namespace := range namespaces {
		readiness, err := checkDaemonSetPodsReady(ctx, c, namespace, daemonSetName)
		if err == nil && readiness.ready > 0 {
			return namespace, nil
		}
		if err != nil && !errors.Is(err, ErrNotInstalled) {
			lastErr = err
		} else if err == nil {
			lastErr = fmt.Errorf("DaemonSet %q in namespace %q has no ready pods", daemonSetName, namespace)
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("%w: DaemonSet %q was not found in usable operator namespaces", ErrNotInstalled, daemonSetName)
}

func readOnlyNodeSelectors(nodes []string, nodeLabels map[string]map[string]string) []map[string]string {
	common := computeCommonLabels(nodes, nodeLabels)
	selectors := make([]map[string]string, 0, 5)
	for _, key := range []string{config.MachineLabelKey, config.GPULabelKey,
		"nvidia.com/gpu.product", "nvidia.com/gpu.machine"} {
		if value := common[key]; value != "" {
			selectors = append(selectors, map[string]string{key: value})
		}
	}
	if len(nodes) == 1 {
		selectors = append(selectors, map[string]string{"kubernetes.io/hostname": nodes[0]})
	}
	return selectors
}

func selectorMatchesOnlyNodes(selector map[string]string, groupNodes []string, nodeLabels map[string]map[string]string) bool {
	wanted := make(map[string]bool, len(groupNodes))
	for _, node := range groupNodes {
		wanted[node] = true
	}
	for node, labels := range nodeLabels {
		matches := true
		for key, value := range selector {
			if labels[key] != value {
				matches = false
				break
			}
		}
		if matches != wanted[node] {
			return false
		}
	}
	return true
}
