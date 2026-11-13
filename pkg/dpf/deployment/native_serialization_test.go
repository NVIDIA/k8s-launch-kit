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

// These anonymous fixtures model the pinned API's value-typed fields, independently
// of the comparator's normalization registry. Optional pointer fields stay absent.
func serializationObject(kind string) *unstructured.Unstructured {
	switch kind {
	case "DPUFlavor":
		return testObject("provisioning.dpu.nvidia.com/v1alpha1", kind, "sentry-system", "flavor", map[string]any{})
	case "DPUService":
		return testObject("svc.dpu.nvidia.com/v1alpha1", kind, "sentry-system", "service", map[string]any{"helmChart": map[string]any{"source": map[string]any{"repoURL": "oci://example.invalid/charts", "chart": "example", "version": "1.0.0"}}, "security": map[string]any{"privileged": false}})
	case "DPUServiceInterface":
		return testObject("svc.dpu.nvidia.com/v1alpha1", kind, "sentry-system", "interface", map[string]any{"template": map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"interfaceType": "physical", "physical": map[string]any{"interfaceName": "p0"}}}}}})
	case "DPUServiceChain":
		return testObject("svc.dpu.nvidia.com/v1alpha1", kind, "sentry-system", "chain", map[string]any{"template": map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"switches": []any{map[string]any{"ports": []any{map[string]any{"serviceInterface": map[string]any{"matchLabels": map[string]any{"example.invalid/port": "p0"}}}}}}}}}}})
	}
	Fail("unsupported serialization fixture: " + kind)
	return nil
}

func serializedValueFields(object *unstructured.Unstructured) {
	switch object.GetKind() {
	case "DPUFlavor":
		for _, field := range []string{"grub", "sysctl", "ovs", "containerdConfig"} {
			Expect(unstructured.SetNestedMap(object.Object, map[string]any{}, "spec", field)).To(Succeed())
		}
	case "DPUService":
		Expect(unstructured.SetNestedField(object.Object, "", "spec", "helmChart", "source", "path")).To(Succeed())
	case "DPUServiceInterface", "DPUServiceChain":
		Expect(unstructured.SetNestedMap(object.Object, map[string]any{}, "spec", "template", "metadata")).To(Succeed())
		Expect(unstructured.SetNestedMap(object.Object, map[string]any{}, "spec", "template", "spec", "template", "metadata")).To(Succeed())
	}
}

