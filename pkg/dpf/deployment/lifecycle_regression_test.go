// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var _ = Describe("lifecycle safety regressions", func() {
	newRunner := func(factory *fakeFactory, verifier *fakeArtifacts) Runner {
		dir, err := os.MkdirTemp("", "dpf-regression-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		dir, err = filepath.EvalSymlinks(dir)
		Expect(err).NotTo(HaveOccurred())
		return Runner{Clients: factory, Artifacts: verifier, EvidenceDir: dir, PollInterval: time.Millisecond, Timeout: 10 * time.Second}
	}
	DescribeTable("rejects reboot methods outside explicit external:{} before writes", func(method map[string]any) {
		b, g, f, v := lifecycleFixture()
		r := newRunner(f, v)
		node := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUNode", "dpf-system", "node-a"))]
		if method == nil {
			unstructured.RemoveNestedField(node.Object, "spec", "nodeRebootMethod")
		} else {
			Expect(unstructured.SetNestedMap(node.Object, method, "spec", "nodeRebootMethod")).To(Succeed())
		}
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("NODE_REBOOT_METHOD_UNSUPPORTED")))
		Expect(f.management.creates).To(BeEmpty())
	}, Entry("default host-agent", nil), Entry("arbitrary script", map[string]any{"script": map[string]any{"name": "unreviewed"}}), Entry("gNOI", map[string]any{"gNOI": map[string]any{}}), Entry("hostless none", map[string]any{"none": map[string]any{}}), Entry("mixed methods", map[string]any{"external": map[string]any{}, "hostAgent": map[string]any{}}), Entry("unexpected external fields", map[string]any{"external": map[string]any{"reboot": "now"}}))
	DescribeTable("rejects a pending prior node transition before writes", func(marker string) {
		b, g, f, v := lifecycleFixture()
		r := newRunner(f, v)
		node := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUNode", "dpf-system", "node-a"))]
		switch marker {
		case "flag":
			Expect(unstructured.SetNestedField(node.Object, true, "status", "rebootInProgress")).To(Succeed())
		case "annotation":
			node.SetAnnotations(map[string]string{"provisioning.dpu.nvidia.com/dpunode-external-reboot-required": ""})
		default:
			conditions, _, _ := unstructured.NestedSlice(node.Object, "status", "conditions")
			conditions = append(conditions, map[string]any{"type": marker, "status": "True"})
			Expect(unstructured.SetNestedSlice(node.Object, conditions, "status", "conditions")).To(Succeed())
		}
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("NODE_TRANSITION_PENDING")))
		Expect(f.management.creates).To(BeEmpty())
	}, Entry("reboot flag", "flag"), Entry("external annotation even empty", "annotation"), Entry("active reboot", "DPUNodeRebootInProgress"), Entry("active DPU update", "DPUUpdateInProgress"), Entry("active node effect", "DPUNodeNodeEffectInProgress"))
	It("accepts a completed reset method without an active flag", func() {
		b, g, f, v := lifecycleFixture()
		r := newRunner(f, v)
		node := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUNode", "dpf-system", "node-a"))]
		Expect(unstructured.SetNestedField(node.Object, "SystemLevelReset", "status", "rebootMethod")).To(Succeed())
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
	})
	It("returns an incomplete non-success result for deferred server-side admission", func() {
		b, g, f, v := lifecycleFixture()
		r := newRunner(f, v)
		f.management.getErrors = map[string]error{"BlueFieldSoftware": &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "provisioning.dpu.nvidia.com", Kind: "BlueFieldSoftware"}}}
		result, err := r.Run(context.Background(), b, g, DryRun)
		Expect(err).To(MatchError(ContainSubstring("DRY_RUN_INCOMPLETE")))
		Expect(result.Outcome).To(Equal("incomplete"))
		Expect(result.Checks).To(ContainElement(HaveField("ReasonCode", "API_NOT_SERVED")))
		Expect(f.management.dryRuns).To(Equal([]bool{true}))
		Expect(v.calls).To(BeZero())
	})
	It("rejects retained object annotations when physical spec fields were added", func() {
		b, g, f, v := lifecycleFixture()
		r := newRunner(f, v)
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		set := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUSet", "dpf-system", "selected-set"))]
		Expect(unstructured.SetNestedField(set.Object, true, "spec", "dpuTemplate", "spec", "secureBoot")).To(Succeed())
		before := len(f.management.creates)
		_, err = r.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("RESOURCE_OWNERSHIP_CONFLICT")))
		Expect(f.management.creates).To(HaveLen(before))
	})
	It("rejects effectful DPUSet admission changes before persistent creation", func() {
		b, g, f, v := lifecycleFixture()
		r := newRunner(f, v)
		f.management.admit = func(o *unstructured.Unstructured, dryRun bool) {
			if o.GetKind() == "DPUSet" {
				Expect(dryRun).To(BeTrue())
				Expect(unstructured.SetNestedField(o.Object, true, "spec", "dpuTemplate", "spec", "secureBoot")).To(Succeed())
			}
		}
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("ADMISSION_CHANGED_DESIRED_STATE")))
		Expect(f.management.dryRuns).To(Equal([]bool{false, true}))
		Expect(f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUSet", "dpf-system", "selected-set"))]).To(BeNil())
	})

	It("does not pass selected Ready nodes with missing scope labels", func() {
		b, g, f, v := lifecycleFixture()
		r := newRunner(f, v)
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		f.tenant.objects[objectKey(ref("", "", "Node", "", "dpu-child"))].SetLabels(nil)
		result, err := r.Run(context.Background(), b, g, Validate)
		Expect(err).To(MatchError(ContainSubstring("SELECTED_DPUS_NOT_READY")))
		Expect(result.Outcome).To(Equal("failed"))
	})
	It("rejects an unselected tenant Node carrying the instance scope label", func() {
		b, g, f, v := lifecycleFixture()
		r := newRunner(f, v)
		sibling := testObject("v1", "Node", "", "unselected", nil)
		setReady(sibling)
		sibling.SetLabels(map[string]string{"nvidia.kubernetes-launch-kit.dpf-instance": "fixture"})
		f.tenant.put(sibling)
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("UNSELECTED_TENANT_NODE_MATCH")))
	})
	It("retains actual DPU and external reboot diagnostics while waiting", func() {
		b, g, f, v := lifecycleFixture()
		r := newRunner(f, v)
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		set := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUSet", "dpf-system", "selected-set"))]
		Expect(unstructured.SetNestedSlice(set.Object, []any{map[string]any{"type": "Ready", "status": "False", "reason": "Pending", "observedGeneration": set.GetGeneration()}}, "status", "conditions")).To(Succeed())
		child := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPU", "dpf-system", "dpu-child"))]
		Expect(unstructured.SetNestedField(child.Object, "DPURebooting", "status", "phase")).To(Succeed())
		node := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUNode", "dpf-system", "node-a"))]
		// Simulate a transition beginning after preflight rather than an old pending operation.
		reads := 0
		f.management.onGet = func(r bundle.ResourceRef) {
			if r.Kind == "DPUNode" {
				reads++
				if reads > 1 {
					node.SetAnnotations(map[string]string{"provisioning.dpu.nvidia.com/dpunode-external-reboot-required": "SystemLevelReset"})
				}
			}
		}
		_, err = r.Run(context.Background(), b, g, Validate)
		Expect(err).To(MatchError(And(ContainSubstring("DPURebooting"), ContainSubstring("SystemLevelReset"))))
	})
})

