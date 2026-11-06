// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

func TestDeployment(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DPF existing infrastructure lifecycle")
}

type fakeClient struct {
	objects       map[string]*unstructured.Unstructured
	creates       []string
	dryRuns       []bool
	bmcErr        error
	secretErr     error
	tenant        *fakeClient
	noChildren    bool
	version       string
	tenantBinding config.ClusterBinding
	listErrors    map[string]error
	afterCreate   func(*unstructured.Unstructured)
	onGet         func(bundle.ResourceRef)
	onList        func(bundle.ResourceRef)
	getErrors     map[string]error
	admit         func(*unstructured.Unstructured, bool)
}

func (f *fakeClient) ServerVersion(context.Context) (string, error) { return f.version, nil }

func objectKey(r bundle.ResourceRef) string {
	return r.Group + "/" + r.Kind + "/" + r.Namespace + "/" + r.Name
}
func keyFor(o *unstructured.Unstructured) string {
	gvk := o.GroupVersionKind()
	return objectKey(ref("", gvk.Group, gvk.Kind, o.GetNamespace(), o.GetName()))
}
func (f *fakeClient) put(o *unstructured.Unstructured) { f.objects[keyFor(o)] = o.DeepCopy() }
func (f *fakeClient) Get(_ context.Context, r bundle.ResourceRef) (*unstructured.Unstructured, error) {
	if f.onGet != nil {
		f.onGet(r)
	}
	if err := f.getErrors[r.Kind]; err != nil {
		return nil, err
	}
	if o := f.objects[objectKey(r)]; o != nil {
		return o.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: r.Group, Resource: r.Kind}, r.Name)
}
func (f *fakeClient) List(_ context.Context, r bundle.ResourceRef, selector string) ([]*unstructured.Unstructured, error) {
	if f.onList != nil {
		f.onList(r)
	}
	if err := f.listErrors[r.Kind]; err != nil {
		return nil, err
	}
	parsed, err := labels.Parse(selector)
	if err != nil {
		return nil, err
	}
	out := []*unstructured.Unstructured{}
	for _, o := range f.objects {
		if o.GroupVersionKind().Group == r.Group && o.GetKind() == r.Kind && (r.Namespace == "" || o.GetNamespace() == r.Namespace) && parsed.Matches(labels.Set(o.GetLabels())) {
			out = append(out, o.DeepCopy())
		}
	}
	return out, nil
}
func (f *fakeClient) Create(_ context.Context, o *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	f.creates = append(f.creates, o.GetKind()+"/"+o.GetName())
	f.dryRuns = append(f.dryRuns, dryRun)
	created := o.DeepCopy()
	created.SetUID(types.UID("created-" + o.GetName()))
	created.SetGeneration(1)
	created.SetResourceVersion("1")
	if f.admit != nil {
		f.admit(created, dryRun)
	}
	if !dryRun {
		setReady(created)
		if created.GetKind() == "BlueFieldSoftware" {
			_ = unstructured.SetNestedField(created.Object, "Ready", "status", "phase")
		}
		f.put(created)
		if f.afterCreate != nil {
			f.afterCreate(created)
		}
		if created.GetKind() == "DPUSet" && !f.noChildren {
			dpu := testObject("provisioning.dpu.nvidia.com/v1alpha1", "DPU", o.GetNamespace(), "dpu-child", map[string]any{"dpuDeviceName": "device-a", "blueFieldSoftware": "software", "cluster": map[string]any{"name": f.tenantBinding.Name, "namespace": f.tenantBinding.Namespace}})
			scope, _, _ := unstructured.NestedStringMap(o.Object, "spec", "dpuTemplate", "spec", "cluster", "nodeLabels")
			// Source-realistic augmentation from buildDPUClusterNodeLabels.
			scope["provisioning.dpu.nvidia.com/dpunode-name"] = "node-a"
			scope["provisioning.dpu.nvidia.com/dpunode-namespace"] = o.GetNamespace()
			scope["operator.dpu.nvidia.com/dpf-version"] = "v26.8.0-rc.1"
			_ = unstructured.SetNestedStringMap(dpu.Object, scope, "spec", "cluster", "nodeLabels")
			if flavor := text(o, "spec", "dpuTemplate", "spec", "dpuFlavor"); flavor != "" {
				_ = unstructured.SetNestedField(dpu.Object, flavor, "spec", "dpuFlavor")
			}
			dpu.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "provisioning.dpu.nvidia.com/v1alpha1", Kind: "DPUSet", Name: o.GetName(), UID: created.GetUID()}})
			setReady(dpu)
			_ = unstructured.SetNestedField(dpu.Object, "Ready", "status", "phase")
			f.put(dpu)
			node := testObject("v1", "Node", "", "dpu-child", nil)
			node.SetLabels(scope)
			setReady(node)
			f.tenant.put(node)
		}
	}
	return created, nil
}
func (f *fakeClient) SecretKeys(context.Context, string, string, []string) error { return f.secretErr }
func (f *fakeClient) VerifyBMC(context.Context, config.Generation, config.DeviceBinding, inventory.Device, string) error {
	return f.bmcErr
}

