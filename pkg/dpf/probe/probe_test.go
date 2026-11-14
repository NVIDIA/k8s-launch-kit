// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0
package probe

import (
	"context"
	"fmt"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stest "k8s.io/client-go/testing"
	"testing"
	"time"
)

func TestProbe(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DPF probe ownership and protocol")
}

var _ = Describe("probe integrity", func() {
	It("declares two network vantages without credentials or privileged mounts", func() {
		node := inventory.Node{Name: "cp", UID: "node-uid"}
		a := Pod("probe", "default", map[string]string{"run": "one"}, node, "pod-network", "repo@sha256:d", "request", nil, 60)
		b := Pod("probe", "default", a.Labels, node, "node-network", "repo@sha256:d", "request", nil, 60)
		Expect(a.Spec.HostNetwork).To(BeFalse())
		Expect(b.Spec.HostNetwork).To(BeTrue())
		Expect(b.Spec.DNSPolicy).To(Equal(corev1.DNSDefault))
		Expect(*a.Spec.AutomountServiceAccountToken).To(BeFalse())
		Expect(a.Spec.Volumes).To(HaveLen(2))
		Expect(a.Spec.Volumes[0].Secret).To(BeNil())
		Expect(*a.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
	})
	It("accepts harmless lifecycle changes but rejects admission and ownership mutations", func() {
		p := Pod("p", "default", nil, inventory.Node{Name: "n", UID: "u"}, "pod-network", "repo@sha256:d", "request", nil, 60)
		live := p.DeepCopy()
		live.ResourceVersion = "2"
		live.Spec.NodeName = "n"
		live.Status.Phase = corev1.PodSucceeded
		Expect(samePod(p, live)).To(BeTrue())
		live.Spec.HostNetwork = true
		Expect(samePod(p, live)).To(BeFalse())
		live = p.DeepCopy()
		live.OwnerReferences = []metav1.OwnerReference{{UID: "foreign"}}
		Expect(samePod(p, live)).To(BeFalse())
	})
	It("accepts a protobuf round trip while rejecting changed resources and execution settings", func() {
		p := Pod("p", "default", map[string]string{"run": "one"}, inventory.Node{Name: "node-one", UID: "node-uid"}, "pod-network", "repo@sha256:d", "request", nil, 60)
		encoded, err := p.Marshal()
		Expect(err).NotTo(HaveOccurred())
		var decoded corev1.Pod
		Expect(decoded.Unmarshal(encoded)).To(Succeed())
		Expect(samePod(p, &decoded)).To(BeTrue())
		for _, mutate := range []func(*corev1.Pod){
			func(p *corev1.Pod) {
				p.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("128Mi")
			},
			func(p *corev1.Pod) {
				p.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("20m")
			},
			func(p *corev1.Pod) { p.Spec.Volumes[1].EmptyDir.SizeLimit = ptr(resource.MustParse("32Mi")) },
			func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.Privileged = ptr(true) },
			func(p *corev1.Pod) {
				p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "injected", Image: "unexpected"})
			},
			func(p *corev1.Pod) {
				p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
			},
			func(p *corev1.Pod) {
				p.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchFields[0].Values = []string{"other-node"}
			},
			func(p *corev1.Pod) { p.OwnerReferences = []metav1.OwnerReference{{UID: "foreign"}} },
		} {
			changed := decoded.DeepCopy()
			mutate(changed)
			Expect(samePod(p, changed)).To(BeFalse())
		}
	})
	It("rejects duplicate rows and invalid address/state combinations", func() {
		req := Request{Version: ProtocolVersion, RunID: "run", Deadline: time.Now().Add(time.Minute), Destinations: []Destination{{ID: "a"}}}
		response := Response{Version: ProtocolVersion, RunID: "run", NodeUID: "node", PodUID: "pod", NetworkMode: "pod-network", StartedAt: time.Now(), FinishedAt: time.Now(), Results: []inventory.ProbeResult{{NodeUID: "node", NetworkMode: "pod-network", Destination: "a", DNS: "resolved", Addresses: []inventory.ProbeAddress{{Address: "192.0.2.1", TCP: "reachable", TLS: "verified", HTTP: "authentication-required"}}}}}
		Expect(ValidateResponse(response, req, "node", "pod", "pod-network")).To(Succeed())
		response.Results[0].Addresses[0].TCP = "unavailable"
		Expect(ValidateResponse(response, req, "node", "pod", "pod-network")).NotTo(Succeed())
		response.Results = append(response.Results, response.Results[0])
		Expect(ValidateResponse(response, req, "node", "pod", "pod-network")).NotTo(Succeed())
	})
	It("rejects absent image assets before Kubernetes writes", func() {
		client := fake.NewClientset()
		runner := Runner{}
		_, _, e := runner.Run(context.Background(), config.Request{}, discovery.ManagementClient{Client: client}, &inventory.Inventory{})
		Expect(e).To(HaveOccurred())
		Expect(client.Actions()).To(BeEmpty())
	})
	It("pulls the selected platform manifest and rejects a different runtime image reference", func() {
		assets := Assets{Qualified: true, Repository: "registry.example/probe", ManifestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PlatformDigests: map[string]string{"amd64": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "arm64": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}}
		for _, architecture := range []string{"amd64", "arm64"} {
			for _, repository := range []string{"", "mirror.example/probe"} {
				repo := repository
				if repo == "" {
					repo = assets.Repository
				}
				want := repo + "@" + assets.PlatformDigests[architecture]
				otherArchitecture := "amd64"
				if architecture == "amd64" {
					otherArchitecture = "arm64"
				}
				for _, imageID := range []string{want, repo + "@" + assets.PlatformDigests[otherArchitecture], repo + "@" + assets.ManifestDigest, "other.example/probe@" + assets.PlatformDigests[architecture], "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", "invalid-prefix" + want} {
					client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp", UID: "node-uid"}})
					client.PrependReactor("create", "configmaps", func(a k8stest.Action) (bool, runtime.Object, error) {
						cm := a.(k8stest.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
						cm.UID = "cm-uid"
						Expect(client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("configmaps"), cm, "default")).To(Succeed())
						return true, cm, nil
					})
					created := false
					client.PrependReactor("create", "pods", func(a k8stest.Action) (bool, runtime.Object, error) {
						p := a.(k8stest.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
						Expect(p.Spec.Containers[0].Image).To(Equal(want))
						p.UID, p.Spec.NodeName = "pod-uid", "cp"
						p.Status.Phase = corev1.PodSucceeded
						p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "probe", ImageID: imageID}}
						Expect(client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), p, "default")).To(Succeed())
						created = true
						return true, p, nil
					})
					inv := &inventory.Inventory{Collection: inventory.Collection{RunID: "image-check"}, Management: &inventory.Management{Nodes: []inventory.Node{{Name: "cp", UID: "node-uid", Ready: true, OS: "linux", Architecture: architecture}}}, Evidence: map[string]inventory.Evidence{}}
					_, cleanup, err := (Runner{Assets: assets, Journal: func([]Receipt) error { return nil }}).Run(context.Background(), config.Request{ImageRepository: repository, Timeout: time.Minute, RequestTimeout: time.Second}, discovery.ManagementClient{Client: client, REST: &rest.Config{Host: "https://api.invalid"}, Namespace: "default"}, inv)
					Expect(created).To(BeTrue())
					Expect(cleanup.State).To(Equal("complete"))
					// The fake log transport does not emit a probe response. The expected
					// image must reach that later protocol gate; all other IDs stop earlier.
					if imageID == want {
						Expect(err).To(MatchError(ContainSubstring("probe result binding/coverage invalid")))
					} else {
						Expect(err).To(MatchError(ContainSubstring("probe pulled image digest differs")))
					}
				}
			}
		}
	})
	It("rejects request ConfigMap admission mutation and preserves changed data", func() {
		client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
		client.PrependReactor("create", "configmaps", func(a k8stest.Action) (bool, runtime.Object, error) {
			desired := a.(k8stest.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
			desired.UID = "owned"
			desired.ResourceVersion = "1"
			desired.Data["request.json"] = "redirected"
			Expect(client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("configmaps"), desired, "default")).To(Succeed())
			return true, desired, nil
		})
		assets := Assets{Qualified: true, Repository: "repo/probe", ManifestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PlatformDigests: map[string]string{"amd64": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}
		runner := Runner{Assets: assets, Journal: func([]Receipt) error { return nil }}
		inv := &inventory.Inventory{Collection: inventory.Collection{RunID: "run"}, Management: &inventory.Management{Nodes: []inventory.Node{}}, Evidence: map[string]inventory.Evidence{}}
		_, _, e := runner.Run(context.Background(), config.Request{Timeout: time.Minute, RequestTimeout: time.Second}, discovery.ManagementClient{Client: client, REST: &rest.Config{Host: "https://api.invalid"}, Namespace: "default"}, inv)
		Expect(e).To(MatchError(ContainSubstring("admission changed")))
		for _, a := range client.Actions() {
			Expect(a.GetResource().Resource).NotTo(Equal("pods"))
		}
	})
})

