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
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Mellanox/doca-driver-build/entrypoint/pkg/mofedmodules"
	netop "github.com/Mellanox/network-operator/api/v1alpha1"
	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/internal/pciids"
	"github.com/nvidia/k8s-launch-kit/pkg/presets"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

//go:embed ns-product-ids
var nsProductIDsData string

// nsProductIDs is the set of product codes / legacy product IDs for
// BlueField DPU devices (not SuperNICs). Devices whose PartNumber matches
// an entry in this set are marked as north-south traffic.
var nsProductIDs = parseNSProductIDs(nsProductIDsData)

func parseNSProductIDs(data string) map[string]bool {
	ids := map[string]bool{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Format: ProductCode | LegacyProductId | ProductName
		parts := strings.SplitN(line, "|", 3)
		if len(parts) >= 2 {
			productCode := strings.TrimSpace(parts[0])
			legacyID := strings.TrimSpace(parts[1])
			if productCode != "" {
				ids[productCode] = true
			}
			if legacyID != "" {
				ids[legacyID] = true
			}
		}
	}
	return ids
}

// isNorthSouthDevice returns true if the device's part number matches
// a known BlueField DPU (non-SuperNIC) product ID from the ns-product-ids file.
func isNorthSouthDevice(partNumber string) bool {
	return nsProductIDs[partNumber]
}

