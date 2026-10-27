// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func secondaryRetryBase(t *testing.T) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

func secondaryRetryBundle(t *testing.T, managed bool) *bundle.Bundle {
	t.Helper()
	content := "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: policy\nspec:\n  nvIpam: {}\n"
	if managed {
		content += "  secondaryNetwork:\n    multus:\n      image: multus-cni\n"
	}
	artifacts, err := bundle.FromFiles([]bundle.File{{Name: "10-nicclusterpolicy.yaml", Content: content}})
	require.NoError(t, err)
	return artifacts
}

func secondaryRetryInstaller() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "platform-multus", Namespace: "kube-system", UID: "platform-uid"}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "multus", Image: "multus-cni:v1", VolumeMounts: []corev1.VolumeMount{{Name: "cni", MountPath: "/host/opt/cni/bin"}}}}, Volumes: []corev1.Volume{{Name: "cni", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/opt/cni/bin"}}}}}}}}
}

func TestSecondaryNetworkPollingRechecksOwnershipAndRetainsManagedIntent(t *testing.T) {
	base := secondaryRetryBase(t)
	applies := 0
	c := interceptor.NewClient(base, interceptor.Funcs{Apply: func(ctx context.Context, c client.WithWatch, ac runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
		applies++
		if applies > 1 {
			return errors.New("unsafe second apply")
		}
		require.NoError(t, c.Create(ctx, secondaryRetryInstaller()))
		// Model an admission response that omits the requested component,
		// then disappearance before observation. Retry must use desired intent.
		response := secondaryRetryBundle(t, false).Documents()[0].ObjectCopy()
		response.SetGeneration(1)
		response.SetResourceVersion("applied-1")
		ac.(interface{ SetUnstructuredContent(map[string]interface{}) }).SetUnstructuredContent(response.Object)
		return nil
	}})
	artifacts := secondaryRetryBundle(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := applyBundleWithInstaller(ctx, c, artifacts, DeployOptions{SkipHelmChart: true}, nil)
	require.ErrorContains(t, err, "refusing to install multus")
	require.Equal(t, 1, applies, "platform takeover must block the polling retry")
	_, found, err := unstructured.NestedMap(artifacts.Documents()[0].ObjectCopy().Object, "spec", "secondaryNetwork", "multus")
	require.NoError(t, err)
	require.True(t, found, "desired bundle remains immutable")
}

func TestSecondaryNetworkPollingDoesNotAdoptServerAddedManagedIntent(t *testing.T) {
	base := secondaryRetryBase(t)
	applies := 0
	safeRetry := errors.New("safe external retry reached")
	c := interceptor.NewClient(base, interceptor.Funcs{Apply: func(ctx context.Context, c client.WithWatch, ac runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
		applies++
		if applies == 1 {
			require.NoError(t, c.Create(ctx, secondaryRetryInstaller()))
			response := secondaryRetryBundle(t, true).Documents()[0].ObjectCopy()
			response.SetGeneration(1)
			response.SetResourceVersion("applied-1")
			ac.(interface{ SetUnstructuredContent(map[string]interface{}) }).SetUnstructuredContent(response.Object)
			return nil
		}
		content := ac.(interface{ UnstructuredContent() map[string]interface{} }).UnstructuredContent()
		_, found, err := unstructured.NestedMap(content, "spec", "secondaryNetwork", "multus")
		require.NoError(t, err)
		require.False(t, found, "reapply must not adopt component intent added by the server")
		return safeRetry
	}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := applyBundleWithInstaller(ctx, c, secondaryRetryBundle(t, false), DeployOptions{SkipHelmChart: true}, nil)
	require.ErrorIs(t, err, safeRetry)
	require.Equal(t, 2, applies)
}

func TestSecondaryNetworkPollingCarriesRequiredWorkersIntoHealthGuard(t *testing.T) {
	base := secondaryRetryBase(t)
	applies := 0
	c := interceptor.NewClient(base, interceptor.Funcs{Apply: func(ctx context.Context, c client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
		applies++
		if applies > 1 {
			return errors.New("unguarded coverage retry")
		}
		installer := secondaryRetryInstaller()
		installer.Spec.Template.Spec.NodeSelector = map[string]string{"platform": "supported"}
		require.NoError(t, c.Create(ctx, installer))
		require.NoError(t, c.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a", Labels: map[string]string{"platform": "different"}}}))
		return nil
	}})
	cfg := &config.LaunchKitConfig{ClusterConfig: []config.ClusterConfig{{WorkerNodes: []string{"worker-a"}}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := applyBundleWithInstaller(ctx, c, secondaryRetryBundle(t, false), DeployOptions{SkipHelmChart: true, Config: cfg}, nil)
	require.ErrorContains(t, err, "excludes required worker worker-a")
	require.Equal(t, 1, applies)
}

func TestSecondaryNetworkPollingRetryPermissionAndCancellationSemantics(t *testing.T) {
	for _, mode := range []string{"optional health denied", "mandatory ownership denied", "caller canceled"} {
		t.Run(mode, func(t *testing.T) {
			base := secondaryRetryBase(t)
			applies := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			retryReached := errors.New("retry apply reached")
			c := interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if mode == "mandatory ownership denied" {
						return errors.New("get denied")
					}
					return c.Get(ctx, key, obj, opts...)
				},
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("optional list denied")
				},
				Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
					applies++
					return retryReached
				},
			})
			desired := secondaryRetryBundle(t, false).Documents()[0].ObjectCopy()
			registry := crstate.NewRegistry()
			registry.Register(desired.GroupVersionKind(), func(context.Context, client.Client, *unstructured.Unstructured) (crstate.Result, error) {
				if mode == "caller canceled" {
					cancel()
				}
				return crstate.Result{State: crstate.StateNotDeployed}, nil
			})
			err := pollUntilTerminalWithRetryableErrorTimeout(ctx, c, registry, desired, "policy", "", 0, time.Millisecond)
			switch mode {
			case "optional health denied":
				require.ErrorIs(t, err, retryReached)
				require.Equal(t, 1, applies)
			case "mandatory ownership denied":
				require.ErrorContains(t, err, "get denied")
				require.Zero(t, applies)
			case "caller canceled":
				require.ErrorIs(t, err, context.Canceled)
				require.Zero(t, applies)
			}
		})
	}
}

func TestSecondaryNetworkPollingSafeRetryPreservesDesiredAndObservationGate(t *testing.T) {
	base := secondaryRetryBase(t)
	applies := 0
	validationCalls := 0
	observationReads := 0
	desired := secondaryRetryBundle(t, true).Documents()[0].ObjectCopy()
	before := desired.DeepCopy()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if applies > 0 {
				observationReads++
				response := obj.(*unstructured.Unstructured)
				response.SetResourceVersion("applied-1")
				if observationReads > 1 {
					response.SetResourceVersion("controller-observed-2")
				}
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Apply: func(_ context.Context, _ client.WithWatch, ac runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			applies++
			response := desired.DeepCopy()
			response.SetGeneration(1)
			response.SetResourceVersion("applied-1")
			unstructured.RemoveNestedField(response.Object, "spec", "secondaryNetwork")
			ac.(interface{ SetUnstructuredContent(map[string]interface{}) }).SetUnstructuredContent(response.Object)
			return nil
		},
	})
	registry := crstate.NewRegistry()
	registry.Register(desired.GroupVersionKind(), func(_ context.Context, _ client.Client, obj *unstructured.Unstructured) (crstate.Result, error) {
		validationCalls++
		require.Equal(t, before.Object, obj.Object, "validation consumes desired intent")
		if validationCalls == 1 {
			return crstate.Result{State: crstate.StateNotDeployed}, nil
		}
		return crstate.Result{State: crstate.StateSuccess}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, pollUntilTerminalWithRetryableErrorTimeout(ctx, c, registry, desired, "policy", "", 0, time.Millisecond))
	require.Equal(t, 1, applies)
	require.Equal(t, 2, validationCalls)
	require.Equal(t, 2, observationReads, "restored NCP must wait past its apply response before validation")
	require.Equal(t, before.Object, desired.Object)
}
