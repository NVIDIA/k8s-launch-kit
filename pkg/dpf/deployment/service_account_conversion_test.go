// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func accountWorkload(kind string) *unstructured.Unstructured {
	return testObject("apps/v1", kind, "service-scope", "example", map[string]any{
		"selector": map[string]any{"matchLabels": map[string]any{"app": "example"}},
		"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": "example"}}, "spec": map[string]any{
			"serviceAccountName": "example-account", "automountServiceAccountToken": false,
			"containers": []any{map[string]any{"name": "server", "image": "example.invalid/server:v1"}},
		}},
	})
}

var _ = Describe("Kubernetes PodSpec service account conversion", func() {
	It("passes owned service preflight with an API alias and refuses a conflicting alias", func() {
		b, g, f, _ := lifecycleFixture()
		native := serializationObject("DPUService")
		workload := accountWorkload("DaemonSet")
		workload.SetNamespace("sentry-system")
		for _, object := range []*unstructured.Unstructured{native, workload} {
			object.SetUID("")
			object.SetResourceVersion("")
			object.SetGeneration(0)
		}
		nativeBytes, err := yaml.Marshal(native.Object)
		Expect(err).NotTo(HaveOccurred())
		projectionBytes, err := yaml.Marshal(workload.Object)
		Expect(err).NotTo(HaveOccurred())
		index, files := b.Index(), b.Files()
		index.BundleDigest = ""
		index.Files = append(index.Files, bundle.FileRecord{Path: "objects/account-service.yaml", Role: bundle.Manifest}, bundle.FileRecord{Path: "projections/account-workload.yaml", Role: bundle.Projection})
		index.Stages = append(index.Stages, bundle.Stage{ID: "account-service", Destination: bundle.Management, Namespace: "sentry-system", Files: []string{"objects/account-service.yaml"}, DependsOn: []string{"provision"}}, bundle.Stage{ID: "account-workload", Destination: bundle.DPU, Namespace: "sentry-system", ProjectionFiles: []string{"projections/account-workload.yaml"}, DependsOn: []string{"account-service"}})
		files = append(files, bundle.File{Path: "objects/account-service.yaml", Data: nativeBytes}, bundle.File{Path: "projections/account-workload.yaml", Data: projectionBytes})
		b, err = bundle.New(index, files)
		Expect(err).NotTo(HaveOccurred())
		f.management.put(mark(native, b.Index().BundleDigest))
		actual := workload.DeepCopy()
		actual.SetUID("captured-workload-uid")
		Expect(unstructured.SetNestedField(actual.Object, "example-account", "spec", "template", "spec", "serviceAccount")).To(Succeed())
		f.tenant.put(actual)
		state := &run{bundle: b, g: g, result: Result{BundleDigest: b.Index().BundleDigest}, clients: map[bundle.Destination]Client{bundle.Management: f.management, bundle.DPU: f.tenant}}
		Expect(state.serviceOwnership(context.Background(), "sentry-system")).To(Succeed())
		Expect(unstructured.SetNestedField(actual.Object, "foreign-account", "spec", "template", "spec", "serviceAccount")).To(Succeed())
		f.tenant.put(actual)
		Expect(state.serviceOwnership(context.Background(), "sentry-system")).To(MatchError(ContainSubstring("FOREIGN_SERVICE_WORKLOAD")))
	})

	DescribeTable("accepts the API-produced matching alias without changing inputs", func(kind string) {
		desired := accountWorkload(kind)
		actual := desired.DeepCopy()
		Expect(unstructured.SetNestedField(actual.Object, "example-account", "spec", "template", "spec", "serviceAccount")).To(Succeed())
		beforeDesired, beforeActual := desired.DeepCopy(), actual.DeepCopy()
		Expect(desiredMatches(desired, actual)).To(BeTrue())
		Expect(desiredMatches(actual, desired)).To(BeTrue())
		Expect(desired).To(Equal(beforeDesired))
		Expect(actual).To(Equal(beforeActual))
		// The legacy input alias resolves to the same canonical account.
		legacy := desired.DeepCopy()
		unstructured.RemoveNestedField(legacy.Object, "spec", "template", "spec", "serviceAccountName")
		Expect(unstructured.SetNestedField(legacy.Object, "example-account", "spec", "template", "spec", "serviceAccount")).To(Succeed())
		Expect(desiredMatches(legacy, actual)).To(BeTrue())
	}, Entry("DaemonSet", "DaemonSet"), Entry("Deployment", "Deployment"))

	DescribeTable("does not hide conflicting aliases, changed identity, token mount or unknown content", func(kind, mutation string) {
		desired := accountWorkload(kind)
		actual := desired.DeepCopy()
		Expect(unstructured.SetNestedField(actual.Object, "example-account", "spec", "template", "spec", "serviceAccount")).To(Succeed())
		switch mutation {
		case "conflict":
			Expect(unstructured.SetNestedField(actual.Object, "other-account", "spec", "template", "spec", "serviceAccount")).To(Succeed())
			// Even self-comparison cannot make conflicting supplied identities valid.
			Expect(desiredMatches(actual, actual.DeepCopy())).To(BeFalse())
		case "identity":
			Expect(unstructured.SetNestedField(actual.Object, "other-account", "spec", "template", "spec", "serviceAccountName")).To(Succeed())
			Expect(unstructured.SetNestedField(actual.Object, "other-account", "spec", "template", "spec", "serviceAccount")).To(Succeed())
		case "token":
			Expect(unstructured.SetNestedField(actual.Object, true, "spec", "template", "spec", "automountServiceAccountToken")).To(Succeed())
		case "unknown":
			Expect(unstructured.SetNestedMap(actual.Object, map[string]any{}, "spec", "template", "spec", "unknownIdentityControl")).To(Succeed())
		}
		Expect(desiredMatches(desired, actual)).To(BeFalse())
	}, Entry("DaemonSet conflicting alias", "DaemonSet", "conflict"), Entry("Deployment conflicting alias", "Deployment", "conflict"), Entry("DaemonSet different account", "DaemonSet", "identity"), Entry("Deployment different account", "Deployment", "identity"), Entry("DaemonSet token change", "DaemonSet", "token"), Entry("Deployment token change", "Deployment", "token"), Entry("DaemonSet unknown field", "DaemonSet", "unknown"), Entry("Deployment unknown field", "Deployment", "unknown"))
})