// isBlueField3Device returns true if the PCI device ID is BlueField-3
// (Mellanox vendor 15b3, device 0xa2dc). BF3 is the only BlueField
// generation where both DPU and SuperNIC SKUs share a device ID, so
// classification has to fall back to part number against ns-product-ids.
// Older (BF2) and newer (BF4+) generations use distinct device IDs and
// stay on the default path — DPUs match ns-product-ids and become
// north-south; anything else flows through the frequency heuristic.
func isBlueField3Device(deviceID string) bool {
	return strings.EqualFold(deviceID, "a2dc")
}

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
					Repository:       defaultConfig.NetworkOperator.Repository,
					Image:            "nic-configuration-operator",
					Version:          defaultConfig.NetworkOperator.ComponentVersion,
					ImagePullSecrets: defaultConfig.NetworkOperator.ImagePullSecrets,
				},
				ConfigurationDaemon: &netop.ImageSpec{
					Repository:       defaultConfig.NetworkOperator.Repository,
					Image:            "nic-configuration-operator-daemon",
					Version:          defaultConfig.NetworkOperator.ComponentVersion,
					ImagePullSecrets: defaultConfig.NetworkOperator.ImagePullSecrets,
				},
			},
		},
	}

	log.Log.Info("Deploying a thin NicClusterPolicy for cluster config discovery")

	// Check if an existing NicClusterPolicy already has NicConfigurationOperator.
	// If so, we can reuse it without redeploying.
	existingPolicies, err := EnsureNicClusterPolicy(ctx, c, policy)
	if err != nil {
		return err
	}

	reuseExisting := false
	patchedPolicyName := ""
	if len(existingPolicies) > 0 {
		// Check if any existing policy already has the nic-configuration daemon with the required version
		requiredVersion := defaultConfig.NetworkOperator.ComponentVersion
		for _, ep := range existingPolicies {
			if ep.Spec.NicConfigurationOperator != nil &&
				ep.Spec.NicConfigurationOperator.ConfigurationDaemon != nil &&
				ep.Spec.NicConfigurationOperator.ConfigurationDaemon.Version == requiredVersion {
				reuseExisting = true
				uiOutput.Info("Existing NicClusterPolicy already includes nic-configuration-daemon %s, reusing", requiredVersion)
				log.Log.Info("Reusing existing NicClusterPolicy with NicConfigurationOperator",
					"name", ep.Name, "version", requiredVersion)
				break
			}
		}

		if !reuseExisting {
			// Patch existing policy to add NicConfigurationOperator (non-disruptive)
			targetPolicy := existingPolicies[0]
			uiOutput.Info("Patching existing NicClusterPolicy %q to add NicConfigurationOperator for discovery", targetPolicy.Name)

			if err := PatchNicConfigOperatorIntoPolicy(ctx, c, targetPolicy.Name, policy.Spec.NicConfigurationOperator); err != nil {
				return fmt.Errorf("failed to patch NicClusterPolicy with NicConfigurationOperator: %w", err)
			}
			if err := WaitNicClusterPolicyReady(ctx, c, targetPolicy.Name); err != nil {
				return err
			}
			patchedPolicyName = targetPolicy.Name
		}
	} else {
		uiOutput.Info("Deploying discovery profile")
	}

	// Cleanup: remove NicConfigurationOperator we patched in, or delete the thin policy we created
	if patchedPolicyName != "" {
		defer func() {
			uiOutput.Info("Removing discovery NicConfigurationOperator from NicClusterPolicy %q", patchedPolicyName)
			if err := RemoveNicConfigOperatorFromPolicy(ctx, c, patchedPolicyName); err != nil {
				log.Log.Error(err, "failed to remove NicConfigurationOperator after discovery")
				uiOutput.Error("Failed to remove NicConfigurationOperator: %v", err)
			}
		}()
	} else if !reuseExisting {
		defer func() {
			if err := DeleteNicClusterPolicy(ctx, c, "nic-cluster-policy"); err != nil {
				log.Log.Error(err, "failed to delete NicClusterPolicy after discovery")
			} else {
				log.Log.Info("NicClusterPolicy deleted after discovery")
			}
		}()
	}

	// After creation/patching, wait for nic-configuration-daemon pods to be Ready.
	// If we just patched the policy, pods need time to start — poll with timeout.
	// If we're reusing an existing policy, pods should already be running — try once,
	// then fall back to the alternate namespace.
	var expectedNodes []string
	var dsPods []corev1.Pod
	if reuseExisting {
		primaryNS := defaultConfig.NetworkOperator.Namespace
		uiOutput.Info("Checking for nic-configuration-daemon pods in namespace %q", primaryNS)
		expectedNodes, dsPods, err = checkDaemonSetPodsReady(ctx, c, primaryNS, "nic-configuration-daemon")
		if err != nil {
			primaryErr := err
			// Try the other common namespace before giving up (legacy chart-rename fallback)
			alternateNS := alternateNamespace(primaryNS)
			if alternateNS != "" {
				uiOutput.Info("Initial probe in namespace %q failed, retrying in fallback namespace %q", primaryNS, alternateNS)
				expectedNodes, dsPods, err = checkDaemonSetPodsReady(ctx, c, alternateNS, "nic-configuration-daemon")
				if err == nil {
					defaultConfig.NetworkOperator.Namespace = alternateNS
				}
			}
			if err != nil {
				if alternateNS != "" {
					return fmt.Errorf("no nic-configuration-daemon pods found in namespace %q (also checked fallback namespace %q); use --network-operator-namespace to specify the correct namespace: %w", primaryNS, alternateNS, primaryErr)
				}
				return err
			}
		}
	} else {
		// We just patched/created the policy — pods need time to start.
		// Poll both the configured namespace and alternate namespace.
		var foundNS string
		expectedNodes, dsPods, foundNS, err = waitForDaemonSetPods(ctx, c, uiOutput, defaultConfig.NetworkOperator.Namespace, "nic-configuration-daemon", 10*time.Minute)
		if err != nil {
			return err
		}
		defaultConfig.NetworkOperator.Namespace = foundNS
	}

	// Fetch node labels early — needed both for filtering and nodeSelector computation
	nodeLabels, err := fetchNodeLabels(ctx, c)
	if err != nil {
		log.Log.Error(err, "failed to fetch node labels; nodeSelectors will be empty")
		nodeLabels = map[string]map[string]string{}
	}

	// Filter expected nodes using the node selector.
	// Nodes not matching will never report NicDevices, so waiting for them
	// would cause a timeout.
	if len(p.NodeSelector) > 0 {
		expectedNodes = filterNodesByLabels(expectedNodes, nodeLabels, p.NodeSelector)
		if len(expectedNodes) == 0 {
			return fmt.Errorf("no nodes match the node selector %v", p.NodeSelector)
		}
	}

	// Wait for all expected nodes to report their NicDevice resources
	if err := waitNicDevicesDiscovered(ctx, c, defaultConfig.NetworkOperator.Namespace, expectedNodes); err != nil {
		return err
	}

	// Get NicDevice resources and build ClusterConfig from their statuses
	devices := &nicop.NicDeviceList{}
	if err := c.List(ctx, devices, client.InNamespace(defaultConfig.NetworkOperator.Namespace)); err != nil {
		return err
	}

	clusterConfig, nsWarnings := buildClusterConfig(devices.Items, nodeLabels, p.NodeSelector)
	defaultConfig.ClusterConfig = clusterConfig

	for _, w := range nsWarnings {
		uiOutput.Warning("%s", w)
	}

	// Discover OFED-dependent kernel modules per group via pod exec.
	// Results are classified into third-party RDMA vs storage modules and
	// saved to config for user inspection. mlx5-prefixed modules (NVIDIA's own)
	// are silently filtered out.
	if p.RESTConfig != nil {
		for i := range defaultConfig.ClusterConfig {
			group := &defaultConfig.ClusterConfig[i]

			// Fill in missing machine/GPU product types by probing hardware
			// directly when GPU operator node labels are absent.
			needMachine := group.MachineType == ""
			needProduct := group.GPUType == ""
			if needMachine || needProduct {
				machine, product := discoverHardwareTypes(ctx, p.RESTConfig,
					defaultConfig.NetworkOperator.Namespace, group.WorkerNodes, dsPods,
					needMachine, needProduct)
				if needMachine && machine != "" {
					group.MachineType = machine
					uiOutput.Info("Discovered machine type for group %s: %s", group.Identifier, machine)
				}
				if needProduct && product != "" {
					group.GPUType = product
					uiOutput.Info("Discovered GPU product type for group %s: %s", group.Identifier, product)
				}
			}

			// Probe GPU topology from nvidia-smi: populates NumaNode,
			// ConnectedGPU, GPUProximity per PF; if any PF has PIX to a GPU,
			// the PIX-gate override rewrites Traffic and re-runs rails.
			// Failures are non-fatal; when nvidia-smi is absent, today's
			// part-number classification continues to govern.
			discoverGPUTopology(ctx, p.RESTConfig,
				defaultConfig.NetworkOperator.Namespace, group, dsPods)

			// Probe per-PF fabric type from active port state + subnet
			// manager presence (more reliable than firmware link_layer
			// alone). Populates PFConfig.LinkType / LinkTypeSource. Used by
			// the declarative defaults in `l8k generate` (Unit 8) to fill
			// `--fabric` from the discovered group.
			discoverGroupFabric(ctx, p.RESTConfig,
				defaultConfig.NetworkOperator.Namespace, group, dsPods)

			// Try to enrich with a predefined topology preset for this (machine,
			// GPU) pair. Presets provide authoritative traffic classification,
			// rail assignments, and NUMA/GPU topology metadata for known
			// hardware configurations. Lookup is exact-match on (machineType,
			// gpuType) — both must be known for a preset to apply.
			if group.MachineType != "" && group.GPUType != "" {
				log.Log.V(1).Info("Looking up preset by (machineType, gpuType)",
					"group", group.Identifier,
					"machineType", group.MachineType,
					"gpuType", group.GPUType)
				preset, presetErr := presets.LoadPreset(group.MachineType, group.GPUType)
				if presetErr != nil {
					log.Log.Error(presetErr, "failed to load preset",
						"machineType", group.MachineType, "gpuType", group.GPUType)
					uiOutput.Warning("Failed to load preset for %s/%s: %v",
						group.MachineType, group.GPUType, presetErr)
				} else if preset == nil {
					log.Log.V(1).Info("No preset matched (machineType, gpuType)",
						"group", group.Identifier,
						"machineType", group.MachineType,
						"gpuType", group.GPUType)
				} else {
					// Always apply the matched preset on a best-effort basis.
					// Any discrepancies (PF count, PCI address drift,
					// device-ID drift) are recorded as soft deviations and
					// re-warned about on every subsequent config load.
					deviations := presets.ValidatePreset(preset, group.PFs)
					presets.ApplyPreset(preset, group)
					log.Log.V(1).Info("Preset matched and applied",
						"group", group.Identifier,
						"machineType", group.MachineType,
						"gpuType", group.GPUType,
						"presetPFCount", len(preset.PFs),
						"discoveredPFCount", len(group.PFs),
						"deviationCount", len(deviations))
					if len(deviations) > 0 {
						group.PresetDeviation = deviations
						log.Log.Info("Preset applied with deviations from matched preset",
							"group", group.Identifier,
							"machineType", group.MachineType,
							"gpuType", group.GPUType,
							"deviationCount", len(deviations))
						uiOutput.Warning(
							"Preset for %s/%s applied with %d deviation(s) from the matched preset. The deployment is not certified — see 'presetDeviation' in cluster-config.yaml.",
							group.MachineType, group.GPUType, len(deviations))
					} else {
						uiOutput.Info("Applied preset configuration for %s", group.MachineType)
					}
				}
			}

			modules, err := discoverThirdPartyRDMAModules(ctx, p.RESTConfig,
				defaultConfig.NetworkOperator.Namespace, group.WorkerNodes, dsPods)
			if err != nil {
				log.Log.Error(err, "failed to discover OFED-dependent modules", "group", group.Identifier)
				uiOutput.Warning("Could not discover OFED-dependent modules for group %s: %v", group.Identifier, err)
				continue
			}
			rdma, storage := classifyDiscoveredModules(modules)
			if len(rdma) > 0 {
				group.ThirdPartyRDMAModules = rdma
				defaultConfig.DOCADriver.UnloadThirdPartyRDMAModules = true
				uiOutput.Info("Discovered %d third-party RDMA module(s) for group %s — enabled unloadThirdPartyRDMAModules",
					len(rdma), group.Identifier)
			}
			if len(storage) > 0 {
				group.StorageModules = storage
				defaultConfig.DOCADriver.UnloadStorageModules = true
				uiOutput.Info("Discovered %d storage module(s) for group %s — enabled unloadStorageModules",
					len(storage), group.Identifier)
			}
		}
	}

	// Now that machineType and gpuType are settled (either from labels or
	// from the per-group hardware probes), assign each group a stable
	// machine label. This replaces the differential-label nodeSelector
	// algorithm: every node in the group is patched with
	// `nvidia.kubernetes-launch-kit.machine: <machineType>-<gpuType>` and
	// the group's Identifier + NodeSelector are aligned with that value.
	applyMachineLabelToGroups(ctx, c, defaultConfig.ClusterConfig)

	// Phase summary — counts surfaced at info level so the default UX shows
	// progress without requiring --log-level=debug.
	totalEW, totalNS, presetMatches, deviationGroups, labelled := 0, 0, 0, 0, 0
	for _, g := range defaultConfig.ClusterConfig {
		for _, pf := range g.PFs {
			switch pf.Traffic {
			case "east-west":
				totalEW++
			case "north-south":
				totalNS++
			}
		}
		if g.PresetApplied {
			presetMatches++
		}
		if len(g.PresetDeviation) > 0 {
			deviationGroups++
		}
		if g.NodeSelector[config.MachineLabelKey] != "" {
			labelled++
		}
	}
	log.Log.Info("Discovery summary",
		"groupCount", len(defaultConfig.ClusterConfig),
		"eastWestPFs", totalEW,
		"northSouthPFsFiltered", totalNS,
		"presetMatches", presetMatches,
		"presetDeviationGroups", deviationGroups,
		"machineLabelledGroups", labelled)

	return nil
}

