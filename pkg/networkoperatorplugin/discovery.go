// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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

package networkoperatorplugin

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	netop "github.com/Mellanox/network-operator/api/v1alpha1"
	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func (p *NetworkOperatorPlugin) DiscoverClusterConfig(ctx context.Context, c client.Client, defaultConfig *config.LaunchKubernetesConfig) error {
	uiOutput := ui.FromContext(ctx)

	// Ensure a NicClusterPolicy exists (error if any already exists, else create one)
	policy := &netop.NicClusterPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nic-cluster-policy",
			Namespace: defaultConfig.NetworkOperator.Namespace,
		},
		Spec: netop.NicClusterPolicySpec{
			NicConfigurationOperator: &netop.NicConfigurationOperatorSpec{
				Operator: &netop.ImageSpec{
					Repository: defaultConfig.NetworkOperator.Repository,
					Image:      "nic-configuration-operator",
					Version:    defaultConfig.NetworkOperator.ComponentVersion,
				},
				ConfigurationDaemon: &netop.ImageSpec{
					Repository: defaultConfig.NetworkOperator.Repository,
					Image:      "nic-configuration-operator-daemon",
					Version:    defaultConfig.NetworkOperator.ComponentVersion,
				},
			},
		},
	}

	uiOutput.Info("Deploying discovery profile")
	log.Log.Info("Deploying a thin NicClusterPolicy for cluster config discovery")

	existingPolicies, err := EnsureNicClusterPolicy(ctx, c, policy)
	if err != nil {
		return err
	}

	var savedPolicies []netop.NicClusterPolicy
	if len(existingPolicies) > 0 {
		// Existing policies detected — ask user for permission to replace
		uiOutput.Warning("Found %d existing NicClusterPolicy resource(s) on the cluster.", len(existingPolicies))
		uiOutput.Info("The launch kit needs to deploy its own NicClusterPolicy for discovery.")
		uiOutput.Info("The existing policy will be saved, deleted, and restored after discovery completes.")

		confirmed, err := uiOutput.Confirm("Do you agree to temporarily replace the existing NicClusterPolicy?")
		if err != nil {
			return fmt.Errorf("failed to get user confirmation: %w", err)
		}
		if !confirmed {
			return fmt.Errorf("discovery aborted: user declined to replace existing NicClusterPolicy")
		}

		// Deep-copy existing policies for later restoration
		savedPolicies = make([]netop.NicClusterPolicy, len(existingPolicies))
		copy(savedPolicies, existingPolicies)

		if err := DeleteNicClusterPolicies(ctx, c, existingPolicies); err != nil {
			return fmt.Errorf("failed to remove existing NicClusterPolicies: %w", err)
		}

		// Now create the thin discovery policy
		if err := c.Create(ctx, policy); err != nil {
			return fmt.Errorf("failed to create discovery NicClusterPolicy: %w", err)
		}
		if err := WaitNicClusterPolicyReady(ctx, c, policy.Name); err != nil {
			return err
		}
	}

	// Defers run LIFO: restore must be registered first so it runs after cleanup.
	// Order of execution: (1) delete thin discovery policy, (2) restore saved policies.
	if len(savedPolicies) > 0 {
		defer func() {
			uiOutput.Info("Restoring previously saved NicClusterPolicy resource(s)")
			if err := RestoreNicClusterPolicies(ctx, c, savedPolicies); err != nil {
				log.Log.Error(err, "failed to restore NicClusterPolicies after discovery")
				uiOutput.Error("Failed to restore original NicClusterPolicy: %v", err)
			} else {
				uiOutput.Success("Original NicClusterPolicy restored successfully")
			}
		}()
	}

	// Always attempt cleanup of the thin discovery NicClusterPolicy
	defer func() {
		if err := DeleteNicClusterPolicy(ctx, c, "nic-cluster-policy"); err != nil {
			log.Log.Error(err, "failed to delete NicClusterPolicy after discovery")
		} else {
			log.Log.Info("NicClusterPolicy deleted after discovery")
		}
	}()

	// After creation, list pods in the target namespace and ensure all pods
	// from the nic-configuration-daemon DaemonSet are Ready
	if err := checkDaemonSetPodsReady(ctx, c, defaultConfig.NetworkOperator.Namespace, "nic-configuration-daemon"); err != nil {
		return err
	}

	// Get NicDevice resources and build ClusterConfig.NvidiaNICs from their statuses
	devices := &nicop.NicDeviceList{}
	if err := c.List(ctx, devices, client.InNamespace(defaultConfig.NetworkOperator.Namespace)); err != nil {
		return err
	}
	if len(devices.Items) == 0 {
		log.Log.Info("No NicDevice resources found yet; waiting for discovery", "namespace", defaultConfig.NetworkOperator.Namespace)
		if err := waitNicDevicesDiscovered(ctx, c, defaultConfig.NetworkOperator.Namespace); err != nil {
			return err
		}
		// re-list after wait
		if err := c.List(ctx, devices, client.InNamespace(defaultConfig.NetworkOperator.Namespace)); err != nil {
			return err
		}
		log.Log.Info("NicDevice resources discovered", "count", len(devices.Items))
	}

	buildClusterConfigFromNicDevices(devices.Items, defaultConfig.ClusterConfig)

	return nil
}

