// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func TestValidateHBNPodBinding(t *testing.T) {
	intent, observedAt := hbnBindingTestIntent()
	application := hbnApplicationFixture(intent, observedAt)
	daemonSet, pod := bindingFixture(intent, "cluster-dpu-hbn-ds")
	anchor, err := validateHBNPodBinding(intent, hbnServiceFixture(intent), application, []appsv1.DaemonSet{daemonSet}, []corev1.Pod{pod}, observedAt)
	if err != nil {
		t.Fatalf("select binding: %v", err)
	}
	if anchor.ApplicationUID != "app-uid" || anchor.ApplicationSpecDigest == "" || anchor.DaemonSetUID != string(daemonSet.UID) || anchor.PodUID != string(pod.UID) {
		t.Fatalf("binding anchor = %+v", anchor)
	}

	for _, tc := range []struct {
		name         string
		mutate       func(*hbnPodBindingIntent, *unstructured.Unstructured, *appsv1.DaemonSet, *corev1.Pod)
		omitApp      bool
		omitDS       bool
		omitPod      bool
		duplicateDS  bool
		duplicatePod bool
		want         hbnPodBindingReason
	}{
		{name: "missing application", omitApp: true, want: hbnBindingApplicationInvalid},
		{name: "wrong application source", mutate: func(_ *hbnPodBindingIntent, app *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			app.Object["spec"].(map[string]any)["source"].(map[string]any)["chart"] = "another-chart"
			app.Object["status"].(map[string]any)["sync"].(map[string]any)["comparedTo"].(map[string]any)["source"].(map[string]any)["chart"] = "another-chart"
		}, want: hbnBindingApplicationSource},
		{name: "stale compared source", mutate: func(_ *hbnPodBindingIntent, app *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			app.Object["status"].(map[string]any)["sync"].(map[string]any)["comparedTo"].(map[string]any)["source"].(map[string]any)["targetRevision"] = "old"
		}, want: hbnBindingApplicationStale},
		{name: "wrong application destination", mutate: func(_ *hbnPodBindingIntent, app *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			app.Object["spec"].(map[string]any)["destination"].(map[string]any)["name"] = "other-cluster"
			app.Object["status"].(map[string]any)["sync"].(map[string]any)["comparedTo"].(map[string]any)["destination"].(map[string]any)["name"] = "other-cluster"
		}, want: hbnBindingApplicationTarget},
		{name: "stale compared destination", mutate: func(_ *hbnPodBindingIntent, app *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			app.Object["status"].(map[string]any)["sync"].(map[string]any)["comparedTo"].(map[string]any)["destination"].(map[string]any)["namespace"] = "old"
		}, want: hbnBindingApplicationStale},
		{name: "unsynced application", mutate: func(_ *hbnPodBindingIntent, app *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			app.Object["status"].(map[string]any)["sync"].(map[string]any)["status"] = "OutOfSync"
		}, want: hbnBindingApplicationStale},
		{name: "unhealthy application", mutate: func(_ *hbnPodBindingIntent, app *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			app.Object["status"].(map[string]any)["health"].(map[string]any)["status"] = "Progressing"
		}, want: hbnBindingApplicationStale},
		{name: "missing application reconciledAt", mutate: func(_ *hbnPodBindingIntent, app *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			delete(app.Object["status"].(map[string]any), "reconciledAt")
		}, want: hbnBindingApplicationStale},
		{name: "application reconciled before retained service", mutate: func(intent *hbnPodBindingIntent, app *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			app.Object["status"].(map[string]any)["reconciledAt"] = intent.DPUServiceCreatedAt.Add(-time.Second).Format(time.RFC3339Nano)
		}, want: hbnBindingApplicationStale},
		{name: "application reconciled in future", mutate: func(_ *hbnPodBindingIntent, app *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			app.Object["status"].(map[string]any)["reconciledAt"] = observedAt.Add(time.Second).Format(time.RFC3339Nano)
		}, want: hbnBindingApplicationStale},
		{name: "wrong application identity label", mutate: func(_ *hbnPodBindingIntent, app *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			app.Object["metadata"].(map[string]any)["labels"].(map[string]any)[dpfDPUClusterNameLabel] = "other-cluster"
		}, want: hbnBindingApplicationInvalid},
		{name: "application without DPF finalizer", mutate: func(_ *hbnPodBindingIntent, app *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			app.SetFinalizers(nil)
		}, want: hbnBindingApplicationInvalid},
		{name: "no daemonset resource in application", mutate: func(_ *hbnPodBindingIntent, app *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			app.Object["status"].(map[string]any)["resources"] = []any{}
		}, want: hbnBindingResourceMissing},
		{name: "duplicate daemonset resource", mutate: func(_ *hbnPodBindingIntent, app *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			resource := app.Object["status"].(map[string]any)["resources"].([]any)[0]
			app.Object["status"].(map[string]any)["resources"] = []any{resource, resource}
		}, want: hbnBindingResourceAmbiguous},
		{name: "unhealthy daemonset resource", mutate: func(_ *hbnPodBindingIntent, app *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			app.Object["status"].(map[string]any)["resources"].([]any)[0].(map[string]any)["health"].(map[string]any)["status"] = "Progressing"
		}, want: hbnBindingResourceUnknown},
		{name: "missing daemonset", omitDS: true, want: hbnBindingDaemonSetMissing},
		{name: "duplicate daemonset observation", duplicateDS: true, want: hbnBindingDaemonSetAmbiguous},
		{name: "wrong service label", mutate: func(_ *hbnPodBindingIntent, _ *unstructured.Unstructured, ds *appsv1.DaemonSet, _ *corev1.Pod) {
			ds.Labels[hbnServiceIDLabel] = "another-service"
		}, want: hbnBindingServiceMismatch},
		{name: "missing selected pod", omitPod: true, want: hbnBindingPodMissing},
		{name: "duplicate selected pod", duplicatePod: true, want: hbnBindingPodAmbiguous},
		{name: "wrong selected node", mutate: func(_ *hbnPodBindingIntent, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, pod *corev1.Pod) {
			pod.Spec.NodeName = "other-node"
		}, want: hbnBindingPodMissing},
		{name: "stale daemonset UID", mutate: func(_ *hbnPodBindingIntent, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, pod *corev1.Pod) {
			pod.OwnerReferences[0].UID = "old-uid"
		}, want: hbnBindingPodStale},
		{name: "deleting pod", mutate: func(_ *hbnPodBindingIntent, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, pod *corev1.Pod) {
			now := metav1.NewTime(time.Now())
			pod.DeletionTimestamp = &now
		}, want: hbnBindingPodStale},
		{name: "wrong image", mutate: func(_ *hbnPodBindingIntent, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, pod *corev1.Pod) {
			pod.Spec.Containers[0].Image = "private-value"
		}, want: hbnBindingPodImageMismatch},
		{name: "not ready", mutate: func(_ *hbnPodBindingIntent, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, pod *corev1.Pod) {
			pod.Status.Conditions[0].Status = corev1.ConditionFalse
		}, want: hbnBindingPodNotReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			currentIntent := intent
			currentApp := hbnApplicationFixture(currentIntent, observedAt)
			currentDS, currentPod := bindingFixture(currentIntent, "cluster-dpu-hbn-ds")
			if tc.mutate != nil {
				tc.mutate(&currentIntent, currentApp, &currentDS, &currentPod)
			}
			apps := []appsv1.DaemonSet{currentDS}
			pods := []corev1.Pod{currentPod}
			if tc.omitDS {
				apps = nil
			}
			if tc.omitPod {
				pods = nil
			}
			if tc.duplicateDS {
				duplicate := *currentDS.DeepCopy()
				duplicate.UID = "duplicate-uid"
				apps = append(apps, duplicate)
			}
			if tc.duplicatePod {
				duplicate := *currentPod.DeepCopy()
				duplicate.Name = "hbn-pod-duplicate"
				duplicate.UID = "duplicate-pod-uid"
				pods = append(pods, duplicate)
			}
			app := currentApp
			if tc.omitApp {
				app = nil
			}
			_, err := validateHBNPodBinding(currentIntent, hbnServiceFixture(currentIntent), app, apps, pods, observedAt)
			if err == nil {
				t.Fatal("expected binding failure")
			}
			bindingErr, ok := err.(*hbnPodBindingError)
			if !ok || bindingErr.reason != tc.want {
				t.Fatalf("error = %T %v, want reason %s", err, err, tc.want)
			}
			if strings.Contains(err.Error(), "private-value") || strings.Contains(err.Error(), "tenant-node") {
				t.Fatalf("binding error leaked observed input: %v", err)
			}
		})
	}
}

