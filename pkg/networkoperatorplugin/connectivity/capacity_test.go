// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package connectivity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestOpenShiftDeviceCapacityChecksSelectedWorkers(t *testing.T) {
	device := corev1.ResourceName("openshift.io/sriov_resource")
	ds := &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "workloads"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{"worker-a"},
				}}}},
			}}},
			Containers: []corev1.Container{{Name: "test", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{device: resource.MustParse("1")},
			}}},
		}}},
	}
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ds)
	require.NoError(t, err)
	manifest := &unstructured.Unstructured{Object: object}
	nodeA := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a", Labels: map[string]string{"kubernetes.io/hostname": "worker-a"}},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{device: resource.MustParse("4")}}}
	nodeB := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-b", Labels: map[string]string{"kubernetes.io/hostname": "worker-b"}},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{}}}
	c := fake.NewClientBuilder().WithObjects(nodeA, nodeB).Build()
	require.NoError(t, checkOpenShiftDeviceCapacity(context.Background(), c, []*unstructured.Unstructured{manifest}))

	nodeA.Status.Allocatable[device] = resource.MustParse("0")
	c = fake.NewClientBuilder().WithObjects(nodeA, nodeB).Build()
	require.ErrorContains(t, checkOpenShiftDeviceCapacity(context.Background(), c, []*unstructured.Unstructured{manifest}), "allocatable capacity is insufficient")

	nodeA.Labels["kubernetes.io/hostname"] = "renamed-a"
	c = fake.NewClientBuilder().WithObjects(nodeA, nodeB).Build()
	require.ErrorContains(t, checkOpenShiftDeviceCapacity(context.Background(), c, []*unstructured.Unstructured{manifest}), "matches no OpenShift worker")
}