type fakeFactory struct {
	management, tenant *fakeClient
	opens              int
}

func (f *fakeFactory) Management(context.Context, config.Generation) (Client, error) {
	f.opens++
	return f.management, nil
}
func (f *fakeFactory) Tenant(_ context.Context, _ Client, binding config.ClusterBinding) (Client, error) {
	f.management.tenantBinding = binding
	return f.tenant, nil
}

type fakeArtifacts struct {
	calls       int
	err         error
	afterVerify func()
}

func (f *fakeArtifacts) Cached(context.Context, Client, config.Generation, *unstructured.Unstructured) error {
	f.calls++
	if f.afterVerify != nil {
		f.afterVerify()
	}
	return f.err
}

func testObject(api, kind, ns, name string, spec map[string]any) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": api, "kind": kind, "metadata": map[string]any{"name": name, "namespace": ns}}}
	if ns == "" {
		o.SetNamespace("")
	}
	if spec != nil {
		o.Object["spec"] = spec
	}
	o.SetGeneration(1)
	o.SetUID(types.UID(name + "-uid"))
	o.SetResourceVersion("1")
	return o
}
func setReady(o *unstructured.Unstructured) {
	o.Object["status"] = map[string]any{"observedGeneration": o.GetGeneration(), "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": o.GetGeneration()}}, "phase": "Ready"}
}
func readyDeployment(ns, name string) *unstructured.Unstructured {
	o := testObject("apps/v1", "Deployment", ns, name, map[string]any{"replicas": int64(1)})
	setReady(o)
	_ = unstructured.SetNestedField(o.Object, int64(1), "status", "replicas")
	_ = unstructured.SetNestedField(o.Object, int64(1), "status", "readyReplicas")
	_ = unstructured.SetNestedField(o.Object, int64(1), "status", "updatedReplicas")
	_ = unstructured.SetNestedField(o.Object, int64(1), "status", "availableReplicas")
	return o
}

