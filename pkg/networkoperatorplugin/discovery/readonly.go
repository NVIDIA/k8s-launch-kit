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

	if _, err := checkDaemonSetPodsReady(ctx, kubeClient,
		nicconfigdaemon.Namespace, nicconfigdaemon.DaemonSetName); err != nil {
		if errors.Is(err, ErrNotInstalled) {
			return nil, err
		}
		return nil, fmt.Errorf("DiscoverReadOnly: inspect existing daemon: %w", err)
	}

	expectedNodes, _, _, err := waitForDaemonSetPods(ctx, kubeClient, ui.FromContext(ctx),
		nicconfigdaemon.Namespace, nicconfigdaemon.DaemonSetName, 5*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("DiscoverReadOnly: wait for existing daemon: %w", err)
	}

	nodeLabels, err := fetchNodeLabels(ctx, kubeClient)
	if err != nil {
		return nil, fmt.Errorf("DiscoverReadOnly: list nodes: %w", err)
	}
	if err := waitNicDevicesDiscovered(ctx, kubeClient, expectedNodes); err != nil {
		return nil, fmt.Errorf("DiscoverReadOnly: wait for NicDevice resources: %w", err)
	}

	devices := &nicop.NicDeviceList{}
	if err := kubeClient.List(ctx, devices); err != nil {
		return nil, fmt.Errorf("DiscoverReadOnly: list NicDevice resources: %w", err)
	}
	clusterConfig, warnings := buildClusterConfig(devices.Items, nodeLabels, nil, true)
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
