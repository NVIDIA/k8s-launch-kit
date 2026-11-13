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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
)

// Anonymous fixture follows rc.1 kamaji/dns.go and its CoreDNS/keepalived chart.
// Operator namespace and tenant control-plane namespace deliberately differ.
func hostedDNSFixture() (*bundle.Bundle, config.Generation, *fakeFactory, *fakeArtifacts) {
	b, g, f, v := lifecycleFixture()
	old := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPUCluster", g.Provisioning.DPUCluster.Namespace, g.Provisioning.DPUCluster.Name))]
	delete(f.management.objects, keyFor(old))
	g.Provisioning.DPUCluster.Namespace = "tenant-control"
	old.SetNamespace("tenant-control")
	selector := map[string]string{"node-role.kubernetes.io/control-plane": ""}
	Expect(unstructured.SetNestedMap(old.Object, map[string]any{"keepalived": map[string]any{"vip": "192.0.2.10", "interface": "eth-test", "nodeSelector": map[string]any{"node-role.kubernetes.io/control-plane": ""}}}, "spec", "clusterEndpoint")).To(Succeed())
	f.management.put(old)
	dnsLabels := map[string]string{"app.kubernetes.io/name": "coredns", "app.kubernetes.io/instance": "dns-tenant", "dpu.nvidia.com/cluster": "tenant", "dpu.nvidia.com/cluster-namespace": "tenant-control"}
	match := map[string]any{"app.kubernetes.io/name": "coredns", "app.kubernetes.io/instance": "dns-tenant"}
	dep := readyDeployment("dpf-system", "dns-tenant")
	dep.SetLabels(dnsLabels)
	Expect(unstructured.SetNestedMap(dep.Object, map[string]any{"matchLabels": match}, "spec", "selector")).To(Succeed())
	Expect(unstructured.SetNestedMap(dep.Object, map[string]any{"metadata": map[string]any{"labels": match}, "spec": map[string]any{"nodeSelector": map[string]any{"node-role.kubernetes.io/control-plane": ""}, "containers": []any{map[string]any{"name": "coredns", "ports": []any{map[string]any{"name": "dns", "containerPort": int64(5353), "protocol": "UDP"}, map[string]any{"name": "dns-tcp", "containerPort": int64(5353), "protocol": "TCP"}}}}}}, "spec", "template")).To(Succeed())
	f.management.put(dep)
	hostSvc := testObject("v1", "Service", "dpf-system", "dns-tenant", map[string]any{"type": "NodePort", "selector": match, "externalTrafficPolicy": "Cluster", "ports": []any{map[string]any{"name": "dns", "protocol": "UDP", "port": int64(53), "targetPort": "dns", "nodePort": int64(32001)}, map[string]any{"name": "dns-tcp", "protocol": "TCP", "port": int64(53), "targetPort": "dns-tcp", "nodePort": int64(32002)}}})
	hostSvc.SetLabels(dnsLabels)
	f.management.put(hostSvc)
	node := testObject("v1", "Node", "", "manager-a", nil)
	node.SetLabels(selector)
	setReady(node)
	f.management.put(node)
	ds := testObject("apps/v1", "DaemonSet", "tenant-control", "tenant-keepalived", map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": "tenant-keepalived"}}, "template": map[string]any{"spec": map[string]any{"hostNetwork": true, "nodeSelector": map[string]any{"node-role.kubernetes.io/control-plane": ""}, "initContainers": []any{map[string]any{"name": "create-conf", "env": []any{map[string]any{"name": "VIRTUAL_IP", "value": "192.0.2.10"}, map[string]any{"name": "INTERFACE", "value": "eth-test"}}}}}}})
	ds.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: old.GetAPIVersion(), Kind: "DPUCluster", Name: old.GetName(), UID: old.GetUID()}})
	setReady(ds)
	for _, field := range []string{"desiredNumberScheduled", "numberReady", "updatedNumberScheduled"} {
		Expect(unstructured.SetNestedField(ds.Object, int64(1), "status", field)).To(Succeed())
	}
	f.management.put(ds)
	keepPod := testObject("v1", "Pod", "tenant-control", "keep-a", map[string]any{"nodeName": "manager-a", "hostNetwork": true})
	keepPod.SetLabels(map[string]string{"app.kubernetes.io/name": "tenant-keepalived"})
	keepPod.SetOwnerReferences([]metav1.OwnerReference{{Kind: "DaemonSet", Name: ds.GetName(), UID: ds.GetUID(), Controller: ptr.To(true)}})
	setReady(keepPod)
	Expect(unstructured.SetNestedField(keepPod.Object, "Running", "status", "phase")).To(Succeed())
	f.management.put(keepPod)
	rs := testObject("apps/v1", "ReplicaSet", "dpf-system", "dns-rs", nil)
	template, _, err := unstructured.NestedMap(dep.Object, "spec", "template")
	Expect(err).NotTo(HaveOccurred())
	Expect(unstructured.SetNestedMap(rs.Object, template, "spec", "template")).To(Succeed())
	rs.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Deployment", Name: dep.GetName(), UID: dep.GetUID(), Controller: ptr.To(true)}})
	f.management.put(rs)
	dnsPod := testObject("v1", "Pod", "dpf-system", "dns-pod", map[string]any{"nodeName": "manager-a"})
	dnsPod.SetLabels(dnsLabels)
	dnsPod.SetOwnerReferences([]metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs.GetName(), UID: rs.GetUID(), Controller: ptr.To(true)}})
	setReady(dnsPod)
	Expect(unstructured.SetNestedField(dnsPod.Object, "Running", "status", "phase")).To(Succeed())
	Expect(unstructured.SetNestedField(dnsPod.Object, "198.51.100.10", "status", "podIP")).To(Succeed())
	f.management.put(dnsPod)
	hostSlice := testObject("discovery.k8s.io/v1", "EndpointSlice", "dpf-system", "dns-backends", nil)
	hostSlice.SetLabels(map[string]string{"kubernetes.io/service-name": "dns-tenant", "endpointslice.kubernetes.io/managed-by": "endpointslice-controller.k8s.io"})
	hostSlice.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Service", Name: hostSvc.GetName(), UID: hostSvc.GetUID(), Controller: ptr.To(true)}})
	hostSlice.Object["addressType"] = "IPv4"
	hostSlice.Object["ports"] = []any{map[string]any{"name": "dns", "port": int64(5353), "protocol": "UDP"}, map[string]any{"name": "dns-tcp", "port": int64(5353), "protocol": "TCP"}}
	hostSlice.Object["endpoints"] = []any{map[string]any{"addresses": []any{"198.51.100.10"}, "conditions": map[string]any{"ready": true}, "targetRef": map[string]any{"kind": "Pod", "name": dnsPod.GetName(), "namespace": dnsPod.GetNamespace(), "uid": string(dnsPod.GetUID())}}}
	f.management.put(hostSlice)
	tenantSvc := testObject("v1", "Service", "kube-system", "kube-dns", map[string]any{"type": "ClusterIP", "clusterIP": "203.0.113.53", "ports": []any{map[string]any{"name": "dns", "protocol": "UDP", "port": int64(53), "targetPort": int64(53)}, map[string]any{"name": "dns-tcp", "protocol": "TCP", "port": int64(53), "targetPort": int64(53)}}})
	tenantSvc.SetLabels(map[string]string{"k8s-app": "kube-dns", "kubernetes.io/name": "CoreDNS"})
	f.tenant.put(tenantSvc)
	tenantSlice := testObject("discovery.k8s.io/v1", "EndpointSlice", "kube-system", "kube-dns", nil)
	tenantSlice.SetLabels(map[string]string{"kubernetes.io/service-name": "kube-dns", "endpointslice.kubernetes.io/managed-by": "kamaji-cluster-manager"})
	tenantSlice.Object["addressType"] = "IPv4"
	tenantSlice.Object["ports"] = []any{map[string]any{"name": "dns", "port": int64(32001), "protocol": "UDP"}, map[string]any{"name": "dns-tcp", "port": int64(32002), "protocol": "TCP"}}
	tenantSlice.Object["endpoints"] = []any{map[string]any{"addresses": []any{"192.0.2.10"}, "conditions": map[string]any{"ready": true}}}
	f.tenant.put(tenantSlice)
	delete(f.tenant.objects, objectKey(ref("", "apps", "Deployment", "kube-system", "coredns")))
	return b, g, f, v
}