func TestRetainedHBNChartDefinesSingleDaemonSetAndContainerIdentity(t *testing.T) {
	file, err := os.Open("../generation/testdata/doca-hbn-3.5.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	archive := tar.NewReader(gz)
	var daemonSetTemplate string
	for {
		header, nextErr := archive.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if header.Name == "doca-hbn/templates/daemonset.yaml" {
			data, readErr := io.ReadAll(archive)
			if readErr != nil {
				t.Fatal(readErr)
			}
			daemonSetTemplate = string(data)
		}
	}
	for _, source := range []string{`kind: DaemonSet`, `name: {{ include "hbn.fullname" . }}-ds`, `.Values.serviceDaemonSet.labels`, "- name: doca-hbn", "name: hbn-init", "- name: hbn-sidecar", "restartPolicy: Always", `.Values.docaHbn.image.repository`, `.Values.docaHbn.image.tag`} {
		if !strings.Contains(daemonSetTemplate, source) {
			t.Errorf("retained chart DaemonSet template lacks identity source %q", source)
		}
	}
}

func hbnBindingTestIntent() (hbnPodBindingIntent, time.Time) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	intent := hbnPodBindingIntent{
		ArgoNamespace: "dpf-operator-system", DPUServiceName: "dpu-hbn", DPUServiceNamespace: "tenant-workloads",
		DPUServiceCreatedAt: now.Add(-time.Minute), DPUClusterName: "cluster", DPUClusterNamespace: "dpf-system",
		TargetNamespace: "tenant-workloads", ChartRepoURL: "https://helm.example/charts", ChartName: "doca-hbn", ChartVersion: "3.5.0",
		ServiceID: "hbn-service-id", TenantNodeName: "tenant-node-a", Image: hbnPodImageIdentity{Reference: "nvcr.io/nvidia/doca/doca-hbn:3.5.0@sha256:" + strings.Repeat("a", 64), ManifestDigest: "sha256:" + strings.Repeat("a", 64), ConfigDigest: "sha256:" + strings.Repeat("b", 64)},
		DPUDeploymentName: "hbn-deployment", DPUDeploymentUID: "deployment-uid", DPUServiceUID: "service-uid",
	}
	intent.ExpectedServiceSpec = map[string]any{"serviceID": intent.ServiceID, "helmChart": map[string]any{"source": map[string]any{"repoURL": intent.ChartRepoURL, "chart": intent.ChartName, "version": intent.ChartVersion}, "values": map[string]any{"configuration": map[string]any{"startup": "retained"}}}, "serviceDaemonSet": map[string]any{"labels": map[string]any{"svc.dpu.nvidia.com/dpudeployment-service": "doca-hbn"}}}
	intent.ApplicationValues = map[string]any{"configuration": map[string]any{"startup": "retained"}, "serviceDaemonSet": map[string]any{"labels": map[string]any{hbnServiceIDLabel: intent.ServiceID, "svc.dpu.nvidia.com/dpudeployment-service": "doca-hbn"}}}
	return intent, now
}

