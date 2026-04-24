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
		expectedNodes, dsPods, err = checkDaemonSetPodsReady(ctx, c, defaultConfig.NetworkOperator.Namespace, "nic-configuration-daemon")
		if err != nil {
			// Try the other common namespace before giving up
			alternateNS := alternateNamespace(defaultConfig.NetworkOperator.Namespace)
			if alternateNS != "" {
				uiOutput.Info("Retrying in namespace %q", alternateNS)
				expectedNodes, dsPods, err = checkDaemonSetPodsReady(ctx, c, alternateNS, "nic-configuration-daemon")
				if err == nil {
					defaultConfig.NetworkOperator.Namespace = alternateNS
				}
			}
			if err != nil {
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
			needProduct := group.ProductType == ""
			if needMachine || needProduct {
				machine, product := discoverHardwareTypes(ctx, p.RESTConfig,
					defaultConfig.NetworkOperator.Namespace, group.WorkerNodes, dsPods,
					needMachine, needProduct)
				if needMachine && machine != "" {
					group.MachineType = machine
					uiOutput.Info("Discovered machine type for group %s: %s", group.Identifier, machine)
				}
				if needProduct && product != "" {
					group.ProductType = product
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

			// Try to enrich with a predefined topology preset for this machine type.
			// Presets provide authoritative traffic classification, rail assignments,
			// and NUMA/GPU topology metadata for known hardware configurations.
			if group.MachineType != "" {
				preset, presetErr := presets.LoadPreset(group.MachineType)
				if presetErr != nil {
					log.Log.Error(presetErr, "failed to load preset", "machineType", group.MachineType)
					uiOutput.Warning("Failed to load preset for %s: %v", group.MachineType, presetErr)
				} else if preset != nil {
					if presetErr := presets.ValidatePreset(preset, group.PFs); presetErr != nil {
						log.Log.Info("Preset did not match discovered hardware",
							"machineType", group.MachineType, "error", presetErr)
						uiOutput.Info("Preset for %s did not match discovered hardware: %v", group.MachineType, presetErr)
					} else {
						presets.ApplyPreset(preset, group)
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

	return nil
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
	progress := uiOutput.StartProgress(fmt.Sprintf("Waiting for %s pods (timeout: %s)", daemonSetName, timeout.Truncate(time.Second)))

	ctx := parentCtx
	if _, hasDeadline := parentCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parentCtx, timeout)
		defer cancel()
	}

	altNS := alternateNamespace(namespace)
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
				progress.Success(fmt.Sprintf("Found %d pod(s) in namespace %q", len(pods), altNS))
				return nodes, pods, altNS, nil
			}
		}

		select {
		case <-ctx.Done():
			progress.Fail("Timeout waiting for daemon pods")
			return nil, nil, "", fmt.Errorf("timeout waiting for %s pods to start: %w", daemonSetName, lastErr)
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
	PSID             string
	PartNumber       string
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

// classifyByPartNumberFrequency applies a heuristic for nodes with 5+ PFs:
// the most common part number (preferring power-of-2 counts) is east-west,
// and all other part numbers are reclassified as north-south.
// PFs already marked north-south by product ID matching are not changed.
// Returns warning messages for the caller to display to the user.
func classifyByPartNumberFrequency(nodeMap map[string]*nodeInfo) []string {
	var warnings []string
	for nodeName, ni := range nodeMap {
		if len(ni.pfs) < 5 {
			continue
		}

		// Count PFs by part number (skip empty)
		partCounts := map[string]int{}
		for _, pf := range ni.pfs {
			if pf.PartNumber != "" {
				partCounts[pf.PartNumber]++
			}
		}
		if len(partCounts) <= 1 {
			// All PFs have the same (or empty) part number — nothing to classify
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
		// Match part number against known DPU product IDs for north-south classification.
		northSouth := isNorthSouthDevice(d.Status.PartNumber)
		for _, p := range d.Status.Ports {
			entry := nodePFEntry{
				pfFingerprint: pfFingerprint{
					DeviceID:   d.Status.Type,
					PciAddress: p.PCI,
				},
				RdmaDevice:       p.RdmaInterface,
				NetworkInterface: p.NetworkInterface,
				IsNorthSouth:     northSouth,
				PSID:             d.Status.PSID,
				PartNumber:       d.Status.PartNumber,
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
		productType := commonLabels["nvidia.com/gpu.product"]

		cc := config.ClusterConfig{
			Identifier:    identifier,
			MachineType:   machineType,
			ProductType:   productType,
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

// discoverHardwareTypes attempts to discover machineType and productType by
// probing hardware directly when label-derived values are empty. It execs into
// a nic-configuration-daemon pod on one of the group's nodes.
// Returns (machineType, productType) — either or both may still be empty if
// discovery fails or hardware info is unavailable.
func discoverHardwareTypes(ctx context.Context, restConfig *rest.Config,
	namespace string, groupNodes []string, dsPods []corev1.Pod,
	needMachine, needProduct bool) (machineType, productType string) {

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
		output, err := execInPod(ctx, restConfig, namespace, targetPod.Name, containerName,
			[]string{"/bin/sh", "-c", "cat /sys/class/dmi/id/product_name 2>/dev/null"})
		if err != nil {
			log.Log.Error(err, "failed to read machine type from DMI", "pod", targetPod.Name)
		} else {
			machineType = parseMachineTypeFromDMI(output)
		}
	}

	if needProduct {
		// Wrap nvidia-smi so the shell always exits 0: if the binary is absent
		// or crashes, stdout is simply empty and we fall through to the sysfs
		// fallback below instead of surfacing an Error-level exec failure.
		nvidiaSmiCmd := `if [ -x /host/usr/bin/nvidia-smi ]; then ` +
			`LD_LIBRARY_PATH=/host/usr/lib/x86_64-linux-gnu:/host/usr/lib/aarch64-linux-gnu:$LD_LIBRARY_PATH ` +
			`/host/usr/bin/nvidia-smi -q 2>/dev/null || true; fi`
		output, err := execInPod(ctx, restConfig, namespace, targetPod.Name, containerName,
			[]string{"/bin/sh", "-c", nvidiaSmiCmd})
		if err != nil {
			log.Log.Error(err, "failed to exec nvidia-smi probe", "pod", targetPod.Name)
		} else {
			productType = parseGPUProductFromNvidiaSmi(output)
		}

		if productType == "" {
			sysfsOutput, sysfsErr := execInPod(ctx, restConfig, namespace, targetPod.Name, containerName,
				[]string{"/bin/sh", "-c", sysfsNvidiaGPUIDCmd})
			if sysfsErr != nil {
				log.Log.Error(sysfsErr, "failed to exec sysfs GPU probe", "pod", targetPod.Name)
			} else {
				productType = parseGPUProductFromSysfs(sysfsOutput)
				if productType == "" && strings.TrimSpace(sysfsOutput) != "" {
					log.Log.Info("GPU product type not resolved from sysfs device ID",
						"pod", targetPod.Name, "unresolvedID", strings.TrimSpace(sysfsOutput))
				}
			}
		}
	}

	return machineType, productType
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
// (e.g. "0x2335\n") to a canonical ProductType via the embedded pci.ids table.
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

	output, err := execInPod(ctx, restConfig, namespace, targetPod.Name, containerName,
		[]string{"/bin/sh", "-c", script})
	if err != nil {
		return nil, fmt.Errorf("exec in pod %q failed: %w", targetPod.Name, err)
	}

	return parseModuleList(output, ofedTargetModules), nil
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

// classifyDiscoveredModules splits a list of discovered OFED-dependent modules into
// third-party RDMA modules and storage modules. mlx5-prefixed modules (NVIDIA's own)
// are silently dropped.
func classifyDiscoveredModules(modules []string) (rdma, storage []string) {
	for _, mod := range modules {
		if strings.HasPrefix(mod, "mlx5") {
			continue // NVIDIA module — always greenlit
		}
		if knownStorageModules[mod] {
			storage = append(storage, mod)
		} else {
			rdma = append(rdma, mod)
		}
	}
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
