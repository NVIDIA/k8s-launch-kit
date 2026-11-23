// SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package nvaie

import (
	"errors"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

var _ = ginkgo.Describe("proven Kubernetes artifact procedures", func() {
	ginkgo.It("reports only supported version checks while preserving the reference catalog", func(ctx ginkgo.SpecContext) {
		client := validClient()
		assessment := Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(assessment.Components).To(HaveLen(7))
		release, found := Lookup("9.0")
		Expect(found).To(BeTrue())
		Expect(release.Components).To(HaveLen(13))
		for _, component := range assessment.Components {
			Expect(component.Method).NotTo(BeEmpty())
			Expect(component.Scope).NotTo(BeEmpty())
		}
		for _, name := range []string{"Base Command Manager (BCM)", "DOCA Microservices", "DPF", "Fabric Manager", "Virtual GPU Manager", vgpuGuest} {
			Expect(findComponent(assessment, name).Expected.Name).To(BeEmpty())
		}
		for _, action := range client.Actions() {
			Expect(action.GetVerb()).To(BeElementOf("get", "list"))
			Expect(action.GetSubresource()).NotTo(Equal("exec"))
		}
	})

	ginkgo.DescribeTable("checks the chart-defined NIM manager artifact", func(ctx ginkgo.SpecContext, tag string, expected Status) {
		image := "nvcr.io/nvidia/cloud-native/k8s-nim-operator:" + tag
		deployment := workloadDeployment("nim", image, 1)
		deployment.Spec.Template.Spec.Containers[0].Name = "manager"
		replicaSet := fixtureReplicaSet("nim")
		pod := readyPod("nim", "worker", "manager", image)
		pod.OwnerReferences = []metav1.OwnerReference{ownerReference("ReplicaSet", replicaSet)}
		assessment := Assess(ctx, validClient(deployment, replicaSet, pod), "9.0", nil, nil, nil)
		component := findComponent(assessment, nimOperator)
		Expect(component.Detection).To(Equal(Detected))
		Expect(component.Check.Status).To(Equal(expected))
		Expect(component.Check.Required).To(BeTrue())
		Expect(component.Observations).To(HaveLen(1))
	},
		ginkgo.Entry("release pin", "v3.1.4", Pass),
		ginkgo.Entry("older artifact", "v3.1.3", Fail),
		ginkgo.Entry("unpublished prerelease", "v3.1.4-rc.1", Unknown),
	)

	ginkgo.It("does not identify a NIM upgrade Job as an operator installation", func(ctx ginkgo.SpecContext) {
		pod := readyPod("nim-upgrade", "worker", "manager", "nvcr.io/nvidia/cloud-native/k8s-nim-operator:v3.1.4")
		pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "upgrade", UID: "upgrade", Controller: new(true)}}
		component := findComponent(Assess(ctx, validClient(pod), "9.0", nil, nil, nil), nimOperator)
		Expect(component.Detection).To(Equal(NotDetected))
		Expect(component.Check.Required).To(BeFalse())
	})

	ginkgo.DescribeTable("compares the Run:ai cluster operator with the published minor release", func(ctx ginkgo.SpecContext, tag string, expected Status) {
		image := "nvcr.io/nvidia/runai-oss/go-operator:" + tag
		deployment := workloadDeployment("runai", image, 1)
		deployment.Spec.Template.Spec.Containers[0].Name = "runai-go-operator"
		replicaSet := fixtureReplicaSet("runai")
		pod := readyPod("runai", "worker", "runai-go-operator", image)
		pod.OwnerReferences = []metav1.OwnerReference{ownerReference("ReplicaSet", replicaSet)}
		component := findComponent(Assess(ctx, validClient(deployment, replicaSet, pod), "9.0", nil, nil, nil), runAI)
		Expect(component.Detection).To(Equal(Detected))
		Expect(component.Check.Status).To(Equal(expected))
		Expect(component.Check.Required).To(BeTrue())
		Expect(component.Scope).To(ContainSubstring("control plane and operands not verified"))
		if expected == Pass {
			Expect(component.Observations[0].Version).To(Equal(tag))
		}
	},
		ginkgo.Entry("documented 3.0 hotfix", "3.0.24", Pass),
		ginkgo.Entry("another minor", "3.1.0", Fail),
		ginkgo.Entry("another major", "4.0.0", Fail),
		ginkgo.Entry("unrecognized prerelease", "3.0.24-rc.1", Unknown),
	)

	ginkgo.It("keeps exact release pins exact", func() {
		Expect(componentVersionMatches(Component{Name: runAI, Version: "3.0.24"}, "3.0.25")).To(BeFalse())
		Expect(componentVersionMatches(Component{Name: nimOperator, Version: "3.1.4"}, "3.1.5")).To(BeFalse())
	})

	ginkgo.It("does not identify a similarly named Run:ai image or KAI scheduler as the cluster operator", func(ctx ginkgo.SpecContext) {
		for _, image := range []string{"example.test/runai-oss/go-operator:3.0.24", "nvcr.io/nvidia/kai-scheduler:0.18.0"} {
			deployment := workloadDeployment("runai", image, 1)
			deployment.Spec.Template.Spec.Containers[0].Name = "runai-go-operator"
			component := findComponent(Assess(ctx, validClient(deployment), "9.0", nil, nil, nil), runAI)
			Expect(component.Detection).To(Equal(NotDetected))
			Expect(component.Check.Required).To(BeFalse())
		}
	})

	ginkgo.DescribeTable("rejects lookalike operator identities", func(ctx ginkgo.SpecContext, image, container string) {
		deployment := workloadDeployment("gpu-operator", image, 1)
		deployment.Spec.Template.Spec.Containers[0].Name = container
		assessment := Assess(ctx, validClient(deployment), "9.0", nil, nil, nil)
		result := findComponent(assessment, GPUOperator)
		Expect(result.Detection).To(Equal(NotDetected))
		Expect(result.Observations).To(BeEmpty())
		Expect(result.Check.Required).To(BeFalse())
	},
		ginkgo.Entry("unrelated repository", "example.test/nvidia/gpu-operator:v26.7.2", "gpu-operator"),
		ginkgo.Entry("validator using official image", "nvcr.io/nvidia/gpu-operator:v26.7.2", "gpu-operator-validator"),
	)

	ginkgo.It("requires an operator Deployment rather than an operand DaemonSet", func(ctx ginkgo.SpecContext) {
		client := validClient()
		Expect(client.AppsV1().Deployments("operators").Delete(ctx, "gpu-operator", metav1.DeleteOptions{})).To(Succeed())
		_, err := client.AppsV1().DaemonSets("operators").Create(ctx, fixtureDaemonSet("validator", "gpu-operator", "nvcr.io/nvidia/gpu-operator:v26.7.2"), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		assessment := Assess(ctx, client, "9.0", nil, nil, nil)
		Expect(findComponent(assessment, GPUOperator).Detection).To(Equal(NotDetected))
	})

	ginkgo.It("keeps absent Network Operator unknown and required for Host networking scope", func(ctx ginkgo.SpecContext) {
		client := validClient()
		Expect(client.AppsV1().Deployments("operators").Delete(ctx, "network-operator", metav1.DeleteOptions{})).To(Succeed())
		assessment := Assess(ctx, client, "9.0", nil, nil, nil)
		component := findComponent(assessment, NetworkOperator)
		Expect(component.Detection).To(Equal(NotDetected))
		Expect(component.Check.Status).To(Equal(Unknown))
		Expect(component.Check.Required).To(BeTrue())
		Expect(assessment.Passed()).To(BeFalse())
	})

	ginkgo.It("does not substitute a stale matching label for the declared driver artifact", func(ctx ginkgo.SpecContext) {
		image := "nvcr.io/nvidia/driver:595.91.07-ubuntu24.04"
		assessment := Assess(ctx, validClient(readyPod("driver", "worker", "nvidia-driver-ctr", image), fixtureDaemonSet("driver", "nvidia-driver-ctr", image)), "9.0", nil, nil, nil)
		component := findComponent(assessment, GPUDriver)
		Expect(component.Check.Status).To(Equal(Fail))
		Expect(component.Observations).To(HaveLen(1))
		Expect(component.Observations[0].Version).To(Equal("595.91.07"))
		Expect(component.Observations[0].Source).To(ContainSubstring("host installation/binary not verified"))
	})

	ginkgo.It("reports recognized toolkit artifact tags without claiming a host binary version", func(ctx ginkgo.SpecContext) {
		image := "nvcr.io/nvidia/k8s/container-toolkit:v1.20.1"
		assessment := Assess(ctx, validClient(readyPod("toolkit", "worker", "nvidia-container-toolkit-ctr", image), fixtureDaemonSet("toolkit", "nvidia-container-toolkit-ctr", image)), "9.0", nil, nil, nil)
		component := findComponent(assessment, ContainerToolkit)
		Expect(component.Check.Status).To(Equal(Pass))
		Expect(component.Observations[0].Version).To(Equal("1.20.1"))
		Expect(component.Scope).To(ContainSubstring("artifact only"))
		Expect(component.Scope).To(ContainSubstring("host installation/binary not verified"))
	})

	ginkgo.It("keeps unknown official artifact tag formats unknown", func(ctx ginkgo.SpecContext) {
		image := "nvcr.io/nvidia/driver:620.70.06-custom-unknown"
		assessment := Assess(ctx, validClient(readyPod("driver", "worker", "nvidia-driver-ctr", image), fixtureDaemonSet("driver", "nvidia-driver-ctr", image)), "9.0", nil, nil, nil)
		component := findComponent(assessment, GPUDriver)
		Expect(component.Detection).To(Equal(Detected))
		Expect(component.Check.Status).To(Equal(Unknown))
		Expect(component.Observations[0].Version).To(BeEmpty())
	})

	ginkgo.It("treats wholly absent optional installation forms as undetected rather than required or absent host software", func(ctx ginkgo.SpecContext) {
		client := validClient()
		for _, name := range []string{"driver", "toolkit"} {
			Expect(client.AppsV1().DaemonSets("operators").Delete(ctx, name, metav1.DeleteOptions{})).To(Succeed())
		}
		assessment := Assess(ctx, client, "9.0", nil, nil, nil)
		for _, name := range []string{GPUDriver, ContainerToolkit} {
			component := findComponent(assessment, name)
			Expect(component.Detection).To(Equal(NotDetected))
			Expect(component.Check.Status).To(Equal(Unknown))
			Expect(component.Check.Required).To(BeFalse())
			Expect(component.Check.Detail).To(ContainSubstring("host or alternative installations remain unchecked"))
		}
	})
	ginkgo.It("ignores an unrelated wrong-version driver DaemonSet outside selected workers", func(ctx ginkgo.SpecContext) {
		image := "nvcr.io/nvidia/driver:595.91.07-ubuntu24.04"
		outside := fixtureDaemonSet("outside-driver", "nvidia-driver-ctr", image)
		pod := readyPod("outside-driver", "outside-worker", "nvidia-driver-ctr", image)
		pod.OwnerReferences = []metav1.OwnerReference{ownerReference("DaemonSet", outside)}
		assessment := Assess(ctx, validClient(worker("outside-worker"), outside, pod), "9.0", []string{"worker"}, nil, nil)
		component := findComponent(assessment, GPUDriver)
		Expect(component.Check.Status).To(Equal(Pass))
		Expect(component.Observations).To(HaveLen(1))
		Expect(component.Workloads).To(HaveLen(1))
		Expect(component.Workloads[0].Name).To(Equal("operators/driver"))
	})

	ginkgo.It("gates selected driver Pods independently of global DaemonSet readiness", func(ctx ginkgo.SpecContext) {
		image := "nvcr.io/nvidia/driver:620.70.06-ubuntu24.04"
		driver := fixtureDaemonSet("driver", "nvidia-driver-ctr", image)
		driver.Status.DesiredNumberScheduled = 2
		driver.Status.CurrentNumberScheduled = 2
		driver.Status.UpdatedNumberScheduled = 2
		outside := readyPod("outside-driver", "outside-worker", "nvidia-driver-ctr", image)
		outside.Status.ContainerStatuses[0].Ready = false
		assessment := Assess(ctx, validClient(worker("outside-worker"), driver, outside), "9.0", []string{"worker"}, nil, nil)
		component := findComponent(assessment, GPUDriver)
		Expect(component.Check.Status).To(Equal(Pass))
		Expect(component.MissingNodes).To(BeEmpty())
		Expect(component.Workloads[0].Complete).To(BeFalse())
		Expect(component.Workloads[0].OwnedReadyPods).To(Equal(int32(1)))
	})

})

