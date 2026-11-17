// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/hostplatform"
	"github.com/nvidia/k8s-launch-kit/pkg/networkoperatorplugin/crstate"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = ginkgo.Describe("concrete Host read policies through bundle validation", func() {
	for _, platform := range []hostplatform.Selection{hostplatform.Kubernetes, hostplatform.OpenShift} {
		ginkgo.It("retains network attachment interpretation for "+string(platform), func() {
			network := ocpTestObject("sriovnetwork.openshift.io/v1", "SriovNetwork", "sriov-operator", "network-a", map[string]any{
				"spec": map[string]any{"networkNamespace": "workloads", "resourceName": "resource-a"},
			})
			raw, err := network.MarshalJSON()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			artifacts, err := bundle.FromFiles([]bundle.File{{Name: "30-network.yaml", Content: string(raw)}})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			c := fake.NewClientBuilder().WithObjects(network).Build()
			results, err := ValidateBundle(context.Background(), c, artifacts, nil, platform)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(results).To(gomega.HaveLen(1))
			gomega.Expect(results[0].Found).To(gomega.BeTrue())
			gomega.Expect(results[0].LiveYAML).NotTo(gomega.BeEmpty())
			if platform == hostplatform.OpenShift {
				gomega.Expect(results[0].State).To(gomega.Equal(crstate.StateInProgress))
				gomega.Expect(results[0].Reason).To(gomega.ContainSubstring("NetworkAttachmentDefinition"))
			} else {
				gomega.Expect(results[0].State).To(gomega.Equal(crstate.StateSuccess))
			}
		})

		ginkgo.It("keeps Kind-specific operator configuration checks on "+string(platform), func() {
			artifacts, err := bundle.FromFiles([]bundle.File{{Name: "05-operator.yaml", Content: "apiVersion: maintenance.nvidia.com/v1alpha1\nkind: MaintenanceOperatorConfig\nmetadata:\n  name: default\nspec:\n  maxParallelOperations: 1\n"}})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			live := artifacts.Documents()[0].ObjectCopy()
			live.Object["spec"] = map[string]any{"maxParallelOperations": int64(2)}
			results, err := ValidateBundle(context.Background(), fake.NewClientBuilder().WithObjects(live).Build(), artifacts, nil, platform)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(results).To(gomega.HaveLen(1))
			gomega.Expect(results[0].State).To(gomega.Equal(crstate.StateError))
			gomega.Expect(results[0].Reason).To(gomega.Equal("operator configuration differs from generated required fields"))
			results, err = ValidateBundle(context.Background(), fake.NewClientBuilder().Build(), artifacts, nil, platform)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(results[0].State).To(gomega.Equal(crstate.StateNotDeployed))
			gomega.Expect(results[0].Missing).To(gomega.BeFalse(), "preserve the existing missing operator-config flags")
		})
	}
})