// checkDaemonSetPodsReady verifies that all pods owned by the given DaemonSet
// in the provided namespace are Ready.
func checkDaemonSetPodsReady(ctx context.Context, c client.Client, namespace, daemonSetName string) error {
	podList := &corev1.PodList{}
	if err := c.List(ctx, podList, client.InNamespace(namespace)); err != nil {
		return err
	}

	var dsPods []corev1.Pod
	for _, pod := range podList.Items {
		for _, owner := range pod.OwnerReferences {
			if owner.Kind == "DaemonSet" && owner.Name == daemonSetName {
				dsPods = append(dsPods, pod)
				break
			}
		}
	}

	if len(dsPods) == 0 {
		return fmt.Errorf("no pods found for DaemonSet %q in namespace %q", daemonSetName, namespace)
	}

	for _, pod := range dsPods {
		if !isPodReady(&pod) {
			return fmt.Errorf("pod %q from DaemonSet %q is not Ready", pod.Name, daemonSetName)
		}
	}

	return nil
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// waitNicDevicesDiscovered polls until one or more NicDevice objects exist in the given namespace.
func waitNicDevicesDiscovered(parentCtx context.Context, c client.Client, namespace string) error {
	uiOutput := ui.FromContext(parentCtx)
	progress := uiOutput.StartProgress("Discovering network devices (timeout: 5 min)")

	// Use a bounded timeout if none supplied
	ctx := parentCtx
	if _, hasDeadline := parentCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parentCtx, 5*time.Minute)
		defer cancel()
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		list := &nicop.NicDeviceList{}
		if err := c.List(ctx, list, client.InNamespace(namespace)); err == nil {
			if len(list.Items) > 0 {
				progress.Success(fmt.Sprintf("Found %d device(s)", len(list.Items)))
				return nil
			}
		}

		select {
		case <-ctx.Done():
			progress.Fail("Timeout waiting for devices")
			return fmt.Errorf("timeout waiting for NicDevice resources in namespace %q", namespace)
		case <-ticker.C:
			progress.Update("Still waiting for device discovery...")
		}
	}
}

// buildClusterConfigFromNicDevices constructs ClusterConfig.NvidiaNICs based on NicDevice statuses.
func buildClusterConfigFromNicDevices(devices []nicop.NicDevice, cluster *config.ClusterConfig) {
	cluster.Capabilities.Nodes.Rdma = false
	cluster.Capabilities.Nodes.Sriov = false
	cluster.Capabilities.Nodes.Ib = true // TODO fix

	cluster.PFs = []config.PFConfig{}
	pfs := map[config.PFConfig]interface{}{}
	workerNodes := map[string]interface{}{}

	for _, d := range devices {
		for _, p := range d.Status.Ports {
			if p.RdmaInterface != "" {
				cluster.Capabilities.Nodes.Rdma = true
			}
			if p.PCI != "" {
				cluster.Capabilities.Nodes.Sriov = true
			}

			pfs[config.PFConfig{
				DeviceID:         d.Status.Type, // NIC model name, e.g. "ConnectX7"
				RdmaDevice:       p.RdmaInterface,
				PciAddress:       p.PCI,
				NetworkInterface: p.NetworkInterface,
				Traffic:          "east-west", // TODO fix
			}] = struct{}{}
		}

		workerNodes[d.Status.Node] = struct{}{}
	}

	for node := range workerNodes {
		if node != "" {
			cluster.WorkerNodes = append(cluster.WorkerNodes, node)
		}
	}

	slices.Sort(cluster.WorkerNodes)

	for pf := range pfs {
		cluster.PFs = append(cluster.PFs, pf)
	}

	slices.SortFunc(cluster.PFs, func(a, b config.PFConfig) int {
		return strings.Compare(a.PciAddress, b.PciAddress)
	})
}
