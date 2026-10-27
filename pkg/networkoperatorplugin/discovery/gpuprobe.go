// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const gpuProbeTimeout = 30 * time.Second
const gpuProbeConcurrency = 4
const gfdContainer = "gpu-feature-discovery"

func execGPUInPod(ctx context.Context, restConfig *rest.Config, namespace, pod, container string, command []string) (string, error) {
	result, err := kubeclient.ExecInPod(ctx, restConfig, namespace, pod, container, command)
	log.Log.V(2).Info("GPU probe output", "namespace", namespace, "pod", pod, "container", container,
		"stdout", truncateForLog(result.Stdout, 500), "stderr", truncateForLog(result.Stderr, 500))
	return result.Stdout, err
}

type nodeGPUObservation struct {
	host    gpuHostObservation
	data    topoData
	product string
	cpuOnly bool
	err     error
}

type gpuSource struct {
	pod       corev1.Pod
	container string
}

// hasHostSys checks the actual source volume, not a conventional volume name.
// Subpath or nested mounts could hide physical devices and invalidate coverage.
func hasHostSys(pod *corev1.Pod, container string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name != container {
			continue
		}
		found := false
		for _, mount := range c.VolumeMounts {
			if strings.HasPrefix(mount.MountPath, "/sys/") {
				return false
			}
			if mount.MountPath != "/sys" {
				continue
			}
			if mount.SubPath != "" || mount.SubPathExpr != "" {
				return false
			}
			for _, volume := range pod.Spec.Volumes {
				if volume.Name == mount.Name && volume.HostPath != nil && volume.HostPath.Path == "/sys" {
					found = true
				}
			}
		}
		return found
	}
	return false
}

func readyGPUContainer(pod *corev1.Pod, container string) bool {
	if pod.UID == "" || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	ready := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == container && status.Ready && status.State.Running != nil {
			return ready
		}
	}
	return false
}

func findGFDSources(ctx context.Context, c client.Client, namespace string) (map[string][]gpuSource, error) {
	pods := &corev1.PodList{}
	options := []client.ListOption{client.MatchingLabels{"app": gfdContainer, "app.kubernetes.io/part-of": "nvidia-gpu"}}
	if namespace != "" {
		options = append(options, client.InNamespace(namespace))
	}
	if err := c.List(ctx, pods, options...); err != nil {
		return nil, fmt.Errorf("list GPU feature discovery pods: %w", err)
	}
	result := map[string][]gpuSource{}
	owners := map[client.ObjectKey]*appsv1.DaemonSet{}
	for _, pod := range pods.Items {
		if !readyGPUContainer(&pod, gfdContainer) || !hasHostSys(&pod, gfdContainer) {
			continue
		}
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || owner.Kind != "DaemonSet" || owner.APIVersion != "apps/v1" || owner.Name != gfdContainer || owner.UID == "" {
			continue
		}
		key := client.ObjectKey{Namespace: pod.Namespace, Name: owner.Name}
		ds, found := owners[key]
		if !found {
			ds = &appsv1.DaemonSet{}
			if err := c.Get(ctx, key, ds); err != nil {
				return nil, fmt.Errorf("inspect GPU feature discovery owner: %w", err)
			}
			owners[key] = ds
		}
		if ds.UID != owner.UID || ds.DeletionTimestamp != nil {
			continue
		}
		result[pod.Spec.NodeName] = append(result[pod.Spec.NodeName], gpuSource{pod: pod, container: gfdContainer})
	}
	return result, nil
}

