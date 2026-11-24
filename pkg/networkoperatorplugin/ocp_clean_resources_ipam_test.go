// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func ocpPoolConsumerFixture() (*config.LaunchKitConfig, *unstructured.Unstructured, *unstructured.Unstructured, []cleanObjectRef) {
	cfg := &config.LaunchKitConfig{NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-system"}}
	root := ocpResourceObject("mellanox.com", "NicClusterPolicy", "", "selected-root", map[string]interface{}{"nvIpam": map[string]interface{}{"enableWebhook": false}})
	pool := ocpResourceObject("nv-ipam.nvidia.com", "IPPool", "operator-system", "selected-pool", map[string]interface{}{"subnet": "192.0.2.0/24"})
	return cfg, root, pool, []cleanObjectRef{ocpResourceRef(root), ocpResourceRef(pool)}
}

func ocpConsumerNAD(name, configuration string) *unstructured.Unstructured {
	nad := testCleanObject(cleanNADGVK, "unrelated-application", name)
	nad.Object["spec"] = map[string]interface{}{"config": configuration}
	return nad
}

var _ = ginkgo.Describe("OpenShift surviving pool consumer protection", func() {
	ginkgo.DescribeTable("blocks referenced pools before any write in both modes", func(group, kind, namespace string) {
		for _, skip := range []bool{false, true} {
			cfg, root, pool, refs := ocpPoolConsumerFixture()
			consumer := ocpResourceObject(group, kind, namespace, "foreign-network", map[string]interface{}{"networkNamespace": "unrelated-application", "ipam": "{\n  \"type\": \"nv-ipam\",\n  \"poolName\": \"selected-pool\"\n}"})
			base := ocpResourceTestClient(root, pool, consumer)
			c := interceptor.NewClient(base, interceptor.Funcs{
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					ginkgo.Fail("pool consumers must be checked before writes")
					return nil
				},
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					ginkgo.Fail("pool consumers must be checked before writes")
					return nil
				},
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					ginkgo.Fail("pool consumers must be checked before deletions")
					return nil
				},
			})
			count, err := cleanOCPResources(context.Background(), c, cfg, refs, CleanOptions{Namespace: "operator-system", SkipWait: skip, PollInterval: time.Millisecond})
			gomega.Expect(count).To(gomega.BeZero())
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("surviving " + kind)))
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("IPPool operator-system/selected-pool")))
			gomega.Expect(base.Get(context.Background(), client.ObjectKeyFromObject(consumer), consumer)).To(gomega.Succeed())
			gomega.Expect(base.Get(context.Background(), client.ObjectKeyFromObject(pool), pool)).To(gomega.Succeed())
			gomega.Expect(base.Get(context.Background(), client.ObjectKeyFromObject(root), root)).To(gomega.Succeed())
		}
	},
		ginkgo.Entry("SR-IOV Ethernet in another operator/application namespace", cleanSriovGroup, "SriovNetwork", "other-sriov-system"),
		ginkgo.Entry("SR-IOV IB", cleanSriovGroup, "SriovIBNetwork", "other-sriov-system"),
		ginkgo.Entry("OVS without a reconciled NAD", cleanSriovGroup, "OVSNetwork", "other-sriov-system"),
		ginkgo.Entry("Mellanox macvlan", "mellanox.com", "MacvlanNetwork", ""),
		ginkgo.Entry("Mellanox host device", "mellanox.com", "HostDeviceNetwork", ""),
		ginkgo.Entry("Mellanox IPoIB", "mellanox.com", "IPoIBNetwork", ""))

	ginkgo.DescribeTable("detects actual NV-IPAM CNI naming forms in external NADs", func(raw string) {
		for _, skip := range []bool{false, true} {
			cfg, root, pool, refs := ocpPoolConsumerFixture()
			nad := ocpConsumerNAD("foreign-nad", raw)
			base := ocpResourceTestClient(root, pool, nad)
			c := interceptor.NewClient(base, interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				ginkgo.Fail("external NAD reference must precede ordinary deletes")
				return nil
			}})
			count, err := cleanOCPResources(context.Background(), c, cfg, refs, CleanOptions{Namespace: "operator-system", SkipWait: skip, PollInterval: time.Millisecond})
			gomega.Expect(count).To(gomega.BeZero())
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("surviving NetworkAttachmentDefinition")))
			gomega.Expect(base.Get(context.Background(), client.ObjectKeyFromObject(nad), nad)).To(gomega.Succeed())
		}
	},
		ginkgo.Entry("single delegate", `{"name":"other","type":"sriov","ipam":{"type":"nv-ipam","poolName":"selected-pool"}}`),
		ginkgo.Entry("dual stack comma list", `{"name":"other","type":"sriov","ipam":{"type":"nv-ipam","poolName":"independent,selected-pool"}}`),
		ginkgo.Entry("plural args override", `{"name":"other","type":"sriov","args":{"cni":{"poolNames":["independent","selected-pool"]}},"ipam":{"type":"nv-ipam","poolName":"other"}}`),
		ginkgo.Entry("case-normalized pool type", `{"name":"other","type":"sriov","ipam":{"type":"nv-ipam","poolName":"selected-pool","poolType":"IPPool"}}`),
		ginkgo.Entry("default CNI name rather than NAD name or namespace", `{"name":"selected-pool","type":"sriov","ipam":{"type":"nv-ipam"}}`),
		ginkgo.Entry("conflist name overrides delegate name", `{"name":"selected-pool","plugins":[{"type":"tuning"},{"name":"independent","type":"sriov","ipam":{"type":"nv-ipam"}}]}`))

	ginkgo.It("allows independent networks and their pools in both modes", func() {
		for _, skip := range []bool{false, true} {
			cfg, root, pool, refs := ocpPoolConsumerFixture()
			network := ocpResourceObject(cleanSriovGroup, "SriovNetwork", "other-sriov-system", "independent-network", map[string]interface{}{"ipam": `{"type":"nv-ipam","poolName":"independent-pool"}`})
			independentPool := ocpResourceObject("nv-ipam.nvidia.com", "IPPool", "other-operator-system", "independent-pool", map[string]interface{}{"subnet": "198.51.100.0/24"})
			nad := ocpConsumerNAD("independent-nad", `{"type":"macvlan","ipam":{"type":"whereabouts","poolName":"selected-pool"}}`)
			c := ocpResourceTestClient(root, pool, network, independentPool, nad)
			count, err := cleanOCPResources(context.Background(), c, cfg, refs, CleanOptions{Namespace: "operator-system", SkipWait: skip, PollInterval: time.Millisecond})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(count).To(gomega.Equal(2))
			gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(network), network)).To(gomega.Succeed())
			gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(independentPool), independentPool)).To(gomega.Succeed())
			gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(nad), nad)).To(gomega.Succeed())
		}
	})

	ginkgo.It("respects pool-type and CNI args overrides instead of treating matching text as a reference", func() {
		cfg, _, pool, refs := ocpPoolConsumerFixture()
		configs := []string{
			`{"type":"other-cni","args":{"cni":{"poolNames":{"custom":"selected-pool"}}},"ipam":{"type":"other-ipam","poolName":{"custom":"selected-pool"}}}`,
			`{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"selected-pool","poolType":"cidrpool"}}`,
			`{"type":"sriov","args":{"cni":{"poolNames":["independent"]}},"ipam":{"type":"nv-ipam","poolName":"selected-pool"}}`,
			`{"type":"sriov","args":{"cni":{"poolType":"CIDRPool"}},"ipam":{"type":"nv-ipam","poolName":"selected-pool"}}`,
			`{"name":"independent","type":"sriov","ipam":{"type":"nv-ipam","poolNames":["selected-pool"]}}`,
		}
		for _, raw := range configs {
			nad := ocpConsumerNAD("independent", raw)
			// Test removing the IPPool alone; the independent CIDRPool's controller
			// remains. Whole-root CIDRPool protection is covered by the shared guard.
			gomega.Expect(checkOCPCleanDependencies(context.Background(), ocpResourceTestClient(pool, nad), cfg, refs[1:])).To(gomega.Succeed())
		}
	})

	ginkgo.It("does not misclassify selected network attachments as surviving consumers", func() {
		cfg, root, pool, refs := ocpPoolConsumerFixture()
		network := ocpResourceObject(cleanSriovGroup, "SriovNetwork", config.DefaultSriovOperatorNamespace, "selected-network", map[string]interface{}{"networkNamespace": "unrelated-application", "ipam": `{"type":"nv-ipam","poolName":"selected-pool"}`})
		nad := ocpConsumerNAD("selected-network", `{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"selected-pool"}}`)
		nad.SetAnnotations(map[string]string{"sriovnetwork.openshift.io/owner-ref": "SriovNetwork.sriovnetwork.openshift.io/" + network.GetNamespace() + "/" + network.GetName()})
		refs = append(refs, ocpResourceRef(network))
		gomega.Expect(checkOCPCleanDependencies(context.Background(), ocpResourceTestClient(root, pool, network, nad), cfg, refs)).To(gomega.Succeed())
	})

	ginkgo.It("uses the CR name when a surviving network omits poolName", func() {
		cfg, root, pool, refs := ocpPoolConsumerFixture()
		network := ocpResourceObject(cleanSriovGroup, "SriovIBNetwork", "application", "selected-pool", map[string]interface{}{"ipam": `{"type":"nv-ipam"}`})
		err := checkOCPCleanDependencies(context.Background(), ocpResourceTestClient(root, pool, network), cfg, refs)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("still references that pool")))
	})

	ginkgo.It("propagates network inventory failure before deleting a selected pool", func() {
		cfg, root, pool, refs := ocpPoolConsumerFixture()
		denied := errors.New("forbidden consumer inventory")
		c := interceptor.NewClient(ocpResourceTestClient(root, pool), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Kind == "SriovNetworkList" {
				return denied
			}
			return c.List(ctx, list, opts...)
		}})
		gomega.Expect(errors.Is(checkOCPCleanDependencies(context.Background(), c, cfg, refs), denied)).To(gomega.BeTrue())
	})

	ginkgo.It("does not hide malformed inline consumer configurations", func() {
		cfg, root, pool, refs := ocpPoolConsumerFixture()
		nad := ocpConsumerNAD("foreign", `{"type":"sriov","ipam":`)
		err := checkOCPCleanDependencies(context.Background(), ocpResourceTestClient(root, pool, nad), cfg, refs)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("config JSON")))
		// A structured spec.ipam is outside the served string contract, and cannot
		// be silently omitted as though the foreign network had no IPAM.
		network := ocpResourceObject(cleanSriovGroup, "SriovNetwork", "application", "foreign", map[string]interface{}{"ipam": map[string]interface{}{"type": "nv-ipam", "poolName": "selected-pool"}})
		err = checkOCPCleanDependencies(context.Background(), ocpResourceTestClient(root, pool, network), cfg, refs)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("spec.ipam")))
	})

	ginkgo.It("parses a standalone CNI configuration without changing its data", func() {
		raw := `{"name":"net","type":"sriov","args":{"cni":{"poolNames":["selected-pool"]}},"ipam":{"type":"nv-ipam","poolName":"independent"}}`
		var configuration ocpPoolCNIConfig
		gomega.Expect(json.Unmarshal([]byte(raw), &configuration)).To(gomega.Succeed())
		pools, err := ocpNVIPAMPoolReferences(configuration, "fallback")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pools).To(gomega.Equal([]string{"selected-pool"}))
		gomega.Expect(string(configuration.IPAM)).To(gomega.Equal(`{"type":"nv-ipam","poolName":"independent"}`))
	})
})

