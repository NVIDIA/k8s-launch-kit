// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package connectivity

import (
	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("validation workload projections", func() {
	ginkgo.It("returns fresh objects and refs for OpenShift rewrites", func() {
		artifacts, err := bundle.FromFiles([]bundle.File{
			{Name: "60-example-daemonset.yaml", Content: "apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: test\n  namespace: original\nspec:\n  template:\n    metadata:\n      annotations:\n        k8s.v1.cni.cncf.io/networks: original/rail-0\n"},
			{Name: "61-example-role.yaml", Content: "apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: role\n  namespace: original\n"},
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		objects, refs, err := ExampleDaemonSets(artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		support, err := OpenShiftExampleSupport(artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(objects).To(gomega.HaveLen(1))
		gomega.Expect(support).To(gomega.HaveLen(1))
		objects[0].SetNamespace("isolated")
		refs[0].Namespace = "isolated"
		support[0].SetNamespace("isolated")
		freshObjects, freshRefs, err := ExampleDaemonSets(artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		freshSupport, err := OpenShiftExampleSupport(artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(freshObjects[0].GetNamespace()).To(gomega.Equal("original"))
		gomega.Expect(freshRefs[0].Namespace).To(gomega.Equal("original"))
		gomega.Expect(freshSupport[0].GetNamespace()).To(gomega.Equal("original"))
	})
})
