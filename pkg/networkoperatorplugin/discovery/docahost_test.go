// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type docaHostWarnings struct {
	ui.Output
	warnings []string
}

func (output *docaHostWarnings) Warning(format string, args ...interface{}) {
	output.warnings = append(output.warnings, fmt.Sprintf(format, args...))
}

type docaHostCollection struct {
	observations []config.DOCAHostObservation
	err          error
}

func TestDOCAHostCollectionBoundsConcurrencyAndOrdersDiagnostics(t *testing.T) {
	groups, _, _, pods := fabricFixtures(docaHostProbeConcurrency+2, true)
	release := map[string]chan struct{}{}
	for i := range pods {
		readyGPUFixture(&pods[i])
		pods[i].Spec.HostPID = true
		release[pods[i].Name] = make(chan struct{})
	}
	c := gpuFixtureClient(t, pods)
	output := &docaHostWarnings{Output: ui.NewSilent()}
	ctx, cancel := context.WithTimeout(ui.WithOutput(context.Background(), output), 5*time.Second)
	defer cancel()
	started := make(chan string, len(pods))
	var active, maximum atomic.Int32
	exec := func(ctx context.Context, _ *rest.Config, _, pod, _ string, _ []string) (string, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for previous := maximum.Load(); current > previous; previous = maximum.Load() {
			if maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- pod
		select {
		case <-release[pod]:
			return "", fmt.Errorf("unavailable on %s", pod)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	finished := make(chan docaHostCollection, 1)
	go func() {
		observations, err := discoverDOCAHost(ctx, c, &rest.Config{}, groups, pods, exec)
		finished <- docaHostCollection{observations: observations, err: err}
	}()
	// All first-batch calls must overlap. A serial implementation cannot reach
	// this barrier; an unbounded implementation exceeds the active-call limit.
	for range docaHostProbeConcurrency {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("host package probes did not run concurrently")
		}
	}
	assert.Equal(t, int32(docaHostProbeConcurrency), active.Load())
	// Complete the last first-batch worker, then its queued successors, before
	// completing the earlier nodes. Results and warnings must still be sorted.
	for i := docaHostProbeConcurrency - 1; i < len(pods)-1; i++ {
		close(release[pods[i].Name])
		select {
		case next := <-started:
			assert.Equal(t, pods[i+1].Name, next)
		case <-ctx.Done():
			t.Fatal("a freed worker did not process the next queued node")
		}
	}
	close(release[pods[len(pods)-1].Name])
	for i := docaHostProbeConcurrency - 2; i >= 0; i-- {
		close(release[pods[i].Name])
	}
	select {
	case result := <-finished:
		require.NoError(t, result.err)
		require.Len(t, result.observations, len(pods))
		require.Len(t, output.warnings, len(pods))
		for i, observation := range result.observations {
			assert.Equal(t, pods[i].Spec.NodeName, observation.Node)
			assert.Equal(t, config.DOCAHostUnknown, observation.Status)
			assert.Equal(t, fmt.Sprintf("Host DOCA-HOST package observation unavailable on %s: unavailable on %s", observation.Node, pods[i].Name), output.warnings[i])
		}
	case <-ctx.Done():
		t.Fatal("host package collection did not finish")
	}
	assert.Equal(t, int32(docaHostProbeConcurrency), maximum.Load())
	assert.Zero(t, active.Load())
}

func TestDOCAHostCollectionCancelsInflightAndQueuedProbes(t *testing.T) {
	groups, _, _, pods := fabricFixtures(docaHostProbeConcurrency*2, true)
	for i := range pods {
		readyGPUFixture(&pods[i])
		pods[i].Spec.HostPID = true
	}
	c := gpuFixtureClient(t, pods)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, len(pods))
	var calls, stopped atomic.Int32
	exec := func(ctx context.Context, _ *rest.Config, _, _, _ string, _ []string) (string, error) {
		calls.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		stopped.Add(1)
		return "", ctx.Err()
	}
	finished := make(chan docaHostCollection, 1)
	go func() {
		observations, err := discoverDOCAHost(ctx, c, &rest.Config{}, groups, pods, exec)
		finished <- docaHostCollection{observations: observations, err: err}
	}()
	for range docaHostProbeConcurrency {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("host package probes did not start")
		}
	}
	cancel()
	select {
	case result := <-finished:
		require.ErrorIs(t, result.err, context.Canceled)
		assert.Nil(t, result.observations)
	case <-time.After(5 * time.Second):
		t.Fatal("caller cancellation did not stop host package probes")
	}
	assert.Equal(t, int32(docaHostProbeConcurrency), calls.Load(), "queued nodes must not execute after cancellation")
	assert.Equal(t, calls.Load(), stopped.Load(), "collection must join every started probe before returning")
}

