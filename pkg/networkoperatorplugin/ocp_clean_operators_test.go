// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

func ocpOperatorCleanFixture() (*config.LaunchKitConfig, *bundle.Bundle, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured) {
	cfg := &config.LaunchKitConfig{Flavor: config.FlavorOCP, NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-system"}, OCPOperatorInstall: &config.OCPOperatorInstallConfig{Manage: true}}
	sub := ocpResourceObject("operators.coreos.com", "Subscription", "operator-system", "custom-subscription", map[string]interface{}{"name": olmNetworkOperatorPkg, "channel": "v26.10", "source": "certified-operators", "sourceNamespace": "openshift-marketplace", "installPlanApproval": "Automatic"})
	sub.SetGroupVersionKind(ocpCleanSubscriptionGVK)
	sub.Object["status"] = map[string]interface{}{"installedCSV": "original-version", "currentCSV": "upgrading-version"}
	csv := ocpResourceObject("operators.coreos.com", "ClusterServiceVersion", "operator-system", "original-version", map[string]interface{}{"version": "26.10.0"})
	csv.SetGroupVersionKind(ocpCleanCSVGVK)
	csv.SetLabels(map[string]string{ocpOperatorPackageLabel(olmNetworkOperatorPkg, "operator-system"): ""})
	group := ocpResourceObject("operators.coreos.com", "OperatorGroup", "operator-system", "custom-group", map[string]interface{}{"targetNamespaces": []interface{}{"operator-system"}})
	group.SetGroupVersionKind(ocpCleanOperatorGroupGVK)
	var files []bundle.File
	for _, entry := range []struct {
		name string
		obj  *unstructured.Unstructured
	}{{"00-subscriptions.yaml", sub}, {"00-operatorgroups.yaml", group}} {
		desired := entry.obj.DeepCopy()
		desired.SetUID("")
		desired.SetResourceVersion("")
		desired.SetManagedFields(nil)
		desired.SetGeneration(0)
		delete(desired.Object, "status")
		data, err := yaml.Marshal(desired.Object)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		files = append(files, bundle.File{Name: entry.name, Content: string(data)})
	}
	files = append(files, bundle.File{Name: "00-operator-namespaces.yaml", Content: "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: operator-system\n"})
	files = append(files, bundle.File{Name: "10-nicclusterpolicy.yaml", Content: "apiVersion: mellanox.com/v1alpha1\nkind: NicClusterPolicy\nmetadata:\n  name: policy\nspec:\n  nvIpam: {}\n"})
	artifacts, err := bundle.FromFiles(files)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return cfg, artifacts, sub, csv, group
}

func ocpOperatorCleanClient(objects ...client.Object) client.WithWatch {
	c := ocpResourceTestClient(objects...)
	for _, gvk := range []schema.GroupVersionKind{ocpCleanSubscriptionGVK, ocpCleanCSVGVK, ocpCleanOperatorGroupGVK, {Group: "nfd.openshift.io", Version: "v1", Kind: "NodeFeatureDiscovery"}, {Group: "maintenance.nvidia.com", Version: "v1alpha1", Kind: "NodeMaintenance"}} {
		c.Scheme().AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		c.Scheme().AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return c
}

var _ = ginkgo.Describe("OpenShift generated OLM teardown", func() {
	ginkgo.It("removes both installed and current CSVs after the exact Subscription, then its dedicated group, preserving namespace contents", func() {
		cfg, artifacts, sub, csv, group := ocpOperatorCleanFixture()
		next := csv.DeepCopy()
		next.SetName("upgrading-version")
		next.SetUID("upgrade-uid")
		namespace := &corev1.Namespace{}
		namespace.Name = "operator-system"
		secret := &corev1.Secret{}
		secret.Namespace = namespace.Name
		secret.Name = "unrelated"
		base := ocpOperatorCleanClient(sub, csv, next, group, namespace, secret)
		var deleted []string
		c := interceptor.NewClient(base, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleted = append(deleted, obj.GetObjectKind().GroupVersionKind().Kind+"/"+obj.GetName())
			return c.Delete(ctx, obj, opts...)
		}})
		before := artifacts.Files()
		plan, err := prepareOCPCleanOperators(context.Background(), c, cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.removesSubscription(sub)).To(gomega.BeTrue())
		gomega.Expect(plan.targets()).To(gomega.ContainElement(gomega.ContainSubstring("Namespace")))
		retained, err := plan.execute(context.Background(), c, CleanOptions{PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(retained).To(gomega.ContainElement(gomega.ContainSubstring("Namespace")))
		gomega.Expect(deleted).To(gomega.Equal([]string{"Subscription/custom-subscription", "ClusterServiceVersion/original-version", "ClusterServiceVersion/upgrading-version", "OperatorGroup/custom-group"}))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(secret), &corev1.Secret{})).To(gomega.Succeed())
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(namespace), &corev1.Namespace{})).To(gomega.Succeed())
		gomega.Expect(artifacts.Files()).To(gomega.Equal(before))
	})

	ginkgo.It("accepts older missing management metadata but retains ambiguous orphan CSVs after the Subscription disappeared", func() {
		cfg, artifacts, _, csv, group := ocpOperatorCleanFixture()
		cfg.OCPOperatorInstall = nil
		unrelated := csv.DeepCopy()
		unrelated.SetName(olmNetworkOperatorPkg + ".looks-similar")
		unrelated.SetUID("foreign")
		unrelated.SetLabels(nil)
		c := ocpOperatorCleanClient(csv, group, unrelated)
		plan, err := prepareOCPCleanOperators(context.Background(), c, cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.operators).To(gomega.BeEmpty())
		retained, err := plan.execute(context.Background(), c, CleanOptions{PollInterval: time.Millisecond, SkipWait: true})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(retained).To(gomega.ContainElement(gomega.ContainSubstring("cannot establish that this bundle installed it")))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(unrelated), unrelated.DeepCopy())).To(gomega.Succeed())
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(csv), csv.DeepCopy())).To(gomega.Succeed())
	})

	ginkgo.It("does no OLM inspection for explicit management false, even if old install manifests remain", func() {
		cfg, artifacts, _, _, _ := ocpOperatorCleanFixture()
		cfg.OCPOperatorInstall.Manage = false
		c := interceptor.NewClient(ocpOperatorCleanClient(), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			ginkgo.Fail("unexpected OLM Get")
			return nil
		}, List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			ginkgo.Fail("unexpected OLM List")
			return nil
		}})
		plan, err := prepareOCPCleanOperators(context.Background(), c, cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.targets()).To(gomega.BeEmpty())
	})

	ginkgo.It("retains an independently installed Subscription without generated provenance", func() {
		cfg, artifacts, sub, csv, group := ocpOperatorCleanFixture()
		sub.SetAnnotations(nil)
		c := ocpOperatorCleanClient(sub, csv, group)
		plan, err := prepareOCPCleanOperators(context.Background(), c, cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.removesSubscription(sub)).To(gomega.BeFalse())
		retained, err := plan.execute(context.Background(), c, CleanOptions{PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(retained).To(gomega.ContainElement(gomega.ContainSubstring("provenance")))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(sub), sub.DeepCopy())).To(gomega.Succeed())
	})

	ginkgo.It("rejects a changed live install spec without mutation", func() {
		cfg, artifacts, sub, csv, group := ocpOperatorCleanFixture()
		gomega.Expect(unstructured.SetNestedField(sub.Object, "different-channel", "spec", "channel")).To(gomega.Succeed())
		_, err := prepareOCPCleanOperators(context.Background(), ocpOperatorCleanClient(sub, csv, group), cfg, artifacts, "test-owner")
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("live install spec differs")))
	})

	ginkgo.It("protects a CSV referenced by another independent Subscription", func() {
		cfg, artifacts, sub, csv, group := ocpOperatorCleanFixture()
		peer := sub.DeepCopy()
		peer.SetName("another")
		peer.SetUID("peer")
		gomega.Expect(unstructured.SetNestedField(peer.Object, "another-package", "spec", "name")).To(gomega.Succeed())
		plan, err := prepareOCPCleanOperators(context.Background(), ocpOperatorCleanClient(sub, csv, group, peer), cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.removesSubscription(sub)).To(gomega.BeFalse())
		gomega.Expect(plan.retained).To(gomega.ContainElement(gomega.ContainSubstring("another Subscription references")))
	})

	ginkgo.It("retains the whole operator when its OLM-created CSV is explicitly protected", func() {
		cfg, artifacts, sub, csv, group := ocpOperatorCleanFixture()
		annotations := csv.GetAnnotations()
		annotations[ocpCleanRetainAnnotation] = "true"
		csv.SetAnnotations(annotations)
		plan, err := prepareOCPCleanOperators(context.Background(), ocpOperatorCleanClient(sub, csv, group), cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.removesSubscription(sub)).To(gomega.BeFalse())
		gomega.Expect(plan.retained).To(gomega.ContainElement(gomega.ContainSubstring("explicitly retained")))
	})

	ginkgo.It("retains shared or global OperatorGroups after uninstalling selected operators", func() {
		cfg, artifacts, sub, csv, group := ocpOperatorCleanFixture()
		gomega.Expect(unstructured.SetNestedStringSlice(group.Object, []string{}, "spec", "targetNamespaces")).To(gomega.Succeed())
		files := artifacts.Files()
		data, err := yaml.Marshal(group.Object)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		for i := range files {
			if files[i].Name == "00-operatorgroups.yaml" {
				files[i].Content = string(data)
			}
		}
		artifacts, err = bundle.FromFiles(files)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		c := ocpOperatorCleanClient(sub, csv, group)
		plan, err := prepareOCPCleanOperators(context.Background(), c, cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		retained, err := plan.execute(context.Background(), c, CleanOptions{PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(retained).To(gomega.ContainElement(gomega.ContainSubstring("global")))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(group), group.DeepCopy())).To(gomega.Succeed())
	})

	ginkgo.It("retains the operator when an external root policy survives cleanup", func() {
		cfg, artifacts, sub, csv, group := ocpOperatorCleanFixture()
		policy := ocpResourceObject("mellanox.com", "NicClusterPolicy", "", "external", map[string]interface{}{"nvIpam": map[string]interface{}{}})
		c := ocpOperatorCleanClient(sub, csv, group, policy)
		plan, err := prepareOCPCleanOperators(context.Background(), c, cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		retained, err := plan.execute(context.Background(), c, CleanOptions{PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(retained).To(gomega.ContainElement(gomega.ContainSubstring("remaining NicClusterPolicy")))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(sub), sub.DeepCopy())).To(gomega.Succeed())
	})

	ginkgo.It("retains the Network Operator for an independent network even without a root policy", func() {
		cfg, artifacts, sub, csv, group := ocpOperatorCleanFixture()
		network := ocpResourceObject("mellanox.com", "MacvlanNetwork", "", "external", map[string]interface{}{"master": "eth1"})
		c := ocpOperatorCleanClient(sub, csv, group, network)
		plan, err := prepareOCPCleanOperators(context.Background(), c, cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		retained, err := plan.execute(context.Background(), c, CleanOptions{PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(retained).To(gomega.ContainElement(gomega.ContainSubstring("MacvlanNetwork")))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(sub), sub.DeepCopy())).To(gomega.Succeed())
	})

	ginkgo.It("rejects a new OLM upgrade target after confirmation", func() {
		cfg, artifacts, sub, csv, group := ocpOperatorCleanFixture()
		c := ocpOperatorCleanClient(sub, csv, group)
		plan, err := prepareOCPCleanOperators(context.Background(), c, cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		next := csv.DeepCopy()
		next.SetName("new-upgrade")
		next.SetUID("new-uid")
		next.SetResourceVersion("")
		gomega.Expect(c.Create(context.Background(), next)).To(gomega.Succeed())
		_, err = plan.execute(context.Background(), c, CleanOptions{PollInterval: time.Millisecond})
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("changed after planning")))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(sub), sub.DeepCopy())).To(gomega.Succeed())
	})

	ginkgo.It("waits for CSV finalizers in skip-wait mode and replans from live state on retry", func() {
		cfg, artifacts, sub, csv, group := ocpOperatorCleanFixture()
		csv.SetFinalizers([]string{"test/finalizer"})
		c := ocpOperatorCleanClient(sub, csv, group)
		plan, err := prepareOCPCleanOperators(context.Background(), c, cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err = plan.execute(ctx, c, CleanOptions{PollInterval: time.Millisecond, SkipWait: true})
		gomega.Expect(errors.Is(err, context.DeadlineExceeded)).To(gomega.BeTrue())
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(csv), csv)).To(gomega.Succeed())
		csv.SetFinalizers(nil)
		gomega.Expect(c.Update(context.Background(), csv)).To(gomega.Succeed())
		plan, err = prepareOCPCleanOperators(context.Background(), c, cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = plan.execute(context.Background(), c, CleanOptions{PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("does not turn a denied live CSV inspection into verified absence", func() {
		cfg, artifacts, sub, _, _ := ocpOperatorCleanFixture()
		denied := errors.New("forbidden")
		c := interceptor.NewClient(ocpOperatorCleanClient(sub), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, objects client.ObjectList, opts ...client.ListOption) error {
			if objects.GetObjectKind().GroupVersionKind().Kind == "ClusterServiceVersionList" {
				return denied
			}
			return c.List(ctx, objects, opts...)
		}})
		_, err := prepareOCPCleanOperators(context.Background(), c, cfg, artifacts, "test-owner")
		gomega.Expect(errors.Is(err, denied)).To(gomega.BeTrue())
	})

	ginkgo.DescribeTable("retains operators needed by concrete independent custom resources", func(pkg, group, version, kind string) {
		consumer := ocpResourceObject(group, kind, "operator-system", "external", map[string]interface{}{"enabled": true})
		consumer.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
		if kind == "NodeMaintenance" {
			consumer.Object["spec"] = map[string]interface{}{"nodeName": "worker"}
		}
		reason, err := ocpRemainingOperatorConsumer(context.Background(), ocpOperatorCleanClient(consumer), pkg, "operator-system")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(reason).NotTo(gomega.BeEmpty())
	},
		ginkgo.Entry("OVS networks", "sriov-network-operator", cleanSriovGroup, "v1", "OVSNetwork"),
		ginkgo.Entry("SR-IOV networks", "sriov-network-operator", cleanSriovGroup, "v1", "SriovNetwork"),
		ginkgo.Entry("SR-IOV pools", "sriov-network-operator", cleanSriovGroup, "v1", "SriovNetworkPoolConfig"),
		ginkgo.Entry("NFD configuration", "nfd", "nfd.openshift.io", "v1", "NodeFeatureDiscovery"),
		ginkgo.Entry("maintenance requests", olmMaintenanceOperator, "maintenance.nvidia.com", "v1alpha1", "NodeMaintenance"),
	)

	ginkgo.It("does not treat the harmless platform default SR-IOV node policy as a consumer", func() {
		policy := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodePolicy", "operator-system", "default", map[string]interface{}{"numVfs": int64(0), "nicSelector": map[string]interface{}{}})
		reason, err := ocpRemainingOperatorConsumer(context.Background(), ocpOperatorCleanClient(policy), "sriov-network-operator", "operator-system")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(reason).To(gomega.BeEmpty())
	})

	ginkgo.It("preserves a Subscription with a recorded claim from another deployment", func() {
		cfg, artifacts, sub, csv, group := ocpOperatorCleanFixture()
		gomega.Expect(writeOCPConfigurationJournal(sub, &ocpConfigurationJournal{Version: 1, UID: string(sub.GetUID()), Cohorts: []string{"another-deployment"}})).To(gomega.Succeed())
		plan, err := prepareOCPCleanOperators(context.Background(), ocpOperatorCleanClient(sub, csv, group), cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.removesSubscription(sub)).To(gomega.BeFalse())
		gomega.Expect(plan.retained).To(gomega.ContainElement(gomega.ContainSubstring("another deployment")))
	})

	ginkgo.It("rejects ownership acquired between the operator snapshot and conditional deletion", func() {
		_, _, sub, _, _ := ocpOperatorCleanFixture()
		c := ocpOperatorCleanClient(sub)
		current := sub.DeepCopy()
		current.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "operators.coreos.com/v1", Kind: "Operator", Name: "foreign", UID: "foreign"}})
		gomega.Expect(c.Update(context.Background(), current)).To(gomega.Succeed())
		err := deleteOCPOperatorObject(context.Background(), c, sub, CleanOptions{PollInterval: time.Millisecond})
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("changed before operator uninstall")))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(sub), sub.DeepCopy())).To(gomega.Succeed())
	})

	ginkgo.It("honors retained manifest intent even when that revised artifact was never applied", func() {
		cfg, artifacts, sub, csv, group := ocpOperatorCleanFixture()
		files := artifacts.Files()
		for i := range files {
			if files[i].Name != "00-subscriptions.yaml" && files[i].Name != "00-operatorgroups.yaml" {
				continue
			}
			obj := &unstructured.Unstructured{}
			gomega.Expect(yaml.Unmarshal([]byte(files[i].Content), &obj.Object)).To(gomega.Succeed())
			annotations := obj.GetAnnotations()
			annotations[ocpCleanRetainAnnotation] = "true"
			obj.SetAnnotations(annotations)
			data, err := yaml.Marshal(obj.Object)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			files[i].Content = string(data)
		}
		artifacts, err := bundle.FromFiles(files)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		c := interceptor.NewClient(ocpOperatorCleanClient(sub, csv, group), interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			ginkgo.Fail("retained install must not be deleted")
			return nil
		}})
		plan, err := prepareOCPCleanOperators(context.Background(), c, cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.removesSubscription(sub)).To(gomega.BeFalse())
		retained, err := plan.execute(context.Background(), c, CleanOptions{PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(retained).To(gomega.ContainElement(gomega.ContainSubstring("retained install intent")))
	})

	ginkgo.It("rejects retained install identities outside the configured operator namespace", func() {
		cfg, artifacts, sub, csv, group := ocpOperatorCleanFixture()
		cfg.NetworkOperator.Namespace = "different-operator-namespace"
		_, err := prepareOCPCleanOperators(context.Background(), ocpOperatorCleanClient(sub, csv, group), cfg, artifacts, "test-owner")
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("configured package namespaces")))
	})

	ginkgo.It("accepts an unserved operator API as absent at the final deletion read", func() {
		_, _, sub, _, _ := ocpOperatorCleanFixture()
		c := interceptor.NewClient(ocpOperatorCleanClient(sub), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return &meta.NoKindMatchError{GroupKind: ocpCleanSubscriptionGVK.GroupKind()}
		}, Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			ginkgo.Fail("absent API must not be deleted")
			return nil
		}})
		gomega.Expect(deleteOCPOperatorObject(context.Background(), c, sub, CleanOptions{PollInterval: time.Millisecond})).To(gomega.Succeed())
	})

	ginkgo.It("omits already-absent operators without inspecting their consumers while still removing an attributable empty group", func() {
		cfg, artifacts, sub, _, group := ocpOperatorCleanFixture()
		var deleted []string
		c := interceptor.NewClient(ocpOperatorCleanClient(group), interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, objects client.ObjectList, opts ...client.ListOption) error {
				gomega.Expect(objects.GetObjectKind().GroupVersionKind().Group).To(gomega.Equal("operators.coreos.com"), "absent operators need no networking or worker inventory")
				return c.List(ctx, objects, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleted = append(deleted, obj.GetName())
				return c.Delete(ctx, obj, opts...)
			},
		})
		plan, err := prepareOCPCleanOperators(context.Background(), c, cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.removesSubscription(sub)).To(gomega.BeFalse())
		gomega.Expect(plan.targets()).NotTo(gomega.ContainElement(gomega.ContainSubstring("Uninstall managed Subscription")))
		_, err = plan.execute(context.Background(), c, CleanOptions{PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(deleted).To(gomega.Equal([]string{group.GetName()}))
	})

	ginkgo.It("accepts the real OperatorGroup admission default and removes the dedicated group", func() {
		cfg, artifacts, sub, csv, group := ocpOperatorCleanFixture()
		gomega.Expect(unstructured.SetNestedField(group.Object, "Default", "spec", "upgradeStrategy")).To(gomega.Succeed())
		var deleted []string
		c := interceptor.NewClient(ocpOperatorCleanClient(sub, csv, group), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleted = append(deleted, obj.GetName())
			return c.Delete(ctx, obj, opts...)
		}})
		plan, err := prepareOCPCleanOperators(context.Background(), c, cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.groups).To(gomega.HaveLen(1))
		_, err = plan.execute(context.Background(), c, CleanOptions{PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(deleted).To(gomega.ContainElement(group.GetName()))
	})

	ginkgo.It("retains an OperatorGroup with administrator-specific spec changes while cleaning selected operators", func() {
		cfg, artifacts, sub, csv, group := ocpOperatorCleanFixture()
		gomega.Expect(unstructured.SetNestedField(group.Object, "site-service-account", "spec", "serviceAccountName")).To(gomega.Succeed())
		c := ocpOperatorCleanClient(sub, csv, group)
		plan, err := prepareOCPCleanOperators(context.Background(), c, cfg, artifacts, "test-owner")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(plan.groups).To(gomega.BeEmpty())
		retained, err := plan.execute(context.Background(), c, CleanOptions{PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(retained).To(gomega.ContainElement(gomega.ContainSubstring("live spec differs")))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(group), group.DeepCopy())).To(gomega.Succeed())
	})
})
