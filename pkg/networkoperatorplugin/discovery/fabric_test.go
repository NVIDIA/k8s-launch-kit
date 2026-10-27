// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func fabricOutput(pcis []string, fabric, rdma string) string {
	var out strings.Builder
	for _, pci := range pcis {
		fmt.Fprintf(&out, "BEGIN\t%s\nPORT\t%s\t%s\t1\nEND\t%s\t1\n", pci, pci, rdma, pci)
	}
	for _, pci := range pcis {
		fmt.Fprintf(&out, "RESULT\t%s\t%s\t1\t%s\tyes\n", pci, rdma, fabric)
	}
	fmt.Fprintf(&out, "DONE\t%d\t%d\n", len(pcis), len(pcis))
	return out.String()
}

func TestParseFabricObservationRequiresCompleteManifestAndResults(t *testing.T) {
	pcis := []string{"0000:18:00.0", "0000:18:00.1"}
	good := fabricOutput(pcis, "InfiniBand", "mlx5_2")
	require.NoError(t, parseFabricObservation(good, pcis).err)
	for name, output := range map[string]string{
		"truncated before manifest": strings.Split(good, "PORT")[0],
		"truncated after manifest":  strings.Split(good, "RESULT")[0],
		"missing completion":        strings.ReplaceAll(good, "DONE\t2\t2\n", ""),
		"missing port result":       strings.ReplaceAll(good, "RESULT\t0000:18:00.1\tmlx5_2\t1\tInfiniBand\tyes\n", ""),
		"missing enumeration end":   strings.ReplaceAll(good, "END\t0000:18:00.1\t1\n", ""),
		"wrong enumeration count":   strings.ReplaceAll(good, "END\t0000:18:00.1\t1", "END\t0000:18:00.1\t2"),
		"duplicate manifest":        strings.ReplaceAll(good, "END\t0000:18:00.0", "PORT\t0000:18:00.0\tmlx5_2\t1\nEND\t0000:18:00.0"),
		"duplicate result":          strings.ReplaceAll(good, "DONE", "RESULT\t0000:18:00.0\tmlx5_2\t1\tInfiniBand\tyes\nDONE"),
		"unexpected pci":            strings.ReplaceAll(good, "0000:18:00.1", "0000:19:00.0"),
		"unknown fabric":            strings.ReplaceAll(good, "InfiniBand", "unreadable"),
		"unreadable netdev":         strings.ReplaceAll(good, "\tyes\n", "\terror\n"),
		"failed enumeration":        strings.ReplaceAll(good, "END\t0000:18:00.0\t1", "ERROR\t0000:18:00.0"),
		"records after completion":  good + "BEGIN\t0000:18:00.0\n",
	} {
		t.Run(name, func(t *testing.T) { require.Error(t, parseFabricObservation(output, pcis).err) })
	}
}

func fabricFixtures(nodes int, collapse bool) ([]config.ClusterConfig, []nicop.NicDevice, groupPFMembers, []corev1.Pod) {
	devices := make([]nicop.NicDevice, 0, nodes)
	pods := make([]corev1.Pod, 0, nodes)
	for i := range nodes {
		node := fmt.Sprintf("worker-%d", i)
		devices = append(devices, nicop.NicDevice{Status: nicop.NicDeviceStatus{Node: node, Type: "1023", PartNumber: "test", Ports: []nicop.NicDevicePortSpec{
			{PCI: "0000:18:00.0", RdmaInterface: fmt.Sprintf("mlx5_%d", i), NetworkInterface: fmt.Sprintf("ib%d", i)},
			{PCI: "0000:18:00.1", RdmaInterface: fmt.Sprintf("mlx5_%d", i+10)},
		}}})
		pods = append(pods, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "daemon-" + node, Namespace: "operator"}, Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "daemon"}}}})
	}
	groups, warnings, members := buildClusterConfigWithMembers(devices, nil, nil, collapse)
	_ = warnings
	return groups, devices, members, pods
}

