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
	ginkgo.It("waits only for NADs owned by the exact selected network including retries", func() {
		network := ocpResourceObject(cleanSriovGroup, "SriovNetwork", "sriov-system", "selected", map[string]interface{}{"networkNamespace": "application"})
		ref := ocpResourceRef(network)
		ref.UID = ""
		selected := testCleanObject(cleanNADGVK, "application", "selected")
		selected.SetAnnotations(map[string]string{"sriovnetwork.openshift.io/owner-ref": "SriovNetwork.sriovnetwork.openshift.io/sriov-system/selected"})
		foreign := testCleanObject(cleanNADGVK, "application", "foreign")
		foreign.SetAnnotations(map[string]string{"sriovnetwork.openshift.io/owner-ref": "SriovNetwork.sriovnetwork.openshift.io/sriov-system/foreign"})
		c := ocpResourceTestClient(selected, foreign)
		pending, err := ocpNetworkAttachments(context.Background(), c, []cleanObjectRef{ref})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pending).To(gomega.Equal([]string{"application/selected"}))
		gomega.Expect(c.Delete(context.Background(), selected)).To(gomega.Succeed())
		pending, err = ocpNetworkAttachments(context.Background(), c, []cleanObjectRef{ref})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pending).To(gomega.BeEmpty())
	})
	ginkgo.It("observes only selected policy workload ancestry across an interrupted retry", func() {
		root := ocpResourceObject("mellanox.com", "NicNodePolicy", "", "selected", map[string]interface{}{"ofedDriver": map[string]interface{}{"image": "driver"}})
		foreignRoot := ocpResourceObject("mellanox.com", "NicNodePolicy", "", "foreign", map[string]interface{}{"ofedDriver": map[string]interface{}{"image": "driver"}})
		controller := &appsv1.DaemonSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}, ObjectMeta: metav1.ObjectMeta{Namespace: "operator-system", Name: "selected-ds", UID: "selected-ds", OwnerReferences: []metav1.OwnerReference{ocpResourceOwner(root)}}}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "operator-system", Name: "selected-pod", UID: "selected-pod", OwnerReferences: []metav1.OwnerReference{ocpResourceOwner(controller)}}}
		foreign := controller.DeepCopy()
		foreign.Name = "foreign-ds"
		foreign.UID = "foreign-ds"
		foreign.OwnerReferences = []metav1.OwnerReference{ocpResourceOwner(foreignRoot)}
		c := ocpResourceTestClient(root, foreignRoot, controller, pod, foreign)
		owned, err := prepareOCPWorkloadClean(context.Background(), c, "operator-system", []cleanObjectRef{ocpResourceRef(root)}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pending, err := owned.observe(context.Background(), c)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pending).To(gomega.ConsistOf("DaemonSet selected-ds", "Pod selected-pod"))
		gomega.Expect(c.Delete(context.Background(), root)).To(gomega.Succeed())
		gomega.Expect(c.Delete(context.Background(), controller)).To(gomega.Succeed())
		ref := ocpResourceRef(root)
		ref.UID = ""
		retry, err := prepareOCPWorkloadClean(context.Background(), c, "operator-system", []cleanObjectRef{ref}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pending, err = retry.observe(context.Background(), c)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pending).To(gomega.Equal([]string{"Pod selected-pod"}))
		gomega.Expect(c.Delete(context.Background(), pod)).To(gomega.Succeed())
		pending, err = retry.observe(context.Background(), c)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(pending).To(gomega.BeEmpty())
		gomega.Expect(retry.removeCheckpoint(context.Background(), c)).To(gomega.Succeed())
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
		count, err := cleanOCPResourcesWithLogs(ctx, c, &config.LaunchKitConfig{}, []cleanObjectRef{ocpResourceRef(policy)}, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond}, fakeSriovLogReader)
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