var _ = Describe("DPF generation freshness", func() {
	DescribeTable("requires exact top-level observed generations", func(kind string) {
		o := testObject("provisioning.dpu.nvidia.com/v1alpha1", kind, "dpf-system", "test", nil)
		o.SetGeneration(2)
		setReady(o)
		for _, observed := range []int64{0, 1, 3} {
			Expect(unstructured.SetNestedField(o.Object, observed, "status", "observedGeneration")).To(Succeed())
			ready, _, _ := Readiness(o)
			Expect(ready).To(BeFalse(), "observed=%d", observed)
		}
		unstructured.RemoveNestedField(o.Object, "status", "observedGeneration")
		ready, _, _ := Readiness(o)
		Expect(ready).To(BeFalse())
		Expect(unstructured.SetNestedField(o.Object, int64(2), "status", "observedGeneration")).To(Succeed())
		ready, _, _ = Readiness(o)
		Expect(ready).To(BeTrue())
	}, Entry("software", "BlueFieldSoftware"), Entry("DPU", "DPU"), Entry("set", "DPUSet"), Entry("service", "DPUService"), Entry("interface", "DPUServiceInterface"), Entry("chain", "DPUServiceChain"))
	DescribeTable("uses Ready condition freshness for APIs without top-level observation", func(kind string) {
		o := testObject("svc.dpu.nvidia.com/v1alpha1", kind, "dpf-system", "test", nil)
		o.SetGeneration(2)
		setReady(o)
		unstructured.RemoveNestedField(o.Object, "status", "observedGeneration")
		ready, _, _ := Readiness(o)
		Expect(ready).To(BeTrue())
		for _, observed := range []int64{0, 1, 3} {
			o.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": observed}}}
			ready, _, _ = Readiness(o)
			Expect(ready).To(BeFalse())
		}
		o.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
		ready, _, _ = Readiness(o)
		Expect(ready).To(BeFalse())
	}, Entry("NAD", "DPUServiceNAD"), Entry("credentials", "DPUServiceCredentialRequest"))
})
