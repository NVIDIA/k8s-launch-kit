// SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package nvaie

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

func TestNVAIE(t *testing.T) {
	RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "NVIDIA AI Enterprise catalog and assessment")
}

const rtxServer = "RTX PRO 6000 Blackwell Server Edition"

var _ = ginkgo.Describe("NVIDIA AI Enterprise 9.0 snapshot", func() {
	ginkgo.It("retains the published pins and the two distinct RKE2 OS rows", func() {
		release, ok := Lookup("9.0")
		Expect(ok).To(BeTrue())
		Expect(SupportedReleases()).To(Equal([]string{"9.0"}))
		Expect(release.Components).To(HaveLen(13))
		Expect(release.GPUs).To(HaveLen(47))
		Expect(release.BareMetalK8s).To(HaveLen(10))
		Expect(release.ComponentVersion(NetworkOperator)).To(Equal("26.7.1"))
		Expect(release.ComponentVersion(DOCADriver)).To(Equal("3.5.0"))
		Expect(release.ComponentVersion(GPUOperator)).To(Equal("26.7.2"))
		Expect(release.ComponentVersion(GPUDriver)).To(Equal("620.70.06"))
		Expect(release.ComponentVersion(ContainerToolkit)).To(Equal("1.20.1"))
		catalog, err := bundledCatalog()
		Expect(err).NotTo(HaveOccurred())
		Expect(catalog.Source.DataSHA256).To(HaveLen(64))
		_, ok = Lookup("9")
		Expect(ok).To(BeFalse())
		var osRows []string
		for _, row := range release.BareMetalK8s {
			if row.Platform == RKE2 {
				osRows = append(osRows, row.OS)
				Expect(row.Version).To(Equal("1.33 - 1.37"))
				Expect(row.DPUOperator).To(Equal("-"))
			}
		}
		Expect(osRows).To(ConsistOf(
			"Ubuntu 22.04 LTS, 24.04 LTS, 26.04 LTS",
			"Red Hat Enterprise Linux 8.6, 8.8, 8.10, 9.4, 9.6, 10.0, 10.2",
		))
	})

	ginkgo.It("rejects bundled schema changes explicitly", func() {
		_, err := decodeCatalog([]byte(`{"schema":"l8k.nvaie.catalog.v2"}`))
		Expect(err).To(MatchError(ContainSubstring("unknown NVAIE catalog schema")))
	})

	ginkgo.It("returns independent catalog rows and footnotes", func() {
		release, _ := Lookup("9.0")
		for _, row := range release.BareMetalK8s {
			if row.Platform == OpenShift {
				Expect(row.NetOpFootnotes).To(Equal([]int{9}))
				row.NetOpFootnotes[0] = 0
			}
		}
		fresh, _ := Lookup("9.0")
		for _, row := range fresh.BareMetalK8s {
			if row.Platform == OpenShift {
				Expect(row.NetOpFootnotes).To(Equal([]int{9}))
			}
		}
	})

	ginkgo.DescribeTable("evaluates node tuples without borrowing support across rows",
		func(platform, version, os, engine, gpu, driver string, expected Status) {
			release, _ := Lookup("9.0")
			result := EvaluateNode(release, NodeFacts{
				Name: "worker", Architecture: "amd64", Platform: platform, PlatformVersion: version,
				ServerPlatformVersion: version, OS: os, Runtime: engine, GPUProduct: gpu, DriverFamily: driver, Deployment: BareMetal,
			})
			matrix := findCheck(result.Checks, "matrix-row")
			Expect(matrix.Status).To(Equal(expected), "%s; facts=%+v; rows=%+v", matrix.Detail, result.Facts, result.Candidates)
		},
		ginkgo.Entry("normalizes the RKE2 Ubuntu LTS point release", RKE2, "1.33", "Ubuntu 24.04.2 LTS", "Containerd", "NVIDIA-RTX-PRO-6000-Blackwell-Server-Edition", "Data Center", Pass),
		ginkgo.Entry("compares RHEL minor versions numerically", RKE2, "1.37", "Red Hat Enterprise Linux 8.10 (Ootpa)", "Containerd", rtxServer, "", Pass),
		ginkgo.Entry("does not borrow Containerd support for CRI-O", "Upstream Kubernetes", "1.34", "Ubuntu 24.04 LTS", "CRI-O", rtxServer, "", Fail),
		ginkgo.Entry("rejects a platform version outside the range", RKE2, "1.38", "Ubuntu 24.04 LTS", "Containerd", rtxServer, "", Fail),
		ginkgo.Entry("rejects an unlisted RHEL minor", RKE2, "1.34", "Red Hat Enterprise Linux 9.8", "Containerd", rtxServer, "", Fail),
		ginkgo.Entry("cannot treat a malformed OS version as known", RKE2, "1.34", "Ubuntu 24.04invalid", "Containerd", rtxServer, "", Unknown),
		ginkgo.Entry("does not identify a generic H100 as an exact variant", RKE2, "1.34", "Ubuntu 24.04", "Containerd", "NVIDIA H100", "", Unknown),
		ginkgo.Entry("does not substitute the only matching GPU prefix", RKE2, "1.34", "Ubuntu 24.04", "Containerd", "NVIDIA B300", "", Unknown),
		ginkgo.Entry("does not silently infer workstation driver support", RKE2, "1.34", "Ubuntu 24.04", "Containerd", "NVIDIA RTX 6000 Ada Generation", "", Unknown),
		ginkgo.Entry("rejects a known workstation Data Center driver", RKE2, "1.34", "Ubuntu 24.04", "Containerd", "NVIDIA RTX 6000 Ada Generation", "Data Center", Fail),
		ginkgo.Entry("preserves pending OpenShift conditions", OpenShift, "4.21", "Red Hat CoreOS 4.21", "CRI-O", rtxServer, "", Unknown),
	)
})