var _ = Describe("pinned native value serialization", func() {
	DescribeTable("accepts only zero serialization without changing either input", func(kind string) {
		desired := serializationObject(kind)
		actual := desired.DeepCopy()
		serializedValueFields(actual)
		beforeDesired, beforeActual := desired.DeepCopy(), actual.DeepCopy()
		for _, release := range []string{"26.8.0-rc.1", "26.8.0"} {
			Expect(nativeMatches(release, desired, actual)).To(BeTrue(), release)
			Expect(nativeMatches(release, actual, desired)).To(BeTrue(), release)
		}
		Expect(desired).To(Equal(beforeDesired))
		Expect(actual).To(Equal(beforeActual))
	}, Entry("flavor value structs", "DPUFlavor"), Entry("chart source empty path", "DPUService"), Entry("interface template metadata", "DPUServiceInterface"), Entry("chain template metadata", "DPUServiceChain"))

	DescribeTable("retains meaningful and unknown content at normalized paths", func(kind string) {
		desired := serializationObject(kind)
		actual := desired.DeepCopy()
		serializedValueFields(actual)
		var path []string
		switch kind {
		case "DPUFlavor":
			path = []string{"spec", "grub", "kernelParameters"}
			Expect(unstructured.SetNestedField(actual.Object, []any{"iommu=off"}, path...)).To(Succeed())
		case "DPUService":
			path = []string{"spec", "helmChart", "source", "path"}
			Expect(unstructured.SetNestedField(actual.Object, "different-chart", path...)).To(Succeed())
		default:
			path = []string{"spec", "template", "metadata", "labels", "example.invalid/selected"}
			Expect(unstructured.SetNestedField(actual.Object, "foreign", path...)).To(Succeed())
		}
		Expect(nativeMatches("26.8.0-rc.1", desired, actual)).To(BeFalse())
		actual = desired.DeepCopy()
		serializedValueFields(actual)
		unknownPath := []string{"spec", "template", "metadata", "unknownEffect"}
		switch kind {
		case "DPUFlavor":
			unknownPath = []string{"spec", "grub", "unknownEffect"}
		case "DPUService":
			unknownPath = []string{"spec", "helmChart", "source", "unknownEffect"}
		}
		Expect(unstructured.SetNestedMap(actual.Object, map[string]any{}, unknownPath...)).To(Succeed())
		Expect(nativeMatches("26.8.0-rc.1", desired, actual)).To(BeFalse())
	}, Entry("configured flavor", "DPUFlavor"), Entry("configured chart path", "DPUService"), Entry("interface metadata selection", "DPUServiceInterface"), Entry("chain metadata selection", "DPUServiceChain"))

	It("retains presence-gated optional pointer objects and missing required parents", func() {
		desired := serializationObject("DPUService")
		actual := desired.DeepCopy()
		Expect(unstructured.SetNestedMap(actual.Object, map[string]any{}, "spec", "security", "spiffe")).To(Succeed())
		Expect(nativeMatches("26.8.0-rc.1", desired, actual)).To(BeFalse())
		broken := serializationObject("DPUServiceInterface")
		unstructured.RemoveNestedField(broken.Object, "spec", "template")
		Expect(nativeMatches("26.8.0-rc.1", broken, broken.DeepCopy())).To(BeFalse())
	})

	It("waits and resumes after controller template metadata serialization while rejecting real drift", func() {
		b, g, f, v := lifecycleFixture()
		index, files := b.Index(), b.Files()
		index.BundleDigest = ""
		stage := bundle.Stage{ID: "service-network", Destination: bundle.Management, Namespace: "sentry-system", DependsOn: []string{"provision"}}
		for _, kind := range []string{"DPUServiceInterface", "DPUServiceChain"} {
			object := serializationObject(kind)
			object.SetUID("")
			object.SetResourceVersion("")
			object.SetGeneration(0)
			data, err := yaml.Marshal(object.Object)
			Expect(err).NotTo(HaveOccurred())
			path := "objects/" + kind + ".yaml"
			files = append(files, bundle.File{Path: path, Data: data})
			index.Files = append(index.Files, bundle.FileRecord{Path: path, Role: bundle.Manifest})
			stage.Files = append(stage.Files, path)
		}
		index.Stages = append(index.Stages, stage)
		var err error
		b, err = bundle.New(index, files)
		Expect(err).NotTo(HaveOccurred())
		f.management.onGet = func(reference bundle.ResourceRef) {
			if reference.Kind != "DPUServiceInterface" && reference.Kind != "DPUServiceChain" {
				return
			}
			if object := f.management.objects[objectKey(reference)]; object != nil {
				serializedValueFields(object)
				object.SetGeneration(2)
				setReady(object)
			}
		}
		r := dnsTestRunner(f, v)
		result, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		creates := len(f.management.creates)
		result, err = r.Run(context.Background(), b, g, Validate)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(f.management.creates).To(HaveLen(creates))
		object := f.management.objects[objectKey(ref("", "svc.dpu.nvidia.com", "DPUServiceInterface", "sentry-system", "interface"))]
		Expect(unstructured.SetNestedField(object.Object, "p1", "spec", "template", "spec", "template", "spec", "physical", "interfaceName")).To(Succeed())
		_, err = r.Run(context.Background(), b, g, Validate)
		Expect(err).To(MatchError(ContainSubstring("RESOURCE_OWNERSHIP_CONFLICT")))
	})
})
