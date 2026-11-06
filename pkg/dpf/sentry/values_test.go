// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package sentry

import (
	"encoding/json"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yaml "sigs.k8s.io/yaml"
)

var _ = Describe("exact-release DPF effective Helm inputs", func() {
	It("projects the real Tower chart with the controller sentinel, JSON shape, ordering and resources for both releases", func() {
		for _, release := range []string{"26.8.0-rc.1", "26.8.0"} {
			g := generationFixture()
			g.Release = release
			r, err := Render(g, profileFixture(), archiveFixture())
			Expect(err).NotTo(HaveOccurred())
			expected := `[{"name":"invalid-network","namespace":"invalid-namespace","interface":"invalid-interface","cni-args":null},{"name":"site-a-egress","namespace":"site-a-services","interface":"net2","cni-args":null},{"name":"site-a-ingress","namespace":"site-a-services","interface":"net1","cni-args":null}]`
			for _, tower := range objectsOf(r, bundle.Projection, "DaemonSet") {
				if tower.GetName() == "site-a-tower" {
					Expect(tower.GetAnnotations()).To(HaveKeyWithValue(networksAnnotation, expected))
					annotations, _, _ := unstructured.NestedStringMap(tower.Object, "spec", "template", "metadata", "annotations")
					Expect(annotations).To(HaveKeyWithValue(networksAnnotation, expected))
					labels, _, _ := unstructured.NestedStringMap(tower.Object, "spec", "template", "metadata", "labels")
					Expect(labels).To(HaveKeyWithValue(serviceIDLabel, "site-a-tower"))
				}
			}
			for _, svc := range objectsOf(r, bundle.Manifest, "DPUService") {
				_, hasDuplicate, _ := unstructured.NestedFieldNoCopy(svc.Object, "spec", "helmChart", "values", "serviceDaemonSet")
				Expect(hasDuplicate).To(BeFalse())
				if svc.GetName() == "site-a-tower" {
					raw, _, _ := unstructured.NestedString(svc.Object, "spec", "serviceDaemonSet", "annotations", networksAnnotation)
					Expect(raw).NotTo(ContainSubstring("invalid-network"))
					Expect(strings.Index(raw, "site-a-ingress")).To(BeNumerically("<", strings.Index(raw, "site-a-egress")))
				}
			}
			var values map[string]interface{}
			for _, f := range r.Files {
				if f.Path == "values/site-a-tower.yaml" {
					Expect(yaml.Unmarshal(f.Data, &values)).To(Succeed())
				}
			}
			annotation, _, _ := unstructured.NestedString(values, "serviceDaemonSet", "annotations", networksAnnotation)
			Expect(annotation).To(Equal(expected))
			trusted, _, _ := unstructured.NestedString(values, "serviceDaemonSet", "resources", "nvidia.com/bf_sf_trusted")
			Expect(trusted).To(Equal("2"))
			cpu, _, _ := unstructured.NestedString(values, "serviceDaemonSet", "resources", "cpu")
			Expect(cpu).To(Equal("1"))
			for _, server := range objectsOf(r, bundle.Projection, "Deployment") {
				if server.GetName() == "site-a-policy-server" {
					labels, _, _ := unstructured.NestedStringMap(server.Object, "spec", "template", "metadata", "labels")
					Expect(labels).To(HaveKeyWithValue(serviceIDLabel, "site-a-policy-server"))
				}
			}
			for _, controller := range objectsOf(r, bundle.Projection, "DaemonSet") {
				if controller.GetName() == "site-a-controller" {
					annotations, _, _ := unstructured.NestedStringMap(controller.Object, "spec", "template", "metadata", "annotations")
					Expect(annotations).NotTo(HaveKey(networksAnnotation))
				}
			}
		}
	})
	It("derives resource counts from NADs, gives generated values precedence, and leaves all inputs independent", func() {
		g := generationFixture()
		r, err := Render(g, profileFixture(), archiveFixture())
		Expect(err).NotTo(HaveOccurred())
		inputs, tower := projectionFixture(r)
		Expect(unstructured.SetNestedField(tower.Object, "99", "spec", "serviceDaemonSet", "resources", "nvidia.com/bf_sf_trusted")).To(Succeed())
		lower := map[string]interface{}{"serviceDaemonSet": map[string]interface{}{"labels": map[string]interface{}{serviceIDLabel: "incorrect-lower-priority-id", "extra.example/label": "retained"}, "resources": map[string]interface{}{"nvidia.com/bf_sf_trusted": "77"}, "annotations": map[string]interface{}{networksAnnotation: "incorrect-lower-priority-networks"}}, "customValue": map[string]interface{}{"keep": "unchanged"}}
		Expect(unstructured.SetNestedMap(tower.Object, lower, "spec", "helmChart", "values")).To(Succeed())
		before, _ := json.Marshal(tower.Object)
		values, err := effectiveHelmValues("26.8.0-rc.1", tower, inputs)
		Expect(err).NotTo(HaveOccurred())
		trusted, _, _ := unstructured.NestedString(values, "serviceDaemonSet", "resources", "nvidia.com/bf_sf_trusted")
		Expect(trusted).To(Equal("2"))
		label, _, _ := unstructured.NestedString(values, "serviceDaemonSet", "labels", serviceIDLabel)
		Expect(label).To(Equal("site-a-tower"))
		kept, _, _ := unstructured.NestedString(values, "serviceDaemonSet", "labels", "extra.example/label")
		Expect(kept).To(Equal("retained"))
		after, _ := json.Marshal(tower.Object)
		Expect(after).To(Equal(before))
		Expect(unstructured.SetNestedField(values, "changed-result", "customValue", "keep")).To(Succeed())
		after, _ = json.Marshal(tower.Object)
		Expect(after).To(Equal(before))
		// Changing one declared NAD changes the controller-injected resource,
		// rather than trusting the now-stale request in the native DPUService.
		for key, nad := range inputs.nads {
			if nad.GetName() == "site-a-egress" {
				copy := nad.DeepCopy()
				Expect(unstructured.SetNestedField(copy.Object, "vf", "spec", "resourceType")).To(Succeed())
				inputs.nads[key] = copy
			}
		}
		values, err = effectiveHelmValues("26.8.0", tower, inputs)
		Expect(err).NotTo(HaveOccurred())
		trusted, _, _ = unstructured.NestedString(values, "serviceDaemonSet", "resources", "nvidia.com/bf_sf_trusted")
		Expect(trusted).To(Equal("1"))
		vf, _, _ := unstructured.NestedString(values, "serviceDaemonSet", "resources", "nvidia.com/bf_vf")
		Expect(vf).To(Equal("1"))
	})
	It("rejects missing dependencies and unsupported transformation branches instead of inventing values", func() {
		g := generationFixture()
		r, err := Render(g, profileFixture(), archiveFixture())
		Expect(err).NotTo(HaveOccurred())
		inputs, tower := projectionFixture(r)
		delete(inputs.nads, *g.Sentry.Namespace+"/site-a-egress")
		_, err = effectiveHelmValues(g.Release, tower, inputs)
		Expect(err).To(MatchError(ContainSubstring("undeclared NAD")))
		inputs, tower = projectionFixture(r)
		Expect(unstructured.SetNestedStringSlice(tower.Object, []string{"missing-interface"}, "spec", "interfaces")).To(Succeed())
		_, err = effectiveHelmValues(g.Release, tower, inputs)
		Expect(err).To(MatchError(ContainSubstring("undeclared interface")))
		_, err = effectiveHelmValues("26.11.0", tower, inputs)
		Expect(err).To(MatchError(ContainSubstring("no effective Helm-input contract")))
		inputs, tower = projectionFixture(r)
		tower.SetLabels(map[string]string{"dpu.nvidia.com/component": "system-component"})
		_, err = effectiveHelmValues(g.Release, tower, inputs)
		Expect(err).To(MatchError(ContainSubstring("system-component")))
		inputs, tower = projectionFixture(r)
		Expect(unstructured.SetNestedMap(tower.Object, map[string]interface{}{}, "spec", "configPorts")).To(Succeed())
		_, err = effectiveHelmValues(g.Release, tower, inputs)
		Expect(err).To(MatchError(ContainSubstring("config-port")))
	})
	It("merges explicit existing network fields then sorts exactly, while refusing unsupported annotation data", func() {
		generated := func() map[string]networkSelection {
			return map[string]networkSelection{"net1": {Name: "generated", Interface: "net1"}, "invalid-interface": {Name: "invalid-network", Namespace: "invalid-namespace", Interface: "invalid-interface"}}
		}
		actual, err := mergeNetworks(generated(), `[{"name":"custom","namespace":"explicit","interface":"net1","cni-args":{"mtu":"1500"}}]`)
		Expect(err).NotTo(HaveOccurred())
		Expect(actual).To(Equal(`[{"name":"custom","namespace":"explicit","interface":"net1","cni-args":{"mtu":"1500"}},{"name":"invalid-network","namespace":"invalid-namespace","interface":"invalid-interface","cni-args":null}]`))
		for _, raw := range []string{`[{"name":"custom","interface":"net1","ips":["192.0.2.1"]}]`, `[] []`, `not-json`} {
			_, err = mergeNetworks(generated(), raw)
			Expect(err).To(HaveOccurred())
		}
	})
})

