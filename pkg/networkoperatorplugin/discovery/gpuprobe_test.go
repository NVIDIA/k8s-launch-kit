// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func readyGPUFixture(pod *corev1.Pod) {
	pod.UID = types.UID(pod.Name)
	pod.Spec.Volumes = []corev1.Volume{{Name: "host-sys", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/sys"}}}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "host-sys", MountPath: "/sys", ReadOnly: true}}
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning,
		Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: pod.Spec.Containers[0].Name, Ready: true, ContainerID: "container://stable", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
	}
}

func gfdFixture(node string) (*corev1.Pod, *appsv1.DaemonSet) {
	controller := true
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: gfdContainer, Namespace: "gpu-operator", UID: "gfd-owner"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gfd-" + node, Namespace: ds.Namespace,
		Labels:          map[string]string{"app": gfdContainer, "app.kubernetes.io/part-of": "nvidia-gpu"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: &controller}},
	}, Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: gfdContainer}}}}
	readyGPUFixture(pod)
	return pod, ds
}

func gpuFixtureClient(t *testing.T, pods []corev1.Pod, extra ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, nicop.AddToScheme(scheme))
	for i := range pods {
		extra = append(extra, &pods[i])
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(extra...).Build()
}

func TestGFDSourcesRequireOwnershipReadyContainerAndActualHostSys(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Pod, *appsv1.DaemonSet){
		"ready":           func(*corev1.Pod, *appsv1.DaemonSet) {},
		"wrong owner UID": func(p *corev1.Pod, _ *appsv1.DaemonSet) { p.OwnerReferences[0].UID = "stale" },
		"no controller":   func(p *corev1.Pod, _ *appsv1.DaemonSet) { p.OwnerReferences = nil },
		"not ready":       func(p *corev1.Pod, _ *appsv1.DaemonSet) { p.Status.ContainerStatuses[0].Ready = false },
		"wrong host path": func(p *corev1.Pod, _ *appsv1.DaemonSet) { p.Spec.Volumes[0].HostPath.Path = "/tmp/fake-sys" },
		"subpath":         func(p *corev1.Pod, _ *appsv1.DaemonSet) { p.Spec.Containers[0].VolumeMounts[0].SubPath = "devices" },
		"masked pci view": func(p *corev1.Pod, _ *appsv1.DaemonSet) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "mask", MountPath: "/sys/bus/pci"})
		},
		"renamed container": func(p *corev1.Pod, _ *appsv1.DaemonSet) { p.Spec.Containers[0].Name = "sidecar" },
	} {
		t.Run(name, func(t *testing.T) {
			pod, ds := gfdFixture("worker-0")
			mutate(pod, ds)
			c := gpuFixtureClient(t, nil, pod, ds)
			sources, err := findGFDSources(context.Background(), c, "gpu-operator")
			require.NoError(t, err)
			if name == "ready" {
				require.Len(t, sources["worker-0"], 1)
				assert.Equal(t, gfdContainer, sources["worker-0"][0].container)
			} else {
				assert.Empty(t, sources)
			}
		})
	}
}

func TestGPUCollectionUsesSameNodeGFDWithoutChangingDaemon(t *testing.T) {
	groups, devices, members, pods := fabricFixtures(1, true)
	readyGPUFixture(&pods[0])
	gfd, ds := gfdFixture("worker-0")
	// A sidecar preceding the main container must never be selected.
	gfd.Spec.Containers = append([]corev1.Container{{Name: "config-manager"}}, gfd.Spec.Containers...)
	c := &fabricReadOnlyClient{Client: gpuFixtureClient(t, pods, gfd, ds)}
	var calls atomic.Int32
	exec := func(_ context.Context, _ *rest.Config, namespace, pod, container string, cmd []string) (string, error) {
		calls.Add(1)
		if cmd[2] == gpuHostProbeCmd {
			assert.Equal(t, pods[0].Name, pod)
			return gpuTestHostOutput(), nil
		}
		if pod == pods[0].Name {
			return "---UNAVAILABLE---\n", nil
		}
		assert.Equal(t, "gpu-operator", namespace)
		assert.Equal(t, gfd.Name, pod)
		assert.Equal(t, gfdContainer, container)
		return gpuTestOutput(gpuTestQuery, gpuTestMatrix), nil
	}
	observations, identities, err := discoverGPUObservations(context.Background(), c, &rest.Config{}, "gpu-operator", groups, nil, devices, members, pods, exec)
	require.NoError(t, err)
	require.NoError(t, observations["worker-0"].err)
	applyGPUObservations(context.Background(), groups, identities, observations)
	assert.Equal(t, "GPU0", groups[0].PFs[0].ConnectedGPU)
	assert.Equal(t, "0000:80:00.0", groups[0].PFs[0].ConnectedGPUPCIAddress)
	assert.Equal(t, "NVIDIA-Test-GPU", groups[0].GPUType)
	assert.Equal(t, int32(4), calls.Load())
	assert.Zero(t, c.writes)
	assert.Empty(t, pods[0].Spec.RuntimeClassName)
}

