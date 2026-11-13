// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"fmt"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/kubeclient"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func execProbeInPod(ctx context.Context, restConfig *rest.Config, namespace, pod, container string, command []string) (string, error) {
	result, err := kubeclient.ExecInPod(ctx, restConfig, namespace, pod, container, command)
	log.Log.V(2).Info("pod probe output", "namespace", namespace, "pod", pod, "container", container,
		"stdout", truncateForLog(result.Stdout, 500), "stderr", truncateForLog(result.Stderr, 500))
	return result.Stdout, err
}

type podProbeSource struct {
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

func readyProbeContainer(pod *corev1.Pod, container string) bool {
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

// execProbeChecked rejects replacement pods and container restarts across an
// observation. API failures are optional-observation failures, not empty data.
func execProbeChecked(ctx context.Context, c client.Client, restConfig *rest.Config, source podProbeSource, exec readOnlyPodExec, command []string) (string, error) {
	before := &corev1.Pod{}
	key := client.ObjectKeyFromObject(&source.pod)
	if err := c.Get(ctx, key, before); err != nil {
		return "", err
	}
	if before.UID != source.pod.UID || before.Spec.NodeName != source.pod.Spec.NodeName || !readyProbeContainer(before, source.container) || !hasHostSys(before, source.container) {
		return "", fmt.Errorf("pod observation source changed or is not ready with host sysfs")
	}
	output, err := exec(ctx, restConfig, key.Namespace, key.Name, source.container, command)
	if err != nil {
		return "", err
	}
	after := &corev1.Pod{}
	if err := c.Get(ctx, key, after); err != nil {
		return "", err
	}
	if before.UID != after.UID || !readyProbeContainer(after, source.container) {
		return "", fmt.Errorf("pod observation source disappeared or restarted")
	}
	for _, previous := range before.Status.ContainerStatuses {
		if previous.Name != source.container {
			continue
		}
		for _, current := range after.Status.ContainerStatuses {
			if current.Name == source.container && (current.RestartCount != previous.RestartCount || current.ContainerID != previous.ContainerID) {
				return "", fmt.Errorf("pod observation container restarted")
			}
		}
	}
	return output, ctx.Err()
}