// applyMachineLabelToGroups walks each group and writes two l8k-specific
// labels onto every node in the group:
//
//   - MachineLabelKey = `<machineType>-<gpuType>` literal — per-source-group
//     identifier, written when both fields are resolved.
//   - GPULabelKey = `<gpuType>` literal — written when gpuType is resolved.
//     Used as the merged-group NodeSelector when source groups span
//     machineTypes but share a GPU type.
//
// Both label values bypass the Kubernetes 63-char limit by skipping the
// label entirely when the value would overflow (logged at debug). Group
// `Identifier` follows the resource-name convention (lowercase via
// `sanitizeIdentifier`); the label values keep their original case to
// match `nvidia.com/gpu.product`-style values.
//
// Groups whose machine label can't be computed (one input missing) keep
// their fallback identifier ("group-N") and an empty NodeSelector. The
// GPU label is still written when gpuType alone is resolved, so
// merged-group selection still works.
func applyMachineLabelToGroups(ctx context.Context, c client.Client, groups []config.ClusterConfig) {
	for i := range groups {
		g := &groups[i]
		machineLabel := config.MachineLabelValue(g.MachineType, g.GPUType)
		gpuLabel := config.GPULabelValue(g.GPUType)

		if machineLabel == "" {
			log.Log.V(1).Info("Skipping machine label: machineType/gpuType unresolved or value > 63 chars",
				"group", g.Identifier,
				"machineType", g.MachineType,
				"gpuType", g.GPUType)
		} else {
			log.Log.V(1).Info("Assigning machine label to group",
				"originalIdentifier", g.Identifier,
				"machineType", g.MachineType,
				"gpuType", g.GPUType,
				"labelValue", machineLabel,
				"nodes", len(g.WorkerNodes))
			g.Identifier = sanitizeIdentifier(machineLabel)
			g.NodeSelector = map[string]string{config.MachineLabelKey: machineLabel}
		}

		labels := map[string]string{}
		if machineLabel != "" {
			labels[config.MachineLabelKey] = machineLabel
		}
		if gpuLabel != "" {
			labels[config.GPULabelKey] = gpuLabel
		}
		if len(labels) == 0 {
			continue
		}
		for _, nodeName := range g.WorkerNodes {
			if err := patchNodeLabels(ctx, c, nodeName, labels); err != nil {
				log.Log.Error(err, "failed to patch node labels",
					"node", nodeName, "labels", labels)
				continue
			}
			log.Log.V(1).Info("Wrote labels to node",
				"node", nodeName, "labels", labels)
		}
	}
}

// patchNodeLabels applies one or more labels to a node via a
// strategic-merge patch. Idempotent — re-applying the same values is a
// no-op on the cluster side, and avoids the read-modify-write conflict
// risk of a full Update.
func patchNodeLabels(ctx context.Context, c client.Client, nodeName string, labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf("%q:%q", k, v))
	}
	patch := []byte(fmt.Sprintf(
		`{"metadata":{"labels":{%s}}}`,
		strings.Join(parts, ",")))
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
	return c.Patch(ctx, node, client.RawPatch(k8stypes.StrategicMergePatchType, patch))
}

// checkDaemonSetPodsReady verifies that all pods owned by the given DaemonSet
// in the provided namespace are Ready. Returns the list of node names running
// those pods and the pods themselves (for later use in pod exec).
func checkDaemonSetPodsReady(ctx context.Context, c client.Client, namespace, daemonSetName string) ([]string, []corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := c.List(ctx, podList, client.InNamespace(namespace)); err != nil {
		return nil, nil, err
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
		return nil, nil, fmt.Errorf(
			"no pods found for DaemonSet %q in namespace %q; "+
				"use --network-operator-namespace to specify the correct namespace",
			daemonSetName, namespace)
	}

	nodes := make([]string, 0, len(dsPods))
	for _, pod := range dsPods {
		if !isPodReady(&pod) {
			return nil, nil, fmt.Errorf("pod %q from DaemonSet %q is not Ready", pod.Name, daemonSetName)
		}
		if pod.Spec.NodeName != "" {
			nodes = append(nodes, pod.Spec.NodeName)
		}
	}

	return nodes, dsPods, nil
}

// waitForDaemonSetPods polls for daemon pods to become ready, checking both the
// configured namespace and the alternate common namespace. This is needed after
// patching a NicClusterPolicy to add NicConfigurationOperator, because the
// network operator needs time to reconcile and create the DaemonSet pods.
func waitForDaemonSetPods(parentCtx context.Context, c client.Client, uiOutput ui.Output, namespace, daemonSetName string, timeout time.Duration) ([]string, []corev1.Pod, string, error) {
	altNS := alternateNamespace(namespace)
	progressLabel := fmt.Sprintf("Waiting for %s pods in namespace %q (timeout: %s)", daemonSetName, namespace, timeout.Truncate(time.Second))
	if altNS != "" {
		progressLabel = fmt.Sprintf("Waiting for %s pods in namespace %q (also polling fallback %q; timeout: %s)", daemonSetName, namespace, altNS, timeout.Truncate(time.Second))
	}
	progress := uiOutput.StartProgress(progressLabel)

	ctx := parentCtx
	if _, hasDeadline := parentCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parentCtx, timeout)
		defer cancel()
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		// Try the configured namespace first
		nodes, pods, err := checkDaemonSetPodsReady(ctx, c, namespace, daemonSetName)
		if err == nil {
			progress.Success(fmt.Sprintf("Found %d pod(s) in namespace %q", len(pods), namespace))
			return nodes, pods, namespace, nil
		}
		lastErr = err

		// Try alternate namespace
		if altNS != "" {
			nodes, pods, err = checkDaemonSetPodsReady(ctx, c, altNS, daemonSetName)
			if err == nil {
				progress.Success(fmt.Sprintf("Found %d pod(s) in fallback namespace %q", len(pods), altNS))
				return nodes, pods, altNS, nil
			}
		}

		select {
		case <-ctx.Done():
			progress.Fail("Timeout waiting for daemon pods")
			if altNS != "" {
				return nil, nil, "", fmt.Errorf("timeout waiting for %s pods to start in namespace %q (also checked fallback namespace %q); use --network-operator-namespace to specify the correct namespace: %w", daemonSetName, namespace, altNS, lastErr)
			}
			return nil, nil, "", fmt.Errorf("timeout waiting for %s pods to start in namespace %q: %w", daemonSetName, namespace, lastErr)
		case <-ticker.C:
			progress.Update(fmt.Sprintf("Waiting for %s pods...", daemonSetName))
		}
	}
}

// alternateNamespace returns the other common network operator namespace,
// or empty string if the current namespace isn't one of the two known defaults.
func alternateNamespace(current string) string {
	switch current {
	case "nvidia-network-operator":
		return "network-operator"
	case "network-operator":
		return "nvidia-network-operator"
	default:
		return ""
	}
}

func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// waitNicDevicesDiscovered polls until NicDevice objects exist for all expected nodes in the given namespace.
func waitNicDevicesDiscovered(parentCtx context.Context, c client.Client, namespace string, expectedNodes []string) error {
	uiOutput := ui.FromContext(parentCtx)
	progress := uiOutput.StartProgress(fmt.Sprintf("Discovering network devices on %d node(s) (timeout: 10 min)", len(expectedNodes)))

	// Use a bounded timeout if none supplied
	ctx := parentCtx
	if _, hasDeadline := parentCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parentCtx, 10*time.Minute)
		defer cancel()
	}

	expectedSet := make(map[string]bool, len(expectedNodes))
	for _, n := range expectedNodes {
		expectedSet[n] = true
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		list := &nicop.NicDeviceList{}
		if err := c.List(ctx, list, client.InNamespace(namespace)); err == nil {
			discoveredNodes := make(map[string]bool)
			for _, d := range list.Items {
				if d.Status.Node != "" {
					discoveredNodes[d.Status.Node] = true
				}
			}

			allFound := true
			for node := range expectedSet {
				if !discoveredNodes[node] {
					allFound = false
					break
				}
			}

			if allFound && len(discoveredNodes) > 0 {
				progress.Success(fmt.Sprintf("Found %d device(s) on %d node(s)", len(list.Items), len(discoveredNodes)))
				return nil
			}

			progress.Update(fmt.Sprintf("Discovered devices on %d/%d node(s)...", len(discoveredNodes), len(expectedSet)))
		}

		select {
		case <-ctx.Done():
			progress.Fail("Timeout waiting for devices")
			return fmt.Errorf("timeout waiting for NicDevice resources from all nodes in namespace %q", namespace)
		case <-ticker.C:
		}
	}
}