func hbnApplicationFixture(intent hbnPodBindingIntent, reconciledAt time.Time) *unstructured.Unstructured {
	appName := dpfApplicationName(intent.DPUClusterName, intent.DPUServiceName)
	source := map[string]any{"repoURL": intent.ChartRepoURL, "chart": intent.ChartName, "targetRevision": intent.ChartVersion,
		"helm": map[string]any{"valuesObject": runtime.DeepCopyJSON(intent.ApplicationValues)}}
	comparedSource := map[string]any{"repoURL": intent.ChartRepoURL, "chart": intent.ChartName, "targetRevision": intent.ChartVersion,
		"helm": map[string]any{"valuesObject": runtime.DeepCopyJSON(intent.ApplicationValues)}}
	destination := map[string]any{"name": intent.DPUClusterName, "namespace": intent.TargetNamespace}
	comparedDestination := map[string]any{"name": intent.DPUClusterName, "namespace": intent.TargetNamespace}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"namespace": intent.ArgoNamespace, "name": appName, "uid": "app-uid", "resourceVersion": "42",
			"finalizers": []any{argoApplicationFinalizer}, "labels": map[string]any{
				dpfComponentLabel: dpfServiceManagerComponent, dpfDPUServiceNameLabel: intent.DPUServiceName,
				dpfDPUServiceNamespaceLabel: intent.DPUServiceNamespace, dpfDPUClusterNameLabel: intent.DPUClusterName,
				dpfDPUClusterNamespaceLabel: intent.DPUClusterNamespace,
			}},
		"spec": map[string]any{"source": source, "destination": destination},
		"status": map[string]any{"sync": map[string]any{"status": "Synced", "comparedTo": map[string]any{
			"source": comparedSource, "destination": comparedDestination,
		}}, "health": map[string]any{"status": "Healthy"}, "reconciledAt": reconciledAt.Format(time.RFC3339Nano),
			"resources": []any{map[string]any{"group": "apps", "version": "v1", "kind": "DaemonSet", "namespace": intent.TargetNamespace,
				"name": "cluster-dpu-hbn-ds", "status": "Synced", "health": map[string]any{"status": "Healthy"}, "hook": false, "requiresPruning": false}},
		},
	}}
}

