// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

var (
	testNamespacedGVK = schema.GroupVersionKind{
		Group: "example.com", Version: "v1", Kind: "Widget",
	}
	testUnrelatedClusterGVK = schema.GroupVersionKind{
		Group: "example.com", Version: "v1", Kind: "ClusterWidget",
	}
)

func newCleanTestClient(t *testing.T, objects ...runtime.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, apiextv1.AddToScheme(scheme))

	gvks := append([]schema.GroupVersionKind{}, clusterScopedCleanKinds...)
	gvks = append(gvks, testNamespacedGVK, testUnrelatedClusterGVK)
	for _, gvk := range gvks {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		scheme.AddKnownTypeWithName(gvk, obj)

		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		scheme.AddKnownTypeWithName(list.GroupVersionKind(), list)
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objects...).
		Build()
}

func testNamespacedCRD() *apiextv1.CustomResourceDefinition {
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextv1.CustomResourceDefinitionNames{
				Plural: "widgets",
				Kind:   testNamespacedGVK.Kind,
			},
			Scope: apiextv1.NamespaceScoped,
			Versions: []apiextv1.CustomResourceDefinitionVersion{{
				Name: "v1", Served: true, Storage: true,
			}},
		},
	}
}

func testCleanObject(gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

func TestListNamespacedCustomResources(t *testing.T) {
	target := testCleanObject(testNamespacedGVK, "operator-system", "target")
	other := testCleanObject(testNamespacedGVK, "application", "preserved")
	kubeClient := newCleanTestClient(t, testNamespacedCRD(), target, other)

	refs, err := listNamespacedCustomResources(context.Background(), kubeClient, "operator-system")
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, cleanObjectRef{
		GVK: testNamespacedGVK, Namespace: "operator-system", Name: "target",
	}, refs[0])
}

func TestDeleteNetworkOperatorCustomResources(t *testing.T) {
	target := testCleanObject(testNamespacedGVK, "operator-system", "target")
	otherNamespace := testCleanObject(testNamespacedGVK, "application", "preserved")
	clusterScoped := make([]*unstructured.Unstructured, 0, len(clusterScopedCleanKinds))
	for i, gvk := range clusterScopedCleanKinds {
		clusterScoped = append(clusterScoped, testCleanObject(gvk, "", fmt.Sprintf("policy-%d", i)))
	}
	unrelatedCluster := testCleanObject(testUnrelatedClusterGVK, "", "preserved")
	crd := testNamespacedCRD()
	objects := []runtime.Object{crd, target, otherNamespace, unrelatedCluster}
	for _, obj := range clusterScoped {
		objects = append(objects, obj)
	}
	kubeClient := newCleanTestClient(t, objects...)

	deleted, err := deleteNetworkOperatorCustomResources(
		context.Background(), kubeClient, "operator-system", time.Millisecond, ui.NewSilent())
	require.NoError(t, err)
	assert.Equal(t, 1+len(clusterScopedCleanKinds), deleted)

	deletedObjects := append([]*unstructured.Unstructured{target}, clusterScoped...)
	for _, deletedObject := range deletedObjects {
		actual := &unstructured.Unstructured{}
		actual.SetGroupVersionKind(deletedObject.GroupVersionKind())
		err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(deletedObject), actual)
		assert.True(t, apierrors.IsNotFound(err), "%s should be deleted", deletedObject.GetName())
	}

	for _, preserved := range []*unstructured.Unstructured{otherNamespace, unrelatedCluster} {
		actual := &unstructured.Unstructured{}
		actual.SetGroupVersionKind(preserved.GroupVersionKind())
		require.NoError(t, kubeClient.Get(
			context.Background(), client.ObjectKeyFromObject(preserved), actual))
	}
	actualCRD := &apiextv1.CustomResourceDefinition{}
	require.NoError(t, kubeClient.Get(
		context.Background(), client.ObjectKey{Name: crd.Name}, actualCRD))
}