func lifecycleFixture() (*bundle.Bundle, config.Generation, *fakeFactory, *fakeArtifacts) {
	namespace := "sentry-system"
	g := config.Generation{Name: "fixture", Profile: "sentry-bf4", Release: "26.8.0-rc.1", Namespace: "dpf-system", ManagementUID: "management-uid", KubeconfigRef: config.FileRef{File: "unread-by-fake"}, Context: "explicit", Provisioning: config.Provisioning{Devices: []config.DeviceBinding{{ID: "physical-a", DPUDevice: "device-a", DPUNode: "node-a"}}, DPUCluster: config.ClusterBinding{Name: "tenant", Namespace: "dpf-system", ClusterUID: "tenant-uid", ResourceUID: "tenant-resource-uid", Selector: map[string]string{"tenant": "first"}}}, Sentry: config.Sentry{Namespace: &namespace, ImagePullSecret: "pull", MITMSecret: "mitm", InferenceSecret: "inference"}, Devices: []inventory.Device{{ID: "physical-a", SerialNumber: "SERIAL-A", BMCEndpoint: "https://192.0.2.10"}}, DevicePSIDs: map[string]string{"physical-a": "TEST-PSID"}}
	g.ManagementKubernetesVersion = "1.35.4"
	g.Provisioning.DPUCluster.KubernetesVersion = "1.35.6"
	management := &fakeClient{objects: map[string]*unstructured.Unstructured{}, version: "v1.35.4"}
	tenant := &fakeClient{objects: map[string]*unstructured.Unstructured{}, version: "v1.35.6"}
	management.tenant = tenant
	for _, entry := range []struct {
		client *fakeClient
		uid    string
	}{{management, "management-uid"}, {tenant, "tenant-uid"}} {
		ns := testObject("v1", "Namespace", "", "kube-system", nil)
		ns.SetUID(types.UID(entry.uid))
		entry.client.put(ns)
		ns = testObject("v1", "Namespace", "", namespace, nil)
		setReady(ns)
		_ = unstructured.SetNestedField(ns.Object, "Active", "status", "phase")
		entry.client.put(ns)
	}
	operator := testObject("operator.dpu.nvidia.com/v1alpha1", "DPFOperatorConfig", "dpf-system", "operator", map[string]any{"deploymentMode": "zero-trust", "provisioningController": map[string]any{"installInterface": map[string]any{"installViaRedfish": map[string]any{}}}})
	operator.Object["status"] = map[string]any{"version": "v26.8.0-rc.1"}
	management.put(operator)
	for _, name := range []string{"dpf-operator-controller-manager", "dpf-provisioning-controller-manager", "dpuservice-controller-manager", "kamaji-cluster-manager"} {
		deployment := readyDeployment("dpf-system", name)
		deployment.SetLabels(map[string]string{"dpu.nvidia.com/component": name})
		management.put(deployment)
	}
	dns := readyDeployment("kube-system", "coredns")
	dns.SetLabels(map[string]string{"k8s-app": "kube-dns"})
	tenant.put(dns)
	for _, component := range []string{"cni-installer", "multus", "sriov-device-plugin", "sfc-controller", "nvidia-k8s-ipam", "flannel", "kube-proxy"} {
		ns := "dpf-system"
		podLabels := map[string]string{"dpu.nvidia.com/component": component}
		if component == "flannel" {
			podLabels = map[string]string{"app": "flannel", "tier": "node"}
		}
		if component == "kube-proxy" {
			ns = "kube-system"
			podLabels = map[string]string{"k8s-app": "kube-proxy"}
		}
		pod := testObject("v1", "Pod", ns, "base-"+component, map[string]any{"nodeName": "dpu-child"})
		pod.SetLabels(podLabels)
		setReady(pod)
		_ = unstructured.SetNestedField(pod.Object, "Running", "status", "phase")
		tenant.put(pod)
	}
	cluster := testObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUCluster", "dpf-system", "tenant", map[string]any{"type": "kamaji", "kubeconfig": "tenant-kubeconfig"})
	cluster.SetUID("tenant-resource-uid")
	cluster.SetLabels(map[string]string{"tenant": "first"})
	// Actual rc.1/GA DPUCluster controller uses cutil.NewCondition, which
	// publishes neither top-level nor per-condition observedGeneration.
	cluster.Object["status"] = map[string]any{"phase": "Ready", "version": "v1.35.6", "conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "HealthCheckPassed"}}}
	management.put(cluster)
	device := testObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUDevice", "dpf-system", "device-a", map[string]any{"serialNumber": "SERIAL-A", "bmcIp": "192.0.2.10"})
	device.SetLabels(map[string]string{"provisioning.dpu.nvidia.com/dpunode-name": "node-a", "provisioning.dpu.nvidia.com/dpudevice-bmc-ip": "192.0.2.10"})
	setReady(device)
	_ = unstructured.SetNestedField(device.Object, "TEST-PSID", "status", "psid")
	for _, name := range []string{"NodeAttached", "Initialized", "Discovered"} {
		conditions, _, _ := unstructured.NestedSlice(device.Object, "status", "conditions")
		conditions = append(conditions, map[string]any{"type": name, "status": "True"})
		_ = unstructured.SetNestedSlice(device.Object, conditions, "status", "conditions")
	}
	management.put(device)
	node := testObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUNode", "dpf-system", "node-a", map[string]any{"dpus": []any{map[string]any{"name": "device-a"}}, "nodeRebootMethod": map[string]any{"external": map[string]any{}}})
	setReady(node)
	_ = unstructured.SetNestedField(node.Object, "redfish", "status", "dpuInstallInterface")
	management.put(node)
	software := testObject("provisioning.dpu.nvidia.com/v1alpha1", "BlueFieldSoftware", "dpf-system", "software", map[string]any{"osIso": "https://example.invalid/image.iso"})
	software.SetUID("")
	software.SetResourceVersion("")
	software.SetGeneration(0)
	set := testObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUSet", "dpf-system", "selected-set", map[string]any{"dpuDeviceSelector": map[string]any{"matchLabels": map[string]any{"provisioning.dpu.nvidia.com/dpunode-name": "node-a", "provisioning.dpu.nvidia.com/dpudevice-bmc-ip": "192.0.2.10"}}, "dpuTemplate": map[string]any{"spec": map[string]any{"blueFieldSoftware": map[string]any{"name": "software"}, "cluster": map[string]any{"nodeLabels": map[string]any{"nvidia.kubernetes-launch-kit.dpf-instance": "fixture"}}}}})
	set.SetUID("")
	set.SetResourceVersion("")
	set.SetGeneration(0)
	files := []bundle.File{{Path: bundle.ResolvedConfigPath, Data: []byte("formatVersion: v1alpha1\ntarget: dpf\nconfig: {}\n")}}
	records := []bundle.FileRecord{{Path: bundle.ResolvedConfigPath, Role: bundle.ResolvedConfig}}
	for i, o := range []*unstructured.Unstructured{software, set} {
		data, _ := yaml.Marshal(o.Object)
		filename := fmt.Sprintf("objects/%d.yaml", i)
		files = append(files, bundle.File{Path: filename, Data: data})
		records = append(records, bundle.FileRecord{Path: filename, Role: bundle.Manifest})
	}
	b, err := bundle.New(bundle.Index{FormatVersion: bundle.FormatVersion, Target: bundle.Target, Phase: bundle.Phase, Profile: g.Profile, Release: g.Release, Renderer: "fixture", SelectedDevices: []string{"physical-a"}, Files: records, Stages: []bundle.Stage{{ID: "software", Destination: bundle.Management, Namespace: "dpf-system", Files: []string{"objects/0.yaml"}}, {ID: "provision", Destination: bundle.Management, Namespace: "dpf-system", Files: []string{"objects/1.yaml"}, DependsOn: []string{"software"}}}}, files)
	Expect(err).NotTo(HaveOccurred())
	return b, g, &fakeFactory{management: management, tenant: tenant}, &fakeArtifacts{}
}

var _ = Describe("existing infrastructure native lifecycle", func() {
	var dir string
	var b *bundle.Bundle
	var g config.Generation
	var factory *fakeFactory
	var verifier *fakeArtifacts
	var runner Runner
	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "dpf-lifecycle-")
		Expect(err).NotTo(HaveOccurred())
		dir, err = filepath.EvalSymlinks(dir)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dir)
		b, g, factory, verifier = lifecycleFixture()
		runner = Runner{Clients: factory, Artifacts: verifier, EvidenceDir: dir, PollInterval: time.Millisecond, Timeout: 10 * time.Second}
	})
	It("creates only native stages after all gates and persists recovery evidence", func() {
		result, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(verifier.calls).To(Equal(1))
		Expect(factory.management.creates).To(Equal([]string{"BlueFieldSoftware/software", "DPUSet/selected-set", "DPUSet/selected-set"}))
		Expect(factory.tenant.creates).To(BeEmpty())
		data, err := os.ReadFile(result.ReportPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(ContainSubstring("create-requested"))
		Expect(string(data)).To(ContainSubstring("GLOBAL_OPERATOR_NOT_READY"))
		Expect(string(data)).NotTo(ContainSubstring("unread-by-fake"))
		_, err = runner.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		Expect(factory.management.creates).To(HaveLen(3))
	})
	It("rejects API server version drift before writes", func() {
		factory.tenant.version = "v1.36.0"
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("KUBERNETES_VERSION_DRIFT")))
		Expect(factory.management.creates).To(BeEmpty())
	})
	It("rejects management UID mismatch before any mutation", func() {
		g.ManagementUID = "elsewhere"
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("CLUSTER_IDENTITY_MISMATCH")))
		Expect(factory.management.creates).To(BeEmpty())
	})
	It("does not let an unhealthy unselected sibling block healthy selected-node networking", func() {
		pod := testObject("v1", "Pod", "dpf-system", "sibling-multus", map[string]any{"nodeName": "unselected-sibling"})
		pod.SetLabels(map[string]string{"dpu.nvidia.com/component": "multus"})
		factory.tenant.put(pod)
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
	})
	It("does not pass a selected node with a missing networking component", func() {
		delete(factory.tenant.objects, objectKey(ref("", "", "Pod", "dpf-system", "base-multus")))
		runner.Timeout = 40 * time.Millisecond
		result, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(result.Outcome).To(Equal("cancelled"))
	})
	It("rejects existing foreign Sentry controllers in the dedicated namespace before writes", func() {
		factory.tenant.put(readyDeployment("sentry-system", "foreign-policy-controller"))
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("FOREIGN_SERVICE_WORKLOAD")))
		Expect(factory.management.creates).To(BeEmpty())
	})
	It("waits for a chart projection without applying it as an independent object", func() {
		projection := readyDeployment("sentry-system", "projected-workload")
		desired := projection.DeepCopy()
		delete(desired.Object, "status")
		desired.SetUID("")
		desired.SetResourceVersion("")
		desired.SetGeneration(0)
		data, err := yaml.Marshal(desired.Object)
		Expect(err).NotTo(HaveOccurred())
		index := b.Index()
		index.BundleDigest = ""
		index.Files = append(index.Files, bundle.FileRecord{Path: ".l8k/helm/chart/projection.yaml", Role: bundle.Projection})
		index.Stages = append(index.Stages, bundle.Stage{ID: "projection", Destination: bundle.DPU, Namespace: "sentry-system", ProjectionFiles: []string{".l8k/helm/chart/projection.yaml"}, DependsOn: []string{"provision"}})
		files := b.Files()
		files = append(files, bundle.File{Path: ".l8k/helm/chart/projection.yaml", Data: data})
		b, err = bundle.New(index, files)
		Expect(err).NotTo(HaveOccurred())
		factory.management.afterCreate = func(o *unstructured.Unstructured) {
			if o.GetKind() == "DPUSet" {
				factory.tenant.put(projection)
			}
		}
		_, err = runner.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		Expect(factory.tenant.creates).To(BeEmpty())
	})
	It("rejects tenant UID mismatch before any mutation", func() {
		g.Provisioning.DPUCluster.ClusterUID = "elsewhere"
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("CLUSTER_IDENTITY_MISMATCH")))
		Expect(factory.management.creates).To(BeEmpty())
	})
	It("requires the actual service-controller role rather than three similarly named Deployments", func() {
		delete(factory.management.objects, objectKey(ref("", "apps", "Deployment", "dpf-system", "dpuservice-controller-manager")))
		factory.management.put(readyDeployment("dpf-system", "dpf-unrelated-controller-manager"))
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("MANAGEMENT_CONTROLLER_MISSING")))
		Expect(factory.management.creates).To(BeEmpty())
	})
	It("rejects an extra selector-matching tenant in a different namespace", func() {
		other := testObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUCluster", "other-system", "other", nil)
		other.SetLabels(map[string]string{"tenant": "first"})
		setReady(other)
		factory.management.put(other)
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("DPU_CLUSTER_SELECTION_MISMATCH")))
		Expect(factory.management.creates).To(BeEmpty())
	})
	It("requires a healthy Kamaji cluster manager for a Kamaji tenant", func() {
		controller := factory.management.objects[objectKey(ref("", "apps", "Deployment", "dpf-system", "kamaji-cluster-manager"))]
		_ = unstructured.SetNestedField(controller.Object, int64(0), "status", "availableReplicas")
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("KAMAJI_CONTROLLER_NOT_READY")))
		Expect(factory.management.creates).To(BeEmpty())
	})
	It("rejects an extra co-resident DPU before any mutation", func() {
		key := objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUNode", "dpf-system", "node-a"))
		_ = unstructured.SetNestedSlice(factory.management.objects[key].Object, []any{map[string]any{"name": "device-a"}, map[string]any{"name": "device-unselected"}}, "spec", "dpus")
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("EFFECT_DOMAIN_MISMATCH")))
		Expect(factory.management.creates).To(BeEmpty())
	})
	It("rejects foreign selected DPU ownership and selectors", func() {
		foreign := testObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUSet", "dpf-system", "foreign", map[string]any{"dpuDeviceSelector": map[string]any{}})
		factory.management.put(foreign)
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("FOREIGN_DPUSET_SELECTOR")))
		Expect(factory.management.creates).To(BeEmpty())
	})
	It("rejects an orphan child before recreating a missing provisioning owner", func() {
		dpu := testObject("provisioning.dpu.nvidia.com/v1alpha1", "DPU", "dpf-system", "orphan", map[string]any{"dpuDeviceName": "device-a"})
		dpu.SetOwnerReferences([]metav1.OwnerReference{{Kind: "DPUSet", Name: "selected-set", UID: "old-owner-uid"}})
		factory.management.put(dpu)
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("STALE_DPU_OWNER")))
		Expect(factory.management.creates).To(BeEmpty())
	})
	It("prevents DPUSet creation when actual cached artifact bytes do not match", func() {
		verifier.err = fmt.Errorf("cached payload hash mismatch")
		result, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("SOFTWARE_INTEGRITY_FAILED")))
		Expect(factory.management.creates).To(Equal([]string{"BlueFieldSoftware/software"}))
		Expect(result.Events).NotTo(BeEmpty())
		Expect(result.ReportPath).To(BeAnExistingFile())
	})
	It("does not adopt an existing unmarked object even if its desired fields match", func() {
		view, _ := b.Stage("software")
		factory.management.put(view.Documents()[0].ObjectCopy())
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("RESOURCE_OWNERSHIP_CONFLICT")))
		Expect(factory.management.creates).To(BeEmpty())
	})
	It("performs only server-side dry-run creates without artifact checks or persistent resources", func() {
		result, err := runner.Run(context.Background(), b, g, DryRun)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(verifier.calls).To(BeZero())
		Expect(factory.management.dryRuns).To(Equal([]bool{true, true}))
		_, err = factory.management.Get(context.Background(), ref("", "provisioning.dpu.nvidia.com", "DPUSet", "dpf-system", "selected-set"))
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
	It("validates without writes and reports missing desired resources", func() {
		_, err := runner.Run(context.Background(), b, g, Validate)
		Expect(err).To(MatchError(ContainSubstring("RESOURCE_MISSING")))
		Expect(factory.management.creates).To(BeEmpty())
	})
	It("does not poll validation when waiting was not requested", func() {
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		software := factory.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "BlueFieldSoftware", "dpf-system", "software"))]
		_ = unstructured.SetNestedField(software.Object, "Downloading", "status", "phase")
		_ = unstructured.SetNestedSlice(software.Object, []any{map[string]any{"type": "Ready", "status": "False"}}, "status", "conditions")
		reads := 0
		factory.management.onGet = func(r bundle.ResourceRef) {
			if r.Kind == "BlueFieldSoftware" {
				reads++
			}
		}
		_, err = runner.Run(context.Background(), b, g, Validate)
		Expect(err).To(MatchError(ContainSubstring("RESOURCE_NOT_READY")))
		Expect(reads).To(Equal(2))
		Expect(factory.management.creates).To(HaveLen(3))
	})
	It("polls requested validation against refreshed live state without writing", func() {
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		software := factory.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "BlueFieldSoftware", "dpf-system", "software"))]
		_ = unstructured.SetNestedField(software.Object, "Downloading", "status", "phase")
		_ = unstructured.SetNestedSlice(software.Object, []any{map[string]any{"type": "Ready", "status": "False"}}, "status", "conditions")
		reads := 0
		factory.management.onGet = func(r bundle.ResourceRef) {
			if r.Kind == "BlueFieldSoftware" {
				reads++
				if reads == 3 {
					setReady(software)
				}
			}
		}
		runner.Wait = true
		_, err = runner.Run(context.Background(), b, g, Validate)
		Expect(err).NotTo(HaveOccurred())
		Expect(reads).To(Equal(3))
		Expect(factory.management.creates).To(HaveLen(3))
	})
	It("cancels requested validation while preserving the pending report", func() {
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		software := factory.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "BlueFieldSoftware", "dpf-system", "software"))]
		_ = unstructured.SetNestedField(software.Object, "Downloading", "status", "phase")
		_ = unstructured.SetNestedSlice(software.Object, []any{map[string]any{"type": "Ready", "status": "False"}}, "status", "conditions")
		runner.Wait = true
		runner.Timeout = 30 * time.Millisecond
		result, err := runner.Run(context.Background(), b, g, Validate)
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(result.Outcome).To(Equal("cancelled"))
		Expect(result.ReportPath).To(BeAnExistingFile())
		Expect(factory.management.creates).To(HaveLen(3))
	})
	It("never treats zero selected DPU children as success and retains timeout evidence", func() {
		factory.management.noChildren = true
		runner.Timeout = 30 * time.Millisecond
		result, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(HaveOccurred())
		Expect(result.Outcome).To(Equal("cancelled"))
		Expect(result.ReportPath).To(BeAnExistingFile())
	})
	It("rejects BMC TLS/identity failure and absent external keys before any mutation", func() {
		factory.management.bmcErr = fmt.Errorf("certificate mismatch")
		_, err := runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("BMC_IDENTITY_UNVERIFIED")))
		Expect(factory.management.creates).To(BeEmpty())
		factory.management.bmcErr = nil
		factory.tenant.secretErr = fmt.Errorf("missing inference key")
		_, err = runner.Run(context.Background(), b, g, Deploy)
		Expect(err).To(MatchError(ContainSubstring("EXTERNAL_SECRET_MISSING")))
		Expect(factory.management.creates).To(BeEmpty())
	})
	It("does not construct ambient clients for a pre-cancelled operation", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := runner.Run(ctx, b, g, Deploy)
		Expect(err).To(MatchError(context.Canceled))
		Expect(factory.opens).To(BeZero())
	})
	It("fails native access without explicit kubeconfig even when ambient configuration exists", func() {
		DeferCleanup(os.Unsetenv, "KUBECONFIG")
		Expect(os.Setenv("KUBECONFIG", "ambient-must-not-be-read")).To(Succeed())
		_, err := (NativeClients{}).Management(context.Background(), config.Generation{})
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("explicit"))
	})
})