func bindingFixture(intent hbnPodBindingIntent, daemonSetName string) (appsv1.DaemonSet, corev1.Pod) {
	controller := true
	labels := map[string]string{"app.kubernetes.io/instance": "dpu-hbn", "app.kubernetes.io/name": "hbn", hbnServiceIDLabel: intent.ServiceID, "svc.dpu.nvidia.com/dpudeployment-service": "doca-hbn"}
	selector := &metav1.LabelSelector{MatchLabels: maps.Clone(labels)}
	daemonSet := appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: intent.TargetNamespace, Name: daemonSetName, UID: "daemonset-uid", ResourceVersion: "12", Generation: 1,
			Labels: map[string]string{hbnServiceIDLabel: intent.ServiceID, hbnServiceReferenceLabel: hbnContainerName}},
		Spec: appsv1.DaemonSetSpec{Selector: selector, Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: maps.Clone(labels)},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: hbnContainerName, Image: intent.Image.Reference}}},
		}},
	}
	daemonSet.Status = appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 1, UpdatedNumberScheduled: 1, NumberReady: 1, NumberAvailable: 1}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: intent.TargetNamespace, Name: "hbn-pod", UID: "pod-uid", ResourceVersion: "34",
			Labels: maps.Clone(labels), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: daemonSet.Name, UID: daemonSet.UID, Controller: &controller}}},
		Spec:   corev1.PodSpec{NodeName: intent.TenantNodeName, Containers: []corev1.Container{{Name: hbnContainerName, Image: intent.Image.Reference}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: hbnContainerName, Ready: true, Started: &controller, Image: intent.Image.Reference, ImageID: hbnImageRepository(intent.Image.Reference) + "@" + intent.Image.ManifestDigest, ContainerID: "containerd://container-a", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(intent.DPUServiceCreatedAt.Add(time.Second))}}}}},
	}
	// Actual HBN 3.5.0 Pod shape: completed ordinary init plus a running
	// restartable init sidecar; all three images use the retained global pin.
	always, notStarted := corev1.ContainerRestartPolicyAlways, false
	pod.Spec.InitContainers = []corev1.Container{{Name: "hbn-init", Image: intent.Image.Reference}, {Name: "hbn-sidecar", Image: intent.Image.Reference, RestartPolicy: &always}}
	for i := range pod.Spec.InitContainers {
		daemonSet.Spec.Template.Spec.InitContainers = append(daemonSet.Spec.Template.Spec.InitContainers, *pod.Spec.InitContainers[i].DeepCopy())
	}
	startedAt := metav1.NewTime(intent.DPUServiceCreatedAt.Add(time.Second))
	imageID := hbnImageRepository(intent.Image.Reference) + "@" + intent.Image.ManifestDigest
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{Name: "hbn-init", ContainerID: "containerd://init-a", Image: intent.Image.ConfigDigest, ImageID: imageID, Ready: true, Started: &notStarted,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: "containerd://init-a", ExitCode: 0, Reason: "Completed", StartedAt: startedAt, FinishedAt: startedAt}}},
		{Name: "hbn-sidecar", ContainerID: "containerd://sidecar-a", Image: intent.Image.ConfigDigest, ImageID: imageID, Ready: true, Started: &controller,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: startedAt}}},
	}
	return daemonSet, pod
}