func TestFabricMembershipRetainsCollapseAndNodeLocalAliases(t *testing.T) {
	groups, devices, members, _ := fabricFixtures(2, true)
	require.Len(t, groups, 1)
	require.Len(t, groups[0].PFs, 1)
	identities := buildNodePFIdentities(groups, devices, members)
	require.Len(t, identities, 4)
	assert.Equal(t, "mlx5_0", identities[0].rdmaDevice)
	assert.Equal(t, "mlx5_1", identities[2].rdmaDevice)
	for _, id := range identities {
		assert.Empty(t, id.missing)
		assert.Equal(t, "0000:18:00.0", id.retainedPCI)
	}
	// The projection follows worker identity, not identifiers or slice position.
	groups[0].Identifier = "changed-after-grouping"
	require.Equal(t, identities, buildNodePFIdentities(groups, devices, members))
	missing := devices[1].DeepCopy()
	missing.Status.Ports = missing.Status.Ports[:1]
	identities = buildNodePFIdentities(groups, []nicop.NicDevice{devices[0], *missing}, members)
	require.Len(t, identities, 4)
	assert.Contains(t, identities[3].missing, "missing NicDevice")
}

func TestDiscoverFabricRequiresEveryWorkerAndCollapsedPF(t *testing.T) {
	for _, mode := range []string{"ib", "ethernet", "mixed-workers", "mixed-siblings", "partial-sibling", "permission", "no-netdev"} {
		t.Run(mode, func(t *testing.T) {
			groups, devices, members, pods := fabricFixtures(2, true)
			original := groups[0].PFs[0]
			var output bytes.Buffer
			ctx := ui.WithOutput(context.Background(), ui.NewWithWriter(&output))
			err := discoverFabric(ctx, &rest.Config{}, "operator", groups, devices, members, pods, func(_ context.Context, _ *rest.Config, namespace, pod, container string, cmd []string) (string, error) {
				assert.Equal(t, "operator", namespace)
				assert.Equal(t, "daemon", container)
				pcis := cmd[4:]
				assert.Len(t, pcis, 2, "collapsed sibling remains expected")
				fabric := "InfiniBand"
				rdma := "mlx5_0"
				if strings.HasSuffix(pod, "1") {
					rdma = "mlx5_7"
				}
				if mode == "ethernet" || (mode == "mixed-workers" && strings.HasSuffix(pod, "1")) {
					fabric = "Ethernet"
				}
				if mode == "permission" {
					return "", apierrors.NewForbidden(schema.GroupResource{Resource: "pods/exec"}, pod, errors.New("denied"))
				}
				result := fabricOutput(pcis, fabric, rdma)
				if mode == "mixed-siblings" {
					result = strings.Replace(result, "\tInfiniBand\tyes", "\tEthernet\tyes", 1)
				}
				if mode == "partial-sibling" {
					result = fabricOutput(pcis[:1], fabric, rdma)
				}
				if mode == "no-netdev" {
					result = strings.ReplaceAll(result, "\tyes\n", "\tno\n")
				}
				return result, nil
			})
			require.NoError(t, err)
			switch mode {
			case "ib", "no-netdev":
				assert.Equal(t, "InfiniBand", groups[0].LinkType)
			case "ethernet":
				assert.Equal(t, "Ethernet", groups[0].LinkType)
			default:
				assert.Empty(t, groups[0].LinkType)
				assert.Contains(t, output.String(), "unknown")
			}
			assert.Equal(t, original, groups[0].PFs[0], "node-local aliases must not mutate saved PFs")
			if mode == "no-netdev" {
				assert.Contains(t, output.String(), "worker-0/0000:18:00.0")
				assert.Contains(t, output.String(), "ib_ipoib")
			}
		})
	}
}

