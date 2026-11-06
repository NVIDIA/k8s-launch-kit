// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	apperrors "github.com/nvidia/k8s-launch-kit/pkg/errors"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	networkingv1beta1 "k8s.io/api/networking/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func collisionBundle(subnet string) *bundle.Bundle {
	b, err := bundle.FromFiles([]bundle.File{{Name: "20-pool.yaml", Content: fmt.Sprintf("apiVersion: nv-ipam.nvidia.com/v1alpha1\nkind: IPPool\nmetadata:\n  name: rail-0\n  namespace: network-operator\nspec:\n  subnet: %s\n", subnet)}, {Name: "values.yaml", Content: "operator: {}\n"}})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return b
}

var _ = ginkgo.Describe("IPPool cluster address collision checks", func() {
	var node *corev1.Node
	var service *networkingv1.ServiceCIDR
	var cfg *config.LaunchKitConfig
	var output *ui.JSONOutput
	var ctx context.Context
	newClient := func(objects ...client.Object) client.WithWatch {
		s := runtime.NewScheme()
		gomega.Expect(corev1.AddToScheme(s)).To(gomega.Succeed())
		gomega.Expect(networkingv1.AddToScheme(s)).To(gomega.Succeed())
		gomega.Expect(networkingv1beta1.AddToScheme(s)).To(gomega.Succeed())
		return fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).Build()
	}
	ginkgo.BeforeEach(func() {
		node = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.244.0.0/24", "fd00:1::/64"}}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "172.16.1.10"}}}}
		service = &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"}, Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"10.96.0.0/12", "fd00:2::/112"}}}
		cfg = &config.LaunchKitConfig{}
		output = ui.NewJSON(&bytes.Buffer{}, &bytes.Buffer{})
		ctx = ui.WithOutput(context.Background(), output)
	})
	ginkgo.DescribeTable("rejects detected overlaps and malformed pool subnets",
		func(subnet, message string) {
			err := checkLiveIPPoolCIDRs(ctx, newClient(node, service), collisionBundle(subnet), cfg)
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(message)))
		},
		ginkgo.Entry("pod subset", "10.244.0.128/25", "Node worker-a pod CIDR"),
		ginkgo.Entry("pool superset", "10.0.0.0/8", "ServiceCIDR kubernetes"),
		ginkgo.Entry("dual-stack pod", "fd00:1::/80", "Node worker-a pod CIDR"),
		ginkgo.Entry("dual-stack service", "fd00:2::/64", "ServiceCIDR kubernetes"),
		ginkgo.Entry("node address", "172.16.1.0/24", "Node worker-a InternalIP"),
		ginkgo.Entry("bad subnet", "not-a-prefix", "20-pool.yaml"),
	)
	ginkgo.It("checks all ServiceCIDRs and all Nodes, regardless of selected hardware", func() {
		extra := &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "extension"}, Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"192.168.0.0/24"}}}
		gomega.Expect(checkLiveIPPoolCIDRs(ctx, newClient(node, service, extra), collisionBundle("192.168.0.0/22"), cfg)).To(gomega.MatchError(gomega.ContainSubstring("ServiceCIDR extension")))
		other := node.DeepCopy()
		other.Name = "unselected-control-plane"
		other.Spec.PodCIDRs = []string{"192.168.0.0/24"}
		gomega.Expect(checkLiveIPPoolCIDRs(ctx, newClient(node, other, service), collisionBundle("192.168.0.0/22"), cfg)).To(gomega.MatchError(gomega.ContainSubstring(other.Name)))
	})
	ginkgo.It("uses singular podCIDR fallback and warns by node name for missing pod ranges", func() {
		node.Spec.PodCIDRs = nil
		node.Spec.PodCIDR = "192.168.0.0/24"
		gomega.Expect(checkLiveIPPoolCIDRs(ctx, newClient(node, service), collisionBundle("192.168.0.0/22"), cfg)).To(gomega.MatchError(gomega.ContainSubstring("pod CIDR")))
		node.Spec.PodCIDR = ""
		gomega.Expect(checkLiveIPPoolCIDRs(ctx, newClient(node, service), collisionBundle("192.168.0.0/22"), cfg)).To(gomega.Succeed())
		gomega.Expect(fmt.Sprint(output.Messages())).To(gomega.ContainSubstring("Node worker-a: pod range unknown"))
	})
	ginkgo.It("treats overrides as authoritative without mutating them or the bundle", func() {
		cfg.Cluster = &config.ClusterNetworkConfig{PodCIDRs: []string{"192.0.2.0/24"}, ServiceCIDRs: []string{"198.51.100.0/24"}}
		b := collisionBundle("10.0.0.0/8")
		original := b.Files()
		c := interceptor.NewClient(newClient(node), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			_, isNode := list.(*corev1.NodeList)
			gomega.Expect(isNode).To(gomega.BeTrue(), "service overrides must avoid the ServiceCIDR read")
			return c.List(ctx, list, opts...)
		}})
		gomega.Expect(checkLiveIPPoolCIDRs(ctx, c, b, cfg)).To(gomega.Succeed())
		gomega.Expect(cfg.Cluster.PodCIDRs).To(gomega.Equal([]string{"192.0.2.0/24"}))
		gomega.Expect(b.Files()).To(gomega.Equal(original))
		gomega.Expect(output.Messages()).To(gomega.BeEmpty())
		gomega.Expect(CheckConfiguredIPPoolCIDRs(collisionBundle("192.0.2.128/25"), cfg)).To(gomega.MatchError(gomega.ContainSubstring("cluster.podCIDRs[0]")))
	})
	ginkgo.It("falls back to beta and warns only for verified API absence", func() {
		versions := []string{}
		c := interceptor.NewClient(newClient(node, &networkingv1beta1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "beta"}, Spec: networkingv1beta1.ServiceCIDRSpec{CIDRs: []string{"192.168.0.0/24"}}}), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok {
				versions = append(versions, u.GetAPIVersion())
				if u.GetAPIVersion() == "networking.k8s.io/v1" {
					return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "networking.k8s.io", Kind: "ServiceCIDR"}}
				}
			}
			return c.List(ctx, list, opts...)
		}})
		gomega.Expect(checkLiveIPPoolCIDRs(ctx, c, collisionBundle("192.168.0.0/22"), cfg)).To(gomega.MatchError(gomega.ContainSubstring("ServiceCIDR beta")))
		gomega.Expect(versions).To(gomega.Equal([]string{"networking.k8s.io/v1", "networking.k8s.io/v1beta1"}))
		absent := interceptor.NewClient(newClient(node), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*unstructured.UnstructuredList); ok {
				return apierrors.NewNotFound(schema.GroupResource{Group: "networking.k8s.io", Resource: "servicecidrs"}, "")
			}
			return c.List(ctx, list, opts...)
		}})
		gomega.Expect(checkLiveIPPoolCIDRs(ctx, absent, collisionBundle("192.168.0.0/22"), cfg)).To(gomega.Succeed())
		gomega.Expect(fmt.Sprint(output.Messages())).To(gomega.ContainSubstring("service ranges unverified"))
	})
	ginkgo.DescribeTable("does not downgrade failed inspection", func(failure error) {
		c := interceptor.NewClient(newClient(node, service), interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return failure }})
		gomega.Expect(checkLiveIPPoolCIDRs(ctx, c, collisionBundle("192.168.0.0/22"), cfg)).To(gomega.MatchError(gomega.ContainSubstring(failure.Error())))
		gomega.Expect(output.Messages()).To(gomega.BeEmpty())
	}, ginkgo.Entry("forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "servicecidrs"}, "", fmt.Errorf("denied"))), ginkgo.Entry("cancelled", context.Canceled), ginkgo.Entry("transient", apierrors.NewServiceUnavailable("offline")))
	ginkgo.It("rejects empty and malformed served service ranges", func() {
		gomega.Expect(checkLiveIPPoolCIDRs(ctx, newClient(node), collisionBundle("192.168.0.0/22"), cfg)).To(gomega.MatchError(gomega.ContainSubstring("no ranges")))
		service.Spec.CIDRs = []string{"broken"}
		gomega.Expect(checkLiveIPPoolCIDRs(ctx, newClient(node, service), collisionBundle("192.168.0.0/22"), cfg)).To(gomega.MatchError(gomega.ContainSubstring("invalid CIDR")))
	})
	ginkgo.It("rejects collisions before Helm and all writes even in dry-run/overwrite", func() {
		for _, dryRun := range []bool{false, true} {
			calls := 0
			c := interceptor.NewClient(newClient(node, service), interceptor.Funcs{Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				calls++
				return nil
			}})
			installer := func(context.Context, *rest.Config, *config.NetworkOperatorConfig, []byte, map[string]any, string, bool, time.Duration, bool) error {
				calls++
				return nil
			}
			err := applyBundleWithInstaller(ctx, c, collisionBundle("10.96.0.0/16"), DeployOptions{Config: cfg, DryRun: dryRun, OverwriteExisting: true}, installer)
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("overlaps")))
			gomega.Expect(apperrors.StructuredFromError(err).Message).To(gomega.ContainSubstring("overlaps"))
			gomega.Expect(calls).To(gomega.BeZero())
		}
	})
	ginkgo.It("does not request cluster APIs for bundles without IPPools", func() {
		b, err := bundle.FromFiles([]bundle.File{{Name: "values.yaml", Content: "operator: {}"}})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(checkLiveIPPoolCIDRs(ctx, nil, b, cfg)).To(gomega.Succeed())
	})
})