// execGPUChecked rejects replacement pods and container restarts across an
// observation. API failures are optional-observation failures, not empty data.
func execGPUChecked(ctx context.Context, c client.Client, restConfig *rest.Config, source gpuSource, exec readOnlyPodExec, command []string) (string, error) {
	before := &corev1.Pod{}
	key := client.ObjectKeyFromObject(&source.pod)
	if err := c.Get(ctx, key, before); err != nil {
		return "", err
	}
	if before.UID != source.pod.UID || before.Spec.NodeName != source.pod.Spec.NodeName || !readyGPUContainer(before, source.container) || !hasHostSys(before, source.container) {
		return "", fmt.Errorf("GPU observation source changed or is not ready with host sysfs")
	}
	output, err := exec(ctx, restConfig, key.Namespace, key.Name, source.container, command)
	if err != nil {
		return "", err
	}
	after := &corev1.Pod{}
	if err := c.Get(ctx, key, after); err != nil {
		return "", err
	}
	if before.UID != after.UID || !readyGPUContainer(after, source.container) {
		return "", fmt.Errorf("GPU observation source disappeared or restarted")
	}
	for _, previous := range before.Status.ContainerStatuses {
		if previous.Name != source.container {
			continue
		}
		for _, current := range after.Status.ContainerStatuses {
			if current.Name == source.container && (current.RestartCount != previous.RestartCount || current.ContainerID != previous.ContainerID) {
				return "", fmt.Errorf("GPU observation container restarted")
			}
		}
	}
	return output, ctx.Err()
}

func discoverGPUObservations(ctx context.Context, c client.Client, restConfig *rest.Config, gpuNamespace string,
	groups []config.ClusterConfig, nodeLabels map[string]map[string]string, devices []nicop.NicDevice, members groupPFMembers, pods []corev1.Pod, exec readOnlyPodExec,
) (map[string]nodeGPUObservation, []nodePFIdentity, error) {
	return collectGPUObservations(ctx, c, restConfig, gpuNamespace, groups, nodeLabels, devices, members, pods, exec, gpuProbeTimeout)
}

func collectGPUObservations(ctx context.Context, c client.Client, restConfig *rest.Config, gpuNamespace string,
	groups []config.ClusterConfig, nodeLabels map[string]map[string]string, devices []nicop.NicDevice, members groupPFMembers, pods []corev1.Pod, exec readOnlyPodExec, timeout time.Duration,
) (map[string]nodeGPUObservation, []nodePFIdentity, error) {
	if exec == nil {
		exec = execGPUInPod
	}
	identities := buildPFIdentities(groups, devices, members, true)
	byNode, knownGPU := map[string][]string{}, map[string]bool{}
	for _, identity := range identities {
		byNode[identity.node] = append(byNode[identity.node], identity.pci)
		knownGPU[identity.node] = knownGPU[identity.node] || groups[identity.groupIndex].GPUType != "" || nodeLabels[identity.node]["nvidia.com/gpu.present"] == "true" || nodeLabels[identity.node]["nvidia.com/gpu.product"] != ""
	}
	nodes := make([]string, 0, len(byNode))
	for node, pcis := range byNode {
		nodes = append(nodes, node)
		slices.Sort(pcis)
		byNode[node] = slices.Compact(pcis)
	}
	slices.Sort(nodes)
	// The first node needing GFD performs one bounded list for this invocation.
	var sourceOnce sync.Once
	var sources map[string][]gpuSource
	var sourceErr error
	var sourceWorkers sync.WaitGroup
	sourceReady := make(chan struct{})
	sourceCtx, cancelSources := context.WithCancel(ctx)
	defer func() { cancelSources(); sourceWorkers.Wait() }()
	getSource := func(nodeCtx context.Context, node string) (gpuSource, error) {
		sourceOnce.Do(func() {
			sourceWorkers.Add(1)
			go func() {
				defer sourceWorkers.Done()
				defer close(sourceReady)
				lookupCtx, cancel := context.WithTimeout(sourceCtx, timeout)
				defer cancel()
				sources, sourceErr = findGFDSources(lookupCtx, c, gpuNamespace)
			}()
		})
		select {
		case <-nodeCtx.Done():
			return gpuSource{}, nodeCtx.Err()
		case <-sourceReady:
		}
		if sourceErr != nil {
			return gpuSource{}, sourceErr
		}
		if len(sources[node]) != 1 {
			return gpuSource{}, fmt.Errorf("expected one ready same-node GPU feature discovery source, found %d", len(sources[node]))
		}
		return sources[node][0], nil
	}
	results := make([]nodeGPUObservation, len(nodes))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(gpuProbeConcurrency, len(nodes)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				nodeCtx, cancel := context.WithTimeout(ctx, timeout)
				pod := findDaemonPod([]string{nodes[i]}, pods)
				if pod == nil {
					results[i].err = fmt.Errorf("no same-node NIC daemon")
				} else {
					results[i] = observeNodeGPU(nodeCtx, c, restConfig, *pod, byNode[nodes[i]], knownGPU[nodes[i]], getSource, exec)
					if results[i].product == "" && !results[i].cpuOnly && nodeCtx.Err() == nil {
						// Resolve missing products on their own node before group
						// aggregation. A later representative fallback would erase
						// a known disagreement and could select an unrelated preset.
						source := gpuSource{pod: *pod, container: podContainerName(pod)}
						output, err := execGPUChecked(nodeCtx, c, restConfig, source, exec, []string{"/bin/sh", "-c", sysfsNvidiaGPUIDCmd})
						if err == nil {
							results[i].product = parseGPUProductFromSysfs(output)
						} else {
							log.Log.V(1).Info("GPU product sysfs fallback unavailable", "node", nodes[i], "reason", truncateForLog(err.Error(), 500))
						}
					}
				}
				if nodeCtx.Err() != nil {
					results[i].err = nodeCtx.Err()
				}
				cancel()
			}
		}()
	}
