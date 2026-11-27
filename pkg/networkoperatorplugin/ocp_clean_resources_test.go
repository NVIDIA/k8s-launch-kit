// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"errors"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func ocpResourceTestClient(objects ...client.Object) client.WithWatch {
	scheme := runtime.NewScheme()
	gomega.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
	gomega.Expect(appsv1.AddToScheme(scheme)).To(gomega.Succeed())
	for group, kinds := range map[string][]string{"mellanox.com": {"NicClusterPolicy", "NicNodePolicy", "MacvlanNetwork", "HostDeviceNetwork", "IPoIBNetwork"}, cleanNICGroup: {"NicDevice", "NicConfigurationTemplate", "NicFirmwareTemplate", "NicInterfaceNameTemplate", "NicFirmwareSource"}, cleanSriovGroup: {"SriovNetworkNodeState", "SriovNetworkNodePolicy", "SriovNetworkPoolConfig", "SriovNetwork", "SriovIBNetwork", "OVSNetwork"}, "nv-ipam.nvidia.com": {"IPPool", "CIDRPool"}, "k8s.cni.cncf.io": {"NetworkAttachmentDefinition"}} {
		version := "v1alpha1"
		if group == cleanSriovGroup || group == "k8s.cni.cncf.io" {
			version = "v1"
		}
		for _, kind := range kinds {
			gvk := schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
			scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
			scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(kind+"List"), &unstructured.UnstructuredList{})
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithReturnManagedFields().WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if obj.GetUID() == "" {
			obj.SetUID(types.UID("created-" + obj.GetName()))
		}
		return c.Create(ctx, obj, opts...)
	}}).Build()
}

func ocpResourceRef(obj *unstructured.Unstructured) cleanObjectRef {
	desired := obj.DeepCopy()
	desired.SetUID("")
	desired.SetResourceVersion("")
	desired.SetManagedFields(nil)
	return cleanObjectRef{GVK: obj.GroupVersionKind(), Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: obj.GetUID(), Desired: desired}
}

