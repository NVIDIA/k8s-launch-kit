// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The daemon shares the host PID namespace, so PID 1's root is the host
// filesystem. Query the host package manager, never the daemon image's DB.
// Enumerating the DB successfully before reporting absence distinguishes a
// missing package from unreadable/corrupt package metadata. No host writes occur.
const docaHostProbeCommand = `
if command -v dpkg-query >/dev/null 2>&1; then
  packages=$(dpkg-query -W -f='${Package}\t${Status}\t${Version}\n') || exit 1
  printf '%s\n' "$packages" | awk -F '\t' '
    $1 == "doca-host" { found=1; if ($2 ~ / ok installed$/) print "installed\t" $3; else if ($2 ~ / ok config-files$/) print "not-installed"; else exit 1 }
    END { if (!found) print "not-installed" }'
elif command -v rpm >/dev/null 2>&1; then
  packages=$(rpm -qa --qf '%{NAME}\t%{VERSION}-%{RELEASE}\n') || exit 1
  printf '%s\n' "$packages" | awk -F '\t' '
    $1 == "doca-host" { found=1; print "installed\t" $2 }
    END { if (!found) print "not-installed" }'
else
  exit 1
fi`

func parseDOCAHostObservation(node, output string) (config.DOCAHostObservation, error) {
	fields := strings.Split(strings.TrimSpace(output), "\t")
	if len(fields) == 1 && fields[0] == config.DOCAHostNotInstalled {
		return config.DOCAHostObservation{Node: node, Status: config.DOCAHostNotInstalled}, nil
	}
	if len(fields) == 2 && fields[0] == config.DOCAHostInstalled && fields[1] != "" && !strings.ContainsAny(fields[1], "\r\n ") {
		return config.DOCAHostObservation{Node: node, Status: config.DOCAHostInstalled, Version: fields[1]}, nil
	}
	return config.DOCAHostObservation{}, fmt.Errorf("invalid host doca-host package observation")
}

const docaHostProbeTimeout = 30 * time.Second
const docaHostProbeConcurrency = 4

func discoverDOCAHost(ctx context.Context, c client.Client, restConfig *rest.Config, groups []config.ClusterConfig,
	pods []corev1.Pod, exec readOnlyPodExec,
) ([]config.DOCAHostObservation, error) {
	return collectDOCAHost(ctx, c, restConfig, groups, pods, exec, docaHostProbeTimeout)
}

func collectDOCAHost(ctx context.Context, c client.Client, restConfig *rest.Config, groups []config.ClusterConfig,
	pods []corev1.Pod, exec readOnlyPodExec, timeout time.Duration,
) ([]config.DOCAHostObservation, error) {
	if exec == nil {
		exec = execProbeInPod // shared bounded stdout/stderr diagnostics
	}
	observations := config.SelectedDOCAHost(&config.LaunchKitConfig{ClusterConfig: groups})
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(docaHostProbeConcurrency, len(observations)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					return
				}
				probeCtx, cancel := context.WithTimeout(ctx, timeout)
				observations[i] = observeDOCAHost(probeCtx, c, restConfig, observations[i].Node, pods, exec)
				cancel()
			}
		}()
	}
dispatch:
	for i := range observations {
		select {
		case <-ctx.Done():
			break dispatch
		case jobs <- i:
		}
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// UI implementations may not support concurrent writes. Emit diagnostics
	// only after workers stop, in the same sorted node order as the inventory.
	for _, observation := range observations {
		if observation.Status == config.DOCAHostUnknown {
			ui.FromContext(ctx).Warning("Host DOCA-HOST package observation unavailable on %s: %s", observation.Node, observation.Reason)
		}
	}
	return observations, nil
}

func observeDOCAHost(ctx context.Context, c client.Client, restConfig *rest.Config, node string,
	pods []corev1.Pod, exec readOnlyPodExec,
) config.DOCAHostObservation {
	observation := config.DOCAHostObservation{Node: node, Status: config.DOCAHostUnknown}
	pod := findDaemonPod([]string{node}, pods)
	var err error
	if restConfig == nil {
		err = fmt.Errorf("REST credentials unavailable")
	} else if pod == nil || !pod.Spec.HostPID {
		err = fmt.Errorf("no same-node daemon sharing the host PID namespace")
	} else {
		// execProbeChecked verifies source UID, readiness, container restart
		// count and host sysfs before and after the read-only command.
		output, probeErr := execProbeChecked(ctx, c, restConfig,
			podProbeSource{pod: *pod, container: podContainerName(pod)}, exec,
			[]string{"chroot", "/proc/1/root", "/bin/sh", "-c", docaHostProbeCommand})
		if probeErr == nil {
			var parsed config.DOCAHostObservation
			parsed, probeErr = parseDOCAHostObservation(node, output)
			if probeErr == nil {
				return parsed
			}
		}
		err = probeErr
	}
	observation.Reason = truncateForLog(err.Error(), 500)
	return observation
}