var _ = ginkgo.Describe("read-only Kubernetes assessment", func() {
	ginkgo.It("matches the current machine and GPU labels without overwriting raw evidence", func(ctx ginkgo.SpecContext) {
		node := worker("worker")
		node.Labels[gpuProductLabel] = "NVIDIA-GB300"
		node.Labels["nvidia.com/gpu.machine"] = "GB300-NVL"
		result := Assess(ctx, validClient(node), "9.0", []string{"worker"}, nil, nil)
		Expect(result.Nodes).To(HaveLen(1))
		Expect(result.Nodes[0].MatrixGPU).To(Equal("GB300 NVL72"))
		Expect(result.Nodes[0].Facts.MachineType).To(Equal("GB300-NVL"))
		Expect(result.Nodes[0].Facts.GPUProduct).To(Equal("NVIDIA-GB300"))
		Expect(findCheck(result.Nodes[0].Checks, "matrix-row").Status).To(Equal(Pass))
	})

	ginkgo.It("passes observed fixture dimensions with separate manual qualification", func(ctx ginkgo.SpecContext) {
		client := validClient()
		a := Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(a.Passed()).To(BeTrue(), "%+v %+v", a.Nodes, a.Components)
		Expect(a.Nodes).To(HaveLen(1))
		Expect(findCheck(a.Nodes[0].Checks, "deployment")).To(Equal(Check{
			Name: "deployment", Status: Unknown, Detail: "Physical bare-metal deployment requires independent qualification",
		}))
		Expect(findComponent(a, vgpuGuest).Expected.Name).To(BeEmpty())
		Expect(findComponent(a, "Run:ai").Check.Required).To(BeFalse())
		Expect(findComponent(a, GPUDriver).Observations).To(HaveLen(1))
		Expect(findComponent(a, GPUDriver).Observations[0].Version).To(Equal("620.70.06"))
		data, err := json.Marshal(a)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(ContainSubstring(`"passed":true`))
		Expect(client.Actions()).To(HaveLen(6))
		for _, action := range client.Actions() {
			Expect(action.GetVerb()).To(BeElementOf("list", "get"))
		}
		Expect(a.ServerVersion).To(Equal("v1.34.3+rke2r1"))
	})

	ginkgo.It("reports all active conflicting images and digest-only evidence", func(ctx ginkgo.SpecContext) {
		client := validClient(
			readyPod("operator-old", "worker", "network-operator", "nvcr.io/nvidia/cloud-native/network-operator:v26.4.1"),
			readyPod("toolkit-digest", "worker", "nvidia-container-toolkit-ctr", "nvcr.io/nvidia/k8s/container-toolkit@sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"),
		)
		a := Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(a.Passed()).To(BeFalse())
		operator := findComponent(a, NetworkOperator)
		Expect(operator.Observations).To(HaveLen(2))
		Expect(operator.Check.Status).To(Equal(Fail))
		Expect(findComponent(a, ContainerToolkit).Check.Status).To(Equal(Unknown))
	})

	ginkgo.It("preserves and rejects contradictory spec and status image references", func(ctx ginkgo.SpecContext) {
		client := validClient()
		pod, err := client.CoreV1().Pods("operators").Get(ctx, "network-operator", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		pod.Status.ContainerStatuses[0].Image = "nvcr.io/nvidia/cloud-native/network-operator:v26.4.1"
		_, err = client.CoreV1().Pods("operators").UpdateStatus(ctx, pod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		a := Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(a.Passed()).To(BeFalse())
		component := findComponent(a, NetworkOperator)
		Expect(component.Check.Status).To(Equal(Fail))
		Expect(component.Observations[0].StatusImage).To(HaveSuffix(":v26.4.1"))
		Expect(component.Observations[0].Image).To(HaveSuffix(":v26.7.1"))
	})

	ginkgo.It("recognizes equivalent spelling for an observed image reference", func() {
		container := corev1.Container{Name: "network-operator", Image: "docker.io/library/network-operator:v26.7.1"}
		_, _, image := identifyImage(container.Image, container.Name)
		_, _, observed := identifyImage("network-operator:v26.7.1", container.Name)
		status, _ := comparePodImages(image, observed)
		Expect(status).To(Equal(Pass))
	})

	ginkgo.It("retains a digest-bearing declared tag without calling it version evidence", func(ctx ginkgo.SpecContext) {
		client := validClient()
		pod, err := client.CoreV1().Pods("operators").Get(ctx, "network-operator", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		pod.Spec.Containers[0].Image += "@sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
		_, err = client.CoreV1().Pods("operators").Update(ctx, pod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		a := Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(a.Passed()).To(BeFalse())
		component := findComponent(a, NetworkOperator)
		Expect(component.Check.Status).To(Equal(Unknown))
		Expect(component.Observations[0].Version).To(Equal("26.7.1"))
		Expect(component.Observations[0].ImageDetail).To(ContainSubstring("unverified digest"))
	})

	ginkgo.DescribeTable("requires compatible RKE2 server and worker versions in the same row",
		func(ctx ginkgo.SpecContext, server, kubelet string, expected Status) {
			client := validClient()
			setServerVersion(client, server)
			node, err := client.CoreV1().Nodes().Get(ctx, "worker", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			node.Status.NodeInfo.KubeletVersion = kubelet
			_, err = client.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			a := Assess(ctx, client, "9.0", nil, nil, nil)
			Expect(findCheck(a.Nodes[0].Checks, "matrix-row").Status).To(Equal(expected))
			Expect(a.Passed()).To(Equal(expected == Pass))
		},
		ginkgo.Entry("server below range and older than kubelet", "v1.32.3+rke2r1", "v1.34.3+rke2r1", Fail),
		ginkgo.Entry("server above range with otherwise compatible skew", "v1.38.1+rke2r1", "v1.36.3+rke2r1", Fail),
		ginkgo.Entry("server marker absent", "v1.34.3", "v1.34.3+rke2r1", Unknown),
		ginkgo.Entry("compatible versions within the row", "v1.36.3+rke2r1", "v1.34.3+rke2r1", Pass),
	)

	ginkgo.It("preserves the real OpenShift OS image independently of its matrix key", func() {
		node := worker("worker")
		node.Status.NodeInfo.OSImage = "Red Hat Enterprise Linux CoreOS 420.94.202606220245-0"
		node.Status.NodeInfo.KubeletVersion = "v1.34.3"
		facts := observeNode(node, "", "v1.34.3", "4.20", false)
		Expect(facts.Platform).To(Equal(OpenShift))
		Expect(facts.OS).To(Equal(node.Status.NodeInfo.OSImage))
		Expect(facts.MatrixOS).To(Equal("Red Hat CoreOS 4.20"))
	})

	ginkgo.It("does not confirm the Data Center driver pin for a vGPU guest", func(ctx ginkgo.SpecContext) {
		client := validClient()
		node, err := client.CoreV1().Nodes().Get(ctx, "worker", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		node.Labels["nvidia.com/vgpu.present"] = "true"
		_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		a := Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(a.Passed()).To(BeFalse())
		Expect(findCheck(a.Nodes[0].Checks, "gpu-driver-family").Status).To(Equal(Fail))
		Expect(findComponent(a, GPUDriver).Check.Status).To(Equal(Fail))
		Expect(client.CoreV1().Pods("operators").Delete(ctx, "driver", metav1.DeleteOptions{})).To(Succeed())
		a = Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(findCheck(a.Nodes[0].Checks, "gpu-driver-family").Status).To(Equal(Unknown))
		Expect(findComponent(a, GPUDriver).Check.Status).To(Equal(Unknown))
		Expect(a.Passed()).To(BeFalse())
	})

	ginkgo.It("gates incomplete Deployments while retaining DaemonSet counters as diagnostics", func(ctx ginkgo.SpecContext) {
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "network-operator", Namespace: "operators", Generation: 1, UID: "network-deployment"},
			Spec: appsv1.DeploymentSpec{Replicas: new(int32(2)), Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "network-operator", Image: "nvcr.io/nvidia/cloud-native/network-operator:v26.7.1"}}},
			}},
			Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, ObservedGeneration: 1},
		}
		daemonSet := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "toolkit", Namespace: "operators", Generation: 2, UID: "toolkit-daemonset"},
			Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "nvidia-container-toolkit-ctr", Image: "nvcr.io/nvidia/k8s/container-toolkit:v1.20.1-ubuntu24.04"}}},
			}},
			Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 1, CurrentNumberScheduled: 1, UpdatedNumberScheduled: 1, NumberReady: 1, NumberAvailable: 1, ObservedGeneration: 1},
		}
		replicaSet := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Name: "network-operator-1234", Namespace: "operators", UID: "network-replicaset",
			OwnerReferences: []metav1.OwnerReference{ownerReference("Deployment", deployment)},
		}}
		client := validClient(deployment, daemonSet, replicaSet)
		attachController(ctx, client, "network-operator", ownerReference("ReplicaSet", replicaSet))
		attachController(ctx, client, "toolkit", ownerReference("DaemonSet", daemonSet))
		a := Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(a.Passed()).To(BeFalse())
		component := findComponent(a, NetworkOperator)
		Expect(component.Check.Status).To(Equal(Unknown))
		Expect(component.Workloads[0].Desired).To(Equal(int32(2)))
		Expect(component.Workloads[0].Observed).To(Equal(int32(1)))
		Expect(findComponent(a, ContainerToolkit).Check.Status).To(Equal(Pass))
		Expect(findComponent(a, ContainerToolkit).Workloads[0].Complete).To(BeFalse())

		deployment.Spec.Replicas = new(int32(1))
		_, err := client.AppsV1().Deployments("operators").Update(ctx, deployment, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		daemonSet.Status.ObservedGeneration = 2
		_, err = client.AppsV1().DaemonSets("operators").UpdateStatus(ctx, daemonSet, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		a = Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(a.Passed()).To(BeTrue(), "%+v", a.Components)
		Expect(findComponent(a, NetworkOperator).Workloads[0].OwnedReadyPods).To(Equal(int32(1)))
		Expect(findComponent(a, ContainerToolkit).Workloads[0].OwnedReadyPods).To(Equal(int32(1)))
	})

	ginkgo.It("rejects stale label-only GPU driver version evidence", func(ctx ginkgo.SpecContext) {
		client := validClient()
		Expect(client.CoreV1().Pods("operators").Delete(ctx, "driver", metav1.DeleteOptions{})).To(Succeed())
		Expect(client.AppsV1().DaemonSets("operators").Delete(ctx, "driver", metav1.DeleteOptions{})).To(Succeed())
		a := Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(a.Passed()).To(BeTrue())
		Expect(findComponent(a, GPUDriver).Detection).To(Equal(NotDetected))
		Expect(findComponent(a, GPUDriver).Check.Required).To(BeFalse())
		component := findComponent(a, GPUDriver)
		Expect(component.Observations).To(BeEmpty())
		Expect(component.Check.Status).To(Equal(Unknown))
	})

	ginkgo.It("omits uncheckable components despite similar workload names", func(ctx ginkgo.SpecContext) {
		deployment := workloadDeployment("dpf", "example.test/nvidia/dpf-operator:v26.8.0", 1)
		client := validClient(deployment)
		a := Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(a.Passed()).To(BeTrue())
		component := findComponent(a, dpuOperator)
		Expect(component.Expected.Name).To(BeEmpty())
		Expect(component.Check.Required).To(BeFalse())
		Expect(component.Observations).To(BeEmpty())
		Expect(component.Workloads).To(BeEmpty())

	})

	ginkgo.It("does not count unrelated Pods or a stale owner UID toward controller coverage", func(ctx ginkgo.SpecContext) {
		deployment := workloadDeployment("network-operator", "nvcr.io/nvidia/cloud-native/network-operator:v26.7.1", 2)
		replicaSet := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Name: "network-operator-1234", Namespace: "operators", UID: "rs-current",
			OwnerReferences: []metav1.OwnerReference{ownerReference("Deployment", deployment)},
		}}
		unowned := readyPod("network-operator-unowned", "worker", "network-operator", deployment.Spec.Template.Spec.Containers[0].Image)
		client := validClient(deployment, replicaSet, unowned)
		attachController(ctx, client, "network-operator", ownerReference("ReplicaSet", replicaSet))
		stale := ownerReference("ReplicaSet", replicaSet)
		stale.UID = "rs-stale"
		attachController(ctx, client, unowned.Name, stale)
		a := Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(a.Passed()).To(BeFalse())
		component := findComponent(a, NetworkOperator)
		Expect(component.Workloads[0].Complete).To(BeTrue())
		Expect(component.Workloads[0].OwnedReadyPods).To(Equal(int32(1)))
		Expect(component.Check.Status).To(Equal(Unknown))
		Expect(component.Check.Detail).To(ContainSubstring("owned, ready Pods"))
	})

	ginkgo.It("retains a running operator identity when its desired image becomes unrecognized", func(ctx ginkgo.SpecContext) {
		old := readyPod("operator-old", "worker", "network-operator", "nvcr.io/nvidia/cloud-native/network-operator:v26.4.1")
		old.Spec.Containers[0].Image = "example.test/nvidia/different-manager:v1.2.3"
		a := Assess(ctx, validClient(old), "9.0", nil, nil, nil)
		Expect(a.Passed()).To(BeFalse())
		component := findComponent(a, NetworkOperator)
		Expect(component.Check.Status).To(Equal(Fail))
		Expect(component.Observations).To(HaveLen(2))
		Expect(component.Observations[1].StatusVersion).To(Equal("26.4.1"))
		Expect(component.Observations[1].StatusImage).To(HaveSuffix(":v26.4.1"))
	})

	ginkgo.It("surfaces ReplicaSet permission failure for an identified Deployment", func(ctx ginkgo.SpecContext) {
		client := validClient(workloadDeployment("network-operator", "nvcr.io/nvidia/cloud-native/network-operator:v26.7.1", 1))
		client.PrependReactor("list", "replicasets", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "replicasets"}, "", errors.New("denied"))
		})
		a := Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(a.Passed()).To(BeFalse())
		Expect(findCheck(a.Checks, "replicasets").Status).To(Equal(Unknown))
		Expect(findComponent(a, NetworkOperator).Workloads[0].OwnedReadyPods).To(BeZero())
	})

	ginkgo.It("treats denied workload inventory as unknown", func(ctx ginkgo.SpecContext) {
		client := validClient()
		client.PrependReactor("list", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "", errors.New("denied"))
		})
		a := Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(findCheck(a.Checks, "deployments").Status).To(Equal(Unknown))
		Expect(findComponent(a, NetworkOperator).Detection).To(Equal(DetectionUnknown))
		Expect(a.Passed()).To(BeFalse())
	})

	ginkgo.It("does not skip unsupported architecture or missing selected nodes", func(ctx ginkgo.SpecContext) {
		other := worker("another-worker")
		other.Status.NodeInfo.Architecture = "riscv64"
		a := Assess(ctx, validClient(other), "9.0", []string{"absent", "another-worker", "worker"}, nil, nil)
		Expect(a.Nodes).To(HaveLen(3))
		Expect(a.Nodes[0].Facts.Name).To(Equal("absent"))
		Expect(findComponent(a, DOCADriver).Check.Status).To(Equal(Unknown))
		Expect(findComponent(a, NetworkOperator).Check.Status).To(Equal(Unknown))
		Expect(a.Passed()).To(BeFalse())
	})

	ginkgo.It("requires declared artifact GPU evidence on every selected GPU worker", func(ctx ginkgo.SpecContext) {
		a := Assess(ctx, validClient(worker("another-worker")), "9.0", nil, nil, nil)
		Expect(findComponent(a, DOCADriver).Check.Status).To(Equal(Unknown))
		Expect(findComponent(a, GPUDriver).Check.Status).To(Equal(Unknown))
		Expect(findComponent(a, GPUDriver).MissingNodes).To(Equal([]string{"another-worker"}))
	})

	ginkgo.It("rejects kind for the bare-metal scope and exposes fresh GPU conflicts", func(ctx ginkgo.SpecContext) {
		node := worker("kind")
		node.Spec.ProviderID = "kind://docker/test/kind"
		a := Assess(ctx, validClient(node), "9.0", []string{"kind"}, map[string]string{"kind": "H100 NVL"}, nil)
		Expect(findCheck(a.Nodes[0].Checks, "deployment").Status).To(Equal(Fail))
		Expect(findCheck(a.Nodes[0].Checks, "gpu-model").Status).To(Equal(Unknown))
		Expect(a.Passed()).To(BeFalse())
	})

	ginkgo.It("returns partial evidence when cluster-wide Pod RBAC is denied", func(ctx ginkgo.SpecContext) {
		client := validClient()
		client.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("denied"))
		})
		a := Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(a.Nodes).To(HaveLen(1))
		Expect(findCheck(a.Checks, "pods").Status).To(Equal(Unknown))
		Expect(findComponent(a, DOCADriver).Check.Status).To(Equal(Unknown))
		Expect(findComponent(a, NetworkOperator).Detection).To(Equal(DetectionUnknown))
		Expect(a.Passed()).To(BeFalse())
	})

	ginkgo.It("reads the real API server version endpoint on the caller transport", func(ctx ginkgo.SpecContext) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/version" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"gitVersion":"v1.34.3+rke2r1"}`))
		}))
		defer server.Close()
		client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
		Expect(err).NotTo(HaveOccurred())
		version, err := observeServerVersion(ctx, client)
		Expect(err).NotTo(HaveOccurred())
		Expect(version).To(Equal("v1.34.3+rke2r1"))
	})

	ginkgo.It("honors a caller deadline on a stalled real API request", func(ctx ginkgo.SpecContext) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
				w.WriteHeader(http.StatusGatewayTimeout)
			}
		}))
		defer server.Close()
		client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
		Expect(err).NotTo(HaveOccurred())
		deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		a := Assess(deadline, client, "9.0", nil, nil, nil)
		Expect(a.Passed()).To(BeFalse())
		Expect(findCheck(a.Checks, "cluster-observation").Detail).To(ContainSubstring("deadline exceeded"))
	})

	ginkgo.It("does not call the API with an already canceled context", func(ctx ginkgo.SpecContext) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		client := validClient()
		a := Assess(canceled, client, "9.0", nil, nil, nil)
		Expect(a.Passed()).To(BeFalse())
		Expect(findCheck(a.Checks, "cluster-observation").Detail).To(ContainSubstring("canceled"))
		Expect(client.Actions()).To(BeEmpty())
	})
})