func TestGPUCollectionPreservesHostExecutableFallbackAfterLocalFailure(t *testing.T) {
	groups, devices, members, pods := fabricFixtures(1, true)
	readyGPUFixture(&pods[0])
	c := &gpuDeniedListClient{Client: gpuFixtureClient(t, pods)}
	observations, _, err := discoverGPUObservations(context.Background(), c, &rest.Config{}, "gpu-operator", groups, nil, devices, members, pods,
		func(_ context.Context, _ *rest.Config, _, _, _ string, cmd []string) (string, error) {
			if cmd[2] == gpuHostProbeCmd {
				return gpuTestHostOutput(), nil
			}
			if cmd[4] == "local" {
				return "", errors.New("local NVML unavailable")
			}
			assert.Equal(t, "host", cmd[4])
			return gpuTestOutput(gpuTestQuery, gpuTestMatrix), nil
		})
	require.NoError(t, err)
	require.NoError(t, observations["worker-0"].err)
	assert.Zero(t, c.calls.Load(), "working host driver must not require GFD permissions")
}

func TestGPUCollectionRejectsWrongNodeAndAmbiguousSources(t *testing.T) {
	for _, mode := range []string{"wrong-node", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			groups, devices, members, pods := fabricFixtures(1, true)
			readyGPUFixture(&pods[0])
			gfd, ds := gfdFixture("worker-0")
			other := gfd.DeepCopy()
			other.Name, other.UID = "gfd-other", "other"
			if mode == "wrong-node" {
				gfd.Spec.NodeName, other.Spec.NodeName = "other-worker", "other-worker"
			}
			c := gpuFixtureClient(t, pods, gfd, other, ds)
			observations, _, err := discoverGPUObservations(context.Background(), c, &rest.Config{}, "gpu-operator", groups, nil, devices, members, pods,
				func(_ context.Context, _ *rest.Config, _, pod, _ string, cmd []string) (string, error) {
					assert.Equal(t, pods[0].Name, pod, "must not execute an ambiguous or other-node GFD")
					if cmd[2] == gpuHostProbeCmd {
						return gpuTestHostOutput(), nil
					}
					return "---UNAVAILABLE---\n", nil
				})
			require.NoError(t, err)
			require.ErrorContains(t, observations["worker-0"].err, "expected one ready same-node")
		})
	}
}