var _ = ginkgo.Describe("OpenShift cleanup verification receipts", func() {
	ginkgo.It("repeats full-profile cleanup after NCP teardown withdraws NicDevices", func() {
		root := ocpResourceObject("mellanox.com", "NicClusterPolicy", "", "selected", map[string]interface{}{"nicConfigurationOperator": map[string]interface{}{"logLevel": "info"}})
		template := ocpResourceObject(cleanNICGroup, "NicConfigurationTemplate", "operator-system", "selected-template", map[string]interface{}{"nodeSelector": map[string]interface{}{"group": "selected"}, "nicSelector": map[string]interface{}{"nicType": "101d", "pciAddresses": []interface{}{"0000:01:00.0"}}})
		device := ocpResourceObject(cleanNICGroup, "NicDevice", "operator-system", "selected-nic", map[string]interface{}{"configuration": map[string]interface{}{"template": map[string]interface{}{"numVfs": int64(4)}}})
		device.Object["status"] = map[string]interface{}{"node": "worker", "type": "101d", "ports": []interface{}{map[string]interface{}{"pci": "0000:01:00.0"}}}
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{"group": "selected"}}}
		base := ocpResourceTestClient(root, template, device, node)
		c := interceptor.NewClient(base, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := c.Delete(ctx, obj, opts...); err != nil {
				return err
			}
			if obj.GetName() == template.GetName() {
				current := device.DeepCopy()
				if err := c.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
					return err
				}
				unstructured.RemoveNestedField(current.Object, "spec", "configuration")
				current.Object["status"].(map[string]interface{})["conditions"] = []interface{}{map[string]interface{}{"type": "ConfigUpdateInProgress", "status": "False", "reason": "DeviceConfigSpecEmpty", "observedGeneration": int64(2)}}
				return c.Update(ctx, current)
			}
			if obj.GetName() == root.GetName() {
				return c.Delete(ctx, device)
			}
			return nil
		}})
		refs := []cleanObjectRef{ocpResourceRef(root), ocpResourceRef(template)}
		for i := range refs {
			setOCPCleanOwner(refs[i].Desired, "stable-root-owner")
		}
		cfg := &config.LaunchKitConfig{NetworkOperator: &config.NetworkOperatorConfig{Namespace: "operator-system"}}
		opts := CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond}
		n, err := cleanOCPResources(context.Background(), c, cfg, refs, opts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(n).To(gomega.Equal(2))
		gomega.Expect(completeOCPCleanReceipt(context.Background(), base, "operator-system", "stable-root-owner")).To(gomega.Succeed())
		for i := range refs {
			refs[i].UID = ""
		}
		retry := interceptor.NewClient(base, interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Kind == "NicDeviceList" {
				ginkgo.Fail("durable receipt must survive withdrawn NicDevice API")
			}
			return c.List(ctx, list, opts...)
		}})
		n, err = cleanOCPResources(context.Background(), retry, cfg, refs, opts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(n).To(gomega.BeZero())
		receipt := &corev1.ConfigMap{}
		gomega.Expect(base.Get(context.Background(), client.ObjectKey{Namespace: "operator-system", Name: ocpCleanReceiptName("stable-root-owner")}, receipt)).To(gomega.Succeed())
		gomega.Expect(receipt.Data["state"]).To(gomega.Equal("resources-complete"))
		gomega.Expect(completeOCPCleanReceipt(context.Background(), base, "operator-system", "stable-root-owner")).To(gomega.Succeed())
		gomega.Expect(invalidateOCPCleanReceipt(context.Background(), base, "operator-system", "stable-root-owner")).To(gomega.Succeed())
		gomega.Expect(base.Get(context.Background(), client.ObjectKeyFromObject(receipt), receipt)).To(gomega.WithTransform(apierrors.IsNotFound, gomega.BeTrue()))
	})
	ginkgo.It("does not claim full-profile verification for absent parents without a receipt", func() {
		template := ocpResourceObject(cleanNICGroup, "NicConfigurationTemplate", "operator-system", "selected-template", map[string]interface{}{"nicSelector": map[string]interface{}{"nicType": "101d"}})
		ref := ocpResourceRef(template)
		ref.UID = ""
		n, err := cleanOCPResources(context.Background(), ocpResourceTestClient(), &config.LaunchKitConfig{}, []cleanObjectRef{ref}, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(n).To(gomega.BeZero())
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("no cleanup verification receipt")))
	})
	ginkgo.It("binds receipts to the deployment incarnation and blocks deployment during incomplete cleanup", func() {
		root := ocpResourceObject("mellanox.com", "NicNodePolicy", "", "selected", map[string]interface{}{"rdmaSharedDevicePlugin": map[string]interface{}{"image": "plugin"}})
		ref := ocpResourceRef(root)
		setOCPCleanOwner(ref.Desired, "receipt-owner")
		c := ocpResourceTestClient(root)
		receipt, verified, err := loadOCPCleanReceipt(context.Background(), c, "operator-system", []cleanObjectRef{ref})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(verified).To(gomega.BeFalse())
		gomega.Expect(receipt.save(context.Background(), c, "hardware-verified")).To(gomega.Succeed())
		gomega.Expect(invalidateOCPCleanReceipt(context.Background(), c, "operator-system", "receipt-owner")).To(gomega.MatchError(gomega.ContainSubstring("interrupted cleanup")))
		replacement := ref
		replacement.UID = "replacement"
		_, _, err = loadOCPCleanReceipt(context.Background(), c, "operator-system", []cleanObjectRef{replacement})
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("earlier deployment incarnation")))
		gomega.Expect(receipt.save(context.Background(), c, "complete")).To(gomega.Succeed())
		gomega.Expect(invalidateOCPCleanReceipt(context.Background(), c, "operator-system", "receipt-owner")).To(gomega.Succeed())
	})
})

