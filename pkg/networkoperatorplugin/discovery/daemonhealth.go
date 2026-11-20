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
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/nicconfigdaemon"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// newBootstrapDaemonObserver reports failure evidence only for nodes still
// missing NicDevices. Pod readiness was a snapshot before the sysfs probes;
// later restarts are diagnostics, not proof that a publisher cannot recover.
func newBootstrapDaemonObserver(c client.Client, output ui.Output) func(context.Context, []string) {
	previous := map[string]string{}
	lastInspectionError := ""
	showedHint := false
	return func(ctx context.Context, pendingNodes []string) {
		if len(pendingNodes) == 0 {
			return
		}
		inspectionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		pods := &corev1.PodList{}
		if err := c.List(inspectionCtx, pods, client.InNamespace(nicconfigdaemon.Namespace)); err != nil {
			if message := err.Error(); message != lastInspectionError {
				output.Warning("Could not inspect discovery daemon health while waiting for NicDevice resources: %v; daemon health is unverified", err)
				lastInspectionError = message
			}
			return
		}
		lastInspectionError = ""
		pending := make(map[string]bool, len(pendingNodes))
		for _, node := range pendingNodes {
			pending[node] = true
		}
		slices.SortFunc(pods.Items, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
		current := map[string]string{}
		for _, pod := range pods.Items {
			if !pending[pod.Spec.NodeName] || !bootstrapDaemonPod(pod) {
				continue
			}
			statuses := append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...)
			statuses = append(statuses, pod.Status.ContainerStatuses...)
			for _, status := range statuses {
				failure := daemonContainerFailure(status)
				if failure == "" {
					continue
				}
				key := string(pod.UID) + "/" + pod.Name + "/" + status.Name
				current[key] = failure
				if previous[key] == failure {
					continue
				}
				output.Warning("Discovery daemon on %s, pod %s, container %s: %s (restarts: %d); NicDevice publication is still pending",
					pod.Spec.NodeName, pod.Name, status.Name, failure, status.RestartCount)
				if !showedHint {
					output.Warning("Inspect daemon logs with kubectl logs -n %s %s -c %s (add --previous for the prior container); re-run discovery with --keep-namespace to retain bootstrap resources",
						nicconfigdaemon.Namespace, pod.Name, status.Name)
					showedHint = true
				}
			}
		}
		previous = current
	}
}

func bootstrapDaemonPod(pod corev1.Pod) bool {
	if pod.Namespace != nicconfigdaemon.Namespace {
		return false
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" && owner.Name == nicconfigdaemon.DaemonSetName {
			return true
		}
	}
	return false
}

func daemonContainerFailure(status corev1.ContainerStatus) string {
	termination := status.State.Terminated
	if waiting := status.State.Waiting; waiting != nil && stuckWaitingReasons[waiting.Reason] {
		failure := "waiting: " + waiting.Reason
		if last := status.LastTerminationState.Terminated; last != nil {
			failure += fmt.Sprintf("; last termination: %s (exit code %d)", last.Reason, last.ExitCode)
		}
		return failure
	}
	if termination != nil {
		if termination.ExitCode == 0 {
			return ""
		}
		return fmt.Sprintf("terminated: %s (exit code %d)", termination.Reason, termination.ExitCode)
	}
	if last := status.LastTerminationState.Terminated; status.RestartCount > 0 && last != nil && last.ExitCode != 0 {
		return fmt.Sprintf("restarted; last termination: %s (exit code %d)", last.Reason, last.ExitCode)
	}
	return ""
}
