// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/profiles"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"
)

func ocpCleanFixture() (*config.LaunchKitConfig, *bundle.Bundle, *unstructured.Unstructured) {
	cfg := &config.LaunchKitConfig{Flavor: config.FlavorOCP, NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-system"}}
	artifacts, err := bundle.FromFiles([]bundle.File{{Name: "10-policy.yaml", Content: "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: policy\n  annotations:\n    nvidia.kubernetes-launch-kit.version: test-version\nspec:\n  nvIpam:\n    enableWebhook: false\n"}})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	obj := artifacts.Documents()[0].ObjectCopy()
	obj.SetUID(types.UID("original-uid"))
	obj.SetResourceVersion("1")
	obj.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "l8k", Operation: metav1.ManagedFieldsOperationApply, APIVersion: obj.GetAPIVersion(), FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:nvIpam":{"f:enableWebhook":{}}}}`)}})
	return cfg, artifacts, obj
}

var _ = ginkgo.Describe("OpenShift exact cleanup inventory", func() {
	ginkgo.It("retains deployment phases and source document order instead of sorting by Kind", func() {
		cfg, _, _ := ocpCleanFixture()
		var files []bundle.File
		for _, entry := range []struct{ file, group, kind, namespace, name string }{
			{"01-network.yaml", "mellanox.com/v1alpha1", "MacvlanNetwork", "", "z-network"},
			{"02-pools.yaml", "nv-ipam.nvidia.com/v1alpha1", "IPPool", "operator-system", "z-pool"},
			{"03-node.yaml", "mellanox.com/v1alpha1", "NicNodePolicy", "", "z-node"},
			{"04-root.yaml", "mellanox.com/v1alpha1", "NicClusterPolicy", "", "root"},
			{"05-node.yaml", "mellanox.com/v1alpha1", "NicNodePolicy", "", "a-node"},
		} {
			files = append(files, bundle.File{Name: entry.file, Content: fmt.Sprintf("apiVersion: %s\nkind: %s\nmetadata:\n  name: %s\n  namespace: %q\nspec:\n  enabled: true\n", entry.group, entry.kind, entry.name, entry.namespace)})
		}
		artifacts, err := bundle.FromFiles(files)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		plan, err := ocpCleanDesiredPlan(cfg, artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		var names []string
		for _, ref := range plan.refs {
			names = append(names, ref.Name)
		}
		gomega.Expect(names).To(gomega.Equal([]string{"root", "z-node", "a-node", "z-network", "z-pool"}))
	})

	ginkgo.It("accepts legacy live apply provenance and keeps absent exact targets without a deletion UID", func() {
		cfg, artifacts, live := ocpCleanFixture()
		c := fake.NewClientBuilder().WithReturnManagedFields().WithObjects(live).Build()
		plan, err := prepareOCPCleanPlan(context.Background(), c, cfg, artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.refs).To(gomega.HaveLen(1))
		gomega.Expect(plan.refs[0].UID).To(gomega.Equal(live.GetUID()))
		gomega.Expect(plan.refs[0].Desired).NotTo(gomega.BeNil())
		gomega.Expect(artifacts.Documents()[0].ObjectCopy().GetAnnotations()).NotTo(gomega.HaveKey(ocpCleanOwnerAnnotation))
		plan, err = prepareOCPCleanPlan(context.Background(), fake.NewClientBuilder().WithReturnManagedFields().Build(), cfg, artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.refs).To(gomega.HaveLen(1))
		gomega.Expect(plan.refs[0].UID).To(gomega.BeEmpty())
	})

	ginkgo.It("fails the complete plan on inspection denial without listing or mutating", func() {
		cfg, artifacts, _ := ocpCleanFixture()
		denied := errors.New("inspection denied")
		c := fake.NewClientBuilder().WithReturnManagedFields().WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return denied
			},
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				ginkgo.Fail("exact cleanup must never list deletion candidates")
				return nil
			},
		}).Build()
		plan, err := prepareOCPCleanPlan(context.Background(), c, cfg, artifacts)
		gomega.Expect(errors.Is(err, denied)).To(gomega.BeTrue())
		gomega.Expect(plan.refs).To(gomega.BeEmpty())
	})

	ginkgo.DescribeTable("rejects ambiguous live ownership", func(change func(*unstructured.Unstructured), reason string) {
		cfg, artifacts, live := ocpCleanFixture()
		change(live)
		_, err := prepareOCPCleanPlan(context.Background(), fake.NewClientBuilder().WithReturnManagedFields().WithObjects(live).Build(), cfg, artifacts)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(reason)))
	},
		ginkgo.Entry("version annotation alone", func(obj *unstructured.Unstructured) { obj.SetManagedFields(nil) }, "server-side apply ownership"),
		ginkgo.Entry("foreign owner", func(obj *unstructured.Unstructured) {
			obj.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "foreign", UID: "foreign-uid"}})
		}, "another controller"),
		ginkgo.Entry("drifted explicit false", func(obj *unstructured.Unstructured) {
			gomega.Expect(unstructured.SetNestedField(obj.Object, true, "spec", "nvIpam", "enableWebhook")).To(gomega.Succeed())
		}, "live spec differs"),
		ginkgo.Entry("foreign spec field owner", func(obj *unstructured.Unstructured) {
			fields := obj.GetManagedFields()
			fields = append(fields, metav1.ManagedFieldsEntry{Manager: "other", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: obj.GetAPIVersion(), FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:other":{}}}`)})
			obj.SetManagedFields(fields)
		}, "outside Launch Kit"),
		ginkgo.Entry("different bundle", func(obj *unstructured.Unstructured) { setOCPCleanOwner(obj, "foreign") }, "different OpenShift bundle"),
		ginkgo.Entry("retained adoption", func(obj *unstructured.Unstructured) {
			a := obj.GetAnnotations()
			a[ocpCleanRetainAnnotation] = "true"
			obj.SetAnnotations(a)
		}, "retained"),
	)

	ginkgo.It("checks UID bindings and rejects replacements even with copied provenance", func() {
		cfg, artifacts, live := ocpCleanFixture()
		owner, err := ocpBundleOwner(artifacts, "operator-system")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		setOCPCleanOwner(live, owner)
		a := live.GetAnnotations()
		a[ocpCleanUIDAnnotation] = "previous-uid"
		live.SetAnnotations(a)
		_, err = prepareOCPCleanPlan(context.Background(), fake.NewClientBuilder().WithReturnManagedFields().WithObjects(live).Build(), cfg, artifacts)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("UID differs")))
		a[ocpCleanUIDAnnotation] = string(live.GetUID())
		live.SetAnnotations(a)
		_, err = prepareOCPCleanPlan(context.Background(), fake.NewClientBuilder().WithReturnManagedFields().WithObjects(live).Build(), cfg, artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("matches associative list keys conservatively without accepting arbitrary list inclusion", func() {
		gomega.Expect(ocpSpecContains(map[string]any{"explicit": false, "default": true}, map[string]any{"explicit": false})).To(gomega.BeTrue())
		gomega.Expect(ocpSpecContains([]any{"ours", "foreign"}, []any{"ours"})).To(gomega.BeFalse())
		gomega.Expect(ocpSpecContains([]any{"b", "a"}, []any{"a", "b"})).To(gomega.BeFalse())
	})

	ginkgo.It("accepts only defaults from the exact served CRD schema and preserves explicit false", func() {
		cfg, artifacts, live := ocpCleanFixture()
		plan, err := ocpCleanDesiredPlan(cfg, artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(unstructured.SetNestedField(live.Object, int64(3), "spec", "defaultCount")).To(gomega.Succeed())
		crd := &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "nicclusterpolicies.mellanox.com"},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "mellanox.com", Scope: apiextensionsv1.ClusterScoped,
				Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "NicClusterPolicy", Plural: "nicclusterpolicies"},
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true, Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{"spec": {Type: "object", XPreserveUnknownFields: ptr.To(true), Properties: map[string]apiextensionsv1.JSONSchemaProps{"defaultCount": {Type: "integer", Default: &apiextensionsv1.JSON{Raw: []byte(`3`)}}}}},
				}}}},
			},
		}
		scheme := runtime.NewScheme()
		gomega.Expect(apiextensionsv1.AddToScheme(scheme)).To(gomega.Succeed())
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(crd).Build()
		gomega.Expect(validateOCPCleanLiveOwnership(context.Background(), c, plan.refs[0].Desired, live)).To(gomega.Succeed())
		gomega.Expect(artifacts.Documents()[0].ObjectCopy().Object["spec"]).To(gomega.Equal(map[string]any{"nvIpam": map[string]any{"enableWebhook": false}}))
		gomega.Expect(unstructured.SetNestedField(live.Object, true, "spec", "nvIpam", "enableWebhook")).To(gomega.Succeed())
		gomega.Expect(validateOCPCleanLiveOwnership(context.Background(), c, plan.refs[0].Desired, live)).To(gomega.MatchError(gomega.ContainSubstring("live spec differs")))
		gomega.Expect(unstructured.SetNestedField(live.Object, false, "spec", "nvIpam", "enableWebhook")).To(gomega.Succeed())
		crd.Spec.Versions[0].Served = false
		gomega.Expect(c.Update(context.Background(), crd)).To(gomega.Succeed())
		gomega.Expect(validateOCPCleanLiveOwnership(context.Background(), c, plan.refs[0].Desired, live)).To(gomega.MatchError(gomega.ContainSubstring("not served")))
	})

	ginkgo.It("rejects a stale desired subset with additional live l8k-owned components for legacy and UID-bound objects", func() {
		for _, bound := range []bool{false, true} {
			cfg, artifacts, live := ocpCleanFixture()
			plan, err := ocpCleanDesiredPlan(cfg, artifacts)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(unstructured.SetNestedField(live.Object, "driver", "spec", "ofedDriver", "image")).To(gomega.Succeed())
			fields := live.GetManagedFields()
			fields[0].FieldsV1 = metav1.NewFieldsV1(`{"f:spec":{"f:nvIpam":{"f:enableWebhook":{}},"f:ofedDriver":{"f:image":{}}}}`)
			live.SetManagedFields(fields)
			if bound {
				setOCPCleanOwner(live, plan.owner)
				a := live.GetAnnotations()
				a[ocpCleanUIDAnnotation] = string(live.GetUID())
				live.SetAnnotations(a)
			}
			gomega.Expect(validateOCPCleanOwnership(plan.refs[0].Desired, live)).To(gomega.MatchError(gomega.ContainSubstring("live spec differs")))
		}
	})

	ginkgo.It("keeps deployment identity stable across regrouping while separating operator namespaces", func() {
		_, artifacts, _ := ocpCleanFixture()
		owner, err := ocpBundleOwner(artifacts, "operator-system")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		files := append(artifacts.Files(), bundle.File{Name: "20-new-policy.yaml", Content: "apiVersion: mellanox.com/v1alpha1\nkind: NicNodePolicy\nmetadata:\n  name: newly-selected-group\n"})
		regenerated, err := bundle.FromFiles(files)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		updatedOwner, err := ocpBundleOwner(regenerated, "operator-system")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(updatedOwner).To(gomega.Equal(owner))
		otherOwner, err := ocpBundleOwner(artifacts, "another-operator-system")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(otherOwner).NotTo(gomega.Equal(owner))
	})

	ginkgo.It("requires complete desired field ownership and allows create Update entries only with a matching UID binding", func() {
		cfg, artifacts, live := ocpCleanFixture()
		plan, err := ocpCleanDesiredPlan(cfg, artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		fields := live.GetManagedFields()
		fields[0].FieldsV1 = metav1.NewFieldsV1(`{"f:spec":{"f:nvIpam":{".":{}}}}`)
		live.SetManagedFields(fields)
		gomega.Expect(validateOCPCleanOwnership(plan.refs[0].Desired, live)).To(gomega.MatchError(gomega.ContainSubstring("server-side apply ownership")))
		fields[0].FieldsV1 = metav1.NewFieldsV1(`{"f:spec":{"f:nvIpam":{"f:enableWebhook":{}}}}`)
		created := fields[0]
		created.Operation = metav1.ManagedFieldsOperationUpdate
		fields = append(fields, created)
		live.SetManagedFields(fields)
		gomega.Expect(validateOCPCleanOwnership(plan.refs[0].Desired, live)).To(gomega.MatchError(gomega.ContainSubstring("outside Launch Kit")))
		setOCPCleanOwner(live, plan.owner)
		annotations := live.GetAnnotations()
		annotations[ocpCleanUIDAnnotation] = string(live.GetUID())
		live.SetAnnotations(annotations)
		gomega.Expect(validateOCPCleanOwnership(plan.refs[0].Desired, live)).To(gomega.Succeed())
	})

	ginkgo.It("selects all documents while retaining shared, installation, example and unrelated resources", func() {
		cfg, artifacts, _ := ocpCleanFixture()
		files := append(artifacts.Files(), bundle.File{Name: "20-multi.yaml", Content: "apiVersion: sriovnetwork.openshift.io/v1\nkind: SriovNetwork\nmetadata:\n  name: net-a\n  namespace: openshift-sriov-network-operator\n---\napiVersion: sriovnetwork.openshift.io/v1\nkind: SriovIBNetwork\nmetadata:\n  name: net-b\n  namespace: openshift-sriov-network-operator\n"}, bundle.File{Name: "01-config.yaml", Content: "apiVersion: sriovnetwork.openshift.io/v1\nkind: SriovOperatorConfig\nmetadata:\n  name: default\n  namespace: openshift-sriov-network-operator\n"}, bundle.File{Name: "40-example.yaml", Content: "apiVersion: sriovnetwork.openshift.io/v1\nkind: SriovNetwork\nmetadata:\n  name: example\n  namespace: openshift-sriov-network-operator\n"}, bundle.File{Name: "00-subscriptions.yaml", Content: "apiVersion: operators.coreos.com/v1alpha1\nkind: Subscription\nmetadata:\n  name: operator\n  namespace: operator-system\n"}, bundle.File{Name: "90-other.yaml", Content: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: unrelated\n  namespace: operator-system\n"})
		combined, err := bundle.FromFiles(files)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		plan, err := ocpCleanDesiredPlan(cfg, combined)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.refs).To(gomega.HaveLen(3))
		gomega.Expect(plan.operatorConfigs).To(gomega.HaveLen(1))
		gomega.Expect(plan.retained).To(gomega.ConsistOf(gomega.ContainSubstring("ConfigMap operator-system/unrelated")))
	})

	ginkgo.It("accepts an explicit Multus allowlist artifact outside shared singleton fallback", func() {
		cfg, artifacts, _ := ocpCleanFixture()
		files := append(artifacts.Files(), bundle.File{Name: ocpSysctlAllowlistPatchFilename, Content: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cni-sysctl-allowlist\n  namespace: openshift-multus\ndata:\n  allowlist.conf: ^net.ipv4.conf.IFNAME.arp_ignore$\n"})
		combined, err := bundle.FromFiles(files)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		plan, err := ocpCleanDesiredPlan(cfg, combined)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.operatorConfigs).To(gomega.HaveLen(1))
		gomega.Expect(isOCPSysctlAllowlistPatch(plan.operatorConfigs[0])).To(gomega.BeTrue())
	})

	ginkgo.It("rejects wrong namespaces and platform SR-IOV default policies", func() {
		cfg, artifacts, _ := ocpCleanFixture()
		for _, test := range []struct{ name, ns, kind string }{{"default", config.DefaultSriovOperatorNamespace, "SriovNetworkNodePolicy"}, {"default", config.DefaultSriovOperatorNamespace, "SriovNetworkPoolConfig"}, {"custom", "foreign", "SriovNetwork"}} {
			files := append(artifacts.Files(), bundle.File{Name: "20-policy.yaml", Content: fmt.Sprintf("apiVersion: sriovnetwork.openshift.io/v1\nkind: %s\nmetadata:\n  name: %s\n  namespace: %s\n", test.kind, test.name, test.ns)})
			combined, err := bundle.FromFiles(files)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = ocpCleanDesiredPlan(cfg, combined)
			gomega.Expect(err).To(gomega.HaveOccurred())
		}
	})

	ginkgo.It("uses the same supported-kind boundary for every rendered OpenShift profile", func() {
		for _, profile := range []struct{ dir, fabric, deployment string }{
			{"sriov-ethernet-rdma-ocp", "ethernet", "sriov"}, {"sriov-ib-rdma-ocp", "infiniband", "sriov"},
			{"host-device-rdma-ocp", "ethernet", "host_device"}, {"ipoib-rdma-shared-ocp", "infiniband", "rdma_shared"}, {"macvlan-rdma-shared-ocp", "ethernet", "rdma_shared"},
		} {
			cfg, err := config.LoadFullConfig(filepath.Join("testdata", "grouping", "mixed-same-type.yaml"), ctrllog.Log)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			cfg.Flavor = config.FlavorOCP
			gomega.Expect(config.ApplyFlavorDefaults(cfg)).To(gomega.Succeed())
			cfg.Profile = &config.Profile{Fabric: profile.fabric, Deployment: profile.deployment, Multirail: true}
			cfg.NetworkOperator.SelectedRelease = "26.7"
			cfg.ClusterConfig = cfg.ClusterConfig[:1]
			cfg.ClusterConfig[0].WorkerNodes = []string{"worker-a"}
			cfg.ClusterConfig[0].NodeSelector = map[string]string{"kubernetes.io/hostname": "worker-a"}
			profileDir, err := filepath.Abs(filepath.Join("..", "..", "profiles", profile.dir))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			data, err := os.ReadFile(filepath.Join(profileDir, "profile.yaml"))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			p := &profiles.Profile{}
			gomega.Expect(yaml.Unmarshal(data, p)).To(gomega.Succeed())
			p.UpdateManifestsPaths(profileDir)
			rendered, err := (&NetworkOperatorPlugin{}).GenerateProfileDeploymentFiles(p, cfg)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), profile.dir)
			var files []bundle.File
			for name, content := range rendered {
				files = append(files, bundle.File{Name: name, Content: content})
			}
			artifacts, err := bundle.FromFiles(files)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			plan, err := ocpCleanDesiredPlan(cfg, artifacts)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), profile.dir)
			gomega.Expect(plan.refs).NotTo(gomega.BeEmpty())
			for _, ref := range plan.refs {
				gomega.Expect(isOCPCleanKind(ref.GVK)).To(gomega.BeTrue())
				gomega.Expect(isOCPOperatorConfig(ref.Desired)).To(gomega.BeFalse())
			}
		}
	})
})