// pfFingerprint identifies a PF by its device ID and PCI address (ignoring RDMA/net names).
type pfFingerprint struct {
	DeviceID   string
	PciAddress string
}

// nodePFEntry holds the full PF info discovered on a specific node.
type nodePFEntry struct {
	pfFingerprint
	RdmaDevice       string
	NetworkInterface string
	IsNorthSouth     bool // true when device is a DPU (not a SuperNIC)
	// IsExplicitEastWest is true when Stage 1 classified this PF as a
	// BlueField SuperNIC (BF chip + part number not in ns-product-ids).
	// The frequency heuristic must not flip it back to north-south, and
	// its presence triggers the "any non-matching unpinned PF on this
	// node is OOB / north-south" rule.
	IsExplicitEastWest bool
	PSID               string
	PartNumber         string
}

// fetchNodeLabels lists all Kubernetes nodes and returns a map of nodeName → labels.
func fetchNodeLabels(ctx context.Context, c client.Client) (map[string]map[string]string, error) {
	nodeList := &corev1.NodeList{}
	if err := c.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	result := make(map[string]map[string]string, len(nodeList.Items))
	for _, node := range nodeList.Items {
		result[node.Name] = node.Labels
	}
	return result, nil
}

// filterNodesByLabels returns only nodes whose labels match all entries in the selector.
func filterNodesByLabels(nodes []string, nodeLabels map[string]map[string]string, selector map[string]string) []string {
	var filtered []string
	for _, n := range nodes {
		labels := nodeLabels[n]
		if labels == nil {
			continue
		}
		match := true
		for k, v := range selector {
			if labels[k] != v {
				match = false
				break
			}
		}
		if match {
			filtered = append(filtered, n)
		}
	}
	return filtered
}

// isPowerOfTwo returns true if n is a positive power of two (2, 4, 8, 16, ...).
func isPowerOfTwo(n int) bool {
	return n >= 2 && (n&(n-1)) == 0
}

// classifyByPartNumberFrequency refines per-node PF traffic classification
// after Stage 1's deterministic rules (DPU list, BF SuperNIC). Two paths:
//
//  1. If any PF on the node is explicitly east-west (Stage 1 BF SuperNIC
//     pin), every other PF whose part number doesn't match an explicit-EW
//     part number is reclassified as north-south (treated as OOB / mgmt).
//     PFs already pinned north-south or explicit east-west are not touched.
//     This handles the common case where a node has BF SuperNICs for
//     east-west GPU traffic and a separate non-BF NIC (e.g. ConnectX-6 Lx)
//     wired up as the OOB management interface.
//
//  2. No explicit-EW pins: fall back to the original 5+-PF frequency
//     heuristic — the most common part number (preferring power-of-2
//     counts) is east-west; minority part numbers become north-south.
//     The tally only considers unpinned PFs, so DPU part numbers don't
//     skew the tiebreak.
//
// Returns warning messages for the caller to display to the user.
func classifyByPartNumberFrequency(nodeMap map[string]*nodeInfo) []string {
	var warnings []string
	for nodeName, ni := range nodeMap {
		// Path 1: explicit east-west pins exist for this node.
		ewPartNumbers := map[string]bool{}
		for _, pf := range ni.pfs {
			if pf.IsExplicitEastWest && pf.PartNumber != "" {
				ewPartNumbers[pf.PartNumber] = true
			}
		}
		if len(ewPartNumbers) > 0 {
			for i := range ni.pfs {
				if ni.pfs[i].IsNorthSouth || ni.pfs[i].IsExplicitEastWest {
					continue
				}
				if !ewPartNumbers[ni.pfs[i].PartNumber] {
					ni.pfs[i].IsNorthSouth = true
				}
			}
			continue
		}

		// Path 2: fall back to the legacy frequency heuristic.
		if len(ni.pfs) < 5 {
			continue
		}

		// Count PFs by part number, ignoring already-pinned ones so DPU
		// part numbers (Stage 1 north-south) don't poison the tiebreak.
		partCounts := map[string]int{}
		for _, pf := range ni.pfs {
			if pf.IsNorthSouth || pf.PartNumber == "" {
				continue
			}
			partCounts[pf.PartNumber]++
		}
		if len(partCounts) <= 1 {
			// All unpinned PFs share a part number — nothing to classify.
			continue
		}

		// Find the east-west part number: prefer the one with a power-of-2 count,
		// break ties by highest count then alphabetically.
		var ewPart string
		var ewCount int
		for part, count := range partCounts {
			better := false
			if ewPart == "" {
				better = true
			} else if isPowerOfTwo(count) && !isPowerOfTwo(ewCount) {
				better = true
			} else if isPowerOfTwo(count) == isPowerOfTwo(ewCount) {
				if count > ewCount {
					better = true
				} else if count == ewCount && part < ewPart {
					better = true
				}
			}
			if better {
				ewPart = part
				ewCount = count
			}
		}

		// Warn if E-W count is not a power of two
		if !isPowerOfTwo(ewCount) {
			warnings = append(warnings, fmt.Sprintf(
				"East-west NIC count (%d) is not a power of two on node %s. Please verify traffic classification in the config file.",
				ewCount, nodeName))
		}

		// Mark minority part numbers as north-south
		reclassified := false
		for i := range ni.pfs {
			if ni.pfs[i].PartNumber != ewPart && !ni.pfs[i].IsNorthSouth {
				ni.pfs[i].IsNorthSouth = true
				reclassified = true
			}
		}

		if !reclassified {
			warnings = append(warnings, fmt.Sprintf(
				"Could not identify north-south NICs on node %s. Please verify traffic classification and rail assignments in the config file.",
				nodeName))
		}
	}
	return warnings
}

// buildClusterConfig groups NicDevices by identical PF fingerprints across nodes
// and returns a ClusterConfig slice with one entry per group.
// nodeInfo holds discovered PF entries and capability flags for a single node.
type nodeInfo struct {
	pfs     []nodePFEntry
	hasRdma bool
	hasPCI  bool
}