func worker(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{gpuProductLabel: rtxServer, gpuDriverLabel: "620.70.06"}},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
			Architecture: "amd64", OSImage: "Ubuntu 24.04.2 LTS", KubeletVersion: "v1.34.3+rke2r1", ContainerRuntimeVersion: "containerd://2.1.5",
		}},
	}
}

func readyPod(name, node, container, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "operators", UID: types.UID("pod-" + name), OwnerReferences: fixturePodOwners(container)},
		Spec: corev1.PodSpec{
			NodeName: node, Containers: []corev1.Container{{Name: container, Image: image, VolumeMounts: []corev1.VolumeMount{{Name: "nvidia-install", MountPath: "/usr/local/nvidia"}}}},
			Volumes: []corev1.Volume{{Name: "nvidia-install", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/usr/local/nvidia"}}}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: container, Image: image, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
		},
	}
}

func validClient(extra ...runtime.Object) *fake.Clientset {
	objects := []runtime.Object{
		workloadDeployment("network-operator", "nvcr.io/nvidia/cloud-native/network-operator:v26.7.1", 1),
		workloadDeployment("gpu-operator", "nvcr.io/nvidia/gpu-operator:v26.7.2", 1),
		fixtureReplicaSet("network-operator"), fixtureReplicaSet("gpu-operator"),
		fixtureDaemonSet("driver", "nvidia-driver-ctr", "nvcr.io/nvidia/driver:620.70.06-ubuntu24.04"),
		fixtureDaemonSet("toolkit", "nvidia-container-toolkit-ctr", "nvcr.io/nvidia/k8s/container-toolkit:v1.20.1-ubuntu24.04"),
		worker("worker"),
		readyPod("network-operator", "worker", "network-operator", "nvcr.io/nvidia/cloud-native/network-operator:v26.7.1"),
		readyPod("gpu-operator", "worker", "gpu-operator", "nvcr.io/nvidia/gpu-operator:v26.7.2"),
		readyPod("driver", "worker", "nvidia-driver-ctr", "nvcr.io/nvidia/driver:620.70.06-ubuntu24.04"),
		readyPod("toolkit", "worker", "nvidia-container-toolkit-ctr", "nvcr.io/nvidia/k8s/container-toolkit:v1.20.1-ubuntu24.04"),
		readyPod("doca", "worker", "mofed-container", "nvcr.io/nvidia/mellanox/doca-driver:doca3.5.0-26.07-0.7.7.0-1"),
	}
	for _, replacement := range extra {
		accessor := replacement.(metav1.Object)
		replaced := false
		for i, object := range objects {
			original := object.(metav1.Object)
			if fmt.Sprintf("%T", object) == fmt.Sprintf("%T", replacement) && original.GetName() == accessor.GetName() && original.GetNamespace() == accessor.GetNamespace() {
				objects[i] = replacement
				replaced = true
				break
			}
		}
		if !replaced {
			objects = append(objects, replacement)
		}
	}
	client := fake.NewClientset(objects...)
	setServerVersion(client, "v1.34.3+rke2r1")
	return client
}