var _ = ginkgo.Describe("OpenShift cleanup ownership during apply", func() {
	ginkgo.It("does not promote original ambiguous legacy ownership when generation metadata appears before the refreshed read", func() {
		cfg, artifacts, live := ocpCleanFixture()
		plan, err := ocpCleanDesiredPlan(cfg, artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		annotations := live.GetAnnotations()
		delete(annotations, launchKitVersionAnnotation)
		live.SetAnnotations(annotations)
		reads, applies := 0, 0
		c := fake.NewClientBuilder().WithReturnManagedFields().WithObjects(live).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				err := c.Get(ctx, key, obj, opts...)
				reads++
				if err == nil && reads == 1 {
					updated := obj.(*unstructured.Unstructured).DeepCopy()
					a := updated.GetAnnotations()
					a[launchKitVersionAnnotation] = "test-version"
					updated.SetAnnotations(a)
					gomega.Expect(c.Update(ctx, updated)).To(gomega.Succeed())
				}
				return err
			},
			Apply: func(_ context.Context, _ client.WithWatch, ac runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				applies++
				gomega.Expect(ac.(client.Object).GetAnnotations()[ocpCleanRetainAnnotation]).To(gomega.Equal("true"))
				return nil
			},
		}).Build()
		gomega.Expect(applyOCPUnstructured(context.Background(), c, plan.refs[0].Desired.DeepCopy(), false)).To(gomega.Succeed())
		gomega.Expect(reads).To(gomega.Equal(2))
		gomega.Expect(applies).To(gomega.Equal(1))
	})

	ginkgo.It("refuses terminating initial and refreshed targets without a subsequent SSA write", func() {
		for _, terminatingRead := range []int{1, 2, 3} {
			cfg, artifacts, live := ocpCleanFixture()
			plan, err := ocpCleanDesiredPlan(cfg, artifacts)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			reads, applies := 0, 0
			c := fake.NewClientBuilder().WithReturnManagedFields().WithObjects(live).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					err := c.Get(ctx, key, obj, opts...)
					reads++
					if err == nil && reads == terminatingRead {
						timestamp := metav1.Now()
						obj.SetDeletionTimestamp(&timestamp)
					}
					return err
				},
				Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
					applies++
					gomega.Expect(terminatingRead).To(gomega.Equal(3), "terminating object must not be applied")
					return apierrors.NewConflict(schema.GroupResource{Group: "mellanox.com", Resource: "nicclusterpolicies"}, live.GetName(), errors.New("status advanced"))
				},
			}).Build()
			gomega.Expect(applyOCPUnstructured(context.Background(), c, plan.refs[0].Desired.DeepCopy(), false)).To(gomega.MatchError(gomega.ContainSubstring("terminating")))
			gomega.Expect(reads).To(gomega.Equal(terminatingRead))
			if terminatingRead == 3 {
				gomega.Expect(applies).To(gomega.Equal(1))
			} else {
				gomega.Expect(applies).To(gomega.BeZero())
			}
		}
	})

	ginkgo.It("refuses binding a newly created target that starts terminating before the metadata patch", func() {
		cfg, artifacts, _ := ocpCleanFixture()
		plan, err := ocpCleanDesiredPlan(cfg, artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		created := false
		c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				obj.SetUID("created-uid")
				created = true
				return c.Create(ctx, obj, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				err := c.Get(ctx, key, obj, opts...)
				if err == nil && created {
					timestamp := metav1.Now()
					obj.SetDeletionTimestamp(&timestamp)
				}
				return err
			},
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				ginkgo.Fail("terminating object must not be bound")
				return nil
			},
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				ginkgo.Fail("terminating object must not be applied")
				return nil
			},
		}).Build()
		gomega.Expect(applyOCPUnstructured(context.Background(), c, plan.refs[0].Desired.DeepCopy(), false)).To(gomega.MatchError(gomega.ContainSubstring("terminating")))
	})

	ginkgo.It("retries final SSA conflicts after status writes with a fresh resourceVersion and unchanged intent", func() {
		cfg, artifacts, live := ocpCleanFixture()
		plan, err := ocpCleanDesiredPlan(cfg, artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		setOCPCleanOwner(live, plan.owner)
		annotations := live.GetAnnotations()
		annotations[ocpCleanUIDAnnotation] = string(live.GetUID())
		live.SetAnnotations(annotations)
		attempts := 0
		firstVersion := ""
		c := fake.NewClientBuilder().WithReturnManagedFields().WithObjects(live).WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, c client.WithWatch, ac runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				attempts++
				candidate := ac.(client.Object)
				gomega.Expect(candidate.GetUID()).To(gomega.Equal(live.GetUID()))
				gomega.Expect(candidate.GetAnnotations()).NotTo(gomega.HaveKey(ocpCleanRetainAnnotation))
				if attempts == 1 {
					firstVersion = candidate.GetResourceVersion()
					current := live.DeepCopy()
					gomega.Expect(c.Get(ctx, client.ObjectKeyFromObject(live), current)).To(gomega.Succeed())
					gomega.Expect(unstructured.SetNestedField(current.Object, "ready", "status", "state")).To(gomega.Succeed())
					fields := current.GetManagedFields()
					fields = append(fields, metav1.ManagedFieldsEntry{Manager: "controller", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: current.GetAPIVersion(), Subresource: "status", FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:status":{"f:state":{}}}`)})
					current.SetManagedFields(fields)
					gomega.Expect(c.Update(ctx, current)).To(gomega.Succeed())
					candidate.SetLabels(map[string]string{"failed-response": "discard"})
					return apierrors.NewConflict(schema.GroupResource{Group: "mellanox.com", Resource: "nicclusterpolicies"}, candidate.GetName(), errors.New("status advanced"))
				}
				gomega.Expect(candidate.GetResourceVersion()).NotTo(gomega.Equal(firstVersion))
				gomega.Expect(candidate.GetLabels()).NotTo(gomega.HaveKey("failed-response"))
				return nil
			},
		}).Build()
		gomega.Expect(applyOCPUnstructured(context.Background(), c, plan.refs[0].Desired.DeepCopy(), false)).To(gomega.Succeed())
		gomega.Expect(attempts).To(gomega.Equal(2))
	})

	ginkgo.DescribeTable("refuses a changed authorization after a final SSA conflict", func(change func(*unstructured.Unstructured), reason string) {
		cfg, artifacts, live := ocpCleanFixture()
		plan, err := ocpCleanDesiredPlan(cfg, artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		setOCPCleanOwner(live, plan.owner)
		annotations := live.GetAnnotations()
		annotations[ocpCleanUIDAnnotation] = string(live.GetUID())
		live.SetAnnotations(annotations)
		attempts := 0
		c := fake.NewClientBuilder().WithReturnManagedFields().WithObjects(live).WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				ginkgo.Fail("retry must not create any object")
				return nil
			},
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				ginkgo.Fail("retry must not delete any object")
				return nil
			},
			Apply: func(ctx context.Context, c client.WithWatch, ac runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				attempts++
				current := live.DeepCopy()
				gomega.Expect(c.Get(ctx, client.ObjectKeyFromObject(live), current)).To(gomega.Succeed())
				before := current.DeepCopy()
				change(current)
				if current.GetUID() != before.GetUID() {
					gomega.Expect(c.Delete(ctx, before)).To(gomega.Succeed())
					current.SetResourceVersion("")
					gomega.Expect(c.Create(ctx, current)).To(gomega.Succeed())
				} else {
					gomega.Expect(c.Update(ctx, current)).To(gomega.Succeed())
				}
				return apierrors.NewConflict(schema.GroupResource{Group: "mellanox.com", Resource: "nicclusterpolicies"}, ac.(client.Object).GetName(), errors.New("concurrent change"))
			},
		}).Build()
		gomega.Expect(applyOCPUnstructured(context.Background(), c, plan.refs[0].Desired.DeepCopy(), false)).To(gomega.MatchError(gomega.ContainSubstring(reason)))
		gomega.Expect(attempts).To(gomega.Equal(1))
	},
		ginkgo.Entry("replacement", func(obj *unstructured.Unstructured) { obj.SetUID("replacement-uid") }, "another incarnation"),
		ginkgo.Entry("foreign bundle marker", func(obj *unstructured.Unstructured) { setOCPCleanOwner(obj, "foreign-bundle") }, "different OpenShift bundle"),
		ginkgo.Entry("removed ownership binding", func(obj *unstructured.Unstructured) {
			a := obj.GetAnnotations()
			delete(a, ocpCleanOwnerAnnotation)
			delete(a, ocpCleanUIDAnnotation)
			obj.SetAnnotations(a)
		}, "ownership binding changed"),
		ginkgo.Entry("concurrent spec change", func(obj *unstructured.Unstructured) {
			gomega.Expect(unstructured.SetNestedField(obj.Object, true, "spec", "nvIpam", "enableWebhook")).To(gomega.Succeed())
		}, "spec or its ownership changed"),
		ginkgo.Entry("foreign spec manager with unchanged spec", func(obj *unstructured.Unstructured) {
			fields := obj.GetManagedFields()
			fields = append(fields, metav1.ManagedFieldsEntry{Manager: "site", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: obj.GetAPIVersion(), FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:nvIpam":{"f:enableWebhook":{}}}}`)})
			obj.SetManagedFields(fields)
		}, "spec or its ownership changed"),
	)

	ginkgo.It("keeps ambiguous adoption retained across final SSA retries", func() {
		cfg, artifacts, live := ocpCleanFixture()
		plan, err := ocpCleanDesiredPlan(cfg, artifacts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		live.SetManagedFields(nil)
		attempts := 0
		c := fake.NewClientBuilder().WithReturnManagedFields().WithObjects(live).WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, ac runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				attempts++
				candidate := ac.(client.Object)
				gomega.Expect(candidate.GetAnnotations()[ocpCleanRetainAnnotation]).To(gomega.Equal("true"))
				if attempts == 1 {
					candidate.SetAnnotations(nil)
					return apierrors.NewConflict(schema.GroupResource{Group: "mellanox.com", Resource: "nicclusterpolicies"}, candidate.GetName(), errors.New("status advanced"))
				}
				return nil
			},
		}).Build()
		gomega.Expect(applyOCPUnstructured(context.Background(), c, plan.refs[0].Desired.DeepCopy(), false)).To(gomega.Succeed())
		gomega.Expect(attempts).To(gomega.Equal(2))
	})

	ginkgo.It("bounds final SSA conflict retries and never retries an ambiguous write result", func() {
		for _, ambiguous := range []bool{false, true} {
			cfg, artifacts, live := ocpCleanFixture()
			plan, err := ocpCleanDesiredPlan(cfg, artifacts)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			attempts := 0
			c := fake.NewClientBuilder().WithReturnManagedFields().WithObjects(live).WithInterceptorFuncs(interceptor.Funcs{
				Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
					attempts++
					if ambiguous {
						return errors.New("ambiguous transport result")
					}
					return apierrors.NewConflict(schema.GroupResource{Group: "mellanox.com", Resource: "nicclusterpolicies"}, live.GetName(), errors.New("status advanced"))
				},
			}).Build()
			gomega.Expect(applyOCPUnstructured(context.Background(), c, plan.refs[0].Desired.DeepCopy(), false)).To(gomega.HaveOccurred())
			if ambiguous {
				gomega.Expect(attempts).To(gomega.Equal(1))
			} else {
				gomega.Expect(attempts).To(gomega.Equal(retry.DefaultRetry.Steps))
			}
		}
	})

	ginkgo.It("retries UID binding after status advances the created object's resource version", func() {
		cfg, artifacts, _ := ocpCleanFixture()
		selected, err := prepareDeploymentObjects(artifacts, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		patches := 0
		c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				obj.SetUID("created-uid")
				return c.Create(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				if patches == 1 {
					live := obj.(*unstructured.Unstructured).DeepCopy()
					gomega.Expect(c.Get(ctx, client.ObjectKeyFromObject(obj), live)).To(gomega.Succeed())
					gomega.Expect(unstructured.SetNestedField(live.Object, "ready", "status", "state")).To(gomega.Succeed())
					gomega.Expect(c.Update(ctx, live)).To(gomega.Succeed())
					return apierrors.NewConflict(schema.GroupResource{Group: "mellanox.com", Resource: "nicclusterpolicies"}, obj.GetName(), errors.New("status advanced"))
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				return nil
			},
		}).Build()
		gomega.Expect(prepareOCPCleanDeployment(context.Background(), c, cfg, artifacts, &selected)).To(gomega.Succeed())
		gomega.Expect(applyOCPUnstructured(context.Background(), c, selected.ncp, false)).To(gomega.Succeed())
		gomega.Expect(patches).To(gomega.Equal(2))
		live := selected.ncp.DeepCopy()
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(live), live)).To(gomega.Succeed())
		gomega.Expect(live.GetAnnotations()[ocpCleanUIDAnnotation]).To(gomega.Equal("created-uid"))
		state, _, err := unstructured.NestedString(live.Object, "status", "state")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(state).To(gomega.Equal("ready"))
	})

	ginkgo.It("refuses racing creators without an apply or ownership patch", func() {
		cfg, artifacts, _ := ocpCleanFixture()
		selected, err := prepareDeploymentObjects(artifacts, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return apierrors.NewAlreadyExists(schema.GroupResource{Group: "mellanox.com", Resource: "nicclusterpolicies"}, "policy")
			},
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				ginkgo.Fail("racing object must not be applied")
				return nil
			},
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				ginkgo.Fail("racing object must not be patched")
				return nil
			},
		}).Build()
		gomega.Expect(prepareOCPCleanDeployment(context.Background(), c, cfg, artifacts, &selected)).To(gomega.Succeed())
		gomega.Expect(applyOCPUnstructured(context.Background(), c, selected.ncp, false)).To(gomega.MatchError(gomega.ContainSubstring("without adoption")))
	})

	ginkgo.It("blocks replay after a partial create whose UID binding failed", func() {
		cfg, artifacts, _ := ocpCleanFixture()
		selected, err := prepareDeploymentObjects(artifacts, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				obj.SetUID("partial-uid")
				return c.Create(ctx, obj, opts...)
			},
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return errors.New("binding failed")
			},
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				ginkgo.Fail("unbound object must not be applied")
				return nil
			},
		}).Build()
		gomega.Expect(prepareOCPCleanDeployment(context.Background(), c, cfg, artifacts, &selected)).To(gomega.Succeed())
		gomega.Expect(applyOCPUnstructured(context.Background(), c, selected.ncp, false)).To(gomega.MatchError(gomega.ContainSubstring("inspect partial deployment")))
		gomega.Expect(applyOCPUnstructured(context.Background(), c, selected.ncp, false)).To(gomega.MatchError(gomega.ContainSubstring("incomplete OpenShift ownership binding")))
		_, err = prepareOCPCleanPlan(context.Background(), c, cfg, artifacts)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("incomplete OpenShift ownership binding")))
	})

	ginkgo.It("atomically creates new objects and binds their actual UID before SSA", func() {
		cfg, artifacts, _ := ocpCleanFixture()
		selected, err := prepareDeploymentObjects(artifacts, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		applied := false
		c := fake.NewClientBuilder().WithReturnManagedFields().WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				obj.SetUID("created-uid")
				return c.Create(ctx, obj, opts...)
			},
			Apply: func(_ context.Context, _ client.WithWatch, ac runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				obj := ac.(client.Object)
				gomega.Expect(obj.GetUID()).To(gomega.Equal(types.UID("created-uid")))
				gomega.Expect(obj.GetResourceVersion()).NotTo(gomega.BeEmpty())
				gomega.Expect(obj.GetAnnotations()[ocpCleanUIDAnnotation]).To(gomega.Equal("created-uid"))
				gomega.Expect(obj.GetAnnotations()).NotTo(gomega.HaveKey(ocpCleanRetainAnnotation))
				applied = true
				return nil
			},
		}).Build()
		gomega.Expect(prepareOCPCleanDeployment(context.Background(), c, cfg, artifacts, &selected)).To(gomega.Succeed())
		gomega.Expect(applyOCPUnstructured(context.Background(), c, selected.ncp, false)).To(gomega.Succeed())
		gomega.Expect(applied).To(gomega.BeTrue())
	})

	ginkgo.It("preserves historical adoption but permanently retains ambiguous existing resources", func() {
		cfg, artifacts, live := ocpCleanFixture()
		live.SetManagedFields(nil)
		selected, err := prepareDeploymentObjects(artifacts, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		applied := false
		c := fake.NewClientBuilder().WithReturnManagedFields().WithObjects(live).WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, ac runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				obj := ac.(client.Object)
				gomega.Expect(obj.GetUID()).To(gomega.Equal(live.GetUID()))
				gomega.Expect(obj.GetAnnotations()[ocpCleanRetainAnnotation]).To(gomega.Equal("true"))
				applied = true
				return nil
			},
		}).Build()
		gomega.Expect(prepareOCPCleanDeployment(context.Background(), c, cfg, artifacts, &selected)).To(gomega.Succeed())
		gomega.Expect(applyOCPUnstructured(context.Background(), c, selected.ncp, false)).To(gomega.Succeed())
		gomega.Expect(applied).To(gomega.BeTrue())
	})
})
