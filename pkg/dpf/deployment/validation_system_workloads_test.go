// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func validationDaemonMembershipFixture() (*appsv1.DaemonSet, []corev1.Pod, map[string]corev1.Node) {
	set := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: "kube-system", UID: "selected-set", Generation: 1}, Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-proxy"}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"k8s-app": "kube-proxy"}}}}, Status: appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 1, CurrentNumberScheduled: 1, UpdatedNumberScheduled: 1, NumberReady: 1, NumberAvailable: 1}}
	pods := []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "selected", Namespace: "kube-system", UID: "selected-pod", ResourceVersion: "1", Labels: map[string]string{"k8s-app": "kube-proxy"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: set.Name, UID: set.UID, Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{NodeName: "selected-node"}}}
	nodes := map[string]corev1.Node{"selected-device": {ObjectMeta: metav1.ObjectMeta{Name: "selected-node", UID: "node-one"}}}
	return set, pods, nodes
}
func TestValidationDaemonSetCompleteOwnedMembership(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*appsv1.DaemonSet, []corev1.Pod) []corev1.Pod
		fail   bool
	}{
		{"selected current Pod", func(_ *appsv1.DaemonSet, p []corev1.Pod) []corev1.Pod { return p }, false},
		{"hidden extra owned Pod", func(_ *appsv1.DaemonSet, p []corev1.Pod) []corev1.Pod {
			extra := p[0].DeepCopy()
			extra.Name = "hidden"
			extra.UID = "hidden"
			extra.Labels["k8s-app"] = "different"
			return append(p, *extra)
		}, true},
		{"outside selected cohort", func(_ *appsv1.DaemonSet, p []corev1.Pod) []corev1.Pod {
			extra := p[0].DeepCopy()
			extra.Name = "outside"
			extra.UID = "outside"
			extra.Spec.NodeName = "other-node"
			return append(p, *extra)
		}, true},
		{"two owned Pods on same Node", func(_ *appsv1.DaemonSet, p []corev1.Pod) []corev1.Pod {
			extra := p[0].DeepCopy()
			extra.Name = "duplicate"
			extra.UID = "duplicate"
			return append(p, *extra)
		}, true},
		{"unrelated Pod ignored", func(_ *appsv1.DaemonSet, p []corev1.Pod) []corev1.Pod {
			extra := p[0].DeepCopy()
			extra.Name = "unrelated"
			extra.UID = "unrelated"
			extra.OwnerReferences[0].UID = types.UID("unrelated-set")
			extra.Spec.NodeName = "other-node"
			extra.Labels["k8s-app"] = "different"
			return append(p, *extra)
		}, false},
		{"wrong namespace", func(_ *appsv1.DaemonSet, p []corev1.Pod) []corev1.Pod { p[0].Namespace = "other"; return p }, true},
		{"contradictory controller", func(_ *appsv1.DaemonSet, p []corev1.Pod) []corev1.Pod {
			p[0].OwnerReferences = append(p[0].OwnerReferences, metav1.OwnerReference{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "other", UID: "other", Controller: ptr.To(true)})
			return p
		}, true},
		{"mismatched current Pod label", func(_ *appsv1.DaemonSet, p []corev1.Pod) []corev1.Pod { p[0].Labels["k8s-app"] = "different"; return p }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			set, pods, nodes := validationDaemonMembershipFixture()
			pods = test.mutate(set, pods)
			got, err := validationDaemonSetPodMembership(set, pods, nodes)
			if (err != nil) != test.fail {
				t.Fatalf("owner membership: got=%v err=%#v", got, err)
			}
			if err == nil && len(got) != 1 {
				t.Fatal("selected current membership lost")
			}
		})
	}
}
func TestValidationDaemonSetGenerationsAndCounts(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*appsv1.DaemonSet)
		state  validation.State
	}{
		{"current", func(*appsv1.DaemonSet) {}, validation.Passed},
		{"missing observed generation", func(s *appsv1.DaemonSet) { s.Status.ObservedGeneration = 0 }, validation.Pending},
		{"old observed generation", func(s *appsv1.DaemonSet) { s.Generation = 2 }, validation.Pending},
		{"future observed generation", func(s *appsv1.DaemonSet) { s.Status.ObservedGeneration = 2 }, validation.Failed},
		{"negative observed generation", func(s *appsv1.DaemonSet) { s.Status.ObservedGeneration = -1 }, validation.Failed},
		{"invalid generation", func(s *appsv1.DaemonSet) { s.Generation = 0 }, validation.Failed},
		{"current node count exceeds desired", func(s *appsv1.DaemonSet) {
			s.Status.CurrentNumberScheduled = 2
			s.Status.UpdatedNumberScheduled = 2
			s.Status.NumberReady = 2
			s.Status.NumberAvailable = 1
			s.Status.NumberUnavailable = 0
		}, validation.Failed},
		{"contradictory unavailable count", func(s *appsv1.DaemonSet) { s.Status.NumberUnavailable = 1 }, validation.Failed},
		{"contradictory available count", func(s *appsv1.DaemonSet) { s.Status.NumberAvailable = 2 }, validation.Failed},
		{"negative ready count", func(s *appsv1.DaemonSet) { s.Status.NumberReady = -1 }, validation.Failed},
		{"ordinary rollout", func(s *appsv1.DaemonSet) { s.Status.NumberReady = 0; s.Status.NumberAvailable = 0 }, validation.Pending},
	} {
		t.Run(test.name, func(t *testing.T) {
			set, _, _ := validationDaemonMembershipFixture()
			test.mutate(set)
			err := validationDaemonSetReady(set, 1)
			if test.state == validation.Passed {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			failure, ok := err.(*validationCheckError)
			if !ok || failure.state != test.state {
				t.Fatalf("wrong terminal/progress classification: %#v", err)
			}
		})
	}
	for _, value := range []any{"malformed", int64(-1)} {
		object := validationObject("apps/v1", "DaemonSet", "kube-system", "selected")
		object.Object["metadata"].(map[string]any)["generation"] = value
		set, err := validationDaemonSetFromObject(object)
		if err == nil {
			err = validationDaemonSetReady(set, 1)
		}
		failure, ok := err.(*validationCheckError)
		if !ok || failure.state != validation.Failed {
			t.Fatalf("malformed typed observation is retryable: %#v", err)
		}
	}
}

