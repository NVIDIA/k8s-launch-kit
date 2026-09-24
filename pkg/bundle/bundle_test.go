// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package bundle_test

import (
	"errors"
	"testing/fstest"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	sigsyaml "sigs.k8s.io/yaml"
)

var _ = ginkgo.Describe("artifact snapshots", func() {
	const policy = "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: policy\nspec:\n  note: original\n"
	const example = "apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: example\n  namespace: test\n"

	ginkgo.It("loads files and memory with identical order, roles, values, and raw bytes", func() {
		files := []bundle.File{{Name: "values.yaml", Content: "nested:\n  enabled: true\n  count: 3\n"}, {Name: "40-example.yaml", Content: example}, {Name: "10-policy.yaml", Content: policy}}
		memory, err := bundle.FromFiles(files)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		disk, err := bundle.Load(fstest.MapFS{
			"values.yaml":     &fstest.MapFile{Data: []byte(files[0].Content)},
			"40-example.yaml": &fstest.MapFile{Data: []byte(files[1].Content)},
			"10-policy.yaml":  &fstest.MapFile{Data: []byte(files[2].Content)},
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(memory.Files()).To(gomega.Equal(disk.Files()))
		gomega.Expect(memory.Files()[0].Name).To(gomega.Equal("10-policy.yaml"))
		gomega.Expect(memory.Documents()[0].Role()).To(gomega.Equal(bundle.Deployment))
		gomega.Expect(memory.Documents()[1].Role()).To(gomega.Equal(bundle.Validation))
		gomega.Expect(memory.Documents()[0].Ref()).To(gomega.Equal(disk.Documents()[0].Ref()))
		gomega.Expect(files[0].Name).To(gomega.Equal("values.yaml"))
		copyValues, present := memory.ValuesCopy()
		gomega.Expect(present).To(gomega.BeTrue())
		copyValues["nested"].(map[string]any)["enabled"] = false
		fresh, _ := memory.ValuesCopy()
		gomega.Expect(fresh["nested"].(map[string]any)["enabled"]).To(gomega.BeTrue())
		object := memory.Documents()[0].ObjectCopy()
		object.Object["spec"].(map[string]any)["note"] = "changed"
		gomega.Expect(memory.Documents()[0].ObjectCopy().Object["spec"].(map[string]any)["note"]).To(gomega.Equal("original"))
	})

	ginkgo.It("rejects a malformed last document without a partial bundle", func() {
		parsed, err := bundle.FromFiles([]bundle.File{{Name: "20-policy.yaml", Content: policy + "---\napiVersion: [invalid\n"}})
		gomega.Expect(parsed).To(gomega.BeNil())
		gomega.Expect(err).To(gomega.HaveOccurred())
		var sourceError *bundle.Error
		gomega.Expect(errors.As(err, &sourceError)).To(gomega.BeTrue())
		gomega.Expect(sourceError.Source).To(gomega.Equal(bundle.Source{File: "20-policy.yaml", Document: 2}))
	})

	ginkgo.It("rejects reserved aliases and path traversal", func() {
		for _, name := range []string{"values.yml", "VALUES.YAML", "../policy.yaml", "nested/policy.yaml"} {
			_, err := bundle.FromFiles([]bundle.File{{Name: name, Content: policy}})
			gomega.Expect(err).To(gomega.HaveOccurred(), name)
		}
	})

	ginkgo.It("rejects duplicate declared identities across served versions", func() {
		other := "apiVersion: mellanox.com/v1beta1\nkind: NicClusterPolicy\nmetadata:\n  name: policy\n"
		_, err := bundle.FromFiles([]bundle.File{{Name: "10-policy.yaml", Content: policy}, {Name: "20-policy.yaml", Content: other}})
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("first declared")))
	})

	ginkgo.It("keeps absent and empty values distinct and rejects multi-document values", func() {
		absent, err := bundle.FromFiles(nil)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, present := absent.ValuesCopy()
		gomega.Expect(present).To(gomega.BeFalse())
		empty, err := bundle.FromFiles([]bundle.File{{Name: "values.yaml", Content: ""}})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		value, present := empty.ValuesCopy()
		gomega.Expect(present).To(gomega.BeTrue())
		gomega.Expect(value).To(gomega.Equal(map[string]any{}))
		_, err = bundle.FromFiles([]bundle.File{{Name: "values.yaml", Content: "null\n---\nfoo: bar\n"}})
		gomega.Expect(err).To(gomega.HaveOccurred())
	})

	ginkgo.It("preserves Helm scalar types from the existing YAML decoder", func() {
		raw := []byte("number: 3\nfloat: 1.5\nquoted: '3'\nyesValue: yes\nnested:\n  enabled: true\n  count: 7\n")
		legacy := map[string]any{}
		gomega.Expect(sigsyaml.Unmarshal(raw, &legacy)).To(gomega.Succeed())
		parsed, err := bundle.ParseValues(raw)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(parsed).To(gomega.Equal(legacy))
		gomega.Expect(parsed["quoted"]).To(gomega.Equal("3"))
	})

	ginkgo.It("tracks physical positions while preserving block scalar separators", func() {
		raw := "# preamble\n---\n" + policy + "---\nnull\n---\n" +
			"apiVersion: sample.io/v1\nkind: UnknownKind\nmetadata:\n  name: other\nspec:\n  note: |\n    ---\n    text\n"
		artifacts, err := bundle.FromFiles([]bundle.File{{Name: "20-resources.yaml", Content: raw}})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(artifacts.Documents()).To(gomega.HaveLen(2))
		gomega.Expect(artifacts.Documents()[1].Source().Document).To(gomega.Equal(4))
		gomega.Expect(artifacts.Documents()[1].ObjectCopy().Object["spec"].(map[string]any)["note"]).To(gomega.ContainSubstring("---"))
	})

	ginkgo.It("rejects missing or nonstring identity fields", func() {
		for _, raw := range []string{
			"kind: ConfigMap\nmetadata:\n  name: missing-api\n",
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: ''\n",
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: 3\n",
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  generateName: generated-\n",
			"apiVersion: v1\nkind: List\nitems: []\n",
		} {
			_, err := bundle.FromFiles([]bundle.File{{Name: "20-invalid.yaml", Content: raw}})
			gomega.Expect(err).To(gomega.HaveOccurred(), raw)
		}
	})
})