var _ = Describe("probe failure regressions", func() {
	It("clears a delete conflict after authoritative owned-object absence", func() {
		client := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
		client.PrependReactor("create", "configmaps", func(a k8stest.Action) (bool, runtime.Object, error) {
			cm := a.(k8stest.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
			cm.UID = "owned"
			cm.ResourceVersion = "1"
			Expect(client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("configmaps"), cm, "default")).To(Succeed())
			return true, cm, nil
		})
		client.PrependReactor("delete", "configmaps", func(a k8stest.Action) (bool, runtime.Object, error) {
			name := a.(k8stest.DeleteAction).GetName()
			Expect(client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("configmaps"), "default", name)).To(Succeed())
			return true, nil, apierrors.NewConflict(corev1.Resource("configmaps"), name, fmt.Errorf("changed then removed"))
		})
		runner := Runner{Assets: Assets{Qualified: true, Repository: "repo/probe", ManifestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PlatformDigests: map[string]string{"amd64": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}, Journal: func([]Receipt) error { return nil }}
		inv := &inventory.Inventory{Collection: inventory.Collection{RunID: "conflict"}, Management: &inventory.Management{Nodes: []inventory.Node{{Name: "not-ready", UID: "node", Ready: false}}}}
		_, cleanup, err := runner.Run(context.Background(), config.Request{Timeout: time.Minute, RequestTimeout: time.Second}, discovery.ManagementClient{Client: client, REST: &rest.Config{Host: "https://api.invalid"}, Namespace: "default"}, inv)
		Expect(err).To(HaveOccurred())
		Expect(cleanup.State).To(Equal("complete"))
		Expect(cleanup.Leftovers).To(BeEmpty())
	})
})