var _ = Describe("pinned DPF schema default copies", func() {
	It("defaults an independent copy for both releases and never prunes additional fields", func() {
		for _, release := range []string{"26.8.0-rc.1", "26.8.0"} {
			input := object("svc.dpu.nvidia.com/v1alpha1", "DPUServiceNAD", "services", "network", map[string]interface{}{"resourceType": "sf", "additionalDesiredField": "must-remain-visible"})
			copy, err := DefaultedCopy(release, input)
			Expect(err).NotTo(HaveOccurred())
			mtu, _, _ := unstructured.NestedFieldNoCopy(copy.Object, "spec", "serviceMTU")
			Expect(mtu).To(BeNumerically("==", 1500))
			_, originalHasMTU, _ := unstructured.NestedFieldNoCopy(input.Object, "spec", "serviceMTU")
			Expect(originalHasMTU).To(BeFalse())
			extra, _, _ := unstructured.NestedString(copy.Object, "spec", "additionalDesiredField")
			Expect(extra).To(Equal("must-remain-visible"))
			Expect(unstructured.SetNestedField(copy.Object, "changed", "spec", "additionalDesiredField")).To(Succeed())
			extra, _, _ = unstructured.NestedString(input.Object, "spec", "additionalDesiredField")
			Expect(extra).To(Equal("must-remain-visible"))
		}
		_, err := DefaultedCopy("26.8.0", nil)
		Expect(err).To(HaveOccurred())
		_, err = DefaultedCopy("26.8.0", object("v1", "ConfigMap", "services", "unsupported", nil))
		Expect(err).To(HaveOccurred())
	})
})

func projectionFixture(r Rendered) (projectionInputs, *unstructured.Unstructured) {
	inputs := projectionInputs{interfaces: map[string]*unstructured.Unstructured{}, nads: map[string]*unstructured.Unstructured{}}
	for _, kind := range []string{"DPUServiceInterface", "DPUServiceNAD"} {
		for _, obj := range objectsOf(r, bundle.Manifest, kind) {
			Expect(inputs.record(obj)).To(Succeed())
		}
	}
	for _, obj := range objectsOf(r, bundle.Manifest, "DPUService") {
		if obj.GetName() == "site-a-tower" {
			return inputs, obj
		}
	}
	Fail("Tower fixture is absent")
	return inputs, nil
}
