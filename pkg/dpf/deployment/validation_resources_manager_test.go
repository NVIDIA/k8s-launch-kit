// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"maps"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

// Exercise the native operator row, rather than calling the child verifier
// directly: a healthy retained Deployment cannot substitute for its live Pod.
func TestValidationManagementOperatorObservesAdmittedPods(t *testing.T) {
	o := validationResourceFixture(t)
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Pod)
		state  validation.State
	}{
		{"current owned Pod", func(*corev1.Pod) {}, validation.Passed},
		{"same image wrong ServiceAccount", func(p *corev1.Pod) {
			p.Spec.ServiceAccountName = "wrong-account"
			p.Spec.DeprecatedServiceAccount = "wrong-account"
		}, validation.Failed},
		{"same image changed process", func(p *corev1.Pod) { p.Spec.Containers[0].Command = []string{"sleep", "infinity"} }, validation.Failed},
		{"same image changed arguments", func(p *corev1.Pod) { p.Spec.Containers[0].Args = []string{"--unexpected"} }, validation.Failed},
		{"injected requested network", func(p *corev1.Pod) {
			if p.Annotations == nil {
				p.Annotations = map[string]string{}
			}
			p.Annotations["k8s.v1.cni.cncf.io/networks"] = "unexpected"
		}, validation.Failed},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := managementFakeWithReadiness(t, o.desired.plan.Digest(), o.desired.manager, o.desired.crds)
			var deployment appsv1.Deployment
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(base.manager.Object, &deployment); err != nil {
				t.Fatal(err)
			}
			// A real API response serializes typed empty optional collections with
			// omitempty; retained Helm YAML can still spell those as [] or {}.
			serialized, err := json.Marshal(&deployment)
			if err != nil {
				t.Fatal(err)
			}
			base.manager = &unstructured.Unstructured{}
			if err := base.manager.UnmarshalJSON(serialized); err != nil {
				t.Fatal(err)
			}
			deployment = appsv1.Deployment{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(base.manager.Object, &deployment); err != nil {
				t.Fatal(err)
			}
			if deployment.Spec.Template.Spec.AutomountServiceAccountToken == nil || *deployment.Spec.Template.Spec.AutomountServiceAccountToken {
				t.Fatal("retained source fixture must explicitly disable automatic credential admission")
			}
			set := appsv1.ReplicaSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSet"}, ObjectMeta: metav1.ObjectMeta{Name: "current-rs", Namespace: deployment.Namespace, UID: "current-rs", ResourceVersion: "2", Labels: maps.Clone(deployment.Spec.Template.Labels), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, UID: deployment.UID, Controller: ptr.To(true)}}}, Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To(int32(1)), Selector: deployment.Spec.Selector.DeepCopy(), Template: *deployment.Spec.Template.DeepCopy()}}
			for _, labels := range []map[string]string{set.Labels, set.Spec.Selector.MatchLabels, set.Spec.Template.Labels} {
				labels[appsv1.DefaultDeploymentUniqueLabelKey] = "current-revision"
			}
			pod := corev1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: "current-pod", Namespace: deployment.Namespace, UID: "current-pod", ResourceVersion: "3", Labels: maps.Clone(set.Spec.Template.Labels), Annotations: maps.Clone(set.Spec.Template.Annotations), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: set.Name, UID: set.UID, Controller: ptr.To(true)}}}, Spec: *deployment.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
			// Standard admission fields are supplied independently of the comparator.
			pod.Spec.NodeName = "selected-node"
			pod.Spec.Priority = ptr.To(int32(0))
			pod.Spec.PreemptionPolicy = ptr.To(corev1.PreemptLowerPriority)
			pod.Spec.EnableServiceLinks = ptr.To(true)
			pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{Key: corev1.TaintNodeNotReady, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To(int64(300))}, corev1.Toleration{Key: corev1.TaintNodeUnreachable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To(int64(300))})
			for _, container := range pod.Spec.Containers {
				pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{Name: container.Name, ImageID: "sha256:running", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})
			}
			test.mutate(&pod)
			reads := map[string]int{}
			reader := &validationReadFake{get: base.Get, discover: base.discoverResourceList, list: func(_ context.Context, wanted *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
				var object any
				switch wanted.GetKind() {
				case "ReplicaSet":
					object = &set
				case "Pod":
					object = &pod
				default:
					return nil, nil
				}
				reads[wanted.GetKind()]++
				if wanted.GetNamespace() != deployment.Namespace || selector != "" {
					t.Errorf("child read lost bounded complete selected namespace: %s %q", wanted.GetNamespace(), selector)
				}
				data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
				if err != nil {
					return nil, err
				}
				return []unstructured.Unstructured{{Object: data}}, nil
			}}
			recorder := newValidationRecorder(o.validationResourceChecks(), nil)
			if err := o.observeManagementResources(context.Background(), reader, recorder); err != nil {
				t.Fatal(err)
			}
			outcome := recorder.outcomes["operator/manager"]
			if outcome.State != test.state {
				t.Fatalf("main operator actual-Pod observation: %+v", outcome)
			}
			if reads["ReplicaSet"] != 1 || reads["Pod"] != 1 {
				t.Fatalf("native main manager skipped actual owned children: %v", reads)
			}
			if test.state == validation.Failed && outcome.Reason != validation.ReasonHealth {
				t.Fatalf("known admitted function drift did not retain health reason: %+v", outcome)
			}
		})
	}
}