func dnsTestRunner(f *fakeFactory, v *fakeArtifacts) Runner {
	dir, err := os.MkdirTemp("", "dpf-dns-topology-")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(os.RemoveAll, dir)
	dir, err = filepath.EvalSymlinks(dir)
	Expect(err).NotTo(HaveOccurred())
	return Runner{Clients: f, Artifacts: v, EvidenceDir: dir, PollInterval: time.Millisecond, Timeout: 10 * time.Second}
}

var _ = Describe("bound tenant DNS resource topology", func() {
	It("converges through management-hosted DNS without a tenant DNS Deployment", func() {
		b, g, f, v := hostedDNSFixture()
		r := dnsTestRunner(f, v)
		result, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
		Expect(result.Checks).To(ContainElement(And(HaveField("ReasonCode", "MANAGED_DNS_RESOURCES_READY"), HaveField("Message", ContainSubstring("reachability is not tested")))))
		Expect(f.tenant.creates).To(BeEmpty())
		result, err = r.Run(context.Background(), b, g, Validate)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
	})

	It("rejects a mixed revision rollout and passes only after the current revision converges", func() {
		b, g, f, v := hostedDNSFixture()
		r := dnsTestRunner(f, v)
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		dep := f.management.objects[objectKey(ref("", "apps", "Deployment", "dpf-system", "dns-tenant"))]
		dep.SetGeneration(2)
		dep.SetAnnotations(map[string]string{"deployment.kubernetes.io/revision": "2"})
		Expect(unstructured.SetNestedField(dep.Object, int64(2), "status", "observedGeneration")).To(Succeed())
		Expect(unstructured.SetNestedField(dep.Object, int64(2), "status", "replicas")).To(Succeed())
		containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
		containers[0].(map[string]any)["image"] = "example.invalid/coredns:current"
		Expect(unstructured.SetNestedSlice(dep.Object, containers, "spec", "template", "spec", "containers")).To(Succeed())
		oldRS := f.management.objects[objectKey(ref("", "apps", "ReplicaSet", "dpf-system", "dns-rs"))]
		oldRS.SetAnnotations(map[string]string{"deployment.kubernetes.io/revision": "1"})
		newRS := oldRS.DeepCopy()
		newRS.SetName("dns-rs-current")
		newRS.SetUID("dns-rs-current-uid")
		newRS.SetAnnotations(map[string]string{"deployment.kubernetes.io/revision": "2"})
		template, _, _ := unstructured.NestedMap(dep.Object, "spec", "template")
		Expect(unstructured.SetNestedMap(newRS.Object, template, "spec", "template")).To(Succeed())
		Expect(unstructured.SetNestedField(newRS.Object, "current-hash", "spec", "template", "metadata", "labels", "pod-template-hash")).To(Succeed())
		f.management.put(newRS)
		oldPod := f.management.objects[objectKey(ref("", "", "Pod", "dpf-system", "dns-pod"))]
		newPod := oldPod.DeepCopy()
		newPod.SetName("dns-pod-current")
		newPod.SetUID("dns-pod-current-uid")
		newPod.SetOwnerReferences([]metav1.OwnerReference{{Kind: "ReplicaSet", Name: newRS.GetName(), UID: newRS.GetUID(), Controller: ptr.To(true)}})
		Expect(unstructured.SetNestedField(newPod.Object, "Pending", "status", "phase")).To(Succeed())
		f.management.put(newPod)
		_, err = r.Run(context.Background(), b, g, Validate)
		Expect(err).To(MatchError(ContainSubstring("DNS_HOST_ROLLOUT_NOT_READY")))

		// Even misleading completed counters must not let the previous template's
		// Ready endpoint stand in for a current-template backend.
		Expect(unstructured.SetNestedField(dep.Object, int64(1), "status", "replicas")).To(Succeed())
		_, err = r.Run(context.Background(), b, g, Validate)
		Expect(err).To(MatchError(ContainSubstring("DNS_HOST_ROLLOUT_NOT_READY")))

		delete(f.management.objects, keyFor(oldPod))
		Expect(unstructured.SetNestedField(newPod.Object, "Running", "status", "phase")).To(Succeed())
		f.management.put(newPod)
		slice := f.management.objects[objectKey(ref("", "discovery.k8s.io", "EndpointSlice", "dpf-system", "dns-backends"))]
		endpoints, _, _ := unstructured.NestedSlice(slice.Object, "endpoints")
		endpoints[0].(map[string]any)["targetRef"] = map[string]any{"kind": "Pod", "name": newPod.GetName(), "namespace": newPod.GetNamespace(), "uid": string(newPod.GetUID())}
		Expect(unstructured.SetNestedSlice(slice.Object, endpoints, "endpoints")).To(Succeed())
		result, err := r.Run(context.Background(), b, g, Validate)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Outcome).To(Equal("complete"))
	})
	DescribeTable("requires every completed-rollout counter", func(field string, value int64) {
		_, _, f, _ := hostedDNSFixture()
		object := f.management.objects[objectKey(ref("", "apps", "Deployment", "dpf-system", "dns-tenant"))]
		Expect(unstructured.SetNestedField(object.Object, value, "status", field)).To(Succeed())
		ready, _, _ := Readiness(object)
		Expect(ready).To(BeFalse())
	}, Entry("old replicas remain", "replicas", int64(2)), Entry("a current pod is not Ready", "readyReplicas", int64(0)), Entry("a replica is not updated", "updatedReplicas", int64(0)), Entry("a replica is not available", "availableReplicas", int64(0)))
	DescribeTable("does not pass drifted, stale or missing hosted DNS chain inputs", func(change, reason string) {
		b, g, f, v := hostedDNSFixture()
		r := dnsTestRunner(f, v)
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		hostSlice := f.management.objects[objectKey(ref("", "discovery.k8s.io", "EndpointSlice", "dpf-system", "dns-backends"))]
		tenantSlice := f.tenant.objects[objectKey(ref("", "discovery.k8s.io", "EndpointSlice", "kube-system", "kube-dns"))]
		switch change {
		case "stale":
			f.management.objects[objectKey(ref("", "apps", "Deployment", "dpf-system", "dns-tenant"))].SetGeneration(2)
		case "vip":
			endpoints, _, _ := unstructured.NestedSlice(tenantSlice.Object, "endpoints")
			endpoints[0].(map[string]any)["addresses"] = []any{"192.0.2.99"}
			Expect(unstructured.SetNestedSlice(tenantSlice.Object, endpoints, "endpoints")).To(Succeed())
		case "port":
			ports, _, _ := unstructured.NestedSlice(tenantSlice.Object, "ports")
			ports[0].(map[string]any)["port"] = int64(32099)
			Expect(unstructured.SetNestedSlice(tenantSlice.Object, ports, "ports")).To(Succeed())
		case "extra-slice":
			extra := tenantSlice.DeepCopy()
			extra.SetName("foreign-dns")
			f.tenant.put(extra)
		case "foreign-service-owner":
			owners := hostSlice.GetOwnerReferences()
			owners[0].UID = "wrong-service"
			hostSlice.SetOwnerReferences(owners)
		case "foreign-pod":
			endpoints, _, _ := unstructured.NestedSlice(hostSlice.Object, "endpoints")
			endpoints[0].(map[string]any)["targetRef"].(map[string]any)["uid"] = "foreign-pod"
			Expect(unstructured.SetNestedSlice(hostSlice.Object, endpoints, "endpoints")).To(Succeed())
		case "foreign-rs":
			rs := f.management.objects[objectKey(ref("", "apps", "ReplicaSet", "dpf-system", "dns-rs"))]
			owners := rs.GetOwnerReferences()
			owners[0].UID = "foreign-deployment"
			rs.SetOwnerReferences(owners)
		case "missing-service":
			delete(f.tenant.objects, objectKey(ref("", "", "Service", "kube-system", "kube-dns")))
		case "missing-host-service":
			delete(f.management.objects, objectKey(ref("", "", "Service", "dpf-system", "dns-tenant")))
		case "endpoint-not-serving":
			endpoints, _, _ := unstructured.NestedSlice(hostSlice.Object, "endpoints")
			endpoints[0].(map[string]any)["conditions"].(map[string]any)["serving"] = false
			Expect(unstructured.SetNestedSlice(hostSlice.Object, endpoints, "endpoints")).To(Succeed())
		case "keepalived-vip":
			ds := f.management.objects[objectKey(ref("", "apps", "DaemonSet", "tenant-control", "tenant-keepalived"))]
			containers, _, _ := unstructured.NestedSlice(ds.Object, "spec", "template", "spec", "initContainers")
			containers[0].(map[string]any)["env"].([]any)[0].(map[string]any)["value"] = "192.0.2.99"
			Expect(unstructured.SetNestedSlice(ds.Object, containers, "spec", "template", "spec", "initContainers")).To(Succeed())
		case "missing-pods":
			delete(f.management.objects, objectKey(ref("", "", "Pod", "dpf-system", "dns-pod")))
		case "keepalived-owner":
			ds := f.management.objects[objectKey(ref("", "apps", "DaemonSet", "tenant-control", "tenant-keepalived"))]
			owners := ds.GetOwnerReferences()
			owners[0].UID = "foreign-cluster"
			ds.SetOwnerReferences(owners)
		case "other-cluster-label":
			dep := f.management.objects[objectKey(ref("", "apps", "Deployment", "dpf-system", "dns-tenant"))]
			l := dep.GetLabels()
			l["dpu.nvidia.com/cluster-namespace"] = "other-tenant"
			dep.SetLabels(l)
		}
		before := len(f.management.creates)
		result, err := r.Run(context.Background(), b, g, Validate)
		Expect(err).To(MatchError(ContainSubstring(reason)))
		Expect(result.Outcome).To(Equal("failed"))
		Expect(f.management.creates).To(HaveLen(before))
		Expect(f.tenant.creates).To(BeEmpty())
	}, Entry("stale management rollout", "stale", "DNS_HOST_ROLLOUT_NOT_READY"), Entry("VIP drift", "vip", "DNS_PUBLICATION_MISMATCH"), Entry("NodePort drift", "port", "DNS_PUBLICATION_MISMATCH"), Entry("additional foreign publication", "extra-slice", "DNS_PUBLICATION_MISMATCH"), Entry("stale Service UID", "foreign-service-owner", "DNS_BACKEND_BINDING_MISMATCH"), Entry("foreign Pod UID", "foreign-pod", "DNS_BACKEND_BINDING_MISMATCH"), Entry("foreign ReplicaSet ownership", "foreign-rs", "DNS_BACKEND_BINDING_MISMATCH"), Entry("missing tenant Service", "missing-service", "TENANT_DNS_NOT_READY"), Entry("no Ready backing pods", "missing-pods", "DNS_BACKEND_BINDING_MISMATCH"), Entry("foreign keepalived owner", "keepalived-owner", "DNS_KEEPALIVED_NOT_READY"), Entry("wrong tenant namespace label", "other-cluster-label", "DNS_HOST_RESOURCES_MISSING"), Entry("missing host Service", "missing-host-service", "DNS_HOST_RESOURCES_MISSING"), Entry("non-serving endpoint", "endpoint-not-serving", "DNS_BACKENDS_NOT_READY"), Entry("keepalived VIP drift", "keepalived-vip", "DNS_KEEPALIVED_BINDING_MISMATCH"))
	It("reports denied EndpointSlice reads instead of interpreting inaccessible data as Ready", func() {
		b, g, f, v := hostedDNSFixture()
		r := dnsTestRunner(f, v)
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		f.management.listErrors = map[string]error{"EndpointSlice": apierrors.NewForbidden(schema.GroupResource{Group: "discovery.k8s.io", Resource: "endpointslices"}, "", nil)}
		result, err := r.Run(context.Background(), b, g, Validate)
		Expect(err).To(MatchError(ContainSubstring("DNS_ACCESS_DENIED")))
		Expect(result.Outcome).To(Equal("failed"))
	})
})

