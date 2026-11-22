// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"reflect"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestHBNBindingManualApplicationHealthShape(t *testing.T) {
	for _, scenario := range []string{"absent", "healthy", "null", "empty", "malformed", "status-null", "unhealthy", "absent-ds-unready", "absent-pod-unready"} {
		t.Run(scenario, func(t *testing.T) {
			intent, now := hbnBindingTestIntent()
			app := hbnApplicationFixture(intent, now)
			ds, pod := bindingFixture(intent, "cluster-dpu-hbn-ds")
			resource := app.Object["status"].(map[string]any)["resources"].([]any)[0].(map[string]any)
			want := hbnPodBindingReason("")
			switch scenario {
			case "absent", "absent-ds-unready", "absent-pod-unready":
				// Argo's actual appTree health mode omitted resource.health on
				// the Synced DaemonSet while Application health was Healthy.
				delete(resource, "health")
			case "null":
				resource["health"] = nil
				want = hbnBindingResourceUnknown
			case "empty":
				resource["health"] = map[string]any{}
				want = hbnBindingResourceUnknown
			case "malformed":
				resource["health"] = "Healthy"
				want = hbnBindingResourceUnknown
			case "status-null":
				resource["health"] = map[string]any{"status": nil}
				want = hbnBindingResourceUnknown
			case "unhealthy":
				resource["health"] = map[string]any{"status": "Degraded"}
				want = hbnBindingResourceUnknown
			}
			if scenario == "absent-ds-unready" {
				ds.Status.NumberReady = 0
				want = hbnBindingResourceUnknown
			}
			if scenario == "absent-pod-unready" {
				pod.Status.InitContainerStatuses[1].Ready = false
				want = hbnBindingPodNotReady
			}
			before := app.DeepCopy()
			_, err := validateHBNPodBinding(intent, hbnServiceFixture(intent), app, []appsv1.DaemonSet{ds}, []corev1.Pod{pod}, now)
			assertHBNBindingReason(t, err, want)
			if !reflect.DeepEqual(before, app) {
				t.Fatal("binding invented or mutated Application health")
			}
		})
	}
}