func buildClusterConfig(devices []nicop.NicDevice, nodeLabels map[string]map[string]string, nodeSelector map[string]string) ([]config.ClusterConfig, []string) {
	// Step 1: Build per-node PF map and track capabilities per node
	nodeMap := map[string]*nodeInfo{}

	for _, d := range devices {
		nodeName := d.Status.Node
		if nodeName == "" {
			continue
		}
		if nodeMap[nodeName] == nil {
			nodeMap[nodeName] = &nodeInfo{}
		}
		ni := nodeMap[nodeName]
		// Stage 1 classification:
		//   - Part number in ns-product-ids → BlueField DPU → north-south.
		//   - BlueField-3 chip (deviceID a2dc) with part number NOT in
		//     ns-product-ids → BlueField-3 SuperNIC → explicit east-west.
		//     BF3 is special-cased because DPU and SuperNIC SKUs share the
		//     same device ID; BF2/BF4 use distinct device IDs and DPUs
		//     among them are already covered by the ns-product-ids match.
		//   - Anything else → unclassified (default east-west, may be
		//     reclassified by the frequency heuristic in Stage 1.5).
		isDPU := isNorthSouthDevice(d.Status.PartNumber)
		isBF3SuperNIC := !isDPU && isBlueField3Device(d.Status.Type)
		var classification string
		switch {
		case isDPU:
			classification = "north-south (matched DPU part-number)"
		case isBF3SuperNIC:
			classification = "east-west (BF3 SuperNIC override)"
		default:
			classification = "unclassified (default east-west; may be reclassified by frequency heuristic)"
		}
		log.Log.V(2).Info("Classified NIC by traffic direction",
			"node", nodeName,
			"deviceID", d.Status.Type,
			"partNumber", d.Status.PartNumber,
			"classification", classification)
		for _, p := range d.Status.Ports {
			entry := nodePFEntry{
				pfFingerprint: pfFingerprint{
					DeviceID:   d.Status.Type,
					PciAddress: p.PCI,
				},
				RdmaDevice:         p.RdmaInterface,
				NetworkInterface:   p.NetworkInterface,
				IsNorthSouth:       isDPU,
				IsExplicitEastWest: isBF3SuperNIC,
				PSID:               d.Status.PSID,
				PartNumber:         d.Status.PartNumber,
			}
			ni.pfs = append(ni.pfs, entry)
			if p.RdmaInterface != "" {
				ni.hasRdma = true
			}
			if p.PCI != "" {
				ni.hasPCI = true
			}
		}
	}

	// Step 1.5: Apply part-number frequency heuristic for nodes with 5+ PFs.
	// The most common part number (ideally with a power-of-2 count) is east-west;
	// all other part numbers are north-south.
	nsWarnings := classifyByPartNumberFrequency(nodeMap)

	// Step 2: Compute PF fingerprint per node and group nodes
	type fingerprintKey string
	computeFingerprint := func(pfs []nodePFEntry) fingerprintKey {
		fps := make([]pfFingerprint, len(pfs))
		for i, p := range pfs {
			fps[i] = p.pfFingerprint
		}
		slices.SortFunc(fps, func(a, b pfFingerprint) int {
			if c := strings.Compare(a.DeviceID, b.DeviceID); c != 0 {
				return c
			}
			return strings.Compare(a.PciAddress, b.PciAddress)
		})
		parts := make([]string, len(fps))
		for i, fp := range fps {
			parts[i] = fp.DeviceID + ":" + fp.PciAddress
		}
		return fingerprintKey(strings.Join(parts, "|"))
	}

	// Group nodes by fingerprint, preserving order by first-seen node
	type nodeGroup struct {
		fingerprint fingerprintKey
		nodes       []string
		pfs         []nodePFEntry // representative PFs from first node
		hasRdma     bool
		hasPCI      bool
	}
	fingerprintOrder := []fingerprintKey{}
	groupMap := map[fingerprintKey]*nodeGroup{}

	// Sort node names for deterministic grouping
	sortedNodes := make([]string, 0, len(nodeMap))
	for n := range nodeMap {
		sortedNodes = append(sortedNodes, n)
	}
	slices.Sort(sortedNodes)

	for _, nodeName := range sortedNodes {
		ni := nodeMap[nodeName]
		fp := computeFingerprint(ni.pfs)
		log.Log.V(1).Info("Bucketing node by PCI fingerprint",
			"node", nodeName, "pfCount", len(ni.pfs), "fingerprint", string(fp))
		if g, ok := groupMap[fp]; ok {
			g.nodes = append(g.nodes, nodeName)
			g.hasRdma = g.hasRdma || ni.hasRdma
			g.hasPCI = g.hasPCI || ni.hasPCI
		} else {
			fingerprintOrder = append(fingerprintOrder, fp)
			groupMap[fp] = &nodeGroup{
				fingerprint: fp,
				nodes:       []string{nodeName},
				pfs:         ni.pfs,
				hasRdma:     ni.hasRdma,
				hasPCI:      ni.hasPCI,
			}
		}
	}

	// Step 3: Build ClusterConfig per group
	singleGroup := len(fingerprintOrder) == 1
	groups := make([]config.ClusterConfig, 0, len(fingerprintOrder))

	for i, fp := range fingerprintOrder {
		g := groupMap[fp]

		identifier := ""
		if !singleGroup {
			identifier = fmt.Sprintf("group-%d", i)
		}

		// Build PFs from the representative node's entries.
		// If multiple nodes exist in the group, RDMA/net device names may differ — omit them.
		pfs := make([]config.PFConfig, len(g.pfs))
		for j, entry := range g.pfs {
			traffic := "east-west"
			if entry.IsNorthSouth {
				traffic = "north-south"
			}
			pfs[j] = config.PFConfig{
				DeviceID:   entry.DeviceID,
				PciAddress: entry.PciAddress,
				Traffic:    traffic,
				PSID:       entry.PSID,
				PartNumber: entry.PartNumber,
			}
			if singleGroup || len(g.nodes) == 1 {
				// Safe to include RDMA/net device names when only one node in group
				pfs[j].RdmaDevice = entry.RdmaDevice
				pfs[j].NetworkInterface = entry.NetworkInterface
			}
		}

		slices.SortFunc(pfs, func(a, b config.PFConfig) int {
			return strings.Compare(a.PciAddress, b.PciAddress)
		})

		// Assign rail numbers sequentially over the E/W set. No PF has
		// GPUProximity populated yet at this stage, so the helper's PIX-gate
		// branch is a no-op here; only the rail loop runs.
		reclassifyAndReassignRails(pfs)

		slices.Sort(g.nodes)

		// Extract machine/product type from common node labels
		commonLabels := computeCommonLabels(g.nodes, nodeLabels)
		machineType := commonLabels["nvidia.com/gpu.machine"]
		gpuType := commonLabels["nvidia.com/gpu.product"]
		log.Log.V(1).Info("Read GPU operator labels for group",
			"group", identifier,
			"nodes", g.nodes,
			"machineTypeFromLabel", machineType,
			"gpuTypeFromLabel", gpuType,
			"willFallBackToHardwareProbe", machineType == "" || gpuType == "")

		cc := config.ClusterConfig{
			Identifier:    identifier,
			MachineType:   machineType,
			GPUType:   gpuType,
			NodeSelector:  nodeSelector,
			Capabilities: &config.ClusterCapabilities{
				Nodes: &config.NodesCapabilities{
					Rdma:  g.hasRdma,
					Sriov: g.hasPCI,
					Ib:    true, // TODO: detect from NicDevice
				},
			},
			PFs:         pfs,
			WorkerNodes: g.nodes,
		}

		groups = append(groups, cc)
	}

	// Step 4: Compute nodeSelectors per group — overrides the initial
	// value with discriminating labels when multiple groups exist.
	if len(groups) > 1 {
		computeNodeSelectors(groups, nodeLabels)
	}

	return groups, nsWarnings
}

// computeNodeSelectors assigns NodeSelectors to each group using ALL label keys
// where groups have differing common values. This ensures every group uses the
// same set of label keys (with different values), making the selectors consistent.
func computeNodeSelectors(groups []config.ClusterConfig, nodeLabels map[string]map[string]string) {
	n := len(groups)
	if n <= 1 {
		return
	}

	// Compute common labels per group (intersection of labels across all nodes)
	groupCommonLabels := make([]map[string]string, n)
	for i, g := range groups {
		groupCommonLabels[i] = computeCommonLabels(g.WorkerNodes, nodeLabels)
	}

	// Collect all label keys present in any group's common labels
	allKeys := map[string]bool{}
	for _, cl := range groupCommonLabels {
		for k := range cl {
			allKeys[k] = true
		}
	}

	// Find all label keys where at least two groups have different common values.
	// A key "differs" if: group A has value X and group B has value Y (Y != X),
	// or one group has the key in common and another doesn't.
	differingKeys := []string{}
	for k := range allKeys {
		values := map[string]bool{}
		missing := false
		for _, cl := range groupCommonLabels {
			v, ok := cl[k]
			if !ok {
				missing = true
			} else {
				values[v] = true
			}
		}
		if len(values) > 1 || (len(values) >= 1 && missing) {
			differingKeys = append(differingKeys, k)
		}
	}

	slices.Sort(differingKeys)

	// Deprioritize feature.node.kubernetes.io/* labels — only include them
	// if the remaining labels can't differentiate all groups on their own.
	var primaryKeys []string
	for _, k := range differingKeys {
		if !strings.HasPrefix(k, "feature.node.kubernetes.io/") {
			primaryKeys = append(primaryKeys, k)
		}
	}
	if canDifferentiate(primaryKeys, groupCommonLabels) {
		differingKeys = primaryKeys
	}
	// else: keep all differingKeys (primary + fallback) as-is

	// Assign ALL differing label keys to each group's NodeSelector
	for i := range groups {
		selector := map[string]string{}
		for _, k := range differingKeys {
			if v, ok := groupCommonLabels[i][k]; ok {
				selector[k] = v
			}
			// If this group doesn't have the key in common, omit it —
			// the key still discriminates because other groups DO have it.
		}
		if len(selector) > 0 {
			groups[i].NodeSelector = selector
		} else {
			log.Log.Info("Warning: could not compute a unique nodeSelector for group",
				"group", groups[i].Identifier, "nodes", groups[i].WorkerNodes)
		}
	}
}

