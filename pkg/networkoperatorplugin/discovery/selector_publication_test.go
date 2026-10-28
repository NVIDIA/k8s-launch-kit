// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"errors"
	"testing"

	nicop "github.com/Mellanox/nic-configuration-operator/api/v1alpha1"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestMachineLabelPublication(t *testing.T) {
	identifier := config.GeneratedGroupIdentifier("machine-a", "gpu-model-x")
	group := func() config.ClusterConfig {
		return config.ClusterConfig{Identifier: "original", MachineType: "machine-a", GPUType: "gpu-model-x", WorkerNodes: []string{"worker"}, NodeSelector: map[string]string{"pci": "true"}}
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	node := func(name string, labels map[string]string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}
	t.Run("successful patches publish only refreshed exact selectors", func(t *testing.T) {
		groups := []config.ClusterConfig{group()}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node("worker", map[string]string{"pci": "true"}), node("other", map[string]string{"pci": "true"})).Build()
		require.NoError(t, applyMachineLabelToGroups(context.Background(), c, groups))
		require.Equal(t, identifier, groups[0].Identifier)
		require.Equal(t, map[string]string{config.MachineLabelKey: identifier}, groups[0].NodeSelector)
	})
	t.Run("failed patch leaves unpublished inventory untouched", func(t *testing.T) {
		groups := []config.ClusterConfig{group()}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node("worker", nil)).WithInterceptorFuncs(interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errors.New("forbidden patch")
		}}).Build()
		require.ErrorContains(t, applyMachineLabelToGroups(context.Background(), c, groups), "forbidden patch")
		require.Equal(t, "original", groups[0].Identifier)
		require.Equal(t, map[string]string{"pci": "true"}, groups[0].NodeSelector)
	})
	for _, label := range []string{config.MachineLabelKey, config.GPULabelKey} {
		t.Run("stale-"+label, func(t *testing.T) {
			value := identifier
			if label == config.GPULabelKey {
				value = config.GPULabelValue("gpu-model-x")
			}
			patches := 0
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node("worker", nil), node("excluded", map[string]string{label: value})).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				patches++
				return c.Patch(ctx, obj, p, opts...)
			}}).Build()
			groups := []config.ClusterConfig{group()}
			err := applyMachineLabelToGroups(context.Background(), c, groups)
			// An exact alternate selector may exist for machine collisions, but the
			// planned machine identity itself must never be published as exact.
			require.Error(t, err)
			require.Zero(t, patches)
			require.Equal(t, "original", groups[0].Identifier)
		})
	}
	t.Run("colliding identities fail before any patch", func(t *testing.T) {
		groups := []config.ClusterConfig{group(), group()}
		groups[1].WorkerNodes = []string{"other"}
		patches := 0
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node("worker", nil), node("other", nil)).WithInterceptorFuncs(interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			patches++
			return nil
		}}).Build()
		require.ErrorContains(t, applyMachineLabelToGroups(context.Background(), c, groups), "colliding")
		require.Zero(t, patches)
	})
	t.Run("unresolved singleton identity uses proven hostname", func(t *testing.T) {
		groups := []config.ClusterConfig{group()}
		groups[0].MachineType = ""
		groups[0].GPUType = ""
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node("worker", map[string]string{"kubernetes.io/hostname": "worker"}), node("other", nil)).Build()
		require.NoError(t, applyMachineLabelToGroups(context.Background(), c, groups))
		require.Equal(t, map[string]string{"kubernetes.io/hostname": "worker"}, groups[0].NodeSelector)
	})
	t.Run("empty label equality requires the key to exist", func(t *testing.T) {
		require.True(t, selectorMatchesOnlyNodes(map[string]string{"empty": ""}, []string{"worker"}, map[string]map[string]string{"worker": {"empty": ""}, "other": {}}))
	})
	t.Run("missing intended node never passes exact membership", func(t *testing.T) {
		require.False(t, selectorMatchesOnlyNodes(map[string]string{"cohort": "gpu"}, []string{"missing"}, map[string]map[string]string{}))
	})
	t.Run("cancellation aborts publication", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		groups := []config.ClusterConfig{group()}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node("worker", nil)).WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			return ctx.Err()
		}}).Build()
		require.ErrorIs(t, applyMachineLabelToGroups(ctx, c, groups), context.Canceled)
		require.Equal(t, "original", groups[0].Identifier)
	})
}

func TestMachineLabelPublicationFromUnnamedBuilderSource(t *testing.T) {
	for _, workers := range [][]string{{"worker-a"}, {"worker-a", "worker-b"}} {
		t.Run(workers[len(workers)-1], func(t *testing.T) {
			nodeLabels := map[string]map[string]string{"other": {"cohort": "other"}}
			var devices []nicop.NicDevice
			var objects []client.Object
			for _, node := range workers {
				nodeLabels[node] = map[string]string{"kubernetes.io/hostname": node, "cohort": "cpu-nic"}
				devices = append(devices, nicop.NicDevice{Status: nicop.NicDeviceStatus{Node: node, Type: "1023", Ports: []nicop.NicDevicePortSpec{{PCI: "0000:18:00.0", RdmaInterface: "mlx5_0"}}}})
				objects = append(objects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node, Labels: nodeLabels[node]}})
			}
			objects = append(objects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "other", Labels: nodeLabels["other"]}})
			groups, _ := buildClusterConfig(devices, nodeLabels, nil, true)
			require.Len(t, groups, 1)
			require.Empty(t, groups[0].Identifier)
			require.Empty(t, groups[0].GPUType)
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			require.NoError(t, applyMachineLabelToGroups(context.Background(), c, groups))
			require.Empty(t, groups[0].Identifier, "preserve established sole-source identity rather than inventing a name")
			require.True(t, selectorMatchesOnlyNodes(groups[0].NodeSelector, workers, nodeLabels))
		})
	}
}
