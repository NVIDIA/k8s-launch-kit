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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

var _ = Describe("pinned DPUFlavor webhook typed serialization", func() {
	flavor := func() *unstructured.Unstructured {
		return testObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUFlavor", "dpf-system", "flavor", map[string]any{
			"dpuMode": "zero-trust", "grub": map[string]any{"kernelParameters": []any{"console=hvc0"}}, "sysctl": map[string]any{"parameters": []any{"net.ipv4.conf.all.arp_ignore=1"}}, "ovs": map[string]any{"rawConfigScript": "ovs-vsctl --may-exist add-br br-sfc\n"},
			"configFiles": []any{map[string]any{"path": "/etc/mellanox/mlnx-sf.conf", "raw": "", "permissions": "0644", "operation": "override"}},
		})
	}
	DescribeTable("accepts the observed empty value-object addition without changing content", func(release string) {
		desired := flavor()
		actual := desired.DeepCopy()
		// This is the exact spec delta in the saved server-side dry-run response.
		Expect(unstructured.SetNestedMap(actual.Object, map[string]any{}, "spec", "containerdConfig")).To(Succeed())
		Expect(nativeMatches(release, desired, actual)).To(BeTrue())
		for _, field := range []string{"grub", "sysctl", "ovs", "containerdConfig"} {
			omitted := flavor()
			unstructured.RemoveNestedField(omitted.Object, "spec", field)
			zero := omitted.DeepCopy()
			Expect(unstructured.SetNestedMap(zero.Object, map[string]any{}, "spec", field)).To(Succeed())
			Expect(nativeMatches(release, omitted, zero)).To(BeTrue(), field)
		}
	}, Entry("installed rc.1", "26.8.0-rc.1"), Entry("GA equivalent types", "26.8.0"))
	DescribeTable("rejects additional meaningful or unknown admitted fields", func(field string, value any) {
		desired := flavor()
		actual := desired.DeepCopy()
		Expect(unstructured.SetNestedMap(actual.Object, map[string]any{field: value}, "spec", "containerdConfig")).To(Succeed())
		Expect(nativeMatches("26.8.0-rc.1", desired, actual)).To(BeFalse())
	}, Entry("different containerd endpoint", "registryEndpoint", "https://unreviewed.invalid"), Entry("unknown containerd configuration", "unreviewed", true))
	It("retains exact script, configuration file and unknown top-level spec comparisons", func() {
		desired := flavor()
		for _, change := range []func(*unstructured.Unstructured){
			func(o *unstructured.Unstructured) {
				Expect(unstructured.SetNestedField(o.Object, "changed\n", "spec", "ovs", "rawConfigScript")).To(Succeed())
			},
			func(o *unstructured.Unstructured) {
				files, _, _ := unstructured.NestedSlice(o.Object, "spec", "configFiles")
				files[0].(map[string]any)["raw"] = "unreviewed content"
				Expect(unstructured.SetNestedSlice(o.Object, files, "spec", "configFiles")).To(Succeed())
			},
			func(o *unstructured.Unstructured) {
				Expect(unstructured.SetNestedMap(o.Object, map[string]any{}, "spec", "unreviewed")).To(Succeed())
			},
		} {
			actual := desired.DeepCopy()
			change(actual)
			Expect(nativeMatches("26.8.0-rc.1", desired, actual)).To(BeFalse())
		}
	})
	It("admits and resumes a native flavor stage with the captured serialization delta", func() {
		b, g, f, v := lifecycleFixture()
		desired := flavor()
		desired.SetGeneration(0)
		desired.SetUID("")
		desired.SetResourceVersion("")
		data, err := yaml.Marshal(desired.Object)
		Expect(err).NotTo(HaveOccurred())
		index := b.Index()
		index.BundleDigest = ""
		index.Files = append(index.Files, bundle.FileRecord{Path: "objects/flavor.yaml", Role: bundle.Manifest})
		index.Stages[0].Files = append(index.Stages[0].Files, "objects/flavor.yaml")
		files := append(b.Files(), bundle.File{Path: "objects/flavor.yaml", Data: data})
		set := resourcesInBundle(b, "DPUSet")[0].ObjectCopy()
		Expect(unstructured.SetNestedField(set.Object, "flavor", "spec", "dpuTemplate", "spec", "dpuFlavor")).To(Succeed())
		setData, err := yaml.Marshal(set.Object)
		Expect(err).NotTo(HaveOccurred())
		for i := range files {
			if files[i].Path == "objects/1.yaml" {
				files[i].Data = setData
			}
		}
		for i := range index.Files {
			if index.Files[i].Path == "objects/1.yaml" {
				index.Files[i].SHA256 = ""
			}
		}
		b, err = bundle.New(index, files)
		Expect(err).NotTo(HaveOccurred())
		f.management.admit = func(o *unstructured.Unstructured, _ bool) {
			if o.GetKind() == "DPUFlavor" {
				Expect(unstructured.SetNestedMap(o.Object, map[string]any{}, "spec", "containerdConfig")).To(Succeed())
			}
		}
		dir, err := os.MkdirTemp("", "dpf-flavor-admission-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		dir, err = filepath.EvalSymlinks(dir)
		Expect(err).NotTo(HaveOccurred())
		r := Runner{Clients: f, Artifacts: v, EvidenceDir: dir, PollInterval: time.Millisecond, Timeout: 10 * time.Second}
		result, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		created := len(f.management.creates)
		result, err = r.Run(context.Background(), b, g, Validate)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(f.management.creates).To(HaveLen(created))
	})
})
