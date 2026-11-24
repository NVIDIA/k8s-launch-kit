// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var _ = ginkgo.Describe("OpenShift clean plan execution", func() {
	ginkgo.It("preserves external OLM and unrelated CRs and reports legacy settings on repeat runs", func() {
		ctx := context.Background()
		cfg, artifacts, live := ocpCleanFixture()
		sub := ocpTestObject("operators.coreos.com/v1alpha1", "Subscription", "operator-system", "nvidia-network-operator", map[string]any{
			"spec": map[string]any{"name": "nvidia-network-operator"},
		})
		// A different site's SR-IOV policy is reconciled by its own OLM operator.
		site := ocpTestObject("sriovnetwork.openshift.io/v1", "SriovNetworkNodePolicy", config.DefaultSriovOperatorNamespace, "site", nil)
		c := fake.NewClientBuilder().WithReturnManagedFields().WithInterceptorFuncs(interceptor.Funcs{Create: policyTestCreate}).WithObjects(live, sub, site).Build()
		plan, err := PrepareOpenShiftClean(ctx, c, cfg, artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.Targets()).To(gomega.ContainElement(gomega.ContainSubstring("Delete NicClusterPolicy")))
		opts := CleanOptions{SkipWait: true, PollInterval: time.Millisecond, ResourceTimeout: time.Second}
		result, err := plan.Execute(ctx, opts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(result.CustomResourcesDeleted).To(gomega.Equal(1))
		gomega.Expect(result.HelmReleaseRemoved).To(gomega.BeFalse())
		gomega.Expect(result.Retained).NotTo(gomega.BeEmpty())
		gomega.Expect(apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(live), live))).To(gomega.BeTrue())
		gomega.Expect(c.Get(ctx, client.ObjectKeyFromObject(sub), sub)).To(gomega.Succeed())
		gomega.Expect(c.Get(ctx, client.ObjectKeyFromObject(site), site)).To(gomega.Succeed())
		plan, err = PrepareOpenShiftClean(ctx, c, cfg, artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		result, err = plan.Execute(ctx, opts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(result.CustomResourcesDeleted).To(gomega.BeZero())
	})
	ginkgo.It("refuses a conflicting namespace without touching the plan", func() {
		ctx := context.Background()
		cfg, artifacts, live := ocpCleanFixture()
		c := fake.NewClientBuilder().WithReturnManagedFields().WithInterceptorFuncs(interceptor.Funcs{Create: policyTestCreate}).WithObjects(live).Build()
		plan, err := PrepareOpenShiftClean(ctx, c, cfg, artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		cfg.NetworkOperator.Namespace = "other" // The caller cannot redirect the approved plan.
		_, err = plan.Execute(ctx, CleanOptions{Namespace: "other"})
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("differs")))
		gomega.Expect(c.Get(ctx, client.ObjectKeyFromObject(live), live)).To(gomega.Succeed())
	})
})
