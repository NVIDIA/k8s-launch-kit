// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

// This read-only seam applies label filtering like a real API server, so a
// selector-filtered implementation cannot accidentally see mislabelled owners.
type deploymentChildrenReader struct {
	deployment *appsv1.Deployment
	sets       []appsv1.ReplicaSet
	pods       []corev1.Pod
	listScopes []string
	selectors  []string
}

func (r *deploymentChildrenReader) Get(_ context.Context, object *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if object.GetKind() != "Deployment" || object.GetNamespace() != r.deployment.Namespace || object.GetName() != r.deployment.Name {
		return nil, fmt.Errorf("unexpected read identity")
	}
	value, err := runtime.DefaultUnstructuredConverter.ToUnstructured(r.deployment)
	return &unstructured.Unstructured{Object: value}, err
}

func (r *deploymentChildrenReader) List(_ context.Context, object *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	r.listScopes = append(r.listScopes, object.GetNamespace())
	r.selectors = append(r.selectors, selector)
	selected, err := labels.Parse(selector)
	if err != nil {
		return nil, err
	}
	var result []unstructured.Unstructured
	appendObject := func(value any) error {
		data, convertErr := runtime.DefaultUnstructuredConverter.ToUnstructured(value)
		if convertErr != nil {
			return convertErr
		}
		current := unstructured.Unstructured{Object: data}
		if current.GetNamespace() == object.GetNamespace() && selected.Matches(labels.Set(current.GetLabels())) {
			result = append(result, current)
		}
		return nil
	}
	switch object.GetKind() {
	case "ReplicaSet":
		for i := range r.sets {
			if err := appendObject(&r.sets[i]); err != nil {
				return nil, err
			}
		}
	case "Pod":
		for i := range r.pods {
			if err := appendObject(&r.pods[i]); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("unexpected list kind")
	}
	return result, nil
}

func deploymentChildrenFixture(t *testing.T) *deploymentChildrenReader {
	t.Helper()
	admitted, template := validationAdmittedPodFixture(t)
	one, controller := int32(1), true
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: "manager", Namespace: "selected", UID: "deployment-one", ResourceVersion: "10", Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "selected"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "selected"}},
				Spec: *template.DeepCopy()},
		},
	}
	set := appsv1.ReplicaSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSet"},
		ObjectMeta: metav1.ObjectMeta{Name: "manager-revision", Namespace: "selected", UID: "set-one", ResourceVersion: "11",
			Labels:          map[string]string{"app": "selected", appsv1.DefaultDeploymentUniqueLabelKey: "revision"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, UID: deployment.UID, Controller: &controller}}},
		Spec: appsv1.ReplicaSetSpec{Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "selected", appsv1.DefaultDeploymentUniqueLabelKey: "revision"}},
			Template: *deployment.Spec.Template.DeepCopy()},
	}
	set.Spec.Template.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "revision"
	pod := corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: "manager-current", Namespace: "selected", UID: "pod-one", ResourceVersion: "12",
			Labels:          map[string]string{"app": "selected", appsv1.DefaultDeploymentUniqueLabelKey: "revision"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: set.Name, UID: set.UID, Controller: &controller}}},
		Spec: *admitted.Spec.DeepCopy(),
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "manager", ImageID: "sha256:running", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}},
	}
	pod.Spec.NodeName = "selected-node"
	return &deploymentChildrenReader{deployment: deployment, sets: []appsv1.ReplicaSet{set}, pods: []corev1.Pod{pod}, listScopes: []string{}, selectors: []string{}}
}

