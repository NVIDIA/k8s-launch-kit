// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sentry"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
)

var _ = Describe("service DaemonSet exact device coverage", func() {
	fixture := func() (*run, *fakeClient, *unstructured.Unstructured) {
		_, g, f, _ := lifecycleFixture()
		second := g.Provisioning.Devices[0]
		second.ID = "physical-b"
		second.DPUDevice = "device-b"
		second.DPUNode = "node-b"
		g.Provisioning.Devices = append(g.Provisioning.Devices, second)
		selected := map[string]map[string]string{}
		for _, name := range []string{"selected-a", "selected-b"} {
			node := testObject("v1", "Node", "", name, nil)
			node.SetLabels(map[string]string{sentry.ScopeLabel: "fixture"})
			setReady(node)
			f.tenant.put(node)
			selected[name] = node.GetLabels()
		}
		ds := testObject("apps/v1", "DaemonSet", "sentry-system", "tower", map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "tower"}}, "template": map[string]any{"spec": map[string]any{"nodeSelector": map[string]any{sentry.ScopeLabel: "fixture"}, "containers": []any{map[string]any{"name": "tower", "image": "example.invalid/tower:v1"}}}}})
		setReady(ds)
		for _, field := range []string{"desiredNumberScheduled", "numberReady", "updatedNumberScheduled"} {
			Expect(unstructured.SetNestedField(ds.Object, int64(2), "status", field)).To(Succeed())
		}
		return &run{g: g, clients: map[bundle.Destination]Client{bundle.DPU: f.tenant}, selectedNodes: selected}, f.tenant, ds
	}
	pod := func(ds *unstructured.Unstructured, node, name string) *unstructured.Unstructured {
		p := testObject("v1", "Pod", ds.GetNamespace(), name, map[string]any{"nodeName": node})
		p.SetLabels(map[string]string{"app": "tower"})
		setReady(p)
		Expect(unstructured.SetNestedField(p.Object, "Running", "status", "phase")).To(Succeed())
		p.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.GetName(), UID: ds.GetUID(), Controller: ptr.To(true)}})
		return p
	}
	It("requires a Ready owned pod on each selected node despite positive rollout counters", func() {
		s, c, ds := fixture()
		c.put(pod(ds, "selected-a", "only-a"))
		ready, message, err := s.daemonSetCoverage(context.Background(), ds)
		Expect(err).NotTo(HaveOccurred())
		Expect(ready).To(BeFalse())
		Expect(message).To(ContainSubstring("selected-b"))
		c.put(pod(ds, "selected-b", "also-b"))
		ready, _, err = s.daemonSetCoverage(context.Background(), ds)
		Expect(err).NotTo(HaveOccurred())
		Expect(ready).To(BeTrue())
	})
	It("does not count a Ready pod owned by a different DaemonSet UID", func() {
		s, c, ds := fixture()
		c.put(pod(ds, "selected-a", "a"))
		foreign := pod(ds, "selected-b", "foreign")
		owners := foreign.GetOwnerReferences()
		owners[0].UID = "foreign"
		foreign.SetOwnerReferences(owners)
		c.put(foreign)
		ready, _, err := s.daemonSetCoverage(context.Background(), ds)
		Expect(err).NotTo(HaveOccurred())
		Expect(ready).To(BeFalse())
	})
	It("rejects owned pods scheduled on unselected nodes", func() {
		s, c, ds := fixture()
		c.put(pod(ds, "selected-a", "a"))
		c.put(pod(ds, "selected-b", "b"))
		c.put(pod(ds, "unselected", "extra"))
		_, _, err := s.daemonSetCoverage(context.Background(), ds)
		Expect(err).To(MatchError(ContainSubstring("DAEMONSET_UNSELECTED_POD")))
	})
	It("rejects scheduling selectors that broaden beyond the selected nodes", func() {
		s, c, ds := fixture()
		node := testObject("v1", "Node", "", "unselected", nil)
		setReady(node)
		c.put(node)
		unstructured.RemoveNestedField(ds.Object, "spec", "template", "spec", "nodeSelector")
		_, _, err := s.daemonSetCoverage(context.Background(), ds)
		Expect(err).To(MatchError(ContainSubstring("DAEMONSET_SCOPE_BROADENED")))
	})
	It("does not pass duplicate Ready owned pods on one selected node", func() {
		s, c, ds := fixture()
		c.put(pod(ds, "selected-a", "a"))
		c.put(pod(ds, "selected-a", "duplicate-a"))
		c.put(pod(ds, "selected-b", "b"))
		ready, _, err := s.daemonSetCoverage(context.Background(), ds)
		Expect(err).NotTo(HaveOccurred())
		Expect(ready).To(BeFalse())
	})
	It("honors OR terms and AND requirements in required node affinity", func() {
		p := &corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
			{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: sentry.ScopeLabel, Operator: corev1.NodeSelectorOpIn, Values: []string{"fixture"}}, {Key: "role", Operator: corev1.NodeSelectorOpIn, Values: []string{"tower"}}}},
			{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"explicit-node"}}}},
		}}}}}
		node := testObject("v1", "Node", "", "other", nil)
		node.SetLabels(map[string]string{sentry.ScopeLabel: "fixture"})
		matched, err := podMatchesNode(p, node)
		Expect(err).NotTo(HaveOccurred())
		Expect(matched).To(BeFalse())
		node.SetName("explicit-node")
		matched, err = podMatchesNode(p, node)
		Expect(err).NotTo(HaveOccurred())
		Expect(matched).To(BeTrue())
	})
})