var _ = ginkgo.Describe("OpenShift selected SRIOV generation proof", func() {
	ginkgo.It("supports RH 4.18 serialized-handler and RH 4.20 reconcileID logs", func() {
		state := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodeState", "sriov-system", "worker", map[string]interface{}{})
		legacy := []byte("1970-01-01T00:03:20Z 1970-01-01T00:03:20Z\tINFO\tnodeStateSyncHandler(): new generation\t{\"generation\":2}\n1970-01-01T00:03:21Z 1970-01-01T00:03:21Z\tINFO\tnodeStateSyncHandler(): sync succeeded\n")
		gomega.Expect(ocpSriovLogProvesGeneration(legacy, state, time.Unix(101, 0))).To(gomega.BeTrue())
		gomega.Expect(ocpSriovLogProvesGeneration(sriovGenerationLog("worker", "sriov-system", "reconcile", 2, true), state, time.Unix(101, 0))).To(gomega.BeTrue())
		gomega.Expect(ocpSriovLogProvesGeneration([]byte("1970-01-01T00:03:20Z {\"msg\":\"nodeStateSyncHandler(): new generation\",\"generation\":2}\n1970-01-01T00:03:21Z {\"msg\":\"nodeStateSyncHandler(): sync succeeded\"}\n"), state, time.Unix(101, 0))).To(gomega.BeTrue())
		invalid := []string{
			"1970-01-01T00:03:20Z INFO nodeStateSyncHandler(): new generation {\"generation\":1}\n1970-01-01T00:03:21Z INFO nodeStateSyncHandler(): sync succeeded\n",
			"1970-01-01T00:03:21Z INFO nodeStateSyncHandler(): sync succeeded\n",
			"1970-01-01T00:03:20Z INFO nodeStateSyncHandler(): new generation {\"generation\":2}\n1970-01-01T00:03:21Z INFO Successfully synced\n",
			"1970-01-01T00:03:20Z INFO nodeStateSyncHandler(): new generation {\"generation\":2}\n1970-01-01T00:03:21Z INFO nodeStateSyncHandler(): new generation {\"generation\":3}\n1970-01-01T00:03:22Z INFO nodeStateSyncHandler(): sync succeeded\n",
		}
		for _, logs := range invalid {
			gomega.Expect(ocpSriovLogProvesGeneration([]byte(logs), state, time.Unix(101, 0))).To(gomega.BeFalse())
		}
		gomega.Expect(ocpSriovLogProvesGeneration(legacy, state, time.Unix(300, 0))).To(gomega.BeFalse())
	})
	ginkgo.It("does not mistake an earlier empty status for applying the current empty generation", func() {
		state := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodeState", "sriov-system", "worker", map[string]interface{}{})
		state.Object["status"] = map[string]interface{}{"syncStatus": "Succeeded", "interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(0)}}}
		c := ocpResourceTestClient(state, sriovDaemonPod("worker", "sriov-system"))
		reset := &ocpSriovClean{nodes: map[string]*ocpSriovNodeClean{"sriov-system/worker": {ref: ocpResourceRef(state), pfs: map[string]bool{"0000:01:00.0": true}, requiresProof: true}}, readLogs: func(context.Context, string, string, string, metav1.Time) ([]byte, error) {
			return sriovGenerationLog("worker", "sriov-system", "old-empty", 1, true), nil
		}}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		err := reset.wait(ctx, c, time.Millisecond)
		gomega.Expect(errors.Is(err, context.DeadlineExceeded)).To(gomega.BeTrue())
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("current generation 2")))
	})
	ginkgo.It("rejects known competing selected-PF claims before deleting intent", func() {
		policy := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodePolicy", "sriov-system", "selected", map[string]interface{}{"nicSelector": map[string]interface{}{"rootDevices": []interface{}{"0000:01:00.0"}}})
		competing := policy.DeepCopy()
		competing.SetName("foreign-policy")
		competing.SetUID("foreign-policy")
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}}
		state := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodeState", "sriov-system", "worker", map[string]interface{}{})
		state.Object["status"] = map[string]interface{}{"syncStatus": "Succeeded", "interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(0)}}}
		c := ocpResourceTestClient(policy, competing, node, state)
		_, err := prepareOCPSriovClean(context.Background(), c, []cleanObjectRef{ocpResourceRef(policy)}, fakeSriovLogReader)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("unselected policy sriov-system/foreign-policy")))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(policy), policy)).To(gomega.Succeed())
	})
	ginkgo.It("requires a daemon log reader before deleting selected non-pristine policy intent", func() {
		policy := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodePolicy", "sriov-system", "selected", map[string]interface{}{"nicSelector": map[string]interface{}{"rootDevices": []interface{}{"0000:01:00.0"}}})
		state := ocpResourceObject(cleanSriovGroup, "SriovNetworkNodeState", "sriov-system", "worker", map[string]interface{}{})
		state.Object["status"] = map[string]interface{}{"syncStatus": "Succeeded", "interfaces": []interface{}{map[string]interface{}{"pciAddress": "0000:01:00.0", "numVfs": int64(0)}}}
		c := ocpResourceTestClient(policy, state, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}})
		n, err := cleanOCPResources(context.Background(), c, &config.LaunchKitConfig{}, []cleanObjectRef{ocpResourceRef(policy)}, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(n).To(gomega.BeZero())
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("log evidence")))
		gomega.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(policy), policy)).To(gomega.Succeed())
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
		scope := &mofedCleanupScope{checkpointName: "scoped-driver-proof", includeDaemonSet: func(*appsv1.DaemonSet) bool { return false }, includePod: func(candidate *corev1.Pod) (bool, error) { return candidate.UID == pod.UID, nil }, hasIntent: func(context.Context) (bool, error) { return true, nil }}
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

