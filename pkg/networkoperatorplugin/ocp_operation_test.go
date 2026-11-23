// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func ocpOperationTestClient() client.WithWatch {
	return fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			obj.SetUID("lock-uid")
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
}

var _ = ginkgo.Describe("OpenShift operation exclusion", func() {
	ginkgo.It("excludes deploy and clean until the exact acquired lock is released", func() {
		ctx := context.Background()
		c := ocpOperationTestClient()
		release, err := acquireOCPOperation(ctx, c, "operator-system", "owner", "deploy")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = acquireOCPOperation(ctx, c, "operator-system", "owner", "clean")
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("is held")))
		gomega.Expect(release()).To(gomega.Succeed())
		gomega.Expect(release()).To(gomega.Succeed())
		release, err = acquireOCPOperation(ctx, c, "operator-system", "owner", "clean")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(release()).To(gomega.Succeed())
	})

	ginkgo.It("releases its lock with a bounded independent context after operation cancellation", func() {
		ctx, cancel := context.WithCancel(context.Background())
		c := ocpOperationTestClient()
		release, err := acquireOCPOperation(ctx, c, "operator-system", "owner", "clean")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		cancel()
		gomega.Expect(release()).To(gomega.Succeed())
		obj := &corev1.ConfigMap{}
		gomega.Expect(apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKey{Namespace: "operator-system", Name: ocpOperationName("owner")}, obj))).To(gomega.BeTrue())
	})

	ginkgo.It("does not delete a changed lock on release", func() {
		ctx := context.Background()
		c := ocpOperationTestClient()
		release, err := acquireOCPOperation(ctx, c, "operator-system", "owner", "clean")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		obj := &corev1.ConfigMap{}
		gomega.Expect(c.Get(ctx, client.ObjectKey{Namespace: "operator-system", Name: ocpOperationName("owner")}, obj)).To(gomega.Succeed())
		obj.Data["external-change"] = "preserve"
		gomega.Expect(c.Update(ctx, obj)).To(gomega.Succeed())
		gomega.Expect(release()).To(gomega.HaveOccurred())
		gomega.Expect(c.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(gomega.Succeed())
		gomega.Expect(obj.Data["external-change"]).To(gomega.Equal("preserve"))
	})

	ginkgo.It("rejects foreign lock structures without mutation", func() {
		obj := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "operator-system", Name: ocpOperationName("owner"), UID: "foreign-uid"}, Data: map[string]string{"owner": "foreign"}}
		c := fake.NewClientBuilder().WithObjects(obj).Build()
		_, err := acquireOCPOperation(context.Background(), c, "operator-system", "owner", "clean")
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("unexpected OpenShift operation lock")))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)).To(gomega.Succeed())
		gomega.Expect(obj.Data["owner"]).To(gomega.Equal("foreign"))
	})

	ginkgo.It("propagates ambiguous creates without attempting a takeover or release", func() {
		failure := errors.New("ambiguous transport failure")
		c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error { return failure },
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				ginkgo.Fail("ambiguous acquisition must not delete anything")
				return nil
			},
		}).Build()
		release, err := acquireOCPOperation(context.Background(), c, "operator-system", "owner", "clean")
		gomega.Expect(errors.Is(err, failure)).To(gomega.BeTrue())
		gomega.Expect(release).To(gomega.BeNil())
	})

	ginkgo.It("never steals an existing lock even when its timestamps appear old", func() {
		obj := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "operator-system", Name: ocpOperationName("owner"), UID: "old-uid", Labels: map[string]string{"app.kubernetes.io/managed-by": ocpOperationManager}}, Data: map[string]string{"owner": "owner", "action": "deploy"}}
		c := fake.NewClientBuilder().WithObjects(obj).WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				ginkgo.Fail("existing lock must not be taken over")
				return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, obj.Name, errors.New("held"))
			},
		}).Build()
		_, err := acquireOCPOperation(context.Background(), c, "operator-system", "owner", "clean")
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("never automatically taken over")))
	})
})