// canDifferentiate returns true if the given label keys are sufficient to produce
// a unique fingerprint for each group (no two groups share the same key-value set).
func canDifferentiate(keys []string, groupLabels []map[string]string) bool {
	if len(keys) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, labels := range groupLabels {
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + "=" + labels[k]
		}
		fp := strings.Join(parts, ",")
		if seen[fp] {
			return false
		}
		seen[fp] = true
	}
	return true
}

// computeCommonLabels returns labels with identical values across all specified nodes.
func computeCommonLabels(nodes []string, nodeLabels map[string]map[string]string) map[string]string {
	if len(nodes) == 0 {
		return map[string]string{}
	}

	// Start with labels from the first node
	firstLabels := nodeLabels[nodes[0]]
	if firstLabels == nil {
		return map[string]string{}
	}

	common := make(map[string]string, len(firstLabels))
	for k, v := range firstLabels {
		if isNoisyLabel(k) {
			continue
		}
		common[k] = v
	}

	// Intersect with remaining nodes
	for _, node := range nodes[1:] {
		labels := nodeLabels[node]
		for k, v := range common {
			if labels[k] != v {
				delete(common, k)
			}
		}
	}

	return common
}

// isNoisyLabel returns true for labels that are node-specific or not useful for discrimination.
func isNoisyLabel(key string) bool {
	noisyPrefixes := []string{
		"kubernetes.io/metadata",
		"node.kubernetes.io/instance-type",
		"kubernetes.io/hostname",
		"kubernetes.io/arch",
		"kubernetes.io/os",
		"pod-security.kubernetes.io",
		"topology.kubernetes.io",
	}
	noisyExact := []string{
		"beta.kubernetes.io/arch",
		"beta.kubernetes.io/os",
		"kubernetes.io/hostname",
	}

	for _, prefix := range noisyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	for _, exact := range noisyExact {
		if key == exact {
			return true
		}
	}
	return false
}

// ofedTargetModules is the list of MLX/OFED kernel modules to check for during
// pre-flight discovery. These modules (and any modules that depend on them) may
// need to be blacklisted when the DOCA driver is deployed.
var ofedTargetModules = []string{
	"mlx5_core", "mlx5_ib", "ib_umad", "ib_uverbs",
	"ib_ipoib", "rdma_cm", "rdma_ucm", "ib_core", "ib_cm",
}

// knownStorageModules is the set of storage-over-RDMA kernel modules handled by
// UNLOAD_STORAGE_MODULES in the driver container. The list is sourced from the
// canonical `mofedmodules.DefaultStorageModules` exported by doca-driver-build
// (entrypoint/pkg/mofedmodules) so l8k's classification stays in lockstep with
// what the driver container will actually unload.
var knownStorageModules = buildKnownStorageModulesSet()

func buildKnownStorageModulesSet() map[string]bool {
	m := make(map[string]bool, len(mofedmodules.DefaultStorageModules))
	for _, mod := range mofedmodules.DefaultStorageModules {
		m[mod] = true
	}
	return m
}

// parseMachineTypeFromDMI extracts and sanitizes a machine type string from
// raw /sys/class/dmi/id/product_name content. It trims whitespace/newlines
// and replaces spaces with dashes to match GPU operator label format.
// Returns empty string if input is blank after trimming.
func parseMachineTypeFromDMI(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	return strings.ReplaceAll(s, " ", "-")
}

// parseGPUProductFromNvidiaSmi extracts the first "Product Name" value from
// nvidia-smi -q output and sanitizes it to match GPU operator label format
// (spaces replaced with dashes). Returns empty string if no product name found.
func parseGPUProductFromNvidiaSmi(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "Product Name") {
			continue
		}
		parts := strings.SplitN(line, " : ", 2)
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimSpace(parts[1])
		if name == "" {
			continue
		}
		return strings.ReplaceAll(name, " ", "-")
	}
	return ""
}

// discoverHardwareTypes attempts to discover machineType and gpuType by
// probing hardware directly when label-derived values are empty. It execs into
// a nic-configuration-daemon pod on one of the group's nodes.
// Returns (machineType, gpuType) — either or both may still be empty if
// discovery fails or hardware info is unavailable.
func discoverHardwareTypes(ctx context.Context, restConfig *rest.Config,
	namespace string, groupNodes []string, dsPods []corev1.Pod,
	needMachine, needProduct bool) (machineType, gpuType string) {

	targetPod := findDaemonPod(groupNodes, dsPods)
	if targetPod == nil {
		log.Log.Info("No nic-configuration-daemon pod found on group nodes; skipping hardware type probing")
		return "", ""
	}

	containerName := ""
	if len(targetPod.Spec.Containers) > 0 {
		containerName = targetPod.Spec.Containers[0].Name
	}

	if needMachine {
		const dmiCmd = "cat /sys/class/dmi/id/product_name 2>/dev/null"
		log.Log.V(1).Info("Probing machine type via DMI",
			"pod", targetPod.Name, "command", dmiCmd)
		output, err := execInPod(ctx, restConfig, namespace, targetPod.Name, containerName,
			[]string{"/bin/sh", "-c", dmiCmd})
		if err != nil {
			log.Log.Error(err, "failed to read machine type from DMI", "pod", targetPod.Name)
		} else {
			machineType = parseMachineTypeFromDMI(output)
			log.Log.V(1).Info("Probed machine type from DMI",
				"pod", targetPod.Name,
				"rawOutput", truncateForLog(output, 200),
				"parsed", machineType)
		}
	}

	if needProduct {
		// Wrap nvidia-smi so the shell always exits 0: if the binary is absent
		// or crashes, stdout is simply empty and we fall through to the sysfs
		// fallback below instead of surfacing an Error-level exec failure.
		nvidiaSmiCmd := `if [ -x /host/usr/bin/nvidia-smi ]; then ` +
			`LD_LIBRARY_PATH=/host/usr/lib/x86_64-linux-gnu:/host/usr/lib/aarch64-linux-gnu:$LD_LIBRARY_PATH ` +
			`/host/usr/bin/nvidia-smi -q 2>/dev/null || true; fi`
		log.Log.V(1).Info("Probing GPU product type via nvidia-smi", "pod", targetPod.Name)
		output, err := execInPod(ctx, restConfig, namespace, targetPod.Name, containerName,
			[]string{"/bin/sh", "-c", nvidiaSmiCmd})
		if err != nil {
			log.Log.Error(err, "failed to exec nvidia-smi probe", "pod", targetPod.Name)
		} else {
			gpuType = parseGPUProductFromNvidiaSmi(output)
			log.Log.V(1).Info("Probed GPU product type via nvidia-smi",
				"pod", targetPod.Name,
				"parsed", gpuType,
				"willFallBackToSysfs", gpuType == "")
		}

		if gpuType == "" {
			log.Log.V(1).Info("Falling back to sysfs/pci.ids for GPU product type", "pod", targetPod.Name)
			sysfsOutput, sysfsErr := execInPod(ctx, restConfig, namespace, targetPod.Name, containerName,
				[]string{"/bin/sh", "-c", sysfsNvidiaGPUIDCmd})
			if sysfsErr != nil {
				log.Log.Error(sysfsErr, "failed to exec sysfs GPU probe", "pod", targetPod.Name)
			} else {
				gpuType = parseGPUProductFromSysfs(sysfsOutput)
				log.Log.V(1).Info("Probed GPU product type via sysfs/pci.ids",
					"pod", targetPod.Name,
					"sysfsID", strings.TrimSpace(sysfsOutput),
					"parsed", gpuType)
				if gpuType == "" && strings.TrimSpace(sysfsOutput) != "" {
					log.Log.Info("GPU product type not resolved from sysfs device ID",
						"pod", targetPod.Name, "unresolvedID", strings.TrimSpace(sysfsOutput))
				}
			}
		}
	}

	return machineType, gpuType
}