func workloadDeployment(name, image string, replicas int32) *appsv1.Deployment {
	container := "network-operator"
	if name == "gpu-operator" {
		container = "gpu-operator"
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "operators", Generation: 1, UID: types.UID("deployment-" + name)},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: container, Image: image}}},
		}},
		Status: appsv1.DeploymentStatus{Replicas: replicas, UpdatedReplicas: replicas, ReadyReplicas: replicas, AvailableReplicas: replicas, ObservedGeneration: 1},
	}
}

func ownerReference(kind string, owner metav1.Object) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "apps/v1", Kind: kind, Name: owner.GetName(), UID: owner.GetUID(), Controller: new(true),
	}
}

func attachController(ctx context.Context, client *fake.Clientset, podName string, owner metav1.OwnerReference) {
	pod, err := client.CoreV1().Pods("operators").Get(ctx, podName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	pod.OwnerReferences = []metav1.OwnerReference{owner}
	_, err = client.CoreV1().Pods("operators").Update(ctx, pod, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func setServerVersion(client *fake.Clientset, gitVersion string) {
	client.Discovery().(*discoveryfake.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: gitVersion}
}

func findCheck(checks []Check, name string) Check {
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	return Check{}
}

func findComponent(assessment *Assessment, name string) ComponentResult {
	for _, component := range assessment.Components {
		if component.Expected.Name == name {
			return component
		}
	}
	return ComponentResult{}
}

func fixturePodOwners(container string) []metav1.OwnerReference {
	name, kind := "", ""
	switch container {
	case "network-operator", "gpu-operator":
		name, kind = container+"-rs", "ReplicaSet"
	case "nvidia-driver-ctr":
		name, kind = "driver", "DaemonSet"
	case "nvidia-container-toolkit-ctr":
		name, kind = "toolkit", "DaemonSet"
	default:
		return nil
	}
	return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: name, UID: types.UID(name), Controller: new(true)}}
}
func fixtureReplicaSet(name string) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: name + "-rs", Namespace: "operators", UID: types.UID(name + "-rs"), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: name, UID: types.UID("deployment-" + name), Controller: new(true)}}}}
}
func fixtureDaemonSet(name, container, image string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "operators", UID: types.UID(name), Generation: 1}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: container, Image: image}}}}}, Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 1, CurrentNumberScheduled: 1, UpdatedNumberScheduled: 1, NumberReady: 1, NumberAvailable: 1, ObservedGeneration: 1}}
}