func hbnServiceFixture(intent hbnPodBindingIntent) *unstructured.Unstructured {
	service := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": "DPUService", "spec": runtime.DeepCopyJSON(intent.ExpectedServiceSpec)}}
	service.SetName(intent.DPUServiceName)
	service.SetNamespace(intent.DPUServiceNamespace)
	service.SetUID(types.UID(intent.DPUServiceUID))
	service.SetResourceVersion("1")
	service.SetGeneration(1)
	service.SetCreationTimestamp(metav1.NewTime(intent.DPUServiceCreatedAt))
	service.SetAnnotations(map[string]string{hbnServiceVersionAnnotation: "controller-version"})
	service.SetLabels(map[string]string{activationParentLabel: intent.DPUServiceNamespace + "_" + intent.DPUDeploymentName, "svc.dpu.nvidia.com/dpudeployment-service": "doca-hbn"})
	controller := true
	service.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "svc.dpu.nvidia.com/v1alpha1", Kind: "DPUDeployment", Name: intent.DPUDeploymentName, UID: types.UID(intent.DPUDeploymentUID), Controller: &controller}})
	return service
}

// PodSpec may already contain the requested image while kubelet still reports
// the old instance. Ready alone is deliberately insufficient.
func TestHBNPodBindingRequiresVerifiedRunningImage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*hbnPodBindingIntent, *corev1.Pod)
		want   hbnPodBindingReason
	}{
		{name: "old running image despite current desired spec", mutate: func(_ *hbnPodBindingIntent, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].ImageID = "nvcr.io/nvidia/doca/doca-hbn@sha256:" + strings.Repeat("c", 64)
		}, want: hbnBindingPodImageMismatch},
		{name: "missing running state", mutate: func(_ *hbnPodBindingIntent, p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Running = nil }, want: hbnBindingPodNotReady},
		{name: "waiting despite stale ready", mutate: func(_ *hbnPodBindingIntent, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{}
		}, want: hbnBindingPodNotReady},
		{name: "terminated despite stale ready", mutate: func(_ *hbnPodBindingIntent, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated = &corev1.ContainerStateTerminated{}
		}, want: hbnBindingPodNotReady},
		{name: "unknown started", mutate: func(_ *hbnPodBindingIntent, p *corev1.Pod) { p.Status.ContainerStatuses[0].Started = nil }, want: hbnBindingPodNotReady},
		{name: "not started", mutate: func(_ *hbnPodBindingIntent, p *corev1.Pod) {
			started := false
			p.Status.ContainerStatuses[0].Started = &started
		}, want: hbnBindingPodNotReady},
		{name: "no container ID", mutate: func(_ *hbnPodBindingIntent, p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "" }, want: hbnBindingPodNotReady},
		{name: "malformed container ID", mutate: func(_ *hbnPodBindingIntent, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].ContainerID = "containerd://"
		}, want: hbnBindingPodNotReady},
		{name: "no ImageID", mutate: func(_ *hbnPodBindingIntent, p *corev1.Pod) { p.Status.ContainerStatuses[0].ImageID = "" }, want: hbnBindingPodNotReady},
		{name: "no runtime image", mutate: func(_ *hbnPodBindingIntent, p *corev1.Pod) { p.Status.ContainerStatuses[0].Image = "" }, want: hbnBindingPodNotReady},
		{name: "no start timestamp", mutate: func(_ *hbnPodBindingIntent, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.Time{}
		}, want: hbnBindingPodNotReady},
		{name: "duplicate container status", mutate: func(_ *hbnPodBindingIntent, p *corev1.Pod) {
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, p.Status.ContainerStatuses[0])
		}, want: hbnBindingPodNotReady},
		{name: "conflicting ready conditions", mutate: func(_ *hbnPodBindingIntent, p *corev1.Pod) {
			p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse})
		}, want: hbnBindingPodNotReady},
		{name: "repo digest", mutate: func(_ *hbnPodBindingIntent, _ *corev1.Pod) {}},
		{name: "repo digest needs no config mapping", mutate: func(i *hbnPodBindingIntent, _ *corev1.Pod) { i.Image.ConfigDigest = "" }},
		{name: "docker pullable repo digest", mutate: func(_ *hbnPodBindingIntent, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].ImageID = "docker-pullable://" + p.Status.ContainerStatuses[0].ImageID
		}},
		{name: "verified config digest", mutate: func(i *hbnPodBindingIntent, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].ImageID = i.Image.ConfigDigest
		}},
		{name: "docker config digest", mutate: func(i *hbnPodBindingIntent, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].ImageID = "docker://" + i.Image.ConfigDigest
		}},
		{name: "containerd config digest", mutate: func(i *hbnPodBindingIntent, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].ImageID = "containerd://" + i.Image.ConfigDigest
		}},
		{name: "config mapping unavailable", mutate: func(i *hbnPodBindingIntent, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].ImageID = i.Image.ConfigDigest
			i.Image.ConfigDigest = ""
		}, want: hbnBindingPodImageMismatch},
		{name: "manifest is not config", mutate: func(i *hbnPodBindingIntent, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].ImageID = i.Image.ManifestDigest
		}, want: hbnBindingPodImageMismatch},
		{name: "other repository same digest", mutate: func(i *hbnPodBindingIntent, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].ImageID = "unrelated.example/hbn@" + i.Image.ManifestDigest
		}, want: hbnBindingPodImageMismatch},
		{name: "runtime image spelling may differ", mutate: func(_ *hbnPodBindingIntent, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].Image = "runtime-canonical-name"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intent, now := hbnBindingTestIntent()
			ds, pod := bindingFixture(intent, "cluster-dpu-hbn-ds")
			tc.mutate(&intent, &pod)
			_, err := validateHBNPodBinding(intent, hbnServiceFixture(intent), hbnApplicationFixture(intent, now), []appsv1.DaemonSet{ds}, []corev1.Pod{pod}, now)
			assertHBNBindingReason(t, err, tc.want)
		})
	}
}

