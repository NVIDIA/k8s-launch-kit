// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
)

// Exercise the public safe option across CRD discovery, controller-specific
// waits, deletion of the root policy, and Helm uninstall.
func TestSafeCleanGateBeforeHelm(t *testing.T) {
	for _, status := range []string{"Succeeded", "Failed"} {
		t.Run(status, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, apiextv1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, appsv1.AddToScheme(scheme))
			rail := testCleanObject(schema.GroupVersionKind{Group: "spectrumx.nvidia.com", Version: "v1alpha2", Kind: "SpectrumXRailPoolConfig"}, "operator-system", "rail")
			policy := sriovCleanObject("SriovNetworkNodePolicy", "policy")
			state := sriovCleanState()
			state.Object["status"].(map[string]interface{})["syncStatus"] = status
			root := testCleanObject(clusterScopedCleanKinds[4], "", "nic-cluster-policy")
			objects := []client.Object{root}
			for _, obj := range []*unstructured.Unstructured{rail, policy, state} {
				gvk := obj.GroupVersionKind()
				crd := testNamespacedCRD()
				crd.Name = gvk.Kind
				crd.Spec.Group = gvk.Group
				crd.Spec.Names.Kind = gvk.Kind
				crd.Spec.Versions[0].Name = gvk.Version
				objects = append(objects, obj, crd)
			}
			var removed []string
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					options := (&client.DeleteOptions{}).ApplyOptions(opts)
					require.NotNil(t, options.PropagationPolicy)
					if obj.GetName() == root.GetName() {
						require.Equal(t, metav1.DeletePropagationBackground, *options.PropagationPolicy)
					} else {
						require.Equal(t, metav1.DeletePropagationForeground, *options.PropagationPolicy)
					}
					removed = append(removed, obj.GetName())
					if err := c.Delete(ctx, obj, opts...); err != nil {
						return err
					}
					if obj.GetName() == rail.GetName() {
						// A final reconciliation can create another node policy as
						// the rail pool disappears, after the initial inventory.
						late := sriovCleanObject("SriovNetworkNodePolicy", "replacement")
						return c.Create(ctx, late)
					}
					return nil
				},
			}).Build()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result, err := cleanWithUninstaller(ctx, c, CleanOptions{Namespace: "operator-system", RestConfig: &rest.Config{}, PollInterval: time.Millisecond}, func(context.Context, *rest.Config, string, time.Duration) (bool, error) {
				removed = append(removed, "helm")
				return true, nil
			})
			if status == "Failed" {
				require.ErrorContains(t, err, "reset failed")
				require.Equal(t, []string{"rail", "policy", "replacement"}, removed)
				require.Equal(t, 3, result.CustomResourcesDeleted)
				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{"rail", "policy", "replacement", "worker", "nic-cluster-policy", "helm"}, removed)
			require.Equal(t, 5, result.CustomResourcesDeleted)
		})
	}
}

func TestSafeCleanWaitsForPolicyPodAfterParentsDisappear(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	const ns = "operator-system"
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "controller", Namespace: ns, UID: "deployment",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(&metav1.PartialObjectMetadata{
			ObjectMeta: metav1.ObjectMeta{Name: "policy", UID: "policy"},
		}, schema.GroupVersionKind{Group: "mellanox.com", Version: "v1alpha1", Kind: "NicNodePolicy"})},
	}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "controller-rs", Namespace: ns, UID: "rs",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "controller-pod", Namespace: ns, UID: "pod",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))},
	}}
	unrelated := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "platform", Namespace: ns, UID: "platform"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, rs, pod, unrelated).Build()
	workloads, err := loadPolicyWorkloads(t.Context(), c, ns)
	require.NoError(t, err)
	pending, err := workloads.observeAndSave(t.Context(), c, ns)
	require.NoError(t, err)
	require.Equal(t, []string{"Deployment controller", "Pod controller-pod", "ReplicaSet controller-rs"}, pending)
	require.NoError(t, c.Delete(t.Context(), deployment))
	require.NoError(t, c.Delete(t.Context(), rs))
	// Losing an owner reference does not establish that the observed Pod exited.
	pod.OwnerReferences = nil
	require.NoError(t, c.Update(t.Context(), pod))
	// Start a fresh invocation after all controller ancestors have disappeared.
	workloads, err = loadPolicyWorkloads(t.Context(), c, ns)
	require.NoError(t, err)
	waitCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err = workloads.wait(waitCtx, c, ns, time.Millisecond)
	require.ErrorContains(t, err, "Pod controller-pod")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, c.Delete(t.Context(), pod))
	require.NoError(t, workloads.wait(t.Context(), c, ns, time.Millisecond))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(unrelated), unrelated))
	require.True(t, apierrors.IsNotFound(c.Get(t.Context(), client.ObjectKey{Name: policyWorkloadCheckpoint, Namespace: ns}, &corev1.ConfigMap{})))
}

func TestPolicyWorkloadCheckpointFailurePrecedesRootDeletion(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, apiextv1.AddToScheme(scheme))
	root := testCleanObject(clusterScopedCleanKinds[4], "", "nic-cluster-policy")
	root.SetUID("policy-uid")
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: "managed", Namespace: "operator", UID: "daemon-uid",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(root, root.GroupVersionKind())},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, ds).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			require.Equal(t, policyWorkloadCheckpoint, obj.GetName())
			return errors.New("checkpoint write denied")
		},
	}).Build()
	_, err := Clean(t.Context(), c, CleanOptions{Namespace: "operator", KeepHelmChart: true})
	require.ErrorContains(t, err, "checkpoint write denied")
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(root), root))
	conflict := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: policyWorkloadCheckpoint, Namespace: "operator"}, Data: map[string]string{"unrelated": "value"}}
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, ds, conflict).Build()
	_, err = Clean(t.Context(), c, CleanOptions{Namespace: "operator", KeepHelmChart: true})
	require.ErrorContains(t, err, "unexpected policy workload checkpoint")
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(root), root))
}