func TestDeleteNetworkOperatorCustomResourcesSignalsAllBeforeWaiting(t *testing.T) {
	namespaced := testCleanObject(testNamespacedGVK, "operator-system", "namespaced")
	clusterScoped := testCleanObject(clusterScopedCleanKinds[3], "", "cluster-scoped")
	baseClient := newCleanTestClient(t, testNamespacedCRD(), namespaced, clusterScoped)
	targets := []*unstructured.Unstructured{namespaced, clusterScoped}
	var deletionSignals []string
	kubeClient := interceptor.NewClient(baseClient, interceptor.Funcs{
		Delete: func(
			ctx context.Context,
			underlying client.WithWatch,
			obj client.Object,
			opts ...client.DeleteOption,
		) error {
			deletionSignals = append(deletionSignals, obj.GetName())
			if len(deletionSignals) < len(targets) {
				// Simulate a finalizer that keeps the first CR until the other CR
				// has also entered deletion.
				return nil
			}
			for _, target := range targets {
				if err := underlying.Delete(ctx, target.DeepCopy(), opts...); err != nil {
					return err
				}
			}
			return nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	deleted, err := deleteNetworkOperatorCustomResources(
		ctx, kubeClient, "operator-system", time.Millisecond, ui.NewSilent())
	require.NoError(t, err)
	assert.Equal(t, 2, deleted)
	assert.Equal(t, []string{"namespaced", "cluster-scoped"}, deletionSignals)
}

func TestDeleteNetworkOperatorCustomResourcesSweepsRecreatedClusterResource(t *testing.T) {
	original := testCleanObject(clusterScopedCleanKinds[3], "", "original-node-policy")
	nicClusterPolicy := testCleanObject(clusterScopedCleanKinds[4], "", "nic-cluster-policy")
	recreated := testCleanObject(clusterScopedCleanKinds[3], "", "recreated-node-policy")
	baseClient := newCleanTestClient(t, original, nicClusterPolicy)
	recreatedOnce := false
	kubeClient := interceptor.NewClient(baseClient, interceptor.Funcs{
		Delete: func(
			ctx context.Context,
			underlying client.WithWatch,
			obj client.Object,
			opts ...client.DeleteOption,
		) error {
			if err := underlying.Delete(ctx, obj, opts...); err != nil {
				return err
			}
			if !recreatedOnce && obj.GetName() == original.GetName() {
				recreatedOnce = true
				return underlying.Create(ctx, recreated.DeepCopy())
			}
			return nil
		},
	})

	deleted, err := deleteNetworkOperatorCustomResources(
		context.Background(), kubeClient, "operator-system", time.Millisecond, ui.NewSilent())
	require.NoError(t, err)
	assert.True(t, recreatedOnce)
	assert.Equal(t, 3, deleted)

	actual := &unstructured.Unstructured{}
	actual.SetGroupVersionKind(recreated.GroupVersionKind())
	err = kubeClient.Get(context.Background(), client.ObjectKeyFromObject(recreated), actual)
	assert.True(t, apierrors.IsNotFound(err), "recreated cluster-scoped CR should be swept")
}

func TestCleanKeepsHelmChart(t *testing.T) {
	uninstallCalled := false
	result, err := cleanWithUninstaller(
		context.Background(),
		newCleanTestClient(t),
		CleanOptions{Namespace: "operator-system", KeepHelmChart: true},
		func(context.Context, *rest.Config, string, time.Duration) (bool, error) {
			uninstallCalled = true
			return true, nil
		},
	)
	require.NoError(t, err)
	assert.False(t, uninstallCalled)
	assert.False(t, result.HelmReleaseRemoved)
}

func TestCleanUninstallsHelmAfterCustomResources(t *testing.T) {
	target := testCleanObject(testNamespacedGVK, "operator-system", "target")
	kubeClient := newCleanTestClient(t, testNamespacedCRD(), target)
	restConfig := &rest.Config{Host: "https://example.invalid"}
	uninstallCalled := false

	result, err := cleanWithUninstaller(
		context.Background(),
		kubeClient,
		CleanOptions{
			Namespace:    "operator-system",
			RestConfig:   restConfig,
			PollInterval: time.Millisecond,
		},
		func(_ context.Context, actualConfig *rest.Config, namespace string, _ time.Duration) (bool, error) {
			uninstallCalled = true
			assert.Same(t, restConfig, actualConfig)
			assert.Equal(t, "operator-system", namespace)

			actual := &unstructured.Unstructured{}
			actual.SetGroupVersionKind(testNamespacedGVK)
			err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(target), actual)
			assert.True(t, apierrors.IsNotFound(err), "custom resources must be gone before Helm uninstall")
			return true, nil
		},
	)
	require.NoError(t, err)
	assert.True(t, uninstallCalled)
	assert.Equal(t, 1, result.CustomResourcesDeleted)
	assert.True(t, result.HelmReleaseRemoved)
}

func TestServedStorageVersion(t *testing.T) {
	crd := testNamespacedCRD()
	crd.Spec.Versions = []apiextv1.CustomResourceDefinitionVersion{
		{Name: "v1alpha1", Served: true, Storage: false},
		{Name: "v1", Served: true, Storage: true},
	}
	assert.Equal(t, "v1", servedStorageVersion(crd))

	crd.Spec.Versions[1].Served = false
	assert.Equal(t, "v1alpha1", servedStorageVersion(crd))

	assert.Empty(t, servedStorageVersion(nil))
}
