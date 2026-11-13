// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe("actual rc.1 and GA DPUCluster readiness contract", func() {
	runnerFor := func(f *fakeFactory, v *fakeArtifacts) Runner {
		dir, err := os.MkdirTemp("", "dpf-cluster-contract-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		dir, err = filepath.EvalSymlinks(dir)
		Expect(err).NotTo(HaveOccurred())
		return Runner{Clients: f, Artifacts: v, EvidenceDir: dir, PollInterval: time.Millisecond, Timeout: 10 * time.Second}
	}
	It("accepts source-realistic Ready clusters without observation fields and records the limitation", func() {
		b, g, f, v := lifecycleFixture()
		r := runnerFor(f, v)
		cluster := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUCluster", "dpf-system", "tenant"))]
		_, exists, _ := unstructured.NestedFieldNoCopy(cluster.Object, "status", "observedGeneration")
		Expect(exists).To(BeFalse())
		conditions, _, _ := unstructured.NestedSlice(cluster.Object, "status", "conditions")
		Expect(conditions[0].(map[string]any)).NotTo(HaveKey("observedGeneration"))
		result, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(result.Checks).To(ContainElement(And(HaveField("State", "diagnostic"), HaveField("ReasonCode", "FRESHNESS_NOT_EXPOSED"))))
		conditions[0].(map[string]any)["observedGeneration"] = int64(0)
		Expect(unstructured.SetNestedSlice(cluster.Object, conditions, "status", "conditions")).To(Succeed())
		result, err = r.Run(context.Background(), b, g, Validate)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
	})
	DescribeTable("requires both actual Ready phase and Ready condition", func(phase, status string) {
		b, g, f, v := lifecycleFixture()
		r := runnerFor(f, v)
		cluster := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUCluster", "dpf-system", "tenant"))]
		cluster.Object["status"] = map[string]any{"phase": phase, "conditions": []any{map[string]any{"type": "Ready", "status": status}}}
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("DPU_CLUSTER_NOT_READY")))
		Expect(f.management.creates).To(BeEmpty())
	}, Entry("Ready condition with Pending phase", "Pending", "True"), Entry("stale True while NotReady", "NotReady", "True"), Entry("Ready phase with false condition", "Ready", "False"), Entry("failed phase", "Failed", "True"))
	DescribeTable("rejects in-run tenant spec drift before persistent DPUSet creation", func(when string) {
		b, g, f, v := lifecycleFixture()
		r := runnerFor(f, v)
		cluster := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUCluster", "dpf-system", "tenant"))]
		change := func() {
			cluster.SetGeneration(cluster.GetGeneration() + 1)
			Expect(unstructured.SetNestedField(cluster.Object, int64(2000), "spec", "maxNodes")).To(Succeed())
		}
		if when == "cache" {
			v.afterVerify = change
		} else {
			f.management.admit = func(object *unstructured.Unstructured, dryRun bool) {
				if object.GetKind() == "DPUSet" && dryRun {
					change()
				}
			}
		}
		result, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("DPU_CLUSTER_GENERATION_DRIFT")))
		Expect(result.Outcome).To(Equal("failed"))
		Expect(v.calls).To(Equal(1))
		Expect(f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUSet", "dpf-system", "selected-set"))]).To(BeNil())
		if when == "cache" {
			Expect(f.management.creates).To(Equal([]string{"BlueFieldSoftware/software"}))
		} else {
			Expect(f.management.dryRuns).To(Equal([]bool{false, true}))
		}
	}, Entry("after cache hashing", "cache"), Entry("during admission precheck", "admission"))

	It("retains direct tenant UID verification when controller freshness is unavailable", func() {
		b, g, f, v := lifecycleFixture()
		r := runnerFor(f, v)
		tenantNamespace := f.tenant.objects[objectKey(ref("", "", "Namespace", "", "kube-system"))]
		tenantNamespace.SetUID("replacement-tenant")
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("CLUSTER_IDENTITY_MISMATCH")))
		Expect(f.management.creates).To(BeEmpty())
	})
})