func TestDOCAHostCollectionKeepsPerNodeDeadlineOptional(t *testing.T) {
	groups, _, _, pods := fabricFixtures(docaHostProbeConcurrency+1, true)
	for i := range pods {
		readyGPUFixture(&pods[i])
		pods[i].Spec.HostPID = true
	}
	c := gpuFixtureClient(t, pods)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exec := func(ctx context.Context, _ *rest.Config, _, _, _ string, _ []string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	observations, err := collectDOCAHost(ctx, c, &rest.Config{}, groups, pods, exec, 10*time.Millisecond)
	require.NoError(t, err)
	require.Len(t, observations, len(pods))
	for _, observation := range observations {
		assert.Equal(t, config.DOCAHostUnknown, observation.Status)
		assert.True(t, strings.Contains(observation.Reason, "context deadline exceeded"))
	}
}

func TestDOCAHostPackageDatabaseProbe(t *testing.T) {
	for _, tc := range []struct {
		name, manager, script, expected string
		wantError                       bool
	}{
		{"deb installed", "dpkg-query", "printf 'doca-host\\tinstall ok installed\\t3.5.0-041000-26.07-ubuntu2404\\n'", "installed\t3.5.0-041000-26.07-ubuntu2404", false},
		{"deb held", "dpkg-query", "printf 'doca-host\\thold ok installed\\t3.5.0-041000\\n'", "installed\t3.5.0-041000", false},
		{"deb absent", "dpkg-query", "printf 'other\\tinstall ok installed\\t1\\n'", "not-installed", false},
		{"deb residual config", "dpkg-query", "printf 'doca-host\\tdeinstall ok config-files\\t3.5.0-1\\n'", "not-installed", false},
		{"deb partial install", "dpkg-query", "printf 'doca-host\\tinstall ok unpacked\\t3.5.0-1\\n'", "", true},
		{"deb database denied", "dpkg-query", "echo 'database access denied' >&2; exit 1", "", true},
		{"rpm installed", "rpm", "printf 'doca-host\\t3.5.0-041000.el9\\n'", "installed\t3.5.0-041000.el9", false},
		{"rpm absent", "rpm", "printf 'other\\t1\\n'", "not-installed", false},
		{"rpm database failed", "rpm", "exit 1", "", true},
		{"unsupported manager", "", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			awk, err := exec.LookPath("awk")
			require.NoError(t, err)
			require.NoError(t, os.Symlink(awk, filepath.Join(bin, "awk")))
			if tc.manager != "" {
				require.NoError(t, os.WriteFile(filepath.Join(bin, tc.manager), []byte("#!/bin/sh\n"+tc.script+"\n"), 0o700))
			}
			command := exec.CommandContext(context.Background(), "/bin/sh", "-c", docaHostProbeCommand)
			command.Env = []string{"PATH=" + bin}
			output, err := command.CombinedOutput()
			t.Logf("package probe output: %s", output)
			assert.Equal(t, tc.wantError, err != nil)
			if !tc.wantError {
				assert.Equal(t, tc.expected+"\n", string(output))
			}
		})
	}
}

func TestDOCAHostProbeRecordsEveryWorkerAndRejectsIncompleteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
		probeErr           error
		hostPID            bool
		restart            bool
	}{
		{name: "installed", output: "installed\t3.5.0-041000-26.07-ubuntu2404\n", want: config.DOCAHostInstalled, hostPID: true},
		{name: "absent", output: "not-installed\n", want: config.DOCAHostNotInstalled, hostPID: true},
		{name: "denied", probeErr: fmt.Errorf("pods/exec forbidden"), want: config.DOCAHostUnknown, hostPID: true},
		{name: "malformed", output: "installed\t\n", want: config.DOCAHostUnknown, hostPID: true},
		{name: "multiple packages", output: "installed\t3.5.0-1\ninstalled\t3.5.0-2\n", want: config.DOCAHostUnknown, hostPID: true},
		{name: "container PID namespace", want: config.DOCAHostUnknown},
		{name: "source restarted", output: "installed\t3.5.0-1\n", want: config.DOCAHostUnknown, hostPID: true, restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groups, _, _, pods := fabricFixtures(1, true)
			readyGPUFixture(&pods[0])
			pods[0].Spec.HostPID = tc.hostPID
			groups[0].WorkerNodes = []string{pods[0].Spec.NodeName, "worker-without-pod"}
			c := gpuFixtureClient(t, pods)
			calls := 0
			exec := func(ctx context.Context, _ *rest.Config, namespace, pod, _ string, command []string) (string, error) {
				calls++
				assert.Equal(t, []string{"chroot", "/proc/1/root", "/bin/sh", "-c", docaHostProbeCommand}, command)
				if tc.restart {
					updated := &corev1.Pod{}
					require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: pod}, updated))
					updated.Status.ContainerStatuses[0].RestartCount++
					require.NoError(t, c.Status().Update(ctx, updated))
				}
				return tc.output, tc.probeErr
			}
			observations, err := discoverDOCAHost(context.Background(), c, &rest.Config{}, groups, pods, exec)
			require.NoError(t, err)
			require.Len(t, observations, 2)
			assert.Equal(t, tc.want, observations[0].Status)
			assert.Equal(t, pods[0].Spec.NodeName, observations[0].Node)
			assert.Equal(t, config.DOCAHostUnknown, observations[1].Status)
			assert.Equal(t, tc.hostPID, calls == 1)
			if tc.want == config.DOCAHostUnknown {
				assert.NotEmpty(t, observations[0].Reason)
			}
		})
	}
}