type validationDaemonSnapshotObserver struct {
	checks []validation.CheckSpec
	set    *appsv1.DaemonSet
	calls  int
}

func (o *validationDaemonSnapshotObserver) ObserveResources(_ context.Context, progress validation.Progress) (validation.ResourceSnapshot, error) {
	o.calls++
	r := newValidationRecorder(o.checks, progress)
	for _, spec := range o.checks {
		if spec.ID == "system/daemon" {
			r.run(spec.ID, func() error { return validationDaemonSetReady(o.set, 1) })
		} else {
			r.record(spec.ID, validation.Passed, validation.ReasonNone, "Independent dependency is ready.", "")
		}
	}
	return r.snapshot(), nil
}
func (*validationDaemonSnapshotObserver) OpenRuntime(context.Context, validation.Progress) (validation.RuntimeSession, error) {
	panic("resources-only fixture")
}
func TestValidationFutureDaemonGenerationIsTerminalThroughRunner(t *testing.T) {
	checks := []validation.CheckSpec{}
	for _, area := range []validation.Area{validation.AreaManagement, validation.AreaOperator, validation.AreaDefinitions, validation.AreaSystem, validation.AreaTenant, validation.AreaDevices, validation.AreaSoftware, validation.AreaHBN, validation.AreaWorkloads, validation.AreaPhysical} {
		id := string(area) + "/coverage"
		if area == validation.AreaSystem {
			id = "system/daemon"
		}
		spec := validationSpec(id, area, string(area), true)
		spec.FullOnly = area == validation.AreaPhysical
		checks = append(checks, spec)
	}
	plan, err := validation.NewPlan(validation.PlanInput{Mode: validation.ModeResources, Checks: checks, Runtime: validation.RuntimeScope{Devices: []validation.DeviceID{"selected"}, Hosts: []validation.HostID{"selected"}}})
	if err != nil {
		t.Fatal(err)
	}
	active := []validation.CheckSpec{}
	for _, spec := range checks {
		if !spec.FullOnly {
			active = append(active, spec)
		}
	}
	set, _, _ := validationDaemonMembershipFixture()
	set.Status.ObservedGeneration = 2
	observer := &validationDaemonSnapshotObserver{checks: active, set: set}
	runner, err := validation.NewRunner(plan, observer, validation.Options{Wait: time.Second, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	result := runner.Run(context.Background())
	if observer.calls != 1 || result.Verdict != validation.VerdictFailed {
		t.Fatalf("impossible future state was retried: verdict=%s calls=%d", result.Verdict, observer.calls)
	}
}
func TestValidationPodCoveragePreservesPending(t *testing.T) {
	pending := validationFailure(validation.Pending, validation.ReasonProgressing, "Current owned Pod is reconciling.")
	failed := validationFailure(validation.Failed, validation.ReasonHealth, "Known Pod contradiction.")
	unverified := validationFailure(validation.Unverified, validation.ReasonDependency, "Current Pod dependency is unavailable.")
	if validationPodCoverageError(nil, pending) != pending || validationPodCoverageError(pending, nil) != pending || validationPodCoverageError(pending, failed) != failed || validationPodCoverageError(failed, pending) != failed || validationPodCoverageError(pending, unverified) != unverified {
		t.Fatal("Pod coverage masked concrete failure or converted pending to failure")
	}
}
func TestValidationKubeProxyEmptyCIDRAction(t *testing.T) {
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: "kube-system", UID: "config", ResourceVersion: "1"}, Data: map[string]string{"config.conf": "apiVersion: kubeproxy.config.k8s.io/v1alpha1\nkind: KubeProxyConfiguration\nbindAddress: 0.0.0.0\nclusterCIDR: \"\"\n", "kubeconfig.conf": "unused-before-specific-error"}}
	err := validationKubeProxyConfiguration(config, "10.244.0.0/16", "https://192.0.2.10:6443")
	failure, ok := err.(*validationCheckError)
	if !ok || failure.state != validation.Failed || failure.reason != validation.ReasonHealth || !strings.Contains(failure.summary, "#1291") || !strings.Contains(failure.summary, "source NAT") {
		t.Fatalf("known producer defect lost actionable health diagnostic: %#v", err)
	}
}