var _ = Describe("controller canonical selected DPU node labels", func() {
	It("adds only the three reserved labels without mutating frozen user intent", func() {
		b, g, _, _ := lifecycleFixture()
		set := resourcesInBundle(b, "DPUSet")[0].ObjectCopy()
		expected, err := expectedDPUNodeLabels(set, "node-a", "dpf-system", g.Release)
		Expect(err).NotTo(HaveOccurred())
		Expect(expected).To(HaveLen(4))
		Expect(expected["operator.dpu.nvidia.com/dpf-version"]).To(Equal("v26.8.0-rc.1"))
		raw, _, _ := unstructured.NestedStringMap(set.Object, "spec", "dpuTemplate", "spec", "cluster", "nodeLabels")
		Expect(raw).To(HaveLen(1))
	})
	It("rejects reserved labels that contradict frozen device intent", func() {
		b, g, _, _ := lifecycleFixture()
		set := resourcesInBundle(b, "DPUSet")[0].ObjectCopy()
		Expect(unstructured.SetNestedField(set.Object, "foreign", "spec", "dpuTemplate", "spec", "cluster", "nodeLabels", "provisioning.dpu.nvidia.com/dpunode-name")).To(Succeed())
		_, err := expectedDPUNodeLabels(set, "node-a", "dpf-system", g.Release)
		Expect(err).To(MatchError(ContainSubstring("DPU_NODE_LABEL_INTENT_CONFLICT")))
	})
	DescribeTable("rejects scope-changing or unreviewed label additions", func(key, value string) {
		b, g, f, v := lifecycleFixture()
		r := dnsTestRunner(f, v)
		_, err := r.Run(context.Background(), b, g, Deploy)
		Expect(err).NotTo(HaveOccurred())
		dpu := f.management.objects[objectKey(ref("", "provisioning.dpu.nvidia.com", "DPU", "dpf-system", "dpu-child"))]
		nodeLabels, _, _ := unstructured.NestedStringMap(dpu.Object, "spec", "cluster", "nodeLabels")
		nodeLabels[key] = value
		Expect(unstructured.SetNestedStringMap(dpu.Object, nodeLabels, "spec", "cluster", "nodeLabels")).To(Succeed())
		result, err := r.Run(context.Background(), b, g, Validate)
		Expect(err).To(MatchError(ContainSubstring("DPU_NODE_SCOPE_MISMATCH")))
		Expect(result.Outcome).To(Equal("failed"))
	}, Entry("scope label changes", "nvidia.kubernetes-launch-kit.dpf-instance", "foreign"), Entry("node association changes", "provisioning.dpu.nvidia.com/dpunode-name", "other-node"), Entry("namespace changes", "provisioning.dpu.nvidia.com/dpunode-namespace", "other-namespace"), Entry("release changes", "operator.dpu.nvidia.com/dpf-version", "v0.0.0"), Entry("extra workload label", "foreign.example/selected", "yes"))
})