var _ = ginkgo.Describe("node-local component evidence", func() {
	const (
		toolkitImage = "nvcr.io/nvidia/k8s/container-toolkit:v1.20.1-ubuntu24.04"
		wrongDriver  = "nvcr.io/nvidia/driver:595.91.07-ubuntu24.04"
	)

	ginkgo.It("distinguishes matching, mismatched and missing worker evidence without changing the aggregate gates", func(ctx ginkgo.SpecContext) {
		client := validClient(worker("missing-worker"),
			fixtureDaemonSet("driver", "nvidia-driver-ctr", wrongDriver),
			readyPod("driver", "worker", "nvidia-driver-ctr", wrongDriver),
		)
		assessment := Assess(ctx, client, "9.0", []string{"worker", "missing-worker"}, nil, nil)
		toolkit := findComponent(assessment, ContainerToolkit)
		Expect(toolkit.Check.Status).To(Equal(Unknown))
		Expect(componentNodeStatuses(toolkit)).To(Equal(map[string]Status{"worker": Pass, "missing-worker": Unknown}))
		Expect(toolkit.ScopeCheck).To(BeNil())
		driver := findComponent(assessment, GPUDriver)
		Expect(driver.Check.Status).To(Equal(Fail))
		Expect(componentNodeStatuses(driver)).To(Equal(map[string]Status{"worker": Fail, "missing-worker": Unknown}))
		Expect(driver.ScopeCheck).To(BeNil())
		Expect(assessment.Passed()).To(BeFalse())

		operator := findComponent(assessment, NetworkOperator)
		Expect(operator.Check.Status).To(Equal(Pass))
		Expect(operator.NodeChecks).To(BeEmpty())
		Expect(operator.ScopeCheck).To(BeNil())
	})

	ginkgo.DescribeTable("uses the existing version, image and readiness rules for each node", func(ctx ginkgo.SpecContext, mutate func(*corev1.Node, *corev1.Pod), expected Status) {
		node := worker("worker")
		pod := readyPod("toolkit", "worker", "nvidia-container-toolkit-ctr", toolkitImage)
		mutate(node, pod)
		toolkit := findComponent(Assess(ctx, validClient(node, pod), "9.0", nil, nil, nil), ContainerToolkit)
		Expect(componentNodeStatuses(toolkit)).To(Equal(map[string]Status{"worker": expected}))
	},
		ginkgo.Entry("not ready", func(_ *corev1.Node, pod *corev1.Pod) { pod.Status.ContainerStatuses[0].Ready = false }, Unknown),
		ginkgo.Entry("unknown architecture", func(node *corev1.Node, _ *corev1.Pod) { node.Status.NodeInfo.Architecture = "" }, Unknown),
		ginkgo.Entry("missing controller owner", func(_ *corev1.Node, pod *corev1.Pod) { pod.OwnerReferences = nil }, Unknown),
		ginkgo.Entry("owner UID differs", func(_ *corev1.Node, pod *corev1.Pod) { pod.OwnerReferences[0].UID = "deleted-controller" }, Unknown),
		ginkgo.Entry("status image differs", func(_ *corev1.Node, pod *corev1.Pod) {
			pod.Status.ContainerStatuses[0].Image = "nvcr.io/nvidia/k8s/container-toolkit:v1.19.1-ubuntu24.04"
		}, Fail),
	)

	ginkgo.It("retains unplaced controller intent at scope level", func(ctx ginkgo.SpecContext) {
		future := fixtureDaemonSet("future-toolkit", "nvidia-container-toolkit-ctr", "nvcr.io/nvidia/k8s/container-toolkit:v1.19.1-ubuntu24.04")
		toolkit := findComponent(Assess(ctx, validClient(future), "9.0", nil, nil, nil), ContainerToolkit)
		Expect(toolkit.Check.Status).To(Equal(Unknown))
		Expect(componentNodeStatuses(toolkit)).To(Equal(map[string]Status{"worker": Pass}))
		Expect(toolkit.ScopeCheck).NotTo(BeNil())
		Expect(toolkit.ScopeCheck.Status).To(Equal(Unknown))
	})

	ginkgo.It("limits an owned template mismatch to the observed node", func(ctx ginkgo.SpecContext) {
		ds := fixtureDaemonSet("old-toolkit", "nvidia-container-toolkit-ctr", "nvcr.io/nvidia/k8s/container-toolkit:v1.19.1-ubuntu24.04")
		pod := readyPod("old-toolkit", "second-worker", "nvidia-container-toolkit-ctr", toolkitImage)
		pod.OwnerReferences = []metav1.OwnerReference{ownerReference("DaemonSet", ds)}
		assessment := Assess(ctx, validClient(worker("second-worker"), ds, pod), "9.0", nil, nil, nil)
		toolkit := findComponent(assessment, ContainerToolkit)
		Expect(toolkit.Check.Status).To(Equal(Fail))
		Expect(componentNodeStatuses(toolkit)).To(Equal(map[string]Status{"worker": Pass, "second-worker": Fail}))
		Expect(toolkit.NodeChecks[0].Node).To(Equal("second-worker"))
		Expect(toolkit.NodeChecks[0].Check.Detail).To(ContainSubstring("workload template declares a different version"))
		Expect(toolkit.NodeChecks[0].Workloads).To(HaveLen(1))
		Expect(toolkit.NodeChecks[0].Workloads[0].Version).To(Equal("1.19.1"))
		Expect(toolkit.NodeChecks[1].Workloads).To(HaveLen(1))
		Expect(toolkit.NodeChecks[1].Workloads[0].Version).To(Equal("1.20.1"))
		Expect(toolkit.ScopeCheck).To(BeNil())
	})

	ginkgo.It("does not pass node evidence when a required read fails", func(ctx ginkgo.SpecContext) {
		client := validClient(readyPod("driver", "worker", "nvidia-driver-ctr", wrongDriver))
		client.PrependReactor("list", "daemonsets", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("daemonset read denied")
		})
		assessment := Assess(ctx, client, "9.0", nil, nil, nil)
		toolkit := findComponent(assessment, ContainerToolkit)
		Expect(componentNodeStatuses(toolkit)).To(Equal(map[string]Status{"worker": Unknown}))
		Expect(toolkit.ScopeCheck).NotTo(BeNil())
		Expect(toolkit.ScopeCheck.Detail).To(ContainSubstring("daemonset read denied"))
		driver := findComponent(assessment, GPUDriver)
		Expect(componentNodeStatuses(driver)).To(Equal(map[string]Status{"worker": Unknown}))
		Expect(assessment.Passed()).To(BeFalse())
	})
})

func componentNodeStatuses(result ComponentResult) map[string]Status {
	statuses := make(map[string]Status, len(result.NodeChecks))
	for _, node := range result.NodeChecks {
		statuses[node.Node] = node.Check.Status
	}
	return statuses
}
