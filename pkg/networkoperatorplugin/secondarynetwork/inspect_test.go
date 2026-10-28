// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package secondarynetwork

import (
	"context"
	"errors"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSecondaryNetwork(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Secondary-network ownership")
}

func fixtureInstaller(name, image, command string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "platform-networking", UID: types.UID(name + "-uid"), Generation: 1}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "installer", Image: image, Command: []string{command}, VolumeMounts: []corev1.VolumeMount{{Name: "bin", MountPath: "/host/opt/cni/bin"}}}}, Volumes: []corev1.Volume{{Name: "bin", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/opt/cni/bin"}}}}}}}, Status: appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 1, UpdatedNumberScheduled: 1, NumberReady: 1, NumberAvailable: 1}}
}

func fixturePolicy(managed ...string) *unstructured.Unstructured {
	policy := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "mellanox.com/v1alpha1", "kind": "NicClusterPolicy", "metadata": map[string]interface{}{"name": "policy", "uid": "policy-uid"}, "spec": map[string]interface{}{}}}
	for _, component := range managed {
		Expect(unstructured.SetNestedMap(policy.Object, map[string]interface{}{"image": "test"}, "spec", "secondaryNetwork", component)).To(Succeed())
	}
	return policy
}

func fixtureClient(objects ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	Expect(corev1.AddToScheme(scheme)).To(Succeed())
	Expect(appsv1.AddToScheme(scheme)).To(Succeed())
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func owner(kind, name, uid string) metav1.OwnerReference {
	yes := true
	apiVersion := "mellanox.com/v1alpha1"
	if kind == "DaemonSet" {
		apiVersion = "apps/v1"
	}
	return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: types.UID(uid), Controller: &yes}
}

type deniedReader struct {
	client.Reader
	listErr, errorGet error
}

func (r deniedReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if r.listErr != nil {
		return r.listErr
	}
	return r.Reader.List(ctx, list, opts...)
}
func (r deniedReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if r.errorGet != nil {
		return r.errorGet
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

var _ = Describe("known API installers", func() {
	It("recognizes independent Multus and plugin signatures through mirrored image paths", func() {
		multus := fixtureInstaller("renamed", "registry.example:5000/platform/multus-cni:v1", "/thin_entrypoint")
		plugins := fixtureInstaller("other", "registry.example:5000/mellanox/plugins:v1", "/entrypoint.sh")
		result, err := Inspect(context.Background(), fixtureClient(multus, plugins))
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Observation.Multus.Status).To(Equal(config.InstallerExternal))
		Expect(result.Observation.CNIPlugins.Status).To(Equal(config.InstallerExternal))
	})
	It("does not infer plugin binaries from Multus or classify a name alone", func() {
		multus := fixtureInstaller("multus", "example/platform-custom:v1", "/usr/bin/multus-daemon")
		unrelated := fixtureInstaller("cni-plugins", "example/unrelated:v1", "/run")
		result, err := Inspect(context.Background(), fixtureClient(multus, unrelated))
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Observation.Multus.Status).To(Equal(config.InstallerExternal))
		Expect(result.Observation.CNIPlugins.Status).To(Equal(config.InstallerNotDetected))
		multus.Spec.Template.Spec.Volumes = nil
		result, err = Inspect(context.Background(), fixtureClient(multus))
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Observation.Multus.Status).To(Equal(config.InstallerNotDetected))
	})
	It("recognizes a platform image with Multus-specific entrypoint arguments", func() {
		ds := fixtureInstaller("platform-network", "platform.example/custom:v1", "/entrypoint.sh")
		ds.Spec.Template.Spec.Containers[0].Args = []string{"--multus-conf-file=auto", "--cni-conf-dir=/host/etc/cni/net.d"}
		result, err := Inspect(context.Background(), fixtureClient(ds))
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Observation.Multus.Status).To(Equal(config.InstallerExternal))
		Expect(result.Observation.CNIPlugins.Status).To(Equal(config.InstallerNotDetected))
	})
	It("requires a matching live controller UID for operator ownership", func() {
		ds := fixtureInstaller("multus", "mirror/multus-cni:v1", "/thin_entrypoint")
		ds.OwnerReferences = []metav1.OwnerReference{owner("NicClusterPolicy", "policy", "policy-uid")}
		result, err := Inspect(context.Background(), fixtureClient(ds, fixturePolicy(Multus)))
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Observation.Multus.Status).To(Equal(config.InstallerOperatorManaged))
		ds.OwnerReferences[0].UID = "old-policy-uid"
		result, err = Inspect(context.Background(), fixtureClient(ds, fixturePolicy(Multus)))
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Observation.Multus.Status).To(Equal(config.InstallerAmbiguous))
	})
	It("keeps a NotReady installer external and reports duplicate installers as ambiguous", func() {
		ds := fixtureInstaller("multus", "mirror/multus-cni:v1", "/thin_entrypoint")
		ds.Status.NumberReady = 0
		result, err := Inspect(context.Background(), fixtureClient(ds))
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Observation.Multus.Status).To(Equal(config.InstallerExternal))
		other := ds.DeepCopy()
		other.Name = "another"
		other.UID = "another-uid"
		result, err = Inspect(context.Background(), fixtureClient(ds, other))
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Observation.Multus.Status).To(Equal(config.InstallerAmbiguous))
	})
	It("preserves unavailable evidence for discovery but never swallows cancellation", func() {
		c := deniedReader{Reader: fixtureClient(), listErr: errors.New("denied")}
		observation, err := DiscoverEvidence(context.Background(), c)
		Expect(err).NotTo(HaveOccurred())
		Expect(observation.Multus.Status).To(Equal(config.InstallerUnavailable))
		Expect(observation.Multus.Reason).To(ContainSubstring("denied"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = DiscoverEvidence(ctx, c)
		Expect(err).To(MatchError(context.Canceled))
	})
})