var _ = ginkgo.Describe("OpenShift pool consumer lifecycle checks", func() {
	ginkgo.DescribeTable("inspects chained meta-plugin IPAM before its NAD exists", func(group, kind, namespace string) {
		for _, skip := range []bool{false, true} {
			cfg, root, pool, refs := ocpPoolConsumerFixture()
			consumer := ocpResourceObject(group, kind, namespace, "foreign-chain", map[string]interface{}{"metaPlugins": `{"type":"tuning"}, {"type":"sriov","ipam":{"type":"nv-ipam","poolName":"selected-pool"}}`})
			base := ocpResourceTestClient(root, pool, consumer)
			c := interceptor.NewClient(base, interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				ginkgo.Fail("pending chained consumer must prevent deletions")
				return nil
			}})
			count, err := cleanOCPResources(context.Background(), c, cfg, refs, CleanOptions{Namespace: "operator-system", SkipWait: skip, PollInterval: time.Millisecond})
			gomega.Expect(count).To(gomega.BeZero())
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("surviving " + kind)))
		}
	}, ginkgo.Entry("SR-IOV Ethernet", cleanSriovGroup, "SriovNetwork", "other-system"), ginkgo.Entry("SR-IOV IB", cleanSriovGroup, "SriovIBNetwork", "other-system"), ginkgo.Entry("OVS", cleanSriovGroup, "OVSNetwork", "other-system"), ginkgo.Entry("Mellanox macvlan", "mellanox.com", "MacvlanNetwork", ""), ginkgo.Entry("Mellanox host device", "mellanox.com", "HostDeviceNetwork", ""), ginkgo.Entry("Mellanox IPoIB", "mellanox.com", "IPoIBNetwork", ""))

	ginkgo.It("allows an unavailable optional OVS API", func() {
		cfg, root, pool, refs := ocpPoolConsumerFixture()
		c := interceptor.NewClient(ocpResourceTestClient(root, pool), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Kind == "OVSNetworkList" {
				return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: cleanSriovGroup, Kind: "OVSNetwork"}, SearchedVersions: []string{"v1"}}
			}
			return c.List(ctx, list, opts...)
		}})
		gomega.Expect(checkOCPCleanDependencies(context.Background(), c, cfg, refs)).To(gomega.Succeed())
	})

	ginkgo.DescribeTable("does not trust copied NAD owner annotations", func(change func(*unstructured.Unstructured, *unstructured.Unstructured, []cleanObjectRef) bool) {
		cfg, root, pool, refs := ocpPoolConsumerFixture()
		network := ocpResourceObject(cleanSriovGroup, "SriovNetwork", config.DefaultSriovOperatorNamespace, "selected-network", map[string]interface{}{"networkNamespace": "unrelated-application", "ipam": `{"type":"nv-ipam","poolName":"selected-pool"}`})
		nad := ocpConsumerNAD("selected-network", `{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"selected-pool"}}`)
		nad.SetAnnotations(map[string]string{"sriovnetwork.openshift.io/owner-ref": "SriovNetwork.sriovnetwork.openshift.io/" + network.GetNamespace() + "/" + network.GetName()})
		refs = append(refs, ocpResourceRef(network))
		present := change(network, nad, refs)
		objects := []client.Object{root, pool, nad}
		if present {
			objects = append(objects, network)
		}
		base := ocpResourceTestClient(objects...)
		c := interceptor.NewClient(base, interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			ginkgo.Fail("ambiguous NAD removal must block pool deletion")
			return nil
		}})
		count, err := cleanOCPResources(context.Background(), c, cfg, refs, CleanOptions{Namespace: "operator-system", SkipWait: true, PollInterval: time.Millisecond})
		gomega.Expect(count).To(gomega.BeZero())
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("surviving NetworkAttachmentDefinition")))
		gomega.Expect(base.Get(context.Background(), client.ObjectKeyFromObject(pool), pool)).To(gomega.Succeed())
	},
		ginkgo.Entry("renamed child", func(_ *unstructured.Unstructured, nad *unstructured.Unstructured, _ []cleanObjectRef) bool {
			nad.SetName("copied-network")
			return true
		}),
		ginkgo.Entry("moved child", func(_ *unstructured.Unstructured, nad *unstructured.Unstructured, _ []cleanObjectRef) bool {
			nad.SetNamespace("another-application")
			return true
		}),
		ginkgo.Entry("vanished parent", func(_ *unstructured.Unstructured, _ *unstructured.Unstructured, refs []cleanObjectRef) bool {
			refs[len(refs)-1].UID = ""
			return false
		}),
		ginkgo.Entry("foreign GC owner", func(_ *unstructured.Unstructured, nad *unstructured.Unstructured, _ []cleanObjectRef) bool {
			nad.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "foreign", UID: "foreign"}})
			return true
		}))

	ginkgo.It("accepts an exact GC-owned child of a pinned selected network", func() {
		cfg, root, pool, refs := ocpPoolConsumerFixture()
		network := ocpResourceObject("mellanox.com", "MacvlanNetwork", "", "selected-network", map[string]interface{}{"ipam": `{"type":"nv-ipam","poolName":"selected-pool"}`})
		nad := ocpConsumerNAD("renamed-but-gc-owned", `{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"selected-pool"}}`)
		nad.SetOwnerReferences([]metav1.OwnerReference{ocpResourceOwner(network)})
		refs = append(refs, ocpResourceRef(network))
		gomega.Expect(checkOCPCleanDependencies(context.Background(), ocpResourceTestClient(root, pool, network, nad), cfg, refs)).To(gomega.Succeed())
	})

	ginkgo.It("uses the SR-IOV parent namespace when target namespace is omitted", func() {
		cfg, root, pool, refs := ocpPoolConsumerFixture()
		network := ocpResourceObject(cleanSriovGroup, "SriovNetwork", config.DefaultSriovOperatorNamespace, "selected-network", map[string]interface{}{"ipam": `{"type":"nv-ipam","poolName":"selected-pool"}`})
		nad := ocpConsumerNAD("selected-network", `{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"selected-pool"}}`)
		nad.SetNamespace(network.GetNamespace())
		nad.SetAnnotations(map[string]string{"sriovnetwork.openshift.io/owner-ref": "SriovNetwork.sriovnetwork.openshift.io/" + network.GetNamespace() + "/" + network.GetName()})
		refs = append(refs, ocpResourceRef(network))
		gomega.Expect(checkOCPCleanDependencies(context.Background(), ocpResourceTestClient(root, pool, network, nad), cfg, refs)).To(gomega.Succeed())
	})

	ginkgo.It("rescans after worker/network teardown before pool deletion in both modes", func() {
		for _, scenario := range []struct{ skip, recreated bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
			cfg, root, pool, refs := ocpPoolConsumerFixture()
			network := ocpResourceObject(cleanSriovGroup, "SriovNetwork", config.DefaultSriovOperatorNamespace, "selected-network", map[string]interface{}{"ipam": `{"type":"nv-ipam","poolName":"selected-pool"}`})
			refs = append(refs, ocpResourceRef(network))
			foreign := ocpResourceObject(cleanSriovGroup, "SriovIBNetwork", "other-system", "late-consumer", map[string]interface{}{"ipam": `{"type":"nv-ipam","poolName":"selected-pool"}`})
			if scenario.recreated {
				foreign = network.DeepCopy()
				foreign.SetUID("recreated-network")
			}
			foreign.SetResourceVersion("")
			base := ocpResourceTestClient(root, pool, network)
			c := interceptor.NewClient(base, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				gomega.Expect(obj.GetName()).To(gomega.Equal(network.GetName()))
				if err := c.Delete(ctx, obj, opts...); err != nil {
					return err
				}
				return c.Create(ctx, foreign)
			}})
			count, err := cleanOCPResources(context.Background(), c, cfg, refs, CleanOptions{Namespace: "operator-system", SkipWait: scenario.skip, PollInterval: time.Millisecond})
			gomega.Expect(count).To(gomega.Equal(1))
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(foreign.GetName())))
			gomega.Expect(base.Get(context.Background(), client.ObjectKeyFromObject(root), root)).To(gomega.Succeed())
			gomega.Expect(base.Get(context.Background(), client.ObjectKeyFromObject(pool), pool)).To(gomega.Succeed())
		}
	})

	ginkgo.It("blocks a surviving NAD after its selected parent disappears in skip-wait mode", func() {
		cfg, root, pool, refs := ocpPoolConsumerFixture()
		network := ocpResourceObject(cleanSriovGroup, "SriovNetwork", config.DefaultSriovOperatorNamespace, "selected-network", map[string]interface{}{"networkNamespace": "unrelated-application", "ipam": `{"type":"nv-ipam","poolName":"selected-pool"}`})
		nad := ocpConsumerNAD("selected-network", `{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"selected-pool"}}`)
		nad.SetAnnotations(map[string]string{"sriovnetwork.openshift.io/owner-ref": "SriovNetwork.sriovnetwork.openshift.io/" + network.GetNamespace() + "/" + network.GetName()})
		refs = append(refs, ocpResourceRef(network))
		c := ocpResourceTestClient(root, pool, network, nad)
		count, err := cleanOCPResources(context.Background(), c, cfg, refs, CleanOptions{Namespace: "operator-system", SkipWait: true, PollInterval: time.Millisecond})
		gomega.Expect(count).To(gomega.Equal(1))
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("surviving NetworkAttachmentDefinition")))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(pool), pool)).To(gomega.Succeed())
	})
})