var _ = Describe("completed Deployment readiness", func() {
	It("does not treat disjoint old available and new updated replicas as a completed rollout", func() {
		deployment := readyDeployment("example", "controller")
		deployment.SetGeneration(2)
		Expect(unstructured.SetNestedField(deployment.Object, int64(2), "status", "observedGeneration")).To(Succeed())
		Expect(unstructured.SetNestedField(deployment.Object, int64(2), "status", "replicas")).To(Succeed())
		ready, terminal, _ := Readiness(deployment)
		Expect(ready).To(BeFalse())
		Expect(terminal).To(BeFalse())
		Expect(unstructured.SetNestedField(deployment.Object, int64(1), "status", "replicas")).To(Succeed())
		ready, terminal, _ = Readiness(deployment)
		Expect(ready).To(BeTrue())
		Expect(terminal).To(BeFalse())
	})
	It("uses the API replica default while requiring positive explicit replicas", func() {
		deployment := readyDeployment("example", "controller")
		unstructured.RemoveNestedField(deployment.Object, "spec", "replicas")
		ready, _, _ := Readiness(deployment)
		Expect(ready).To(BeTrue())
		Expect(unstructured.SetNestedField(deployment.Object, int64(0), "spec", "replicas")).To(Succeed())
		ready, _, _ = Readiness(deployment)
		Expect(ready).To(BeFalse())
	})
})
