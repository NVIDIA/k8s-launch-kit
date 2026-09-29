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
	"sort"
	"time"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/nicconfigdaemon"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrNotInstalled reports that no existing NIC Configuration Daemon was found
// for a read-only discovery request.
var ErrNotInstalled = errors.New("nic configuration daemon is not installed")

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
func DiscoverReadOnly(ctx context.Context, kubeClient client.Client) (*config.LaunchKitConfig, error) {
	if kubeClient == nil {
		return nil, errors.New("DiscoverReadOnly: kubeClient must not be nil")
	}

	namespace, err := findDaemonSetNamespace(ctx, kubeClient, nicconfigdaemon.DaemonSetName)
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
	for _, device := range devices.Items {
		if readyNodes[device.Status.Node] {
			filtered = append(filtered, device)
		}
	}
	devices.Items = filtered
	if len(devices.Items) == 0 {
		return nil, fmt.Errorf("DiscoverReadOnly: no NicDevice resources found on ready daemon nodes")
	}
	clusterConfig, warnings := buildClusterConfig(devices.Items, nodeLabels, nil, true)
	if len(clusterConfig) == 1 && len(clusterConfig[0].NodeSelector) == 0 {
		selector := readOnlyNodeSelector(clusterConfig[0].WorkerNodes, nodeLabels)
		if len(selector) == 0 {
			return nil, fmt.Errorf("DiscoverReadOnly: unable to derive a node selector for the discovered group")
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

func findDaemonSetNamespace(ctx context.Context, c client.Client, daemonSetName string) (string, error) {
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods); err != nil {
		return "", err
	}
	namespaces := make(map[string]bool)
	for _, pod := range pods.Items {
		for _, owner := range pod.OwnerReferences {
			if owner.Kind == "DaemonSet" && owner.Name == daemonSetName {
				namespaces[pod.Namespace] = true
				break
			}
		}
	}
	if len(namespaces) == 0 {
		return "", fmt.Errorf("%w: DaemonSet %q was not found", ErrNotInstalled, daemonSetName)
	}
	ordered := make([]string, 0, len(namespaces))
	for namespace := range namespaces {
		ordered = append(ordered, namespace)
	}
	sort.Strings(ordered)
	return ordered[0], nil
}

func readOnlyNodeSelector(nodes []string, nodeLabels map[string]map[string]string) map[string]string {
	common := computeCommonLabels(nodes, nodeLabels)
	for _, key := range []string{config.MachineLabelKey, config.GPULabelKey,
		"nvidia.com/gpu.product", "nvidia.com/gpu.machine"} {
		if value := common[key]; value != "" {
			return map[string]string{key: value}
		}
	}
	if len(nodes) == 1 {
		return map[string]string{"kubernetes.io/hostname": nodes[0]}
	}
	return nil
}