func TestHBNPodBindingPlatformManifestIsDistinctFromIndex(t *testing.T) {
	intent, now := hbnBindingTestIntent()
	intent.Image.Reference = "nvcr.io/nvidia/doca/doca-hbn@sha256:" + strings.Repeat("c", 64) // retained index
	ds, pod := bindingFixture(intent, "cluster-dpu-hbn-ds")
	for _, tc := range []struct {
		digest string
		want   hbnPodBindingReason
	}{
		{digest: intent.Image.ManifestDigest},
		{digest: "sha256:" + strings.Repeat("c", 64), want: hbnBindingPodImageMismatch},
	} {
		pod.Status.ContainerStatuses[0].ImageID = hbnImageRepository(intent.Image.Reference) + "@" + tc.digest
		_, err := validateHBNPodBinding(intent, hbnServiceFixture(intent), hbnApplicationFixture(intent, now), []appsv1.DaemonSet{ds}, []corev1.Pod{pod}, now)
		assertHBNBindingReason(t, err, tc.want)
	}
}

func TestHBNPodBindingRejectsStaleServiceAndValues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*hbnPodBindingIntent, *unstructured.Unstructured, *unstructured.Unstructured, *appsv1.DaemonSet, *corev1.Pod)
		want   hbnPodBindingReason
	}{
		{name: "service old config same stable service ID", mutate: func(i *hbnPodBindingIntent, _ *unstructured.Unstructured, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			i.ExpectedServiceSpec["helmChart"].(map[string]any)["values"].(map[string]any)["configuration"].(map[string]any)["startup"] = "new-retained-config"
		}, want: hbnBindingServiceInvalid},
		{name: "app old values compared to itself same stable service ID", mutate: func(i *hbnPodBindingIntent, s *unstructured.Unstructured, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			i.ExpectedServiceSpec["helmChart"].(map[string]any)["values"].(map[string]any)["configuration"].(map[string]any)["startup"] = "new-retained-config"
			s.Object["spec"] = runtime.DeepCopyJSON(i.ExpectedServiceSpec)
			i.ApplicationValues["configuration"].(map[string]any)["startup"] = "new-retained-config"
		}, want: hbnBindingApplicationSource},
		{name: "service replaced with same name", mutate: func(_ *hbnPodBindingIntent, s *unstructured.Unstructured, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			s.SetUID("replacement")
		}, want: hbnBindingServiceInvalid},
		{name: "foreign deployment owner", mutate: func(_ *hbnPodBindingIntent, s *unstructured.Unstructured, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			o := s.GetOwnerReferences()
			o[0].UID = "other-deployment"
			s.SetOwnerReferences(o)
		}, want: hbnBindingServiceInvalid},
		{name: "deleting service", mutate: func(_ *hbnPodBindingIntent, s *unstructured.Unstructured, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			now := metav1.Now()
			s.SetDeletionTimestamp(&now)
		}, want: hbnBindingServiceInvalid},
		{name: "missing version annotation", mutate: func(_ *hbnPodBindingIntent, s *unstructured.Unstructured, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			s.SetAnnotations(nil)
		}, want: hbnBindingServiceInvalid},
		{name: "unretained alternate helm input", mutate: func(_ *hbnPodBindingIntent, _ *unstructured.Unstructured, a *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			source := a.Object["spec"].(map[string]any)["source"].(map[string]any)
			source["helm"].(map[string]any)["values"] = "configuration: {startup: other}"
			a.Object["status"].(map[string]any)["sync"].(map[string]any)["comparedTo"].(map[string]any)["source"] = runtime.DeepCopyJSON(source)
		}, want: hbnBindingApplicationSource},
		{name: "daemonset controller has not observed spec", mutate: func(_ *hbnPodBindingIntent, _ *unstructured.Unstructured, _ *unstructured.Unstructured, d *appsv1.DaemonSet, _ *corev1.Pod) {
			d.Generation++
		}, want: hbnBindingResourceUnknown},
		{name: "old config pod rollout incomplete", mutate: func(_ *hbnPodBindingIntent, _ *unstructured.Unstructured, _ *unstructured.Unstructured, d *appsv1.DaemonSet, _ *corev1.Pod) {
			d.Status.UpdatedNumberScheduled = 0
		}, want: hbnBindingResourceUnknown},
		{name: "pod has foreign injected service label", mutate: func(_ *hbnPodBindingIntent, _ *unstructured.Unstructured, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, p *corev1.Pod) {
			p.Labels[hbnServiceIDLabel] = "foreign"
		}, want: hbnBindingPodMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intent, now := hbnBindingTestIntent()
			service, app := hbnServiceFixture(intent), hbnApplicationFixture(intent, now)
			ds, pod := bindingFixture(intent, "cluster-dpu-hbn-ds")
			tc.mutate(&intent, service, app, &ds, &pod)
			_, err := validateHBNPodBinding(intent, service, app, []appsv1.DaemonSet{ds}, []corev1.Pod{pod}, now)
			assertHBNBindingReason(t, err, tc.want)
		})
	}
}