func TestFabricCollectionCancellationAndBoundedConcurrency(t *testing.T) {
	groups, devices, members, pods := fabricFixtures(12, true)
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{}, 12)
	var active, maxActive atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- discoverFabric(ctx, &rest.Config{}, "operator", groups, devices, members, pods, func(probeCtx context.Context, _ *rest.Config, _, _, _ string, _ []string) (string, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for {
				m := maxActive.Load()
				if n <= m || maxActive.CompareAndSwap(m, n) {
					break
				}
			}
			entered <- struct{}{}
			<-probeCtx.Done()
			return "", probeCtx.Err()
		})
	}()
	for range 4 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("workers did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("collection did not stop")
	}
	assert.Equal(t, int32(0), active.Load())
	assert.Equal(t, int32(4), maxActive.Load())
	assert.Empty(t, entered, "queued nodes must not exec after cancellation")
}

func TestFabricInternalTimeoutIsUnknownAndDoesNotRetry(t *testing.T) {
	groups, devices, members, pods := fabricFixtures(1, true)
	var calls int
	err := discoverFabricWithTimeout(context.Background(), &rest.Config{}, "operator", groups, devices, members, pods, func(ctx context.Context, _ *rest.Config, _, _, _ string, _ []string) (string, error) {
		calls++
		<-ctx.Done()
		return "", ctx.Err()
	}, time.Millisecond)
	require.NoError(t, err)
	assert.Empty(t, groups[0].LinkType)
	assert.Equal(t, 1, calls)
}

func TestFabricProbeCommandEnumeratesAllPortsWithoutRequiringLinkUp(t *testing.T) {
	sysfs := filepath.Join(t.TempDir(), "sys")
	pci := "0000:18:00.0"
	for _, port := range []string{"1", "2"} {
		dir := filepath.Join(sysfs, "bus/pci/devices", pci, "infiniband/mlx5_7/ports", port)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "link_layer"), []byte("InfiniBand\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "state"), []byte("1: DOWN\n"), 0o600))
	}
	// A missing net directory is a valid observation when IPoIB is unprovisioned.
	command := strings.ReplaceAll(fabricProbeCmd, "/host/sys", sysfs)
	command = strings.ReplaceAll(command, "base=/sys", "base="+sysfs)
	run := func() nodeFabricObservation {
		out, err := exec.Command("/bin/sh", "-c", command, "l8k-fabric", pci).CombinedOutput()
		require.NoError(t, err, string(out))
		return parseFabricObservation(string(out), []string{pci})
	}
	observation := run()
	require.NoError(t, observation.err)
	require.Len(t, observation.ports, 2)
	for _, value := range observation.ports {
		assert.Equal(t, "InfiniBand", value.fabric)
		assert.False(t, value.netdev)
	}
	// An enumerated port without a readable link_layer may not disappear.
	require.NoError(t, os.Remove(filepath.Join(sysfs, "bus/pci/devices", pci, "infiniband/mlx5_7/ports/2/link_layer")))
	out, err := exec.Command("/bin/sh", "-c", command, "l8k-fabric", pci).CombinedOutput()
	require.Error(t, err)
	assert.Error(t, parseFabricObservation(string(out), []string{pci}).err)
}

func TestReadOnlyFabricOptionRequiresRESTBeforeAPIReads(t *testing.T) {
	_, err := DiscoverReadOnly(context.Background(), errorListClient{err: errors.New("API touched")}, WithReadOnlyFabricDiscovery())
	require.ErrorContains(t, err, "requires WithReadOnlyRESTConfig")
}

type fabricReadOnlyClient struct {
	client.Client
	writes int
}

func (c *fabricReadOnlyClient) Create(context.Context, client.Object, ...client.CreateOption) error {
	c.writes++
	return errors.New("write prohibited")
}
func (c *fabricReadOnlyClient) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	c.writes++
	return errors.New("write prohibited")
}
func (c *fabricReadOnlyClient) Update(context.Context, client.Object, ...client.UpdateOption) error {
	c.writes++
	return errors.New("write prohibited")
}
func (c *fabricReadOnlyClient) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	c.writes++
	return errors.New("write prohibited")
}