func TestGPUCollectionDistinguishesUnknownUnlabeledAndCPUOnly(t *testing.T) {
	for _, mode := range []string{"unlabeled-GPU", "CPU-only", "host-unreadable", "label-conflicts-with-empty-host"} {
		t.Run(mode, func(t *testing.T) {
			groups, devices, members, pods := fabricFixtures(1, true)
			readyGPUFixture(&pods[0])
			c := gpuFixtureClient(t, pods)
			labels := map[string]map[string]string{}
			if mode == "label-conflicts-with-empty-host" {
				labels["worker-0"] = map[string]string{"nvidia.com/gpu.present": "true"}
			}
			var gpuCalls atomic.Int32
			observations, _, err := discoverGPUObservations(context.Background(), c, &rest.Config{}, "gpu-operator", groups, labels, devices, members, pods,
				func(_ context.Context, _ *rest.Config, _, _, _ string, cmd []string) (string, error) {
					if cmd[2] == gpuHostProbeCmd {
						switch mode {
						case "CPU-only", "label-conflicts-with-empty-host":
							return "HOSTDONE\t0\nDONE\n", nil
						case "host-unreadable":
							return "", errors.New("sysfs inaccessible")
						default:
							return gpuTestHostOutput(), nil
						}
					}
					gpuCalls.Add(1)
					return gpuTestOutput(gpuTestQuery, gpuTestMatrix), nil
				})
			require.NoError(t, err)
			observation := observations["worker-0"]
			if mode == "CPU-only" {
				assert.True(t, observation.cpuOnly)
				assert.Zero(t, gpuCalls.Load())
			} else {
				assert.False(t, observation.cpuOnly)
				assert.Equal(t, int32(1), gpuCalls.Load(), "unknown/unlabeled nodes keep the local product path")
				if mode == "unlabeled-GPU" {
					require.NoError(t, observation.err)
				} else {
					require.Error(t, observation.err)
				}
			}
		})
	}
}

func TestGPUCheckedExecRejectsContainerRestart(t *testing.T) {
	gfd, ds := gfdFixture("worker-0")
	c := gpuFixtureClient(t, nil, gfd, ds)
	_, err := execGPUChecked(context.Background(), c, &rest.Config{}, gpuSource{pod: *gfd, container: gfdContainer},
		func(ctx context.Context, _ *rest.Config, _, _, _ string, _ []string) (string, error) {
			updated := &corev1.Pod{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(gfd), updated))
			updated.Status.ContainerStatuses[0].RestartCount++
			require.NoError(t, c.Status().Update(ctx, updated))
			return gpuTestOutput(gpuTestQuery, gpuTestMatrix), nil
		}, []string{"/bin/sh", "-c", gpuObservationCmd})
	require.ErrorContains(t, err, "restarted")
}

func TestGPUCollectionDeadlinesAndConcurrency(t *testing.T) {
	groups, devices, members, pods := fabricFixtures(9, true)
	for i := range pods {
		readyGPUFixture(&pods[i])
	}
	c := gpuFixtureClient(t, pods)
	var active, maximum atomic.Int32
	exec := func(ctx context.Context, _ *rest.Config, _, _, _ string, _ []string) (string, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for previous := maximum.Load(); current > previous; previous = maximum.Load() {
			if maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
	observations, _, err := collectGPUObservations(context.Background(), c, &rest.Config{}, "gpu-operator", groups, nil, devices, members, pods, exec, 15*time.Millisecond)
	require.NoError(t, err, "child timeout is optional unknown")
	assert.LessOrEqual(t, maximum.Load(), int32(gpuProbeConcurrency))
	for _, observation := range observations {
		require.ErrorIs(t, observation.err, context.DeadlineExceeded)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = collectGPUObservations(ctx, c, &rest.Config{}, "gpu-operator", groups, nil, devices, members, pods, exec, time.Second)
	require.ErrorIs(t, err, context.Canceled)
}

func TestReadOnlyGPURequiresExplicitOptionsAndNeverWrites(t *testing.T) {
	for _, opts := range [][]ReadOnlyOption{{WithReadOnlyGPUTopology("gpu-operator")}, {WithReadOnlyGPUTopology(""), WithReadOnlyRESTConfig(&rest.Config{})}} {
		_, err := DiscoverReadOnly(context.Background(), errorListClient{err: errors.New("API touched")}, opts...)
		require.ErrorContains(t, err, "requires a GPU Operator namespace and WithReadOnlyRESTConfig")
	}
	for _, mode := range []string{"default", "rest-only", "gpu"} {
		t.Run(mode, func(t *testing.T) {
			_, devices, _, pods := fabricFixtures(2, true)
			var objects []client.Object
			for i := range pods {
				pods[i].Namespace = "nvidia-network-operator"
				pods[i].OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "nic-configuration-daemon"}}
				readyGPUFixture(&pods[i])
				devices[i].Name = fmt.Sprintf("device-%d", i)
				objects = append(objects, &devices[i], &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: pods[i].Spec.NodeName, Labels: map[string]string{"group": "selected"}}})
			}
			c := &fabricReadOnlyClient{Client: gpuFixtureClient(t, pods, objects...)}
			var calls atomic.Int32
			opts := []ReadOnlyOption{withReadOnlyPodExec(func(_ context.Context, _ *rest.Config, _, _, _ string, cmd []string) (string, error) {
				calls.Add(1)
				assert.NotContains(t, cmd[2], "netplan", "GPU opt-in must not run the host netplan workflow")
				if cmd[2] == gpuHostProbeCmd {
					return gpuTestHostOutput(), nil
				}
				return gpuTestOutput(gpuTestQuery, gpuTestMatrix), nil
			})}
			if mode != "default" {
				opts = append(opts, WithReadOnlyRESTConfig(&rest.Config{}))
			}
			if mode == "gpu" {
				opts = append(opts, WithReadOnlyGPUTopology("gpu-operator"))
			}
			cfg, err := DiscoverReadOnly(context.Background(), c, opts...)
			require.NoError(t, err)
			assert.Zero(t, c.writes)
			if mode == "gpu" {
				assert.Equal(t, int32(4), calls.Load())
				assert.Equal(t, "GPU0", cfg.ClusterConfig[0].PFs[0].ConnectedGPU)
			} else {
				assert.Zero(t, calls.Load())
				assert.Empty(t, cfg.ClusterConfig[0].PFs[0].ConnectedGPU)
			}
		})
	}
}

