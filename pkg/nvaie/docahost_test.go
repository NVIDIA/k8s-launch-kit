// SPDX-FileCopyrightText: Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package nvaie

import (
	"errors"
	corev1 "k8s.io/api/core/v1"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

var _ = ginkgo.Describe("DOCA product evidence", func() {
	ginkgo.DescribeTable("normalizes official driver build tags without equating build suffixes", func(ctx ginkgo.SpecContext, tag string, expected Status) {
		image := "nvcr.io/nvidia/mellanox/doca-driver:" + tag
		ds := fixtureDaemonSet("doca", "mofed-container", image)
		pod := readyPod("doca", "worker", "mofed-container", image)
		pod.OwnerReferences = []metav1.OwnerReference{ownerReference("DaemonSet", ds)}
		result := findComponent(Assess(ctx, validClient(ds, pod), "9.0", nil, nil, nil), DOCADriver)
		Expect(result.Detection).To(Equal(Detected))
		Expect(result.Check.Status).To(Equal(expected))
		Expect(result.Observations).To(HaveLen(1))
		Expect(result.Observations[0].Image).To(Equal(image))
	},
		ginkgo.Entry("base build", "doca3.5.0-26.07-0.7.7.0-1", Pass),
		ginkgo.Entry("arm OS build", "doca3.5.0-26.07-0.7.7.0-1-ubuntu24.04-arm64", Pass),
		ginkgo.Entry("precompiled build", "doca3.5.0-26.07-0.7.7.0-1-6.8.0-87-generic-ubuntu24.04-amd64", Pass),
		ginkgo.Entry("different release", "doca3.6.0-26.10-070000-1-ubuntu24.04-arm64", Fail),
		ginkgo.Entry("prerelease", "doca3.5.0-rc1", Unknown),
		ginkgo.Entry("unrecognized build", "doca3.5.0-custom", Unknown),
		ginkgo.Entry("prerelease suffix on a release-shaped build", "doca3.5.0-26.07-0.7.7.0-1-rc.1", Unknown),
	)

	ginkgo.It("requires version coverage on selected CPU-only networking workers", func(ctx ginkgo.SpecContext) {
		image := "nvcr.io/nvidia/mellanox/doca-driver:doca3.5.0-26.07-0.7.7.0-1-ubuntu24.04-amd64"
		ds := fixtureDaemonSet("doca", "mofed-container", image)
		pod := readyPod("doca", "worker", "mofed-container", image)
		pod.OwnerReferences = []metav1.OwnerReference{ownerReference("DaemonSet", ds)}
		client := validClient(ds, pod)
		node, err := client.CoreV1().Nodes().Get(ctx, "worker", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		second := node.DeepCopy()
		second.Name = "cpu-worker"
		second.UID = "cpu-worker"
		second.Labels = map[string]string{}
		_, err = client.CoreV1().Nodes().Create(ctx, second, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		result := findComponent(Assess(ctx, client, "9.0", []string{"worker", "cpu-worker"}, nil, nil), DOCADriver)
		Expect(result.Check.Status).To(Equal(Unknown))
		Expect(result.MissingNodes).To(ConsistOf("cpu-worker"))
		// Excluding that worker must remove its coverage requirement.
		result = findComponent(Assess(ctx, client, "9.0", []string{"worker"}, nil, nil), DOCADriver)
		Expect(result.Check.Status).To(Equal(Pass))
	})

	ginkgo.It("does not let matching host snapshots hide a conflicting live driver container", func(ctx ginkgo.SpecContext) {
		image := "nvcr.io/nvidia/mellanox/doca-driver:doca3.6.0-26.10-070000-1"
		ds := fixtureDaemonSet("doca", "mofed-container", image)
		pod := readyPod("doca", "worker", "mofed-container", image)
		pod.OwnerReferences = []metav1.OwnerReference{ownerReference("DaemonSet", ds)}
		client := validClient(ds, pod)
		host := &DOCAHostEvidence{MembershipKnown: true, Fresh: false, Observations: []HostPackageObservation{{Node: "worker", Status: "installed", Version: "3.5.0-041000-26.07-ubuntu2404"}}}
		result := findComponent(Assess(ctx, client, "9.0", nil, nil, host), DOCADriver)
		Expect(result.Check.Status).To(Equal(Fail))
		Expect(componentNodeStatuses(result)).To(Equal(map[string]Status{"worker": Fail}))
		Expect(result.PackageObservations[0].Release).To(Equal("3.5.0"))
		Expect(result.PackageObservations[0].Version).To(Equal(host.Observations[0].Version))
		Expect(host.Observations[0].Release).To(BeEmpty())
		Expect(result.Scope).To(ContainSubstring("Saved discovery snapshot"))
		Expect(result.Observations).To(HaveLen(1))
		Expect(result.Workloads).To(HaveLen(1))
		result = findComponent(Assess(ctx, client, "9.0", nil, nil, nil), DOCADriver)
		Expect(result.Check.Status).To(Equal(Fail))
		Expect(result.PackageObservations).To(BeEmpty())
		client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("denied") })
		result = findComponent(Assess(ctx, client, "9.0", nil, nil, nil), DOCADriver)
		Expect(result.Check.Status).To(Equal(Unknown))
		Expect(result.PackageObservations).To(BeEmpty())
	})

	ginkgo.It("requires delivery reconciliation even when concurrent host and container versions match", func(ctx ginkgo.SpecContext) {
		image := "nvcr.io/nvidia/mellanox/doca-driver:doca3.5.0-26.07-0.7.7.0-1"
		ds := fixtureDaemonSet("doca", "mofed-container", image)
		pod := readyPod("doca", "worker", "mofed-container", image)
		pod.OwnerReferences = []metav1.OwnerReference{ownerReference("DaemonSet", ds)}
		host := &DOCAHostEvidence{MembershipKnown: true, Fresh: false, Observations: []HostPackageObservation{{Node: "worker", Status: "installed", Version: "3.5.0"}}}
		result := findComponent(Assess(ctx, validClient(ds, pod), "9.0", nil, nil, host), DOCADriver)
		Expect(result.Check.Status).To(Equal(Unknown))
		Expect(result.Check.Required).To(BeTrue())
		Expect(result.Check.Detail).To(ContainSubstring("requires reconciliation"))
		Expect(componentNodeStatuses(result)).To(Equal(map[string]Status{"worker": Unknown}))
		Expect(result.NodeChecks[0].Workloads).To(HaveLen(1))
		Expect(result.NodeChecks[0].Workloads[0].Version).To(Equal("3.5.0"))
		Expect(result.ScopeCheck).To(BeNil())

		host.Observations[0].Node = "historical-worker"
		result = findComponent(Assess(ctx, validClient(ds, pod), "9.0", []string{"worker"}, nil, host), DOCADriver)
		Expect(componentNodeStatuses(result)).To(Equal(map[string]Status{"worker": Pass, "historical-worker": Pass}))
		Expect(result.ScopeCheck).NotTo(BeNil())
		Expect(result.ScopeCheck.Status).To(Equal(Unknown))
		Expect(result.ScopeCheck.Detail).To(ContainSubstring("requires reconciliation"))

		host.Observations = append(host.Observations, HostPackageObservation{Node: "worker", Status: "not-installed"})
		result = findComponent(Assess(ctx, validClient(ds, pod), "9.0", []string{"worker"}, nil, host), DOCADriver)
		Expect(componentNodeStatuses(result)).To(Equal(map[string]Status{"worker": Pass, "historical-worker": Pass}))
		Expect(result.ScopeCheck).NotTo(BeNil())
		Expect(result.ScopeCheck.Status).To(Equal(Unknown))

		host.Observations[1] = HostPackageObservation{Node: "worker", Status: "unknown", Reason: "package probe unavailable"}
		result = findComponent(Assess(ctx, validClient(ds, pod), "9.0", []string{"worker"}, nil, host), DOCADriver)
		Expect(componentNodeStatuses(result)).To(Equal(map[string]Status{"worker": Unknown, "historical-worker": Pass}))
		Expect(result.NodeChecks[1].Check.Detail).To(Equal("package probe unavailable"))
		Expect(result.ScopeCheck).NotTo(BeNil())
		Expect(result.ScopeCheck.Status).To(Equal(Unknown))
	})

	ginkgo.It("does not let matching host snapshots hide driver rollout scope", func(ctx ginkgo.SpecContext) {
		image := "nvcr.io/nvidia/mellanox/doca-driver:doca3.5.0-26.07-0.7.7.0-1"
		ds := fixtureDaemonSet("doca", "mofed-container", image)
		ds.Status.DesiredNumberScheduled = 2
		pod := readyPod("doca", "outside-worker", "mofed-container", image)
		pod.OwnerReferences = []metav1.OwnerReference{ownerReference("DaemonSet", ds)}
		host := &DOCAHostEvidence{MembershipKnown: true, Fresh: false, Observations: []HostPackageObservation{{Node: "worker", Status: "installed", Version: "3.5.0"}}}
		assessment := Assess(ctx, validClient(ds, pod, worker("outside-worker")), "9.0", []string{"worker"}, nil, host)
		result := findComponent(assessment, DOCADriver)
		Expect(result.Check.Status).To(Equal(Unknown))
		Expect(result.Check.Required).To(BeTrue())
		Expect(result.Observations).To(BeEmpty())
		Expect(result.Workloads).To(HaveLen(1))
		Expect(result.Workloads[0].ImageStatus).To(Equal(Unknown))
		Expect(result.PackageObservations).To(HaveLen(1))
		Expect(componentNodeStatuses(result)).To(Equal(map[string]Status{"worker": Pass}))
		Expect(result.ScopeCheck).NotTo(BeNil())
		Expect(result.ScopeCheck.Status).To(Equal(Unknown))
		Expect(assessment.Passed()).To(BeFalse())
	})

	ginkgo.DescribeTable("retains incomplete controller intent without inventing scheduled scope", func(ctx ginkgo.SpecContext, selector map[string]string, nodeName string, affinity, pending, missingNode bool, detected bool) {
		image := "nvcr.io/nvidia/mellanox/doca-driver:doca3.6.0-26.10-070000-1"
		ds := fixtureDaemonSet("doca", "mofed-container", image)
		ds.Spec.Template.Spec.NodeSelector = selector
		ds.Spec.Template.Spec.NodeName = nodeName
		if affinity {
			ds.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "site", Operator: corev1.NodeSelectorOpIn, Values: []string{"elsewhere"}}}}}}}}
		}
		client := validClient(ds)
		Expect(client.CoreV1().Pods("operators").Delete(ctx, "doca", metav1.DeleteOptions{})).To(Succeed())
		if pending {
			pod := readyPod("doca", "", "mofed-container", image)
			pod.OwnerReferences = []metav1.OwnerReference{ownerReference("DaemonSet", ds)}
			pod.Status.Phase = corev1.PodPending
			_, err := client.CoreV1().Pods("operators").Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		selected := []string{"worker"}
		if missingNode {
			selected = []string{"missing-worker"}
		}
		result := findComponent(Assess(ctx, client, "9.0", selected, nil, nil), DOCADriver)
		Expect(result.Check.Status).To(Equal(Unknown))
		Expect(result.Check.Required).To(Equal(detected))
		if detected {
			Expect(result.Detection).To(Equal(Detected))
			Expect(result.Workloads).To(HaveLen(1))
		} else {
			Expect(result.Detection).To(Equal(NotDetected))
			Expect(result.Workloads).To(BeEmpty())
		}
	},
		ginkgo.Entry("no Pods", map[string]string{}, "", false, false, false, true),
		ginkgo.Entry("selector excludes workers", map[string]string{"site": "outside"}, "", false, false, false, false),
		ginkgo.Entry("node name excludes workers", map[string]string{}, "outside", false, false, false, false),
		ginkgo.Entry("affinity is unresolved", map[string]string{}, "", true, false, false, true),
		ginkgo.Entry("unscheduled owned Pod", map[string]string{}, "", false, true, false, true),
		ginkgo.Entry("missing node labels", map[string]string{"site": "outside"}, "", false, false, true, true),
	)

	ginkgo.It("keeps the host snapshot cohort and separates mismatches from incomplete inventory", func() {
		evidence := DOCAHostEvidence{MembershipKnown: true, Fresh: false, Observations: []HostPackageObservation{
			{Node: "wrong-b", Status: "installed", Version: "3.6.0-2"},
			{Node: "wrong-a", Status: "installed", Version: "3.6.0-1"},
			{Node: "matches", Status: "installed", Version: "3.5.0"},
			{Node: "missing", Status: "unknown", Reason: "probe unavailable"},
		}}
		result := AssessDOCAHost("9.0", evidence)
		Expect(result.Check.Status).To(Equal(Fail))
		Expect(componentNodeStatuses(result)).To(Equal(map[string]Status{
			"matches": Pass, "missing": Unknown, "wrong-a": Fail, "wrong-b": Fail,
		}))
		Expect(result.PackageObservations).To(HaveLen(4))
		Expect(result.PackageEvidenceFresh).To(BeFalse())
		Expect(result.NodeChecks[0].Node).To(Equal("matches"))
		Expect(result.NodeChecks[3].Node).To(Equal("wrong-b"))

		evidence.MembershipKnown = false
		result = AssessDOCAHost("9.0", evidence)
		Expect(result.Check.Status).To(Equal(Fail))
		Expect(componentNodeStatuses(result)).To(Equal(map[string]Status{
			"matches": Unknown, "missing": Unknown, "wrong-a": Fail, "wrong-b": Fail,
		}))
		evidence.MembershipKnown = true
		evidence.Observations = append(evidence.Observations, HostPackageObservation{Node: "matches", Status: "installed", Version: "3.5.0"})
		result = AssessDOCAHost("9.0", evidence)
		Expect(componentNodeStatuses(result)["matches"]).To(Equal(Unknown))

		evidence.Observations = []HostPackageObservation{{Status: "installed", Version: "3.5.0"}}
		result = AssessDOCAHost("9.0", evidence)
		Expect(result.Check.Status).To(Equal(Unknown))
		Expect(result.NodeChecks).To(BeEmpty())
		Expect(result.ScopeCheck).NotTo(BeNil())
		Expect(result.ScopeCheck.Status).To(Equal(Unknown))
	})

	ginkgo.It("keeps host-only snapshot gate semantics while exposing an unavailable live read", func(ctx ginkgo.SpecContext) {
		host := &DOCAHostEvidence{MembershipKnown: true, Fresh: false, Observations: []HostPackageObservation{
			{Node: "worker", Status: "installed", Version: "3.5.0"},
		}}
		client := validClient()
		client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("denied")
		})
		assessment := Assess(ctx, client, "9.0", nil, nil, host)
		result := findComponent(assessment, DOCADriver)
		Expect(result.Check.Status).To(Equal(Pass))
		Expect(componentNodeStatuses(result)).To(Equal(map[string]Status{"worker": Unknown}))
		Expect(result.ScopeCheck).NotTo(BeNil())
		Expect(result.ScopeCheck.Status).To(Equal(Unknown))
		Expect(assessment.Passed()).To(BeFalse())
	})

	ginkgo.DescribeTable("preserves historical host coverage and mismatch semantics", func(observations []HostPackageObservation, complete bool, expected Status, required bool) {
		result := AssessDOCAHost("9.0", DOCAHostEvidence{Observations: observations, MembershipKnown: complete, Fresh: false})
		Expect(result.Check.Status).To(Equal(expected))
		Expect(result.Check.Required).To(Equal(required))
		Expect(result.PackageEvidenceFresh).To(BeFalse())
	},
		ginkgo.Entry("matching package", []HostPackageObservation{{Node: "a", Status: "installed", Version: "3.5.0-041000-26.07"}}, true, Pass, true),
		ginkgo.Entry("incomplete membership", []HostPackageObservation{{Node: "a", Status: "installed", Version: "3.5.0-041000"}}, false, Unknown, true),
		ginkgo.Entry("no package snapshot", []HostPackageObservation{}, true, Unknown, false),
		ginkgo.Entry("inbox is not DOCA-HOST", []HostPackageObservation{{Node: "a", Status: "not-installed"}}, true, Unknown, false),
		ginkgo.Entry("unknown host", []HostPackageObservation{{Node: "a", Status: "unknown", Reason: "probe unavailable"}}, true, Unknown, false),
		ginkgo.Entry("mixed workers", []HostPackageObservation{{Node: "a", Status: "installed", Version: "3.5.0"}, {Node: "b", Status: "not-installed"}}, true, Unknown, true),
		ginkgo.Entry("unparseable installed version", []HostPackageObservation{{Node: "a", Status: "installed", Version: "unrecognized"}}, true, Unknown, true),
		ginkgo.Entry("mismatch wins over incomplete coverage", []HostPackageObservation{{Node: "a", Status: "installed", Version: "3.6.0"}, {Node: "b", Status: "unknown"}}, false, Fail, true),
		ginkgo.Entry("mismatch wins over later ambiguous identity", []HostPackageObservation{{Node: "a", Status: "installed", Version: "3.6.0"}, {Node: "a", Status: "installed", Version: "3.5.0"}}, true, Fail, true),
	)
})
