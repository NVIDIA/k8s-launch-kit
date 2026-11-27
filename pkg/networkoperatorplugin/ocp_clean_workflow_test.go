// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"slices"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

var _ = ginkgo.Describe("OpenShift bundle cleanup workflow", func() {
	ginkgo.It("removes the network, pool, policy, tuning expressions and managed operator in order and can run again", func() {
		cfg, install, sub, csv, group := ocpOperatorCleanFixture()
		_, root, pool, _ := ocpPoolConsumerFixture()
		network := ocpResourceObject(cleanSriovGroup, "SriovNetwork", config.DefaultSriovOperatorNamespace, "workload-network", map[string]interface{}{
			"networkNamespace": "unrelated-application", "ipam": `{"type":"nv-ipam","poolName":"selected-pool"}`,
		})
		nad := ocpConsumerNAD(network.GetName(), `{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"selected-pool"}}`)
		cm := sysctlConfigMap()
		cm.SetUID("allowlist-uid")
		cm.Data["allowlist.conf"] = "site-entry\nrequested-entry\n"
		files := slices.DeleteFunc(install.Files(), func(f bundle.File) bool { return f.Name == "10-nicclusterpolicy.yaml" })
		for _, row := range []struct {
			name string
			obj  *unstructured.Unstructured
		}{
			{"10-policy.yaml", root}, {"20-ippool.yaml", pool}, {"50-network.yaml", network},
		} {
			desired := ocpResourceRef(row.obj).Desired
			body, err := yaml.Marshal(desired.Object)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			files = append(files, bundle.File{Name: row.name, Content: string(body)})
		}
		files = append(files, bundle.File{Name: ocpSysctlAllowlistPatchFilename, Content: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cni-sysctl-allowlist\n  namespace: openshift-multus\ndata:\n  allowlist.conf: requested-entry\n"})
		artifacts, err := bundle.FromFiles(files)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		base := ocpOperatorCleanClient(sub, csv, group, root, pool, network, nad, cm)
		var writes []string
		c := interceptor.NewClient(base, interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				writes = append(writes, obj.GetName())
				if obj.GetName() == network.GetName() {
					// The network controller removes its generated attachment.
					if err := c.Delete(ctx, nad); err != nil {
						return err
					}
				}
				return c.Delete(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				writes = append(writes, obj.GetName())
				return c.Patch(ctx, obj, p, opts...)
			},
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				ginkgo.Fail("cleanup must not create cluster records")
				return nil
			},
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for run := 0; run < 2; run++ {
			plan, err := PrepareOpenShiftClean(ctx, c, cfg, artifacts)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			result, err := plan.Execute(ctx, CleanOptions{PollInterval: time.Millisecond})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if run == 0 {
				gomega.Expect(result.CustomResourcesDeleted).To(gomega.Equal(3))
				gomega.Expect(writes).To(gomega.Equal([]string{network.GetName(), pool.GetName(), root.GetName(), cm.Name, sub.GetName(), csv.GetName(), group.GetName()}))
			} else {
				gomega.Expect(result.CustomResourcesDeleted).To(gomega.BeZero())
				gomega.Expect(writes).To(gomega.BeEmpty())
			}
			live := &corev1.ConfigMap{}
			gomega.Expect(c.Get(ctx, client.ObjectKeyFromObject(cm), live)).To(gomega.Succeed())
			gomega.Expect(live.Data["allowlist.conf"]).To(gomega.Equal("site-entry\n"))
			writes = nil
		}
	})
})

var _ = ginkgo.Describe("OpenShift NFD cleanup from the retained bundle", func() {
	ginkgo.DescribeTable("compares the complete worker rule set", func(extraClass string) {
		cfg, base, _ := ocpCleanFixture()
		cfg.OCPOperatorInstall = &config.OCPOperatorInstallConfig{Manage: true}
		nfd := ocpTestObject("nfd.openshift.io/v1", "NodeFeatureDiscovery", config.DefaultNFDOperatorNamespace, config.DefaultNFDConfigurationName, map[string]interface{}{
			"spec": map[string]interface{}{"workerConfig": map[string]interface{}{"configData": "sources: {pci: {deviceClassWhitelist: ['02', '03', '0200', '0207'], deviceLabelFields: [vendor]}}"}},
		})
		data, err := yaml.Marshal(nfd.Object)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		sub := ocpTestObject("operators.coreos.com/v1alpha1", "Subscription", nfd.GetNamespace(), "nfd", map[string]interface{}{"spec": map[string]interface{}{"name": "nfd"}})
		sub.SetAnnotations(map[string]string{launchKitVersionAnnotation: "test"})
		install, err := yaml.Marshal(sub.Object)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		sub.SetUID("nfd-subscription")
		artifacts, err := bundle.FromFiles(append(base.Files(), bundle.File{Name: "03-nodefeaturediscovery.yaml", Content: string(data)}, bundle.File{Name: "00-subscriptions.yaml", Content: string(install)}))
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		nfd.SetUID("discovery-uid")
		// A preexisting instance may have no Launch Kit annotations. Ordinary spec
		// defaults and YAML formatting do not add any worker rules.
		rules := "sources:\n  pci:\n    deviceLabelFields: [vendor]\n    deviceClassWhitelist: ['02', '03', '0200', '0207'" + extraClass + "]\n"
		gomega.Expect(unstructured.SetNestedField(nfd.Object, rules, "spec", "workerConfig", "configData")).To(gomega.Succeed())
		gomega.Expect(unstructured.SetNestedField(nfd.Object, false, "spec", "topologyUpdater")).To(gomega.Succeed())
		c := ocpOperatorCleanClient(nfd, sub)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for attempt := 0; attempt < 2; attempt++ {
			plan, err := PrepareOpenShiftClean(ctx, c, cfg, artifacts)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = plan.Execute(ctx, CleanOptions{PollInterval: time.Millisecond})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		live := nfd.DeepCopy()
		err = c.Get(ctx, client.ObjectKeyFromObject(nfd), live)
		if extraClass == "" {
			gomega.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
			gomega.Expect(apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(sub), sub))).To(gomega.BeTrue())
			return
		}
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(live.GetUID()).To(gomega.Equal(nfd.GetUID()))
		gomega.Expect(c.Get(ctx, client.ObjectKeyFromObject(sub), sub)).To(gomega.Succeed())
		raw, _, err := unstructured.NestedString(live.Object, "spec", "workerConfig", "configData")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		var worker map[string]interface{}
		gomega.Expect(yaml.Unmarshal([]byte(raw), &worker)).To(gomega.Succeed())
		classes, _, err := unstructured.NestedStringSlice(worker, "sources", "pci", "deviceClassWhitelist")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(classes).To(gomega.Equal([]string{"0302"}))
		fields, _, err := unstructured.NestedStringSlice(worker, "sources", "pci", "deviceLabelFields")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(fields).To(gomega.Equal([]string{"vendor"}))
	}, ginkgo.Entry("exactly the generated rules", ""), ginkgo.Entry("another PCI class using the same vendor labels", ", '0302'"))
})