func TestValidationDeploymentChildrenUseCompleteOwnedMembership(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*deploymentChildrenReader)
		state  validation.State
	}{
		{name: "current revision", mutate: func(*deploymentChildrenReader) {}},
		{name: "Pod-only command drift", state: validation.Failed, mutate: func(r *deploymentChildrenReader) {
			r.pods[0].Spec.Containers[0].Command = []string{"sleep", "infinity"}
		}},
		{name: "Pod-only arguments drift", state: validation.Failed, mutate: func(r *deploymentChildrenReader) { r.pods[0].Spec.Containers[0].Args = []string{"--unexpected"} }},
		{name: "Pod-only ServiceAccount drift", state: validation.Failed, mutate: func(r *deploymentChildrenReader) {
			r.pods[0].Spec.ServiceAccountName = "wrong"
			r.pods[0].Spec.DeprecatedServiceAccount = "wrong"
		}},
		{name: "Pod-only requested network drift", state: validation.Failed, mutate: func(r *deploymentChildrenReader) {
			r.pods[0].Annotations = map[string]string{"k8s.v1.cni.cncf.io/networks": "unexpected"}
		}},
		{name: "extra owned Pod hidden by Deployment selector", state: validation.Failed, mutate: func(r *deploymentChildrenReader) {
			pod := r.pods[0].DeepCopy()
			pod.Name, pod.UID, pod.Labels["app"] = "hidden", "hidden-pod", "wrong"
			r.pods = append(r.pods, *pod)
		}},
		{name: "extra owned Pod hidden by revision selector", state: validation.Failed, mutate: func(r *deploymentChildrenReader) {
			pod := r.pods[0].DeepCopy()
			pod.Name, pod.UID, pod.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "hidden", "hidden-pod", "wrong"
			r.pods = append(r.pods, *pod)
		}},
		{name: "extra active owned ReplicaSet hidden by labels", state: validation.Failed, mutate: func(r *deploymentChildrenReader) {
			set := r.sets[0].DeepCopy()
			set.Name, set.UID, set.Labels["app"] = "hidden", "hidden-set", "wrong"
			r.sets = append(r.sets, *set)
		}},
		{name: "changed ReplicaSet selector", state: validation.Failed, mutate: func(r *deploymentChildrenReader) {
			r.sets[0].Spec.Selector.MatchLabels["injected"] = "true"
			r.sets[0].Spec.Template.Labels["injected"] = "true"
		}},
		{name: "missing ReplicaSet selector", state: validation.Failed, mutate: func(r *deploymentChildrenReader) { r.sets[0].Spec.Selector = nil }},
		{name: "inconsistent revision hash", state: validation.Failed, mutate: func(r *deploymentChildrenReader) {
			r.sets[0].Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "changed"
		}},
		{name: "old active Pod template remains progressing", state: validation.Pending, mutate: func(r *deploymentChildrenReader) {
			r.sets[0].Spec.Template.Spec.Containers[0].Image = "registry.example/manager:old"
		}},
		{name: "unknown owned ReplicaSet count", state: validation.Failed, mutate: func(r *deploymentChildrenReader) { r.sets[0].Spec.Replicas = nil }},
		{name: "extra ready owned Pod", state: validation.Failed, mutate: func(r *deploymentChildrenReader) {
			pod := r.pods[0].DeepCopy()
			pod.Name, pod.UID = "extra", "extra-pod"
			r.pods = append(r.pods, *pod)
		}},
		{name: "unrelated and historical objects", mutate: func(r *deploymentChildrenReader) {
			set := r.sets[0].DeepCopy()
			set.Name, set.UID, set.OwnerReferences[0].UID = "unrelated", "unrelated-set", "other-deployment"
			r.sets = append(r.sets, *set)
			pod := r.pods[0].DeepCopy()
			pod.Name, pod.UID, pod.OwnerReferences[0].UID = "unrelated", "unrelated-pod", "other-set"
			r.pods = append(r.pods, *pod)
			old := r.sets[0].DeepCopy()
			zero := int32(0)
			old.Name, old.UID, old.Spec.Replicas, old.Spec.Selector = "historical", "historical-set", &zero, nil
			old.Labels["app"], old.Spec.Template.Spec.Containers[0].Image = "historical", "registry.example/manager:old"
			r.sets = append(r.sets, *old)
			historicalPod := r.pods[0].DeepCopy()
			historicalPod.Name, historicalPod.UID, historicalPod.OwnerReferences[0].Name, historicalPod.OwnerReferences[0].UID = "historical", "historical-pod", old.Name, old.UID
			historicalPod.Labels["app"] = "wrong"
			r.pods = append(r.pods, *historicalPod)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := deploymentChildrenFixture(t)
			test.mutate(reader)
			object, err := reader.Get(context.Background(), validationObject("apps/v1", "Deployment", "selected", "manager"))
			if err != nil {
				t.Fatal(err)
			}
			before := object.DeepCopy()
			err = verifyValidationDeploymentPods(context.Background(), reader, object)
			if test.state == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var failure *validationCheckError
				if !errors.As(err, &failure) || failure.state != test.state {
					t.Fatalf("wanted %s, got %v", test.state, err)
				}
			}
			if !reflect.DeepEqual(before.Object, object.Object) {
				t.Fatal("ownership observation mutated the Deployment")
			}
			for i, scope := range reader.listScopes {
				if scope != "selected" || reader.selectors[i] != "" {
					t.Fatalf("ownership list hid candidates: namespace=%q selector=%q", scope, reader.selectors[i])
				}
			}
		})
	}
}
