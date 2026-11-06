// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Extend the existing lifecycle fixture with a second physical device, retaining
// distinct per-device selectors even when both cards share a host reset domain.
func multiDeviceFixture(sharedNode bool) (*bundle.Bundle, config.Generation, *fakeFactory, *fakeArtifacts) {
	b, g, f, v := lifecycleFixture()
	nodeName := "node-b"
	if sharedNode {
		nodeName = "node-a"
	}
	g.Provisioning.Devices = append(g.Provisioning.Devices, config.DeviceBinding{ID: "physical-b", DPUDevice: "device-b", DPUNode: nodeName})
	g.Devices = append(g.Devices, inventory.Device{ID: "physical-b", SerialNumber: "SERIAL-B", BMCEndpoint: "https://192.0.2.11"})
	g.DevicePSIDs["physical-b"] = "TEST-PSID"
	device := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUDevice", g.Namespace, "device-a"))].DeepCopy()
	device.SetName("device-b")
	device.SetUID("device-b-uid")
	device.SetLabels(map[string]string{"provisioning.dpu.nvidia.com/dpunode-name": nodeName, "provisioning.dpu.nvidia.com/dpudevice-bmc-ip": "192.0.2.11"})
	Expect(unstructured.SetNestedField(device.Object, "SERIAL-B", "spec", "serialNumber")).To(Succeed())
	Expect(unstructured.SetNestedField(device.Object, "192.0.2.11", "spec", "bmcIp")).To(Succeed())
	f.management.put(device)
	node := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUNode", g.Namespace, "node-a"))].DeepCopy()
	if sharedNode {
		Expect(unstructured.SetNestedSlice(node.Object, []any{map[string]any{"name": "device-a"}, map[string]any{"name": "device-b"}}, "spec", "dpus")).To(Succeed())
	} else {
		node.SetName(nodeName)
		node.SetUID("node-b-uid")
		Expect(unstructured.SetNestedSlice(node.Object, []any{map[string]any{"name": "device-b"}}, "spec", "dpus")).To(Succeed())
	}
	f.management.put(node)
	set := resourcesInBundle(b, "DPUSet")[0].ObjectCopy()
	set.SetName("selected-set-b")
	Expect(unstructured.SetNestedStringMap(set.Object, map[string]string{"provisioning.dpu.nvidia.com/dpunode-name": nodeName, "provisioning.dpu.nvidia.com/dpudevice-bmc-ip": "192.0.2.11"}, "spec", "dpuDeviceSelector", "matchLabels")).To(Succeed())
	data, err := yaml.Marshal(set.Object)
	Expect(err).NotTo(HaveOccurred())
	i := b.Index()
	i.BundleDigest = ""
	i.SelectedDevices = append(i.SelectedDevices, "physical-b")
	i.Files = append(i.Files, bundle.FileRecord{Path: "objects/2.yaml", Role: bundle.Manifest})
	i.Stages[1].Files = append(i.Stages[1].Files, "objects/2.yaml")
	b, err = bundle.New(i, append(b.Files(), bundle.File{Path: "objects/2.yaml", Data: data}))
	Expect(err).NotTo(HaveOccurred())
	pods, err := f.tenant.List(context.Background(), ref("", "", "Pod", "", ""), "")
	Expect(err).NotTo(HaveOccurred())
	for _, pod := range pods {
		pod.SetName(pod.GetName() + "-b")
		pod.SetUID(pod.GetUID() + "-b")
		Expect(unstructured.SetNestedField(pod.Object, "dpu-child-b", "spec", "nodeName")).To(Succeed())
		f.tenant.put(pod)
	}
	return b, g, f, v
}

func setDeviceReady(device *unstructured.Unstructured, ready bool) {
	conditions, _, err := unstructured.NestedSlice(device.Object, "status", "conditions")
	Expect(err).NotTo(HaveOccurred())
	for _, item := range conditions {
		condition := item.(map[string]any)
		if condition["type"] == "Ready" {
			condition["status"] = "False"
			if ready {
				condition["status"] = "True"
			}
		}
	}
	Expect(unstructured.SetNestedSlice(device.Object, conditions, "status", "conditions")).To(Succeed())
}