var _ = ginkgo.Describe("OpenShift selected absent-target evidence", func() {
	ginkgo.It("refuses first-time absent resources without manufacturing a receipt", func() {
		root := ocpResourceObject("mellanox.com", "NicClusterPolicy", "", "selected", map[string]interface{}{"nvIpam": map[string]interface{}{"enableWebhook": false}})
		ref := ocpResourceRef(root)
		ref.UID = ""
		c := ocpResourceTestClient()
		n, err := cleanOCPResources(context.Background(), c, &config.LaunchKitConfig{}, []cleanObjectRef{ref}, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(n).To(gomega.BeZero())
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("--skip-wait")))
		objects := &corev1.ConfigMapList{}
		gomega.Expect(c.List(context.Background(), objects)).To(gomega.Succeed())
		gomega.Expect(objects.Items).To(gomega.BeEmpty())
		n, err = cleanOCPResources(context.Background(), c, &config.LaunchKitConfig{}, []cleanObjectRef{ref}, CleanOptions{Namespace: "operator-system", SkipWait: true, PollInterval: time.Millisecond})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(n).To(gomega.BeZero())
		receipt, verified, err := loadOCPCleanReceipt(context.Background(), c, "operator-system", []cleanObjectRef{ref})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(verified).To(gomega.BeFalse())
		gomega.Expect(receipt.object.Data["state"]).To(gomega.Equal("resources-unverified"))
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

var _ = ginkgo.Describe("OpenShift selected missing-root history", func() {
	ginkgo.It("refuses a missing root even when its IPPool still survives", func() {
		root := ocpResourceObject("mellanox.com", "NicClusterPolicy", "", "selected", map[string]interface{}{"nvIpam": map[string]interface{}{"enableWebhook": false}})
		pool := ocpResourceObject("nv-ipam.nvidia.com", "IPPool", "operator-system", "surviving-pool", map[string]interface{}{"subnet": "192.0.2.0/24"})
		rootRef := ocpResourceRef(root)
		rootRef.UID = ""
		base := ocpResourceTestClient(pool)
		c := interceptor.NewClient(base, interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			ginkgo.Fail("surviving pool does not qualify missing root ancestry")
			return nil
		}})
		count, err := cleanOCPResources(context.Background(), c, &config.LaunchKitConfig{}, []cleanObjectRef{rootRef, ocpResourceRef(pool)}, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(count).To(gomega.BeZero())
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("prior verified root UID and workload ancestry")))
		gomega.Expect(base.Get(context.Background(), client.ObjectKeyFromObject(pool), pool)).To(gomega.Succeed())
	})
	ginkgo.DescribeTable("does not promote tentative or waived receipts into root teardown proof", func(state string) {
		root := ocpResourceObject("mellanox.com", "NicClusterPolicy", "", "selected", map[string]interface{}{"nvIpam": map[string]interface{}{"enableWebhook": false}})
		pool := ocpResourceObject("nv-ipam.nvidia.com", "IPPool", "operator-system", "surviving-pool", map[string]interface{}{"subnet": "192.0.2.0/24"})
		refs := []cleanObjectRef{ocpResourceRef(root), ocpResourceRef(pool)}
		base := ocpResourceTestClient(pool)
		receipt, _, err := loadOCPCleanReceipt(context.Background(), base, "operator-system", refs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		receipt.ancestryCheckpoint = "unqualified-history"
		receipt.ancestryUID = "unqualified-history"
		gomega.Expect(receipt.save(context.Background(), base, state)).To(gomega.Succeed())
		refs[0].UID = ""
		c := interceptor.NewClient(base, interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			ginkgo.Fail("waived/tentative history must not authorize deletion")
			return nil
		}})
		count, err := cleanOCPResources(context.Background(), c, &config.LaunchKitConfig{}, refs, CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond})
		gomega.Expect(count).To(gomega.BeZero())
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("prior verified root UID and workload ancestry")))
	}, ginkgo.Entry("active cleanup", "cleanup-active"), ginkgo.Entry("waived resource stage", "resources-unverified"), ginkgo.Entry("completed waiver", "complete-unverified"))
	ginkgo.It("retains completed proof if ancestry checkpoint deletion is interrupted", func() {
		root := ocpResourceObject("mellanox.com", "NicClusterPolicy", "", "selected", map[string]interface{}{"nvIpam": map[string]interface{}{"enableWebhook": false}})
		ref := ocpResourceRef(root)
		setOCPCleanOwner(ref.Desired, "completion-order-owner")
		base := ocpResourceTestClient(root)
		failed := false
		c := interceptor.NewClient(base, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok && !failed {
				failed = true
				return errors.New("checkpoint deletion interrupted")
			}
			return c.Delete(ctx, obj, opts...)
		}})
		opts := CleanOptions{Namespace: "operator-system", PollInterval: time.Millisecond}
		count, err := cleanOCPResources(context.Background(), c, &config.LaunchKitConfig{}, []cleanObjectRef{ref}, opts)
		gomega.Expect(count).To(gomega.Equal(1))
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("checkpoint deletion interrupted")))
		receipt := &corev1.ConfigMap{}
		gomega.Expect(base.Get(context.Background(), client.ObjectKey{Namespace: "operator-system", Name: ocpCleanReceiptName("completion-order-owner")}, receipt)).To(gomega.Succeed())
		gomega.Expect(receipt.Data["state"]).To(gomega.Equal("resources-complete"))
		witness := receipt.Data["workload-checkpoint-uid"]
		ref.UID = ""
		count, err = cleanOCPResources(context.Background(), base, &config.LaunchKitConfig{}, []cleanObjectRef{ref}, opts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(count).To(gomega.BeZero())
		gomega.Expect(base.Get(context.Background(), client.ObjectKeyFromObject(receipt), receipt)).To(gomega.Succeed())
		gomega.Expect(receipt.Data["workload-checkpoint-uid"]).To(gomega.Equal(witness))
	})
})