func ocpResourceObject(group, kind, namespace, name string, spec map[string]interface{}) *unstructured.Unstructured {
	version := "v1alpha1"
	if group == cleanSriovGroup {
		version = "v1"
	}
	obj := testCleanObject(schema.GroupVersionKind{Group: group, Version: version, Kind: kind}, namespace, name)
	obj.SetUID(types.UID(name + "-uid"))
	obj.SetResourceVersion("1")
	obj.SetGeneration(2)
	obj.SetCreationTimestamp(metav1.NewTime(time.Unix(100, 0)))
	obj.Object["spec"] = spec
	obj.SetAnnotations(map[string]string{launchKitVersionAnnotation: "test"})
	obj.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "l8k", APIVersion: obj.GetAPIVersion(), Operation: metav1.ManagedFieldsOperationApply, FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{}}`)}})
	return obj
}

func ocpResourceOwner(obj client.Object) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{APIVersion: obj.GetObjectKind().GroupVersionKind().GroupVersion().String(), Kind: obj.GetObjectKind().GroupVersionKind().Kind, Name: obj.GetName(), UID: obj.GetUID(), Controller: &controller}
}

var _ = ginkgo.Describe("OpenShift selected resource reconciliation", func() {
	ginkgo.It("uses live UID and resource-version preconditions with background root deletion", func() {
		policy := ocpResourceObject("mellanox.com", "NicClusterPolicy", "", "selected", map[string]interface{}{"nvIpam": map[string]interface{}{"enableWebhook": false}})
		foreign := ocpResourceObject("mellanox.com", "NicClusterPolicy", "", "foreign", map[string]interface{}{"ofedDriver": map[string]interface{}{"image": "driver"}})
		base := ocpResourceTestClient(policy, foreign)
		c := interceptor.NewClient(base, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			resolved := &client.DeleteOptions{}
			resolved.ApplyOptions(opts)
			gomega.Expect(resolved.Preconditions.UID).To(gomega.HaveValue(gomega.Equal(policy.GetUID())))
			gomega.Expect(resolved.Preconditions.ResourceVersion).To(gomega.HaveValue(gomega.Equal("1")))
			gomega.Expect(resolved.PropagationPolicy).To(gomega.HaveValue(gomega.Equal(metav1.DeletePropagationBackground)))
			return c.Delete(ctx, obj, opts...)
		}})
		cfg := &config.LaunchKitConfig{}
		n, err := cleanOCPResources(context.Background(), c, cfg, []cleanObjectRef{ocpResourceRef(policy)}, CleanOptions{Namespace: "operator-system", SkipWait: true, PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(n).To(gomega.Equal(1))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(foreign), foreign)).To(gomega.Succeed())
	})
	ginkgo.DescribeTable("refuses stale authorization immediately before deletion", func(change func(*unstructured.Unstructured, *cleanObjectRef), message string) {
		policy := ocpResourceObject("mellanox.com", "NicNodePolicy", "", "selected", map[string]interface{}{"ofedDriver": map[string]interface{}{"image": "driver"}})
		ref := ocpResourceRef(policy)
		change(policy, &ref)
		c := interceptor.NewClient(ocpResourceTestClient(policy), interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			ginkgo.Fail("must not delete a changed resource")
			return nil
		}})
		_, err := deleteOCPRefs(context.Background(), c, []cleanObjectRef{ref}, time.Millisecond)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(message)))
	},
		ginkgo.Entry("replacement", func(obj *unstructured.Unstructured, _ *cleanObjectRef) { obj.SetUID("new-uid") }, "replaced"),
		ginkgo.Entry("absent target appeared", func(_ *unstructured.Unstructured, ref *cleanObjectRef) { ref.UID = "" }, "appeared"),
		ginkgo.Entry("foreign ownership", func(obj *unstructured.Unstructured, _ *cleanObjectRef) { obj.SetManagedFields(nil) }, "ownership"),
		ginkgo.Entry("spec drift", func(obj *unstructured.Unstructured, _ *cleanObjectRef) {
			gomega.Expect(unstructured.SetNestedField(obj.Object, "other", "spec", "ofedDriver", "image")).To(gomega.Succeed())
		}, "differs"))
	ginkgo.It("does not follow a replacement that appears during deletion polling", func() {
		policy := ocpResourceObject("mellanox.com", "NicClusterPolicy", "", "selected", map[string]interface{}{"nvIpam": map[string]interface{}{"enableWebhook": false}})
		c := interceptor.NewClient(ocpResourceTestClient(policy), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := c.Delete(ctx, obj, opts...); err != nil {
				return err
			}
			replacement := policy.DeepCopy()
			replacement.SetUID("replacement")
			replacement.SetResourceVersion("")
			return c.Create(ctx, replacement)
		}})
		n, err := deleteOCPRefs(context.Background(), c, []cleanObjectRef{ocpResourceRef(policy)}, time.Millisecond)
		gomega.Expect(n).To(gomega.Equal(1))
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("new identity")))
	})
	ginkgo.It("observes exact generated NAD identities without requiring owner metadata on retries", func() {
		network := ocpResourceObject(cleanSriovGroup, "SriovNetwork", "sriov-system", "selected", map[string]interface{}{"networkNamespace": "application"})
		ref := ocpResourceRef(network)
		ref.UID = ""
		selected := testCleanObject(cleanNADGVK, "application", "selected")

		foreign := testCleanObject(cleanNADGVK, "application", "foreign")
		foreign.SetAnnotations(map[string]string{"sriovnetwork.openshift.io/owner-ref": "SriovNetwork.sriovnetwork.openshift.io/sriov-system/selected"})
		c := ocpResourceTestClient(selected, foreign)
		pending, err := ocpNetworkAttachments(context.Background(), c, []cleanObjectRef{ref})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pending).To(gomega.Equal([]string{"application/selected"}))
		gomega.Expect(c.Delete(context.Background(), selected)).To(gomega.Succeed())
		pending, err = ocpNetworkAttachments(context.Background(), c, []cleanObjectRef{ref})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pending).To(gomega.BeEmpty())
	})
	ginkgo.It("observes selected workload ancestry in memory as parent controllers disappear", func() {
		root := ocpResourceObject("mellanox.com", "NicNodePolicy", "", "selected", map[string]interface{}{"ofedDriver": map[string]interface{}{"image": "driver"}})
		foreignRoot := ocpResourceObject("mellanox.com", "NicNodePolicy", "", "foreign", map[string]interface{}{"ofedDriver": map[string]interface{}{"image": "driver"}})
		controller := &appsv1.DaemonSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}, ObjectMeta: metav1.ObjectMeta{Namespace: "operator-system", Name: "selected-ds", UID: "selected-ds", OwnerReferences: []metav1.OwnerReference{ocpResourceOwner(root)}}}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "operator-system", Name: "selected-pod", UID: "selected-pod", OwnerReferences: []metav1.OwnerReference{ocpResourceOwner(controller)}}}
		foreign := controller.DeepCopy()
		foreign.Name = "foreign-ds"
		foreign.UID = "foreign-ds"
		foreign.OwnerReferences = []metav1.OwnerReference{ocpResourceOwner(foreignRoot)}
		c := ocpResourceTestClient(root, foreignRoot, controller, pod, foreign)
		owned, err := prepareOCPWorkloadClean(context.Background(), c, "operator-system", []cleanObjectRef{ocpResourceRef(root)})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pending, err := owned.observe(context.Background(), c)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pending).To(gomega.ConsistOf("DaemonSet selected-ds", "Pod selected-pod"))
		gomega.Expect(c.Delete(context.Background(), root)).To(gomega.Succeed())
		gomega.Expect(c.Delete(context.Background(), controller)).To(gomega.Succeed())
		pending, err = owned.observe(context.Background(), c)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pending).To(gomega.Equal([]string{"Pod selected-pod"}))
		gomega.Expect(c.Delete(context.Background(), pod)).To(gomega.Succeed())
		pending, err = owned.observe(context.Background(), c)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pending).To(gomega.BeEmpty())
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(foreign), foreign)).To(gomega.Succeed())
	})
	ginkgo.It("does not require unrelated MOFED drivers to disappear", func() {
		root := ocpResourceObject("mellanox.com", "NicNodePolicy", "", "selected", map[string]interface{}{"rdmaSharedDevicePlugin": map[string]interface{}{"image": "plugin"}})
		foreignRoot := ocpResourceObject("mellanox.com", "NicNodePolicy", "", "foreign", map[string]interface{}{"ofedDriver": map[string]interface{}{"image": "driver"}})
		ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "operator-system", Name: "mofed-ds", UID: "ds-1", Labels: map[string]string{mofedDriverLabel: ""}, OwnerReferences: []metav1.OwnerReference{ocpResourceOwner(foreignRoot)}}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: mofedDriverContainer}}}}}}
		pod := shutdownTestPod()
		c := ocpResourceTestClient(root, foreignRoot, ds, pod)
		cfg := &config.LaunchKitConfig{}
		n, err := cleanOCPResources(context.Background(), c, cfg, []cleanObjectRef{ocpResourceRef(root)}, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(n).To(gomega.Equal(1))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(pod), pod)).To(gomega.Succeed())
	})
	ginkgo.It("refuses configured selected MOFED with no observable driver before root deletion", func() {
		root := ocpResourceObject("mellanox.com", "NicNodePolicy", "", "selected", map[string]interface{}{"ofedDriver": map[string]interface{}{"image": "driver"}})
		c := ocpResourceTestClient(root)
		n, err := cleanOCPResources(context.Background(), c, &config.LaunchKitConfig{}, []cleanObjectRef{ocpResourceRef(root)}, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(n).To(gomega.BeZero())
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("shutdown cannot be verified")))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(root), root)).To(gomega.Succeed())
	})
	ginkgo.It("honors a bounded caller timeout while a selected NAD is retained", func() {
		network := ocpResourceObject(cleanSriovGroup, "SriovNetwork", "sriov-system", "selected", map[string]interface{}{"networkNamespace": "application"})
		nad := testCleanObject(cleanNADGVK, "application", "selected")
		nad.SetAnnotations(map[string]string{"sriovnetwork.openshift.io/owner-ref": "SriovNetwork.sriovnetwork.openshift.io/sriov-system/selected"})
		c := ocpResourceTestClient(network, nad)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		n, err := cleanOCPResources(ctx, c, &config.LaunchKitConfig{}, []cleanObjectRef{ocpResourceRef(network)}, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(n).To(gomega.Equal(1))
		gomega.Expect(errors.Is(err, context.DeadlineExceeded)).To(gomega.BeTrue())
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(network), network)).To(gomega.WithTransform(apierrors.IsNotFound, gomega.BeTrue()))
	})
})

var _ = ginkgo.Describe("OpenShift selected hardware evidence", func() {
	ginkgo.It("accepts reset selected PFs while another policy retains other PFs", func() {
		state := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodeState", "sriov-system", "worker", map[string]interface{}{"interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:02:00.0", "numVfs": int64(8)}}})
		state.Object["status"] = map[string]interface{}{"syncStatus": "Succeeded", "interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(0)}, map[string]interface{}{"pciAddress": "0000:02:00.0", "numVfs": int64(8)}}}
		pending, err := ocpSriovResetPending(state, map[string]bool{"0000:01:00.0": true})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pending).To(gomega.BeEmpty())
		gomega.Expect(unstructured.SetNestedSlice(state.Object, []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(4)}}, "spec", "interfaces")).To(gomega.Succeed())
		pending, err = ocpSriovResetPending(state, map[string]bool{"0000:01:00.0": true})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pending).To(gomega.ContainSubstring("concurrent policy"))
	})
	ginkgo.DescribeTable("does not accept Succeeded without selected hardware evidence", func(ifaces []interface{}, expected string) {
		state := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodeState", "sriov-system", "worker", map[string]interface{}{})
		state.Object["status"] = map[string]interface{}{"syncStatus": "Succeeded", "interfaces": ifaces}
		pending, err := ocpSriovResetPending(state, map[string]bool{"0000:01:00.0": true})
		if err != nil {
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(expected)))
		} else {
			gomega.Expect(pending).To(gomega.ContainSubstring(expected))
		}
	},
		ginkgo.Entry("old VF count", []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(4)}}, "still reports VFs"),
		ginkgo.Entry("old VF inventory", []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(0), "Vfs": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.1"}}}}, "still reports VFs"),
		ginkgo.Entry("missing PF", []interface{}{map[string]interface{}{"pciAddress": "0000:02:00.0", "numVfs": int64(0)}}, "missing"),
		ginkgo.Entry("external management", []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "externallyManaged": true}}, "externally managed"))
	ginkgo.It("selects only requested node PFs and waits through delayed Red Hat API reconciliation", func() {
		policy := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodePolicy", "sriov-system", "selected", map[string]interface{}{"nodeSelector": map[string]interface{}{"group": "selected"}, "nicSelector": map[string]interface{}{"rootDevices": []interface{}{"0000:01:00.0"}}})
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{"group": "selected"}}}
		unrelatedNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "other", Labels: map[string]string{"group": "other"}}}
		state := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodeState", "sriov-system", "worker", map[string]interface{}{"interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(4)}, map[string]interface{}{"pciAddress": "0000:02:00.0", "numVfs": int64(8)}}})
		state.Object["status"] = map[string]interface{}{"syncStatus": "Succeeded", "interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(4)}, map[string]interface{}{"pciAddress": "0000:02:00.0", "numVfs": int64(8)}}}
		other := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodeState", "sriov-system", "other", map[string]interface{}{"interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(8)}}})
		base := ocpResourceTestClient(policy, node, unrelatedNode, state, other, sriovDaemonPod("worker", "sriov-system"))
		deleted := false
		observations := 0
		c := interceptor.NewClient(base, interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				gomega.Expect(obj.GetName()).To(gomega.Equal("selected"))
				deleted = true
				return c.Delete(ctx, obj, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if deleted && key.Name == "worker" {
					observations++
					if observations == 3 {
						current := state.DeepCopy()
						gomega.Expect(c.Get(ctx, key, current)).To(gomega.Succeed())
						current.Object["spec"] = map[string]interface{}{"interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:02:00.0", "numVfs": int64(8)}}}
						current.Object["status"] = map[string]interface{}{"syncStatus": "Succeeded", "interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(0)}, map[string]interface{}{"pciAddress": "0000:02:00.0", "numVfs": int64(8)}}}
						gomega.Expect(c.Update(ctx, current)).To(gomega.Succeed())
					}
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		count, err := cleanOCPResources(ctx, c, &config.LaunchKitConfig{}, []cleanObjectRef{ocpResourceRef(policy)}, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(count).To(gomega.Equal(1))
		gomega.Expect(observations).To(gomega.BeNumerically(">=", 3))
		gomega.Expect(c.Get(ctx, client.ObjectKeyFromObject(other), other)).To(gomega.Succeed())
	})
	ginkgo.It("supports PF-name ranges and alternate names without broadening root-device selection", func() {
		policy := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodePolicy", "sriov-system", "selected", map[string]interface{}{"nicSelector": map[string]interface{}{"vendor": "15b3", "rootDevices": []interface{}{"0000:01:00.0"}, "pfNames": []interface{}{"renamed#0-3"}}})
		iface := map[string]interface{}{"vendor": "15b3", "pciAddress": "0000:01:00.0", "name": "eno1", "altNames": []interface{}{"renamed"}}
		matched, err := ocpSriovMatches(policy, iface)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(matched).To(gomega.BeTrue())
		iface["pciAddress"] = "0000:02:00.0"
		matched, err = ocpSriovMatches(policy, iface)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(matched).To(gomega.BeFalse())
	})
	ginkgo.It("waits only for selected NIC operation fields and fresh empty acknowledgements", func() {
		device := ocpResourceObject(cleanNICGroup, "NicDevice", "operator-system", "nic", map[string]interface{}{"configuration": map[string]interface{}{"template": map[string]interface{}{"numVfs": int64(4)}}, "firmware": map[string]interface{}{"nicFirmwareSourceRef": "foreign-source"}})
		entry := &ocpNICDeviceClean{fields: map[string]bool{"configuration": true}, observation: cleanNICDeviceObservation{uid: device.GetUID(), minimumGeneration: map[string]int64{}}}
		pending, err := observeOCPNICDevice(device, entry)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pending).NotTo(gomega.BeEmpty())
		unstructured.RemoveNestedField(device.Object, "spec", "configuration")
		device.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "ConfigUpdateInProgress", "status": "False", "reason": "DeviceConfigSpecEmpty", "observedGeneration": int64(1)}, map[string]interface{}{"type": "FirmwareUpdateInProgress", "status": "True", "reason": "ForeignFirmwareUpdate"}}}
		pending, err = observeOCPNICDevice(device, entry)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pending).To(gomega.HaveLen(1))
		conditions, _, err := unstructured.NestedSlice(device.Object, "status", "conditions")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		conditions[0].(map[string]interface{})["observedGeneration"] = int64(2)
		gomega.Expect(unstructured.SetNestedSlice(device.Object, conditions, "status", "conditions")).To(gomega.Succeed())
		pending, err = observeOCPNICDevice(device, entry)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pending).To(gomega.BeEmpty())
		firmware, present, err := unstructured.NestedMap(device.Object, "spec", "firmware")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(present).To(gomega.BeTrue())
		gomega.Expect(firmware).NotTo(gomega.BeEmpty())
	})
	ginkgo.It("protects omitted NNPs and same-root CRs even when reconciliation waits are skipped", func() {
		root := ocpResourceObject("mellanox.com", "NicClusterPolicy", "", "selected", map[string]interface{}{"nvIpam": map[string]interface{}{"enableWebhook": false}})
		peer := ocpResourceObject("mellanox.com", "NicNodePolicy", "", "omitted", map[string]interface{}{"ofedDriver": map[string]interface{}{"image": "driver"}})
		base := ocpResourceTestClient(root, peer)
		_, err := cleanOCPResources(context.Background(), base, &config.LaunchKitConfig{}, []cleanObjectRef{ocpResourceRef(root)}, CleanOptions{Namespace: "operator-system", SkipWait: true})
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("omitted NicNodePolicy")))
		gomega.Expect(base.Get(context.Background(), client.ObjectKeyFromObject(root), root)).To(gomega.Succeed())
		gomega.Expect(base.Delete(context.Background(), peer)).To(gomega.Succeed())
		setOCPCleanOwner(root, "root-owner")
		annotations := root.GetAnnotations()
		annotations[ocpCleanUIDAnnotation] = string(root.GetUID())
		root.SetAnnotations(annotations)
		network := ocpResourceObject(cleanSriovGroup, "SriovNetwork", config.DefaultSriovOperatorNamespace, "omitted-network", map[string]interface{}{"resourceName": "ours"})
		setOCPCleanOwner(network, "root-owner")
		c := ocpResourceTestClient(root, network)
		err = checkOCPCleanDependencies(context.Background(), c, &config.LaunchKitConfig{}, []cleanObjectRef{ocpResourceRef(root)})
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("omitted SriovNetwork")))
		setOCPCleanOwner(network, "other-root")
		gomega.Expect(c.Update(context.Background(), network)).To(gomega.Succeed())
		gomega.Expect(checkOCPCleanDependencies(context.Background(), c, &config.LaunchKitConfig{}, []cleanObjectRef{ocpResourceRef(root)})).To(gomega.Succeed())
	})
})

var _ = ginkgo.Describe("OpenShift selected driver shutdown", func() {
	ginkgo.It("requires selected terminal evidence while an unrelated live driver remains", func() {
		pod := shutdownTestPod()
		foreign := pod.DeepCopy()
		foreign.Name = "foreign-driver"
		foreign.UID = "foreign-driver"
		foreign.OwnerReferences[0].UID = "foreign-ds"
		base := shutdownTestClient(pod, foreign)
		stream := watch.NewRaceFreeFake()
		c := interceptor.NewClient(base, interceptor.Funcs{Watch: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
			return stream, nil
		}})
		scope := &mofedCleanupScope{includeDaemonSet: func(*appsv1.DaemonSet) bool { return false }, includePod: func(candidate *corev1.Pod) (bool, error) { return candidate.UID == pod.UID, nil }, hasIntent: func(context.Context) (bool, error) { return true, nil }}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		count, err := withScopedMOFEDShutdown(ctx, c, pod.Namespace, scope, func(ctx context.Context) (int, error) {
			if err := base.Delete(ctx, pod); err != nil {
				return 0, err
			}
			stream.Modify(shutdownTerminal(pod, 0))
			stream.Delete(shutdownTerminal(pod, 0))
			// A failing foreign driver event must neither join this proof nor block it.
			stream.Modify(shutdownTerminal(foreign, 1))
			stream.Action(watch.Bookmark, &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "100"}})
			return 1, nil
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(count).To(gomega.Equal(1))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(foreign), foreign)).To(gomega.Succeed())
	})
})

var _ = ginkgo.Describe("OpenShift selected finalizer ordering", func() {
	ginkgo.It("keeps the owning NCP alive until the IPPool finalizer completes", func() {
		root := ocpResourceObject("mellanox.com", "NicClusterPolicy", "", "selected", map[string]interface{}{"nvIpam": map[string]interface{}{"enableWebhook": false}})
		pool := ocpResourceObject("nv-ipam.nvidia.com", "IPPool", "operator-system", "selected-pool", map[string]interface{}{"subnet": "192.0.2.0/24"})
		pool.SetFinalizers([]string{"test.ipam/cleanup"})
		base := ocpResourceTestClient(root, pool)
		finalized := false
		c := interceptor.NewClient(base, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				err := c.Get(ctx, key, obj, opts...)
				if err == nil && key.Name == pool.GetName() && obj.GetDeletionTimestamp() != nil {
					if err := c.Get(ctx, client.ObjectKeyFromObject(root), root); err != nil {
						return errors.New("IPPool controller lost its NCP before finalization")
					}
					obj.SetFinalizers(nil)
					if err := c.Update(ctx, obj); err != nil {
						return err
					}
					finalized = true
					return c.Get(ctx, key, obj, opts...)
				}
				return err
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetName() == root.GetName() {
					gomega.Expect(finalized).To(gomega.BeTrue())
				}
				return c.Delete(ctx, obj, opts...)
			},
		})
		cfg := &config.LaunchKitConfig{NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-system"}}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		count, err := cleanOCPResources(ctx, c, cfg, []cleanObjectRef{ocpResourceRef(root), ocpResourceRef(pool)}, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(count).To(gomega.Equal(2))
		gomega.Expect(finalized).To(gomega.BeTrue())
	})
})

var _ = ginkgo.Describe("OpenShift selected controller dependency protection", func() {
	ginkgo.DescribeTable("retains foreign controller consumers in both wait modes", func(group, kind, component string) {
		for _, skip := range []bool{false, true} {
			root := ocpResourceObject("mellanox.com", "NicClusterPolicy", "", "selected", map[string]interface{}{component: map[string]interface{}{"enabled": true}})
			foreign := ocpResourceObject(group, kind, "operator-system", "foreign-consumer", map[string]interface{}{"source": "foreign"})
			c := interceptor.NewClient(ocpResourceTestClient(root, foreign), interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				ginkgo.Fail("dependent peer must prevent every ordinary deletion")
				return nil
			}})
			cfg := &config.LaunchKitConfig{NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-system"}}
			count, err := cleanOCPResources(context.Background(), c, cfg, []cleanObjectRef{ocpResourceRef(root)}, CleanOptions{Namespace: "operator-system", SkipWait: skip, PollInterval: time.Millisecond})
			gomega.Expect(count).To(gomega.BeZero())
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("omitted " + kind)))
			gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(foreign), foreign)).To(gomega.Succeed())
			gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(root), root)).To(gomega.Succeed())
		}
	}, ginkgo.Entry("NV-IPAM CIDRPool", "nv-ipam.nvidia.com", "CIDRPool", "nvIpam"), ginkgo.Entry("NIC firmware source", cleanNICGroup, "NicFirmwareSource", "nicConfigurationOperator"))
})

var _ = ginkgo.Describe("OpenShift reverse deployment cleanup", func() {
	ginkgo.It("waits for an unmarked asynchronous NAD before its pool in both modes", func() {
		for _, skip := range []bool{false, true} {
			cfg, root, pool, refs := ocpPoolConsumerFixture()
			nodeRoot := ocpResourceObject("mellanox.com", "NicNodePolicy", "", "selected-node-root", map[string]interface{}{"rdmaSharedDevicePlugin": map[string]interface{}{"image": "plugin"}})
			network := ocpResourceObject(cleanSriovGroup, "SriovNetwork", config.DefaultSriovOperatorNamespace, "selected-network", map[string]interface{}{"networkNamespace": "unrelated-application", "ipam": `{"type":"nv-ipam","poolName":"selected-pool"}`})
			nad := ocpConsumerNAD(network.GetName(), `{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"selected-pool"}}`)
			base := ocpResourceTestClient(root, nodeRoot, pool, network, nad)
			var deleted []string
			observations := 0
			c := interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if obj.GetObjectKind().GroupVersionKind() == cleanNADGVK && key.Name == network.GetName() && len(deleted) > 0 {
						observations++
						if observations == 3 {
							if err := c.Delete(ctx, nad); err != nil {
								return err
							}
						}
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if obj.GetName() == pool.GetName() {
						gomega.Expect(observations).To(gomega.BeNumerically(">=", 3))
					}
					deleted = append(deleted, obj.GetName())
					return c.Delete(ctx, obj, opts...)
				},
			})
			refs = []cleanObjectRef{refs[0], ocpResourceRef(nodeRoot), refs[1], ocpResourceRef(network)}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			count, err := cleanOCPResources(ctx, c, cfg, refs, CleanOptions{Namespace: "operator-system", SkipWait: skip, PollInterval: time.Millisecond})
			cancel()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(count).To(gomega.Equal(4))
			gomega.Expect(deleted).To(gomega.Equal([]string{network.GetName(), pool.GetName(), nodeRoot.GetName(), root.GetName()}))
		}
	})

	ginkgo.It("accepts a fully removed deployment without creating cleanup records", func() {
		cfg, _, _, refs := ocpPoolConsumerFixture()
		for i := range refs {
			refs[i].UID = ""
		}
		base := ocpResourceTestClient()
		c := interceptor.NewClient(base, interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			ginkgo.Fail("already absent deployment must not create cleanup records")
			return nil
		}})
		count, err := cleanOCPResources(context.Background(), c, cfg, refs, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(count).To(gomega.BeZero())
	})

	ginkgo.It("does not inspect consumers of an already absent pool", func() {
		cfg, _, _, refs := ocpPoolConsumerFixture()
		refs = refs[1:]
		refs[0].UID = ""
		foreign := ocpConsumerNAD("unrelated-network", `{"type":"sriov","ipam":{"type":"nv-ipam","poolName":"selected-pool"}}`)
		c := ocpResourceTestClient(foreign)
		count, err := cleanOCPResources(context.Background(), c, cfg, refs, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(count).To(gomega.BeZero())
	})
})

var _ = ginkgo.Describe("OpenShift shared reset barriers", func() {
	ginkgo.It("deletes every selected SR-IOV policy before waiting on their shared PF", func() {
		policy := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodePolicy", "sriov-system", "first-policy", map[string]interface{}{"nicSelector": map[string]interface{}{"rootDevices": []interface{}{"0000:01:00.0"}}})
		second := policy.DeepCopy()
		second.SetName("second-policy")
		second.SetUID("second-policy-uid")
		state := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodeState", "sriov-system", "worker", map[string]interface{}{"interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(4)}}})
		state.Object["status"] = map[string]interface{}{"syncStatus": "Succeeded", "interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(4)}}}
		base := ocpResourceTestClient(policy, second, state, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}})
		var deleted []string
		c := interceptor.NewClient(base, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := c.Delete(ctx, obj, opts...); err != nil {
				return err
			}
			deleted = append(deleted, obj.GetName())
			if len(deleted) == 2 {
				current := state.DeepCopy()
				if err := c.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
					return err
				}
				current.Object["spec"] = map[string]interface{}{}
				current.Object["status"] = map[string]interface{}{"syncStatus": "Succeeded", "interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(0)}}}
				return c.Update(ctx, current)
			}
			return nil
		}})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		count, err := cleanOCPResources(ctx, c, &config.LaunchKitConfig{}, []cleanObjectRef{ocpResourceRef(policy), ocpResourceRef(second)}, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(count).To(gomega.Equal(2))
		gomega.Expect(deleted).To(gomega.Equal([]string{second.GetName(), policy.GetName()}))
	})

	ginkgo.It("signals node and cluster roots before waiting on cross-root finalizers", func() {
		root := ocpResourceObject("mellanox.com", "NicClusterPolicy", "", "root", map[string]interface{}{"secondaryNetwork": map[string]interface{}{"enable": true}})
		node := ocpResourceObject("mellanox.com", "NicNodePolicy", "", "node", map[string]interface{}{"rdmaSharedDevicePlugin": map[string]interface{}{"image": "plugin"}})
		root.SetFinalizers([]string{"test/cross-policy"})
		node.SetFinalizers([]string{"test/cross-policy"})
		base := ocpResourceTestClient(root, node)
		var deleted []string
		c := interceptor.NewClient(base, interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleted = append(deleted, obj.GetName())
				return c.Delete(ctx, obj, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				err := c.Get(ctx, key, obj, opts...)
				if err == nil && obj.GetDeletionTimestamp() != nil && len(deleted) == 2 {
					obj.SetFinalizers(nil)
					if err := c.Update(ctx, obj); err != nil {
						return err
					}
					return c.Get(ctx, key, obj, opts...)
				}
				return err
			},
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		count, err := cleanOCPResources(ctx, c, &config.LaunchKitConfig{}, []cleanObjectRef{ocpResourceRef(root), ocpResourceRef(node)}, CleanOptions{Namespace: "operator-system", SkipWait: true, PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(count).To(gomega.Equal(2))
		gomega.Expect(deleted).To(gomega.Equal([]string{node.GetName(), root.GetName()}))
	})
})

var _ = ginkgo.Describe("OpenShift cleanup live retry state", func() {
	ginkgo.DescribeTable("allows missing SR-IOV observation state only after the selected policy is absent", func(apiMissing bool) {
		for _, policyPresent := range []bool{false, true} {
			policy := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodePolicy", "sriov-system", "selected", map[string]interface{}{"nicSelector": map[string]interface{}{"rootDevices": []interface{}{"0000:01:00.0"}}})
			ref := ocpResourceRef(policy)
			objects := []client.Object{&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}}}
			if policyPresent {
				objects = append(objects, policy)
			} else {
				ref.UID = ""
			}
			c := interceptor.NewClient(ocpResourceTestClient(objects...), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if apiMissing && obj.GetObjectKind().GroupVersionKind().Kind == "SriovNetworkNodeState" {
					return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: cleanSriovGroup, Kind: "SriovNetworkNodeState"}, SearchedVersions: []string{"v1"}}
				}
				return c.Get(ctx, key, obj, opts...)
			}})
			count, err := cleanOCPResources(context.Background(), c, &config.LaunchKitConfig{}, []cleanObjectRef{ref}, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
			gomega.Expect(count).To(gomega.BeZero())
			if policyPresent {
				gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("capture selected SriovNetworkNodeState")))
				gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(policy), policy)).To(gomega.Succeed())
			} else {
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
		}
	}, ginkgo.Entry("state removed", false), ginkgo.Entry("API uninstalled", true))

	ginkgo.It("still waits for observable nonzero VFs after the selected policy is absent", func() {
		policy := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodePolicy", "sriov-system", "selected", map[string]interface{}{"nicSelector": map[string]interface{}{"rootDevices": []interface{}{"0000:01:00.0"}}})
		ref := ocpResourceRef(policy)
		ref.UID = ""
		state := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodeState", "sriov-system", "worker", map[string]interface{}{})
		state.Object["status"] = map[string]interface{}{"syncStatus": "Succeeded", "interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(4)}}}
		c := ocpResourceTestClient(state, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		count, err := cleanOCPResources(ctx, c, &config.LaunchKitConfig{}, []cleanObjectRef{ref}, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(count).To(gomega.BeZero())
		gomega.Expect(errors.Is(err, context.DeadlineExceeded)).To(gomega.BeTrue())
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("still reports VFs")))
	})

	ginkgo.It("allows an absent NIC template after its observation API is uninstalled", func() {
		template := ocpResourceObject(cleanNICGroup, "NicConfigurationTemplate", "operator-system", "selected", map[string]interface{}{"nicSelector": map[string]interface{}{"nicType": "ConnectX"}})
		ref := ocpResourceRef(template)
		ref.UID = ""
		c := interceptor.NewClient(ocpResourceTestClient(), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Kind == "NicDeviceList" {
				return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: cleanNICGroup, Kind: "NicDevice"}, SearchedVersions: []string{"v1alpha1"}}
			}
			return c.List(ctx, list, opts...)
		}})
		count, err := cleanOCPResources(context.Background(), c, &config.LaunchKitConfig{}, []cleanObjectRef{ref}, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(count).To(gomega.BeZero())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
})

var _ = ginkgo.Describe("OpenShift live root dependency boundary", func() {
	ginkgo.It("cleans surviving targets when their retained root is absent and an independent node policy exists", func() {
		cfg, _, pool, refs := ocpPoolConsumerFixture()
		refs[0].UID = ""
		independent := ocpResourceObject("mellanox.com", "NicNodePolicy", "", "independent", map[string]interface{}{"rdmaSharedDevicePlugin": map[string]interface{}{"image": "plugin"}})
		c := ocpResourceTestClient(pool, independent)
		count, err := cleanOCPResources(context.Background(), c, cfg, refs, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(count).To(gomega.Equal(1))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(independent), independent)).To(gomega.Succeed())
	})

	ginkgo.It("does not protect former dependencies when a previously observed root has since disappeared", func() {
		cfg, _, _, refs := ocpPoolConsumerFixture()
		independent := ocpResourceObject("mellanox.com", "NicNodePolicy", "", "independent", map[string]interface{}{"rdmaSharedDevicePlugin": map[string]interface{}{"image": "plugin"}})
		c := ocpResourceTestClient(independent)
		gomega.Expect(checkOCPCleanDependencies(context.Background(), c, cfg, refs)).To(gomega.Succeed())
	})

	ginkgo.It("retains a live shared root when an omitted dependent appears during earlier cleanup", func() {
		for _, skip := range []bool{false, true} {
			cfg, root, _, refs := ocpPoolConsumerFixture()
			network := ocpResourceObject(cleanSriovGroup, "SriovNetwork", config.DefaultSriovOperatorNamespace, "selected-network", map[string]interface{}{"networkNamespace": "application"})
			independent := ocpResourceObject("mellanox.com", "NicNodePolicy", "", "new-dependent", map[string]interface{}{"rdmaSharedDevicePlugin": map[string]interface{}{"image": "plugin"}})
			independent.SetResourceVersion("")
			base := ocpResourceTestClient(root, network)
			c := interceptor.NewClient(base, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				gomega.Expect(obj.GetName()).To(gomega.Equal(network.GetName()))
				if err := c.Delete(ctx, obj, opts...); err != nil {
					return err
				}
				return c.Create(ctx, independent)
			}})
			refs = []cleanObjectRef{refs[0], ocpResourceRef(network)}
			count, err := cleanOCPResources(context.Background(), c, cfg, refs, CleanOptions{Namespace: "operator-system", SkipWait: skip, PollInterval: time.Millisecond})
			gomega.Expect(count).To(gomega.Equal(1))
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("omitted NicNodePolicy /new-dependent")))
			gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(root), root)).To(gomega.Succeed())
		}
	})
})