var _ = Describe("serialized multi-device provisioning", func() {
	newRunner := func(f *fakeFactory, v *fakeArtifacts) Runner {
		dir, err := os.MkdirTemp("", "dpf-multidevice-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		dir, err = filepath.EvalSymlinks(dir)
		Expect(err).NotTo(HaveOccurred())
		return Runner{Clients: f, Artifacts: v, EvidenceDir: dir, PollInterval: time.Millisecond, Timeout: 10 * time.Second}
	}
	// Progress each real persisted DPUSet through NotReady, native Ready with
	// a still-pending host reset, then a clear host and Ready device. No timer or
	// invocation count alone can create the second set before these live states.
	progress := func(g config.Generation, f *fakeFactory, finished func(string)) *[]string {
		f.management.noChildren = true
		completed := []string{}
		activeSet, activeNode, activeDevice := "", "", ""
		setReads, nodeReads := 0, 0
		f.management.afterCreate = func(set *unstructured.Unstructured) {
			if set.GetKind() != "DPUSet" {
				return
			}
			Expect(activeSet).To(BeEmpty(), "another physical provisioning operation is still active")
			binding := g.Provisioning.Devices[0]
			childName := "dpu-child"
			if set.GetName() == "selected-set-b" {
				binding = g.Provisioning.Devices[1]
				childName = "dpu-child-b"
				Expect(completed).To(Equal([]string{"selected-set"}))
			}
			activeSet, activeNode, activeDevice = set.GetName(), binding.DPUNode, binding.DPUDevice
			setReads, nodeReads = 0, 0
			current := f.management.objects[keyFor(set)]
			Expect(unstructured.SetNestedField(current.Object, int64(0), "status", "availableReplicas")).To(Succeed())
			conditions, _, _ := unstructured.NestedSlice(current.Object, "status", "conditions")
			conditions[0].(map[string]any)["status"] = "False"
			Expect(unstructured.SetNestedSlice(current.Object, conditions, "status", "conditions")).To(Succeed())
			node := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUNode", g.Namespace, activeNode))]
			node.SetAnnotations(map[string]string{"provisioning.dpu.nvidia.com/dpunode-external-reboot-required": "SystemLevelReset"})
			setDeviceReady(f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUDevice", g.Namespace, activeDevice))], false)
			child := testObject("provisioning.dpu.nvidia.com/v1alpha1", "DPU", g.Namespace, childName, map[string]any{"dpuDeviceName": binding.DPUDevice, "blueFieldSoftware": "software", "cluster": map[string]any{"name": g.Provisioning.DPUCluster.Name, "namespace": g.Provisioning.DPUCluster.Namespace}})
			scope, err := expectedDPUNodeLabels(set, binding.DPUNode, g.Namespace, g.Release)
			Expect(err).NotTo(HaveOccurred())
			Expect(unstructured.SetNestedStringMap(child.Object, scope, "spec", "cluster", "nodeLabels")).To(Succeed())
			child.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: set.GetAPIVersion(), Kind: "DPUSet", Name: set.GetName(), UID: set.GetUID()}})
			setReady(child)
			f.management.put(child)
			tenantNode := testObject("v1", "Node", "", childName, nil)
			tenantNode.SetLabels(scope)
			setReady(tenantNode)
			f.tenant.put(tenantNode)
		}
		f.management.onGet = func(reference bundle.ResourceRef) {
			if activeSet == "" {
				return
			}
			if reference.Kind == "DPUSet" && reference.Name == activeSet {
				setReads++
				if setReads >= 2 {
					setReady(f.management.objects[objectKey(reference)])
				}
			}
			if reference.Kind == "DPUNode" && reference.Name == activeNode && setReads >= 2 {
				nodeReads++
				if nodeReads >= 2 {
					node := f.management.objects[objectKey(reference)]
					node.SetAnnotations(nil)
					setDeviceReady(f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUDevice", g.Namespace, activeDevice))], true)
					name := activeSet
					completed = append(completed, name)
					activeSet = ""
					if finished != nil {
						finished(name)
					}
				}
			}
		}
		return &completed
	}
	DescribeTable("waits for each physical operation and host reset before creating the next set", func(sharedNode bool) {
		b, g, f, v := multiDeviceFixture(sharedNode)
		completed := progress(g, f, nil)
		r := newRunner(f, v)
		result, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(*completed).To(Equal([]string{"selected-set", "selected-set-b"}))
		Expect(v.calls).To(Equal(2))
		before := len(f.management.creates)
		result, err = r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(f.management.creates).To(HaveLen(before))
		result, err = r.Run(context.Background(), b, g, Validate)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(f.management.creates).To(HaveLen(before))
	}, Entry("distinct hosts", false), Entry("two cards sharing one host reset domain", true))
	It("continues validating DPUSet projections that were never submitted for creation", func() {
		b, g, f, v := lifecycleFixture()
		projection := resourcesInBundle(b, "DPUSet")[0].ObjectCopy()
		projection.SetName("projected-set")
		Expect(unstructured.SetNestedStringMap(projection.Object, map[string]string{"provisioning.dpu.nvidia.com/dpunode-name": "external-node"}, "spec", "dpuDeviceSelector", "matchLabels")).To(Succeed())
		data, err := yaml.Marshal(projection.Object)
		Expect(err).NotTo(HaveOccurred())
		i := b.Index()
		i.BundleDigest = ""
		i.Files = append(i.Files, bundle.FileRecord{Path: "projections/set.yaml", Role: bundle.Projection})
		i.Stages[1].ProjectionFiles = append(i.Stages[1].ProjectionFiles, "projections/set.yaml")
		b, err = bundle.New(i, append(b.Files(), bundle.File{Path: "projections/set.yaml", Data: data}))
		Expect(err).NotTo(HaveOccurred())
		actual := projection.DeepCopy()
		Expect(unstructured.SetNestedField(actual.Object, "foreign-software", "spec", "dpuTemplate", "spec", "blueFieldSoftware", "name")).To(Succeed())
		f.management.put(actual)
		_, err = newRunner(f, v).Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("PROJECTION_DRIFT")))
		Expect(f.management.creates).NotTo(ContainElement("DPUSet/projected-set"))
	})
	It("still rejects a pending prior transition on the second selected host before any writes", func() {
		b, g, f, v := multiDeviceFixture(false)
		node := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUNode", g.Namespace, "node-b"))]
		node.SetAnnotations(map[string]string{"provisioning.dpu.nvidia.com/dpunode-external-reboot-required": "SystemLevelReset"})
		_, err := newRunner(f, v).Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("NODE_TRANSITION_PENDING")))
		Expect(f.management.creates).To(BeEmpty())
	})
	DescribeTable("retains full selected-scope checks before the next physical create", func(change, code string) {
		b, g, f, v := multiDeviceFixture(false)
		progress(g, f, func(name string) {
			if name == "selected-set" {
				switch change {
				case "owner":
					f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUSet", g.Namespace, name))].SetUID("replacement-set-uid")
				case "device":
					device := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUDevice", g.Namespace, "device-b"))]
					Expect(unstructured.SetNestedField(device.Object, "FOREIGN-SERIAL", "spec", "serialNumber")).To(Succeed())
				case "foreign":
					child := testObject("provisioning.dpu.nvidia.com/v1alpha1", "DPU", g.Namespace, "foreign-child", map[string]any{"dpuDeviceName": "device-b"})
					child.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "provisioning.dpu.nvidia.com/v1alpha1", Kind: "DPUSet", Name: "foreign-set", UID: "foreign-set-uid"}})
					f.management.put(child)
				}
			}
		})
		_, err := newRunner(f, v).Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring(code)))
		Expect(f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUSet", g.Namespace, "selected-set-b"))]).To(BeNil())
	}, Entry("replaced owner UID", "owner", "STALE_DPU_OWNER"), Entry("second device identity drift", "device", "DEVICE_IDENTITY_MISMATCH"), Entry("foreign ownership of the second device", "foreign", "FOREIGN_DPU_OWNER"))
	It("stops on cancellation with a pending first host reset without creating the second set", func() {
		b, g, f, v := multiDeviceFixture(false)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := false
		nodeReads := 0
		f.management.afterCreate = func(object *unstructured.Unstructured) {
			if object.GetKind() == "DPUSet" {
				Expect(object.GetName()).To(Equal("selected-set"))
				node := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUNode", g.Namespace, "node-a"))]
				node.SetAnnotations(map[string]string{"provisioning.dpu.nvidia.com/dpunode-external-reboot-required": "SystemLevelReset"})
				started = true
			}
		}
		f.management.onGet = func(reference bundle.ResourceRef) {
			if started && reference.Kind == "DPUNode" && reference.Name == "node-a" {
				nodeReads++
				if nodeReads >= 3 {
					cancel()
				}
			}
		}
		result, err := newRunner(f, v).Run(ctx, b, g, Deploy)
		Expect(err).To(MatchError(context.Canceled))
		Expect(result.Outcome).To(Equal("cancelled"))
		Expect(f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUSet", g.Namespace, "selected-set"))]).NotTo(BeNil())
		Expect(f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUSet", g.Namespace, "selected-set-b"))]).To(BeNil())
		Expect(v.calls).To(Equal(1))
	})
})