func TestHBNPodBindingSemanticInstanceFence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*unstructured.Unstructured, *unstructured.Unstructured, *appsv1.DaemonSet, *corev1.Pod)
		stable bool
	}{
		{name: "resource version and status timestamp churn", stable: true, mutate: func(s, a *unstructured.Unstructured, d *appsv1.DaemonSet, p *corev1.Pod) {
			s.SetResourceVersion("2")
			a.SetResourceVersion("43")
			d.ResourceVersion = "13"
			p.ResourceVersion = "35"
			a.Object["status"].(map[string]any)["reconciledAt"] = "2026-11-14T12:00:01Z"
		}},
		{name: "container replaced", mutate: func(_, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
		}},
		{name: "restart count changed", mutate: func(_, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].RestartCount++
		}},
		{name: "running start changed", mutate: func(_, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(p.Status.ContainerStatuses[0].State.Running.StartedAt.Add(time.Second))
		}},
		{name: "pod replaced", mutate: func(_, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, p *corev1.Pod) { p.UID = "replacement" }},
		{name: "pod spec changed", mutate: func(_, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, p *corev1.Pod) {
			p.Spec.Containers[0].Args = []string{"changed"}
		}},
		{name: "pod annotation changed", mutate: func(_, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, p *corev1.Pod) {
			p.Annotations = map[string]string{"config-revision": "new"}
		}},
		{name: "application replaced", mutate: func(_, a *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) { a.SetUID("replacement") }},
		{name: "daemonset replaced", mutate: func(_, _ *unstructured.Unstructured, d *appsv1.DaemonSet, p *corev1.Pod) {
			d.UID = "replacement"
			p.OwnerReferences[0].UID = d.UID
		}},
		{name: "daemonset spec changed", mutate: func(_, _ *unstructured.Unstructured, d *appsv1.DaemonSet, _ *corev1.Pod) {
			d.Spec.Template.Spec.Containers[0].Args = []string{"changed"}
		}},
		{name: "service version changed", mutate: func(s, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) {
			s.SetAnnotations(map[string]string{hbnServiceVersionAnnotation: "next-controller-version"})
		}},
		{name: "service generation changed", mutate: func(s, _ *unstructured.Unstructured, _ *appsv1.DaemonSet, _ *corev1.Pod) { s.SetGeneration(2) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intent, now := hbnBindingTestIntent()
			service, app := hbnServiceFixture(intent), hbnApplicationFixture(intent, now)
			ds, pod := bindingFixture(intent, "cluster-dpu-hbn-ds")
			before, err := validateHBNPodBinding(intent, service, app, []appsv1.DaemonSet{ds}, []corev1.Pod{pod}, now)
			assertHBNBindingReason(t, err, "")
			tc.mutate(service, app, &ds, &pod)
			after, err := validateHBNPodBinding(intent, service, app, []appsv1.DaemonSet{ds}, []corev1.Pod{pod}, now.Add(time.Second))
			assertHBNBindingReason(t, err, "")
			if got := sameHBNPodBinding(before, after); got != tc.stable {
				t.Fatalf("stable = %t, want %t", got, tc.stable)
			}
		})
	}
	if sameHBNPodBinding(hbnPodBindingAnchor{}, hbnPodBindingAnchor{}) {
		t.Fatal("empty anchors must not authorize a probe")
	}
}