func TestReadOnlyFabricIsExplicitAndDoesNotMutateCluster(t *testing.T) {
	for _, mode := range []string{"no-option", "rest-only", "fabric"} {
		t.Run(mode, func(t *testing.T) {
			_, devices, _, pods := fabricFixtures(2, true)
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, nicop.AddToScheme(scheme))
			var objects []client.Object
			for i := range pods {
				pods[i].Namespace = "nvidia-network-operator"
				pods[i].OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "nic-configuration-daemon"}}
				pods[i].Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				devices[i].Name = fmt.Sprintf("device-%d", i)
				devices[i].Namespace = "default"
				objects = append(objects, &pods[i], &devices[i], &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: pods[i].Spec.NodeName, Labels: map[string]string{"group": "selected"}}})
			}
			c := &fabricReadOnlyClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
			var calls atomic.Int32
			var mu sync.Mutex
			var observed [][]string
			opts := []ReadOnlyOption{withReadOnlyPodExec(func(_ context.Context, _ *rest.Config, _, _, _ string, cmd []string) (string, error) {
				calls.Add(1)
				mu.Lock()
				observed = append(observed, cmd)
				mu.Unlock()
				return fabricOutput(cmd[4:], "InfiniBand", "mlx5_local"), nil
			})}
			if mode != "no-option" {
				opts = append(opts, WithReadOnlyRESTConfig(&rest.Config{}))
			}
			if mode == "fabric" {
				opts = append(opts, WithReadOnlyFabricDiscovery())
			}
			cfg, err := DiscoverReadOnly(context.Background(), c, opts...)
			require.NoError(t, err)
			require.Len(t, cfg.ClusterConfig, 1)
			assert.Zero(t, c.writes)
			if mode == "fabric" {
				assert.Equal(t, int32(2), calls.Load())
				assert.Equal(t, "InfiniBand", cfg.ClusterConfig[0].LinkType)
			} else {
				assert.Zero(t, calls.Load())
				assert.Empty(t, cfg.ClusterConfig[0].LinkType)
			}
			for _, cmd := range observed {
				assert.Equal(t, fabricProbeCmd, cmd[2])
				assert.NotContains(t, cmd[2], "modprobe")
			}
		})
	}
}

func TestPFProjectionSurvivesGroupFilteringAndOrdering(t *testing.T) {
	_, devices, _, _ := fabricFixtures(2, true)
	devices[1].Status.Ports[0].PCI = "0000:29:00.0"
	devices[1].Status.Ports[1].PCI = "0000:29:00.1"
	devices = append(devices, nicop.NicDevice{Status: nicop.NicDeviceStatus{Node: "north-south-only", DPU: true, Type: "a2dc", Ports: []nicop.NicDevicePortSpec{{PCI: "0000:09:00.0"}}}})
	groups, _, members := buildClusterConfigWithMembers(devices, nil, nil, true)
	require.Len(t, groups, 2)
	groups[0], groups[1] = groups[1], groups[0]
	identities := buildNodePFIdentities(groups[:1], devices, members)
	require.Len(t, identities, 2)
	for _, identity := range identities {
		assert.Equal(t, "worker-1", identity.node)
		assert.Equal(t, "0000:29:00.0", identity.retainedPCI)
		assert.Empty(t, identity.missing)
	}
}

func TestFabricAuthorizationFailureStopsQueuedExecs(t *testing.T) {
	groups, devices, members, pods := fabricFixtures(20, true)
	var calls atomic.Int32
	err := discoverFabric(context.Background(), &rest.Config{}, "operator", groups, devices, members, pods, func(context.Context, *rest.Config, string, string, string, []string) (string, error) {
		calls.Add(1)
		return "", apierrors.NewForbidden(schema.GroupResource{Resource: "pods/exec"}, "daemon", errors.New("denied"))
	})
	require.NoError(t, err)
	assert.Empty(t, groups[0].LinkType)
	assert.LessOrEqual(t, calls.Load(), int32(fabricProbeConcurrency), "authorization failure must not fan out across all queued workers")
}