dispatch:
	for i := range nodes {
		select {
		case <-ctx.Done():
			break dispatch
		case jobs <- i:
		}
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return nil, identities, err
	}
	observations := map[string]nodeGPUObservation{}
	for i, node := range nodes {
		observations[node] = results[i]
		if results[i].err != nil {
			log.Log.V(1).Info("GPU observation unavailable", "node", node, "reason", truncateForLog(results[i].err.Error(), 500))
		}
	}
	return observations, identities, nil
}

func observeNodeGPU(ctx context.Context, c client.Client, restConfig *rest.Config, pod corev1.Pod, pcis []string, knownGPU bool,
	getSource func(context.Context, string) (gpuSource, error), exec readOnlyPodExec) nodeGPUObservation {
	result := nodeGPUObservation{}
	source := gpuSource{pod: pod, container: podContainerName(&pod)}
	for _, pci := range pcis {
		if !fabricPCIRE.MatchString(pci) {
			result.err = fmt.Errorf("invalid expected NIC PCI identity")
			return result
		}
	}
	command := append([]string{"/bin/sh", "-c", gpuHostProbeCmd, "l8k-gpu-host", "/sys/bus/pci/devices"}, pcis...)
	hostOutput, hostErr := execGPUChecked(ctx, c, restConfig, source, exec, command)
	if hostErr == nil {
		result.host, hostErr = parseGPUHostObservation(hostOutput)
	}
	if hostErr == nil && len(result.host.gpus) == 0 && !knownGPU {
		result.cpuOnly = true
		return result
	}
	if hostErr == nil && len(result.host.gpus) == 0 {
		hostErr = fmt.Errorf("physical GPU inventory is empty despite positive GPU evidence")
	}
	// Even when host coverage is unknown, preserve the unlabeled host-driver
	// product fallback. Topology still cannot be accepted without the denominator.
	for attempt := 0; attempt < 3; attempt++ {
		mode := "local"
		if attempt == 1 {
			mode = "host"
		}
		if attempt == 2 {
			var err error
			source, err = getSource(ctx, pod.Spec.NodeName)
			if err != nil {
				result.err = fmt.Errorf("local GPU observation: %v; GPU feature discovery unavailable: %w", result.err, err)
				break
			}
		}
		output, err := execGPUChecked(ctx, c, restConfig, source, exec, []string{"/bin/sh", "-c", gpuObservationCmd, "l8k-gpu", mode})
		if err == nil {
			product, data, parseErr := parseGPUObservation(output, result.host)
			if product != "" {
				result.product = product
			}
			result.data, err = data, parseErr
		}
		if hostErr != nil && result.product != "" {
			result.err = fmt.Errorf("host physical GPU inventory unavailable: %w", hostErr)
			return result
		}
		if err == nil && hostErr == nil {
			result.err = nil
			log.Log.V(1).Info("GPU topology source", "node", pod.Spec.NodeName, "namespace", source.pod.Namespace, "pod", source.pod.Name, "uid", source.pod.UID, "container", source.container)
			return result
		}
		result.err = err
		if ctx.Err() != nil {
			return result
		}
	}
	if hostErr != nil {
		result.err = fmt.Errorf("host physical GPU inventory unavailable: %w", hostErr)
	}
	return result
}