func assertHBNBindingReason(t *testing.T, err error, want hbnPodBindingReason) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	got, ok := err.(*hbnPodBindingError)
	if !ok || got.reason != want {
		t.Fatalf("error = %v, want %s", err, want)
	}
}

func TestHBNPodBindingRejectsUnknownInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*hbnPodBindingIntent)
	}{
		{name: "missing retained service spec", mutate: func(i *hbnPodBindingIntent) { i.ExpectedServiceSpec = nil }},
		{name: "missing retained app values", mutate: func(i *hbnPodBindingIntent) { i.ApplicationValues = nil }},
		{name: "missing platform manifest proof", mutate: func(i *hbnPodBindingIntent) { i.Image.ManifestDigest = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intent, now := hbnBindingTestIntent()
			service, app := hbnServiceFixture(intent), hbnApplicationFixture(intent, now)
			ds, pod := bindingFixture(intent, "cluster-dpu-hbn-ds")
			tc.mutate(&intent)
			_, err := validateHBNPodBinding(intent, service, app, []appsv1.DaemonSet{ds}, []corev1.Pod{pod}, now)
			assertHBNBindingReason(t, err, hbnBindingInputInvalid)
		})
	}
	intent, now := hbnBindingTestIntent()
	ds, pod := bindingFixture(intent, "cluster-dpu-hbn-ds")
	_, err := validateHBNPodBinding(intent, nil, hbnApplicationFixture(intent, now), []appsv1.DaemonSet{ds}, []corev1.Pod{pod}, now)
	assertHBNBindingReason(t, err, hbnBindingServiceInvalid)
}