var _ = Describe("deployment ownership boundary", func() {
	It("does not inspect an unrelated bundle", func() {
		checks, err := CheckPolicy(context.Background(), nil, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(checks).To(BeEmpty())
	})
	It("blocks installing either managed component over a foreign installer", func() {
		for _, component := range []string{Multus, CNIPlugins} {
			image, command := "mirror/multus-cni:v1", "/thin_entrypoint"
			if component == CNIPlugins {
				image = "mirror/plugins:v1"
				command = "/entrypoint.sh"
			}
			_, err := CheckPolicy(context.Background(), fixtureClient(fixtureInstaller(component, image, command)), fixturePolicy(Multus, CNIPlugins), nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("refusing to install " + component))
		}
	})
	It("preserves an existing operator-owned installation and refuses implicit removal even without its DaemonSet", func() {
		ds := fixtureInstaller("multus", "mirror/multus-cni:v1", "/thin_entrypoint")
		ds.OwnerReferences = []metav1.OwnerReference{owner("NicClusterPolicy", "policy", "policy-uid")}
		_, err := CheckPolicy(context.Background(), fixtureClient(ds, fixturePolicy(Multus)), fixturePolicy(Multus, CNIPlugins), nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = CheckPolicy(context.Background(), fixtureClient(fixturePolicy(Multus)), fixturePolicy(), nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("managed-to-external"))
		_, err = CheckPolicy(context.Background(), fixtureClient(fixturePolicy(Multus)), &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "mellanox.com/v1alpha1", "kind": "NicClusterPolicy", "metadata": map[string]interface{}{"name": "policy"}, "spec": map[string]interface{}{"secondaryNetwork": map[string]interface{}{"multus": nil}}}}, nil)
		Expect(err).To(HaveOccurred())
	})
	It("permits omission with unverified health but requires live-policy evidence", func() {
		base := fixtureClient()
		checks, err := CheckPolicy(context.Background(), base, fixturePolicy(), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(checks).To(HaveLen(2))
		Expect(checks[0].Skipped).To(BeTrue())
		Expect(checks[0].Reason).To(ContainSubstring("host/platform"))
		checks, err = CheckPolicy(context.Background(), deniedReader{Reader: base, listErr: errors.New("list denied")}, fixturePolicy(), nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(checks[0].Skipped).To(BeTrue())
		Expect(checks[0].Reason).To(ContainSubstring("list denied"))
		_, err = CheckPolicy(context.Background(), deniedReader{Reader: base, errorGet: errors.New("get denied")}, fixturePolicy(), nil)
		Expect(err).To(HaveOccurred())
		_, err = CheckPolicy(context.Background(), deniedReader{Reader: base, listErr: errors.New("list denied")}, fixturePolicy(Multus), nil)
		Expect(err).To(HaveOccurred())
	})
	It("handles an uninstalled policy CRD as absence, unlike denied access", func() {
		reader := deniedReader{Reader: fixtureClient(), errorGet: apierrors.NewNotFound(schema.GroupResource{Group: "mellanox.com", Resource: "nicclusterpolicies"}, "policy")}
		policy, err := GetPolicy(context.Background(), reader, "policy")
		Expect(err).NotTo(HaveOccurred())
		Expect(policy).To(BeNil())
	})
	It("distinguishes required-worker coverage, reconciling pods, and incompatible selectors", func() {
		ds := fixtureInstaller("multus", "mirror/multus-cni:v1", "/thin_entrypoint")
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "installer", Namespace: ds.Namespace, OwnerReferences: []metav1.OwnerReference{owner("DaemonSet", ds.Name, string(ds.UID))}}, Spec: corev1.PodSpec{NodeName: "worker-a"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
		checks, err := CheckPolicy(context.Background(), fixtureClient(ds, pod), fixturePolicy(), []string{"worker-a"})
		Expect(err).NotTo(HaveOccurred())
		Expect(checks[0].Skipped).To(BeFalse())
		Expect(checks[0].Failed).To(BeFalse())
		Expect(checks[0].Progressing).To(BeFalse())
		Expect(checks[0].Reason).To(ContainSubstring("Ready pods on all 1"))
		checks, err = CheckPolicy(context.Background(), fixtureClient(ds, pod), fixturePolicy(), []string{"worker-a", "worker-b"})
		Expect(err).NotTo(HaveOccurred())
		Expect(checks[0].Progressing).To(BeTrue())
		Expect(checks[0].Reason).To(ContainSubstring("worker-b"))
		ds.Spec.Template.Spec.NodeSelector = map[string]string{"platform": "supported"}
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-b", Labels: map[string]string{"platform": "other"}}}
		checks, err = CheckPolicy(context.Background(), fixtureClient(ds, pod, node), fixturePolicy(), []string{"worker-b"})
		Expect(err).NotTo(HaveOccurred())
		Expect(checks[0].Failed).To(BeTrue())
		Expect(checks[0].Reason).To(ContainSubstring("excludes required worker"))
	})
})