func TestHBNBindingCompleteContainerCohort(t *testing.T) {
	for _, scenario := range []string{"actual-ready-shape", "missing-init", "extra-container", "wrong-role", "wrong-sidecar-spec-image", "wrong-init-runtime-image", "missing-init-status", "duplicate-init-status", "sidecar-not-ready", "sidecar-not-started", "sidecar-terminated", "init-running", "init-failed", "init-missing-identity", "init-invalid-time", "duplicate-runtime-identity"} {
		t.Run(scenario, func(t *testing.T) {
			intent, now := hbnBindingTestIntent()
			ds, pod := bindingFixture(intent, "cluster-dpu-hbn-ds")
			// Actual CRI status.Image was the config digest; ImageID was the
			// verified repository/platform-manifest digest for all three roles.
			pod.Status.ContainerStatuses[0].Image = intent.Image.ConfigDigest
			want := hbnPodBindingReason("")
			switch scenario {
			case "missing-init":
				ds.Spec.Template.Spec.InitContainers = nil
				pod.Spec.InitContainers = nil
				pod.Status.InitContainerStatuses = nil
				want = hbnBindingPodStale
			case "extra-container":
				pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "unqualified", Image: intent.Image.Reference})
				want = hbnBindingPodStale
			case "wrong-role":
				pod.Spec.InitContainers[1].RestartPolicy = nil
				want = hbnBindingPodStale
			case "wrong-sidecar-spec-image":
				ds.Spec.Template.Spec.InitContainers[1].Image = "foreign.example/hbn@" + intent.Image.ManifestDigest
				want = hbnBindingPodImageMismatch
			case "wrong-init-runtime-image":
				pod.Status.InitContainerStatuses[0].ImageID = "foreign.example/hbn@" + intent.Image.ManifestDigest
				want = hbnBindingPodImageMismatch
			case "missing-init-status":
				pod.Status.InitContainerStatuses = pod.Status.InitContainerStatuses[:1]
				want = hbnBindingPodNotReady
			case "duplicate-init-status":
				pod.Status.InitContainerStatuses[1] = *pod.Status.InitContainerStatuses[0].DeepCopy()
				want = hbnBindingPodNotReady
			case "sidecar-not-ready":
				pod.Status.InitContainerStatuses[1].Ready = false
				want = hbnBindingPodNotReady
			case "sidecar-not-started":
				pod.Status.InitContainerStatuses[1].Started = nil
				want = hbnBindingPodNotReady
			case "sidecar-terminated":
				pod.Status.InitContainerStatuses[1].State = pod.Status.InitContainerStatuses[0].State
				want = hbnBindingPodNotReady
			case "init-running":
				pod.Status.InitContainerStatuses[0].State = pod.Status.InitContainerStatuses[1].State
				want = hbnBindingPodNotReady
			case "init-failed":
				pod.Status.InitContainerStatuses[0].State.Terminated.ExitCode = 1
				want = hbnBindingPodNotReady
			case "init-missing-identity":
				pod.Status.InitContainerStatuses[0].ContainerID = ""
				want = hbnBindingPodNotReady
			case "init-invalid-time":
				pod.Status.InitContainerStatuses[0].State.Terminated.FinishedAt = pod.Status.InitContainerStatuses[0].State.Terminated.StartedAt
				pod.Status.InitContainerStatuses[0].State.Terminated.FinishedAt.Time = pod.Status.InitContainerStatuses[0].State.Terminated.FinishedAt.Add(-time.Second)
				want = hbnBindingPodNotReady
			case "duplicate-runtime-identity":
				pod.Status.InitContainerStatuses[1].ContainerID = pod.Status.ContainerStatuses[0].ContainerID
				want = hbnBindingPodNotReady
			}
			anchor, err := validateHBNPodBinding(intent, hbnServiceFixture(intent), hbnApplicationFixture(intent, now), []appsv1.DaemonSet{ds}, []corev1.Pod{pod}, now)
			assertHBNBindingReason(t, err, want)
			if err == nil && (len(anchor.Containers) != 3 || anchor.Containers[1].Role != "init" || anchor.Containers[1].Started || anchor.Containers[2].Role != "restartable-init" || !anchor.Containers[2].Started) {
				t.Fatalf("incomplete or incorrect role identities: %+v", anchor.Containers)
			}
		})
	}
}

func TestHBNBindingContainerIdentitiesAreOwnedAndOrdered(t *testing.T) {
	intent, now := hbnBindingTestIntent()
	ds, pod := bindingFixture(intent, "cluster-dpu-hbn-ds")
	bind := func() hbnPodBindingAnchor {
		a, err := validateHBNPodBinding(intent, hbnServiceFixture(intent), hbnApplicationFixture(intent, now), []appsv1.DaemonSet{ds}, []corev1.Pod{pod}, now)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	before := bind()
	slices.Reverse(pod.Status.InitContainerStatuses)
	if !sameHBNPodBinding(before, bind()) {
		t.Fatal("status-list order changed container identity")
	}
	slices.Reverse(pod.Status.InitContainerStatuses)
	pod.Status.InitContainerStatuses[1].RestartCount++
	pod.Status.InitContainerStatuses[1].ContainerID = "containerd://sidecar-new"
	if sameHBNPodBinding(before, bind()) || before.Containers[2].ContainerID != "containerd://sidecar-a" {
		t.Fatal("sidecar replacement was missed or mutated retained identity")
	}
	pod.Status.InitContainerStatuses[0].State.Terminated.FinishedAt.Time = pod.Status.InitContainerStatuses[0].State.Terminated.FinishedAt.Add(time.Second)
	if before.Containers[1].FinishedAt == bind().Containers[1].FinishedAt {
		t.Fatal("completed init identity time was not retained")
	}
}