func gpuParsedFixture(t *testing.T) nodeGPUObservation {
	t.Helper()
	host, err := parseGPUHostObservation(gpuTestHostOutput())
	require.NoError(t, err)
	product, data, err := parseGPUObservation(gpuTestOutput(gpuTestQuery, gpuTestMatrix), host)
	require.NoError(t, err)
	return nodeGPUObservation{host: host, product: product, data: data}
}

func TestGPUAggregationRequiresEveryWorkerAndCollapsedSibling(t *testing.T) {
	for _, mode := range []string{"agree", "missing-worker", "missing-sibling", "disagree", "partial-matrix"} {
		t.Run(mode, func(t *testing.T) {
			groups, devices, members, _ := fabricFixtures(2, true)
			groups[0].PFs[0].RdmaDevice = ""
			observations := map[string]nodeGPUObservation{"worker-0": gpuParsedFixture(t), "worker-1": gpuParsedFixture(t)}
			switch mode {
			case "missing-worker":
				delete(observations, "worker-1")
			case "missing-sibling":
				delete(observations["worker-1"].host.rdma, "0000:18:00.1")
			case "disagree":
				observations["worker-1"].data.matrix["mlx5_local1"] = map[int]proximity{0: proxNODE, 1: proxPIX}
			case "partial-matrix":
				delete(observations["worker-1"].data.matrix["mlx5_local1"], 1)
			}
			identities := buildPFIdentities(groups, devices, members, true)
			applyGPUObservations(context.Background(), groups, identities, observations)
			if mode == "agree" {
				assert.Equal(t, "GPU0", groups[0].PFs[0].ConnectedGPU)
			} else {
				assert.Empty(t, groups[0].PFs[0].ConnectedGPU)
			}
			assert.Empty(t, groups[0].PFs[0].RdmaDevice, "transient aliases must not leak into the group")
		})
	}
}

