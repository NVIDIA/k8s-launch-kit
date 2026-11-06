// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"encoding/json"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/sentry"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe("closed desired state comparison", func() {
	It("accepts the API-owned metadata and status of native resources", func() {
		b, g, _, _ := lifecycleFixture()
		for _, kind := range []string{"BlueFieldSoftware", "DPUSet"} {
			doc := resourcesInBundle(b, kind)[0]
			desired := doc.ObjectCopy()
			actual := mark(desired, b.Index().BundleDigest)
			actual.SetUID("new-uid")
			actual.SetGeneration(1)
			actual.SetResourceVersion("2")
			setReady(actual)
			a, err := sentry.DefaultedCopy(g.Release, desired)
			Expect(err).NotTo(HaveOccurred())
			c, err := sentry.DefaultedCopy(g.Release, actual)
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Object["spec"]).To(Equal(c.Object["spec"]))
			delete(a.Object, "spec")
			delete(a.Object, "status")
			aa, _ := json.Marshal(a.Object)
			cc, _ := json.Marshal(c.Object)
			Expect(contains(c.Object, a.Object)).To(BeTrue(), "want=%s actual=%s", aa, cc)
			Expect(nativeMatches(g.Release, desired, actual)).To(BeTrue())
		}
	})
	It("accepts pinned CRD defaults while rejecting additional physical spec fields", func() {
		b, g, _, _ := lifecycleFixture()
		desired := resourcesInBundle(b, "DPUSet")[0].ObjectCopy()
		actual, err := sentry.DefaultedCopy(g.Release, mark(desired, b.Index().BundleDigest))
		Expect(err).NotTo(HaveOccurred())
		Expect(owns(actual, desired, b.Index().BundleDigest, g.Release)).To(BeTrue())
		_ = unstructured.SetNestedField(actual.Object, true, "spec", "dpuTemplate", "spec", "secureBoot")
		Expect(owns(actual, desired, b.Index().BundleDigest, g.Release)).To(BeFalse())
	})
	It("compares nested ordered arrays without allowing changed or added members", func() {
		want := map[string]any{"items": []any{map[string]any{"name": "first", "value": "keep"}}}
		Expect(contains(map[string]any{"items": []any{map[string]any{"name": "first", "value": "keep", "apiDefault": "value"}}}, want)).To(BeTrue())
		Expect(contains(map[string]any{"items": []any{map[string]any{"name": "first", "value": "changed"}}}, want)).To(BeFalse())
		Expect(contains(map[string]any{"items": []any{map[string]any{"name": "first", "value": "keep"}, map[string]any{"name": "extra"}}}, want)).To(BeFalse())
	})
	It("normalizes real nested workload defaults while rejecting extra security fields", func() {
		desired := testObject("apps/v1", "DaemonSet", "sentry-system", "tower", map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "tower"}}, "template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": "tower"}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "server", "image": "example.invalid/server:v1", "ports": []any{map[string]any{"containerPort": int64(8080)}}}}}}})

		// API-server defaults captured from the Kubernetes v1.35 apps/core
		// defaulting contract; do not construct these with the tested normalizer.
		actual := desired.DeepCopy()
		Expect(unstructured.SetNestedMap(actual.Object, map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxUnavailable": int64(1), "maxSurge": int64(0)}}, "spec", "updateStrategy")).To(Succeed())
		Expect(unstructured.SetNestedField(actual.Object, int64(10), "spec", "revisionHistoryLimit")).To(Succeed())
		Expect(unstructured.SetNestedField(actual.Object, "ClusterFirst", "spec", "template", "spec", "dnsPolicy")).To(Succeed())
		Expect(unstructured.SetNestedField(actual.Object, "Always", "spec", "template", "spec", "restartPolicy")).To(Succeed())
		Expect(unstructured.SetNestedField(actual.Object, "default-scheduler", "spec", "template", "spec", "schedulerName")).To(Succeed())
		Expect(unstructured.SetNestedField(actual.Object, int64(30), "spec", "template", "spec", "terminationGracePeriodSeconds")).To(Succeed())
		Expect(unstructured.SetNestedMap(actual.Object, map[string]any{}, "spec", "template", "spec", "securityContext")).To(Succeed())
		expectedContainers, _, _ := unstructured.NestedSlice(actual.Object, "spec", "template", "spec", "containers")
		container := expectedContainers[0].(map[string]any)
		container["imagePullPolicy"] = "IfNotPresent"
		container["terminationMessagePath"] = "/dev/termination-log"
		container["terminationMessagePolicy"] = "File"
		container["resources"] = map[string]any{}
		container["ports"].([]any)[0].(map[string]any)["protocol"] = "TCP"
		Expect(unstructured.SetNestedSlice(actual.Object, expectedContainers, "spec", "template", "spec", "containers")).To(Succeed())
		Expect(desiredMatches(desired, actual)).To(BeTrue())
		containers, _, _ := unstructured.NestedSlice(actual.Object, "spec", "template", "spec", "containers")
		containers[0].(map[string]any)["securityContext"] = map[string]any{"privileged": true}
		Expect(unstructured.SetNestedSlice(actual.Object, containers, "spec", "template", "spec", "containers")).To(Succeed())
		Expect(desiredMatches(desired, actual)).To(BeFalse())
		containers = append(containers, map[string]any{"name": "extra", "image": "example.invalid/extra:v1"})
		Expect(unstructured.SetNestedSlice(actual.Object, containers, "spec", "template", "spec", "containers")).To(Succeed())
		Expect(desiredMatches(desired, actual)).To(BeFalse())
	})
})