var _ = Describe("existing tool image adapter", func() {
	It("normalizes IPv6 and separate TLS identity without injecting commands", func() {
		req := Request{Version: ProtocolVersion, RunID: "run", Deadline: time.Now().Add(time.Minute), RequestTimeout: time.Second, Destinations: []Destination{{ID: "dpu", Endpoint: "https://[2001:db8::1]:8443", Path: "/redfish/v1/", ServerName: "bmc.example.invalid"}}}
		files, err := requestFiles(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(files["collect.sh"]).To(Equal(collectorScript))
		Expect(files["request.json"]).To(ContainSubstring(`"hostname":"2001:db8::1"`))
		Expect(files["request.json"]).To(ContainSubstring(`"literal":true`))
		Expect(files["request.json"]).To(ContainSubstring(`"url":"https://bmc.example.invalid:8443/redfish/v1/"`))
		Expect(files["request.json"]).NotTo(ContainSubstring("password"))
		req.Destinations[0].ServerName = "bmc.invalid\r\nInjected: yes"
		_, err = requestFiles(req)
		Expect(err).To(HaveOccurred())
	})
	It("rejects duplicate scope, expired deadlines, bad public trust and oversized maps", func() {
		req := Request{Version: ProtocolVersion, RunID: "run", Deadline: time.Now().Add(time.Minute), RequestTimeout: time.Second, Destinations: []Destination{{ID: "api", Endpoint: "https://api.invalid", Path: "/version"}}}
		req.Destinations = append(req.Destinations, req.Destinations[0])
		_, err := requestFiles(req)
		Expect(err).To(HaveOccurred())
		req.Destinations = req.Destinations[:1]
		req.Destinations[0].CA = []byte("not a certificate")
		_, err = requestFiles(req)
		Expect(err).To(MatchError(ContainSubstring("public probe CA")))
		req.Destinations[0].CA = nil
		req.Deadline = time.Now().Add(-time.Second)
		_, err = requestFiles(req)
		Expect(err).To(HaveOccurred())
		req.Deadline = time.Now().Add(time.Minute)
		req.RunID = string(make([]byte, 900<<10))
		_, err = requestFiles(req)
		Expect(err).To(MatchError(ContainSubstring("ConfigMap budget")))
	})
	It("does not activate a pinned image before runtime qualification", func() {
		assets := Assets{Repository: "repo/probe", ManifestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PlatformDigests: map[string]string{"amd64": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, Qualified: false}
		Expect(assets.Validate()).To(MatchError(ContainSubstring("runtime qualification")))
		assets.Qualified = true
		Expect(assets.Validate()).To(Succeed())
	})
	It("binds the fixed adapter and scratch volume into admission invariants", func() {
		pod := Pod("p", "default", nil, inventory.Node{Name: "n", UID: "u"}, "pod-network", "repo@sha256:d", "request", nil, 60)
		Expect(pod.Spec.Containers[0].Command).To(Equal([]string{"/bin/bash", "/request/collect.sh"}))
		Expect(pod.Spec.Containers[0].WorkingDir).To(Equal("/scratch"))
		Expect(*pod.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem).To(BeTrue())
		Expect(pod.Spec.Volumes[1].EmptyDir.Medium).To(Equal(corev1.StorageMediumMemory))
		changed := pod.DeepCopy()
		changed.Spec.Containers[0].Command = []string{"/bin/bash", "-c", "curl bad.invalid"}
		Expect(samePod(pod, changed)).To(BeFalse())
		changed = pod.DeepCopy()
		changed.Spec.Volumes[1].EmptyDir = nil
		changed.Spec.Volumes[1].HostPath = &corev1.HostPathVolumeSource{Path: "/"}
		Expect(samePod(pod, changed)).To(BeFalse())
	})
})