func TestGPUIncompleteCoverageDoesNotReclassifyAndPresetPCIIsReconciled(t *testing.T) {
	groups, devices, members, _ := fabricFixtures(1, false)
	observations := map[string]nodeGPUObservation{"worker-0": gpuParsedFixture(t)}
	delete(observations["worker-0"].data.matrix, "mlx5_local1")
	identities := buildPFIdentities(groups, devices, members, true)
	applyGPUObservations(context.Background(), groups, identities, observations)
	assert.Equal(t, "PIX", groups[0].PFs[0].GPUProximity)
	assert.Empty(t, groups[0].PFs[1].GPUProximity)
	assert.Equal(t, "east-west", groups[0].PFs[1].Traffic, "partial PIX data cannot demote the unobserved PF")
	groups[0].PFs[0].ConnectedGPU = "GPU1" // Simulate authoritative preset replacement.
	reconcileGPUPCIAddresses(&groups[0], observations)
	assert.Equal(t, "0000:a0:00.0", groups[0].PFs[0].ConnectedGPUPCIAddress)
	groups[0].PFs[0].ConnectedGPU = "GPU8"
	reconcileGPUPCIAddresses(&groups[0], observations)
	assert.Empty(t, groups[0].PFs[0].ConnectedGPUPCIAddress)
	// GPU source failure must never retain a stale address after preset apply.
	observations["worker-0"] = nodeGPUObservation{err: errors.New("unavailable")}
	groups[0].PFs[0] = config.PFConfig{ConnectedGPU: "GPU0", ConnectedGPUPCIAddress: "stale"}
	reconcileGPUPCIAddresses(&groups[0], observations)
	assert.Empty(t, groups[0].PFs[0].ConnectedGPUPCIAddress)
}

type gpuDeniedListClient struct {
	client.Client
	calls atomic.Int32
}

func (c *gpuDeniedListClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	c.calls.Add(1)
	return errors.New("GFD pods list forbidden")
}

func TestGPUFallbackPermissionFailureIsVisibleAndListedOnce(t *testing.T) {
	groups, devices, members, pods := fabricFixtures(5, true)
	for i := range pods {
		readyGPUFixture(&pods[i])
	}
	c := &gpuDeniedListClient{Client: gpuFixtureClient(t, pods)}
	observations, identities, err := discoverGPUObservations(context.Background(), c, &rest.Config{}, "gpu-operator", groups, nil, devices, members, pods,
		func(_ context.Context, _ *rest.Config, _, _, _ string, cmd []string) (string, error) {
			if cmd[2] == gpuHostProbeCmd {
				return gpuTestHostOutput(), nil
			}
			return "---UNAVAILABLE---\n", nil
		})
	require.NoError(t, err)
	assert.Equal(t, int32(1), c.calls.Load())
	var output bytes.Buffer
	applyGPUObservations(ui.WithOutput(context.Background(), ui.NewWithWriter(&output)), groups, identities, observations)
	assert.Empty(t, groups[0].PFs[0].ConnectedGPU)
	assert.Contains(t, output.String(), "forbidden")
	assert.Contains(t, output.String(), "worker-0")
}

func TestReadOnlyGPUEnrichmentPrecedesFinalVRRailMapping(t *testing.T) {
	devices := vrRailTestDevices("worker-a")
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "daemon-worker-a", Namespace: "nvidia-network-operator",
		OwnerReferences: []metav1.OwnerReference{{Kind: "DaemonSet", Name: "nic-configuration-daemon"}}},
		Spec: corev1.PodSpec{NodeName: "worker-a", Containers: []corev1.Container{{Name: "daemon"}}}}
	readyGPUFixture(&pod)
	objects := []client.Object{&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a", Labels: map[string]string{"nvidia.com/gpu.machine": "VR"}}}}
	var host, matrix strings.Builder
	host.WriteString("GPU\t0000:80:00.0\nGPU\t0000:a0:00.0\nHOSTDONE\t2\n")
	matrix.WriteString("        GPU0 GPU1")
	for i := range devices {
		objects = append(objects, &devices[i])
		fmt.Fprintf(&host, "NIC\t%s\tmlx5_%d\n", devices[i].Status.Ports[0].PCI, i)
		fmt.Fprintf(&matrix, " NIC%d", i)
	}
	host.WriteString("DONE\n")
	matrix.WriteString("\nGPU0 X SYS" + strings.Repeat(" PIX", len(devices)) + "\nGPU1 SYS X" + strings.Repeat(" NODE", len(devices)) + "\n")
	for i := range devices {
		fmt.Fprintf(&matrix, "NIC%d PIX NODE%s\n", i, strings.Repeat(" PIX", len(devices)))
	}
	matrix.WriteString("NIC Legend:\n")
	for i := range devices {
		fmt.Fprintf(&matrix, "NIC%d: mlx5_%d\n", i, i)
	}
	c := &fabricReadOnlyClient{Client: gpuFixtureClient(t, []corev1.Pod{pod}, objects...)}
	cfg, err := DiscoverReadOnly(context.Background(), c, WithReadOnlyRESTConfig(&rest.Config{}), WithReadOnlyGPUTopology("gpu-operator"),
		withReadOnlyPodExec(func(_ context.Context, _ *rest.Config, _, _, _ string, cmd []string) (string, error) {
			if cmd[2] == gpuHostProbeCmd {
				return host.String(), nil
			}
			return gpuTestOutput(gpuTestQuery, matrix.String()), nil
		}))
	require.NoError(t, err)
	require.Len(t, cfg.ClusterConfig, 1)
	assertVRTestRails(t, cfg.ClusterConfig[0])
	for _, pf := range cfg.ClusterConfig[0].PFs {
		assert.Equal(t, "GPU0", pf.ConnectedGPU)
	}
	assert.Zero(t, c.writes)
}

