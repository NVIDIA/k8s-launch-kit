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

var _ = Describe("mutually dependent native service creation stage", func() {
	It("creates Tower, controller and Argus before waiting for Tower's controller-dependent readiness", func() {
		b, g, f, v := lifecycleFixture()
		index, files := b.Index(), b.Files()
		index.BundleDigest = ""
		stage := bundle.Stage{ID: "services", Destination: bundle.Management, Namespace: "sentry-system", DependsOn: []string{"provision"}}
		for _, name := range []string{"tower", "controller", "argus"} {
			service := serializationObject("DPUService")
			service.SetName(name)
			service.SetUID("")
			service.SetResourceVersion("")
			service.SetGeneration(0)
			data, err := yaml.Marshal(service.Object)
			Expect(err).NotTo(HaveOccurred())
			path := "objects/" + name + ".yaml"
			files = append(files, bundle.File{Path: path, Data: data})
			index.Files = append(index.Files, bundle.FileRecord{Path: path, Role: bundle.Manifest})
			stage.Files = append(stage.Files, path)
		}
		index.Stages = append(index.Stages, stage)
		var err error
		b, err = bundle.New(index, files)
		Expect(err).NotTo(HaveOccurred())
		serviceKey := func(name string) string {
			return objectKey(ref("", "svc.dpu.nvidia.com", "DPUService", "sentry-system", name))
		}
		towerBlocked := false
		f.management.afterCreate = func(created *unstructured.Unstructured) {
			if created.GetKind() != "DPUService" || created.GetName() != "tower" {
				return
			}
			tower := f.management.objects[serviceKey("tower")]
			Expect(unstructured.SetNestedSlice(tower.Object, []any{map[string]any{"type": "Ready", "status": "False"}}, "status", "conditions")).To(Succeed())
			ready, _, _ := Readiness(tower)
			Expect(ready).To(BeFalse())
			Expect(f.management.objects[serviceKey("controller")]).To(BeNil())
			towerBlocked = true
		}
		readyObservations := 0
		f.management.onGet = func(reference bundle.ResourceRef) {
			if reference.Kind != "DPUService" || reference.Name != "tower" {
				return
			}
			if tower := f.management.objects[serviceKey("tower")]; tower != nil {
				// A separate Tower wait stage would reach here before the
				// controller exists and could never satisfy its own readiness.
				Expect(f.management.objects[serviceKey("controller")]).NotTo(BeNil())
				Expect(f.management.objects[serviceKey("argus")]).NotTo(BeNil())
				setReady(tower)
				readyObservations++
			}
		}
		r := dnsTestRunner(f, v)
		result, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(towerBlocked).To(BeTrue())
		Expect(readyObservations).To(BeNumerically(">", 0))
		Expect(f.management.creates).To(ContainElements("DPUService/tower", "DPUService/controller", "DPUService/argus"))
		creates := len(f.management.creates)
		result, err = r.Run(context.Background(), b, g, Validate)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(f.management.creates).To(HaveLen(creates))
	})
})