// truncateForLog clips a string to maxLen characters and appends "…" when
// the input was longer. Used to keep V(1) probe logs readable for raw
// command output without overwhelming the log volume.
func truncateForLog(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}

// discoverGroupFabric probes the InfiniBand sysfs entries on a representative
// daemon pod for every east-west PF in `group` that has an RdmaDevice and,
// when the per-port verdicts unanimously agree on a confirmed value, sets
// `group.LinkType`. Otherwise the field is left empty — discovery couldn't
// prove the cluster is using a specific fabric, and downstream code treats
// absence as "unknown".
//
// "Confirmed" means the port is ACTIVE and (for InfiniBand) a subnet
// manager is present (sm_lid != 0). Anything else — port down, IB without
// SM, malformed sysfs output — yields no contribution to the group's
// verdict. Reading link_layer alone would be unreliable: that file just
// reflects firmware config and may be a default the cluster doesn't use.
//
// Multi-node groups whose RdmaDevice is empty (per the existing
// per-node-vs-group safety rule) skip the probe — there's no ibdev name
// to point sysfs at — but other PFs in the group can still contribute.
func discoverGroupFabric(ctx context.Context, restConfig *rest.Config,
	namespace string, group *config.ClusterConfig, dsPods []corev1.Pod) {

	if restConfig == nil {
		return
	}
	targetPod := findDaemonPod(group.WorkerNodes, dsPods)
	if targetPod == nil {
		log.Log.V(1).Info("Skipping fabric probe: no daemon pod on group nodes",
			"group", group.Identifier)
		return
	}
	containerName := ""
	if len(targetPod.Spec.Containers) > 0 {
		containerName = targetPod.Spec.Containers[0].Name
	}

	verdicts := map[string]int{} // confirmed linkType -> count
	probed := 0
	for _, pf := range group.PFs {
		if pf.Traffic != "east-west" || pf.RdmaDevice == "" {
			continue
		}
		probed++
		linkType, raw, err := discoverPortFabric(ctx, restConfig, namespace,
			targetPod.Name, containerName, pf.RdmaDevice, 1)
		if err != nil {
			log.Log.V(1).Info("Fabric probe failed",
				"group", group.Identifier,
				"pod", targetPod.Name,
				"pci", pf.PciAddress,
				"rdmaDevice", pf.RdmaDevice,
				"error", err.Error())
			continue
		}
		log.Log.V(1).Info("Fabric port probe",
			"group", group.Identifier,
			"pod", targetPod.Name,
			"pci", pf.PciAddress,
			"rdmaDevice", pf.RdmaDevice,
			"linkType", linkType,
			"raw", raw)
		if linkType != "" {
			verdicts[linkType]++
		}
	}

	switch {
	case len(verdicts) == 1:
		for k := range verdicts {
			group.LinkType = k
			log.Log.V(1).Info("Group fabric resolved",
				"group", group.Identifier,
				"linkType", k,
				"probedPFs", probed,
				"confirmedPFs", verdicts[k])
		}
	case len(verdicts) > 1:
		log.Log.V(1).Info("Group fabric ambiguous (probes disagree); leaving linkType unset",
			"group", group.Identifier,
			"probedPFs", probed,
			"verdicts", verdicts)
	default:
		log.Log.V(1).Info("Group fabric unconfirmed (no port produced a confirmed verdict); leaving linkType unset",
			"group", group.Identifier,
			"probedPFs", probed)
	}
}

// discoverPortFabric reads
// /sys/class/infiniband/<rdmaDevice>/ports/<port>/{state,phys_state,link_layer,sm_lid}
// inside the daemon pod via a single shell exec and returns the confirmed
// fabric for that port (empty when the port could not produce a confirmed
// verdict). rawSummary is a short human-readable joined version of the
// four sysfs values for debug logs.
func discoverPortFabric(ctx context.Context, restConfig *rest.Config,
	namespace, podName, containerName, rdmaDevice string, port int) (string, string, error) {

	base := fmt.Sprintf("/sys/class/infiniband/%s/ports/%d", rdmaDevice, port)
	cmd := fmt.Sprintf(
		"echo state=$(cat %s/state 2>/dev/null); "+
			"echo phys_state=$(cat %s/phys_state 2>/dev/null); "+
			"echo link_layer=$(cat %s/link_layer 2>/dev/null); "+
			"echo sm_lid=$(cat %s/sm_lid 2>/dev/null)",
		base, base, base, base)
	output, err := execInPod(ctx, restConfig, namespace, podName, containerName,
		[]string{"/bin/sh", "-c", cmd})
	if err != nil {
		return "", "", err
	}
	linkType, raw := parsePortFabricVerdict(output)
	return linkType, raw, nil
}

// parsePortFabricVerdict converts the four-line "key=value" output of the
// sysfs probe into a confirmed fabric verdict (or empty when no
// confirmation is possible).
//
// Confirmation rule:
//   - Active + InfiniBand + sm_lid != 0  → "InfiniBand".
//   - Active + Ethernet                  → "Ethernet".
//   - Anything else                      → "" (no confirmation; caller
//     leaves group.LinkType unset).
//
// Active means the state file matches "ACTIVE" (case-insensitive); the
// kernel formats it as "4: ACTIVE", "1: DOWN", etc.
func parsePortFabricVerdict(output string) (linkType, raw string) {
	fields := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		fields[strings.TrimSpace(line[:eq])] = strings.TrimSpace(line[eq+1:])
	}
	state := fields["state"]
	linkLayer := normalizeLinkLayer(fields["link_layer"])
	smLid := fields["sm_lid"]

	raw = fmt.Sprintf("state=%q phys_state=%q link_layer=%q sm_lid=%q",
		state, fields["phys_state"], fields["link_layer"], smLid)

	active := strings.Contains(strings.ToUpper(state), "ACTIVE")
	hasSM := smLidIsNonZero(smLid)

	switch {
	case active && linkLayer == "InfiniBand" && hasSM:
		return "InfiniBand", raw
	case active && linkLayer == "Ethernet":
		return "Ethernet", raw
	default:
		return "", raw
	}
}

// smLidIsNonZero parses a sysfs `sm_lid` value (e.g. "0", "0x0", "0x0000",
// "0x0001") as an unsigned integer and returns true when the value is
// strictly greater than zero. Kernel versions disagree on the format —
// some emit decimal, some emit hex — so we accept both via auto-base
// (base=0 in strconv.ParseUint).
func smLidIsNonZero(s string) bool {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 0, 32)
	if err != nil {
		return false
	}
	return v != 0
}

// normalizeLinkLayer canonicalises sysfs link_layer strings to the YAML
// vocabulary l8k uses elsewhere ("Ethernet" / "InfiniBand"). The kernel
// emits "Ethernet" and "InfiniBand" already, but accept common variants
// (case differences, whitespace) to avoid silent misclassification.
func normalizeLinkLayer(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ethernet":
		return "Ethernet"
	case "infiniband":
		return "InfiniBand"
	default:
		return ""
	}
}