func TestGPUProductFallbackIsPerNodeBeforeGroupAggregation(t *testing.T) {
	for _, mode := range []string{"measured-disagreement", "sysfs-agreement", "sysfs-disagreement", "partial-sysfs", "mixed-sources"} {
		t.Run(mode, func(t *testing.T) {
			groups, devices, members, pods := fabricFixtures(2, true)
			for i := range pods {
				readyGPUFixture(&pods[i])
			}
			c := gpuFixtureClient(t, pods)
			var fallbackCalls atomic.Int32
			observations, identities, err := discoverGPUObservations(context.Background(), c, &rest.Config{}, "gpu-operator", groups, nil, devices, members, pods,
				func(_ context.Context, _ *rest.Config, _, pod, _ string, cmd []string) (string, error) {
					if cmd[2] == gpuHostProbeCmd {
						return gpuTestHostOutput(), nil
					}
					second := strings.HasSuffix(pod, "1")
					if cmd[2] == sysfsNvidiaGPUIDCmd {
						fallbackCalls.Add(1)
						if mode == "partial-sysfs" && second {
							return "", errors.New("sysfs inaccessible")
						}
						if mode == "sysfs-disagreement" && second {
							return "0x26b9\n", nil
						}
						return "0x2335\n", nil
					}
					output := gpuTestOutput(gpuTestQuery, gpuTestMatrix)
					if mode == "measured-disagreement" {
						product := "NVIDIA Model A"
						if second {
							product = "NVIDIA Model B"
						}
						return strings.Replace(output, "NVIDIA Test GPU", product, 1), nil
					}
					if mode == "mixed-sources" && !second {
						return strings.Replace(output, "NVIDIA Test GPU", "NVIDIA H200 SXM 141GB", 1), nil
					}
					// Even without nvidia-smi, the original sysfs/pci.ids product
					// fallback remains available on each worker, not just the first.
					return "---UNAVAILABLE---\n", nil
				})
			require.NoError(t, err)
			var diagnostics bytes.Buffer
			applyGPUObservations(ui.WithOutput(context.Background(), ui.NewWithWriter(&diagnostics)), groups, identities, observations)
			switch mode {
			case "sysfs-agreement", "mixed-sources":
				assert.Equal(t, "NVIDIA-H200-SXM-141GB", groups[0].GPUType)
			default:
				assert.Empty(t, groups[0].GPUType, "incomplete or conflicting evidence must remain unknown before preset selection")
				assert.Contains(t, diagnostics.String(), "GPU product unconfirmed")
			}
			switch mode {
			case "measured-disagreement":
				assert.Zero(t, fallbackCalls.Load(), "a measured disagreement must not be overwritten by sysfs")
			case "mixed-sources":
				assert.Equal(t, int32(1), fallbackCalls.Load())
			default:
				assert.Equal(t, int32(2), fallbackCalls.Load())
			}
		})
	}
}