// sysfsNvidiaGPUIDCmd emits the first NVIDIA (vendor 10de) GPU device ID found
// via sysfs. A node is assumed to be homogeneous across its NVIDIA GPUs, so we
// break after the first match. Class filter keeps only VGA (0x030000) and 3D
// controller (0x030200) so NVSwitch / audio / USB-C controllers under vendor
// 10de don't produce false positives. Output is a single line like "0x2335",
// or empty when no NVIDIA GPU is present; exit is always 0.
const sysfsNvidiaGPUIDCmd = `for d in /sys/bus/pci/devices/*; do
  v=$(cat "$d/vendor" 2>/dev/null)
  [ "$v" = "0x10de" ] || continue
  c=$(cat "$d/class" 2>/dev/null)
  case "$c" in 0x030000|0x030200) ;; *) continue ;; esac
  cat "$d/device" 2>/dev/null
  break
done`

// parseGPUProductFromSysfs resolves a single-line sysfs device-ID output
// (e.g. "0x2335\n") to a canonical GPUType via the embedded pci.ids table.
// Returns empty for blank input or an unknown device ID.
func parseGPUProductFromSysfs(output string) string {
	id := strings.TrimSpace(output)
	if id == "" {
		return ""
	}
	// The probe emits at most one ID; take the first line defensively.
	if nl := strings.IndexByte(id, '\n'); nl >= 0 {
		id = strings.TrimSpace(id[:nl])
	}
	return pciids.LookupNVIDIA(id)
}

// findDaemonPod returns a nic-configuration-daemon pod running on one of the
// given nodes. Returns nil if no pod is found on any of the nodes.
func findDaemonPod(groupNodes []string, dsPods []corev1.Pod) *corev1.Pod {
	nodeSet := make(map[string]bool, len(groupNodes))
	for _, n := range groupNodes {
		nodeSet[n] = true
	}
	for i := range dsPods {
		if nodeSet[dsPods[i].Spec.NodeName] {
			return &dsPods[i]
		}
	}
	return nil
}

// discoverThirdPartyRDMAModules execs into a nic-configuration-daemon pod on one
// of the group's nodes and discovers third-party RDMA modules that depend on
// OFED target modules via the kernel module holder graph.
func discoverThirdPartyRDMAModules(ctx context.Context, restConfig *rest.Config,
	namespace string, groupNodes []string, dsPods []corev1.Pod) ([]string, error) {

	if len(groupNodes) == 0 {
		return nil, nil
	}

	// Find a daemon pod running on one of the group's nodes.
	// All nodes in a group have identical hardware, so one is sufficient.
	targetPod := findDaemonPod(groupNodes, dsPods)
	if targetPod == nil {
		return nil, fmt.Errorf("no nic-configuration-daemon pod found on group nodes")
	}

	// Build a shell script that does full transitive BFS discovery of
	// third-party RDMA modules. It scans ALL /sys/module/*/holders/ to build a
	// complete reverse dependency map, then BFS-traverses from OFED target
	// modules upward through non-OFED holders. This matches the init
	// container's checker.go approach.
	modList := strings.Join(ofedTargetModules, " ")
	script := fmt.Sprintf(`targets="%s"
awk_script='
BEGIN {
  split(ENVIRON["TARGETS"], arr)
  for (i in arr) target[arr[i]] = 1
}
{
  mod = $1; holder = $2
  holders[mod] = holders[mod] " " holder
}
END {
  n = 0
  for (t in target) {
    split(holders[t], h)
    for (i in h) {
      if (h[i] != "" && !(h[i] in target) && !(h[i] in visited)) {
        visited[h[i]] = 1
        queue[n++] = h[i]
      }
    }
  }
  qi = 0
  while (qi < n) {
    cur = queue[qi++]
    split(holders[cur], h)
    for (i in h) {
      if (h[i] != "" && !(h[i] in target) && !(h[i] in visited)) {
        visited[h[i]] = 1
        queue[n++] = h[i]
      }
    }
  }
  for (m in visited) print m
}'
for mod_dir in /sys/module/*/; do
  mod=$(basename "$mod_dir")
  for dep in "$mod_dir"holders/*; do
    [ -e "$dep" ] && echo "$mod $(basename "$dep")"
  done
done | TARGETS="$targets" awk "$awk_script" | sort -u`, modList)

	containerName := ""
	if len(targetPod.Spec.Containers) > 0 {
		containerName = targetPod.Spec.Containers[0].Name
	}

	log.Log.V(1).Info("Probing OFED-dependent kernel modules",
		"pod", targetPod.Name, "ofedTargets", ofedTargetModules)
	output, err := execInPod(ctx, restConfig, namespace, targetPod.Name, containerName,
		[]string{"/bin/sh", "-c", script})
	if err != nil {
		return nil, fmt.Errorf("exec in pod %q failed: %w", targetPod.Name, err)
	}
	modules := parseModuleList(output, ofedTargetModules)
	log.Log.V(1).Info("Discovered OFED-dependent kernel modules",
		"pod", targetPod.Name,
		"rawOutput", truncateForLog(output, 200),
		"parsed", modules)
	return modules, nil
}

// parseModuleList splits newline-separated module names, deduplicates, sorts them,
// and filters out any modules in the exclude list (the OFED target modules themselves).
func parseModuleList(output string, exclude []string) []string {
	excludeSet := make(map[string]bool, len(exclude))
	for _, m := range exclude {
		excludeSet[m] = true
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		mod := strings.TrimSpace(line)
		if mod != "" && !excludeSet[mod] {
			seen[mod] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	modules := make([]string, 0, len(seen))
	for mod := range seen {
		modules = append(modules, mod)
	}
	sort.Strings(modules)
	return modules
}

// coreRdmaInfrastructureModules is the set of kernel-native RDMA core
// modules that MOFED's openibd unload sequence handles natively. Per
// upstream guidance in `mofedmodules.DefaultThirdPartyRDMAModules`'s
// doc comment ("Do NOT add core RDMA infrastructure modules (iw_cm,
// ib_cm, rdma_cm, rdma_ucm, ib_core, ib_uverbs, etc.)"), these are NOT
// third-party — they're shared kernel infrastructure that the driver
// container does not need to unload separately. l8k discovery silently
// drops them so they never appear as `thirdPartyRDMAModules` in
// cluster-config.yaml or trigger the `unloadThirdPartyRDMAModules`
// auto-enable + warning.
var coreRdmaInfrastructureModules = map[string]bool{
	"iw_cm":     true,
	"ib_cm":     true,
	"rdma_cm":   true,
	"rdma_ucm":  true,
	"ib_core":   true,
	"ib_uverbs": true,
}

// classifyDiscoveredModules splits a list of discovered OFED-dependent modules into
// third-party RDMA modules and storage modules. mlx5-prefixed modules (NVIDIA's own)
// and kernel-native RDMA core modules are silently dropped.
func classifyDiscoveredModules(modules []string) (rdma, storage []string) {
	var droppedMlx, droppedCore []string
	for _, mod := range modules {
		if strings.HasPrefix(mod, "mlx5") {
			droppedMlx = append(droppedMlx, mod)
			continue // NVIDIA module — always greenlit
		}
		if coreRdmaInfrastructureModules[mod] {
			droppedCore = append(droppedCore, mod)
			continue // Kernel-native RDMA core — MOFED's openibd handles it
		}
		if knownStorageModules[mod] {
			storage = append(storage, mod)
		} else {
			rdma = append(rdma, mod)
		}
	}
	log.Log.V(1).Info("Classified OFED-dependent modules",
		"total", len(modules),
		"thirdPartyRDMA", rdma,
		"storage", storage,
		"droppedMlx5Prefixed", droppedMlx,
		"droppedCoreRdma", droppedCore)
	return rdma, storage
}

// execInPod runs a command in a pod container and returns stdout.
func execInPod(ctx context.Context, restConfig *rest.Config,
	namespace, podName, containerName string, command []string) (string, error) {

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return "", fmt.Errorf("failed to create clientset: %w", err)
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("failed to create SPDY executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return "", fmt.Errorf("exec stream failed (stderr: %s): %w", stderr.String(), err)
	}

	return stdout.String(), nil
}
