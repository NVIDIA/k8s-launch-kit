// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type dnsObservation struct {
	ready                 bool
	code, message, source string
}

func dnsPending(code, message string) dnsObservation {
	return dnsObservation{code: code, message: message, source: "management/tenant DNS resource chain"}
}

// DNS is a resource-readiness prerequisite, not an active DNS query. rc.1
// Kamaji+keepalived publishes management-hosted CoreDNS through a selectorless
// tenant Service. Other tenants retain their local DNS Deployment contract.
func (s *run) observeDNS(ctx context.Context) (dnsObservation, error) {
	if err := s.cluster(ctx); err != nil {
		return dnsObservation{}, err
	}
	binding := s.g.Provisioning.DPUCluster
	cluster, err := s.clients[bundle.Management].Get(ctx, ref(bundle.Management, "provisioning.dpu.nvidia.com", "DPUCluster", binding.Namespace, binding.Name))
	if err != nil {
		return dnsObservation{}, dnsReadError(err, "bound DPUCluster")
	}
	if string(cluster.GetUID()) != binding.ResourceUID || cluster.GetGeneration() != s.clusterGeneration {
		return dnsObservation{}, fail("DPU_CLUSTER_GENERATION_DRIFT", "DPUCluster changed while selecting its DNS topology")
	}
	vip := text(cluster, "spec", "clusterEndpoint", "keepalived", "vip")
	if text(cluster, "spec", "type") == "kamaji" && vip != "" {
		return s.hostedDNS(ctx, cluster, vip)
	}
	objects, err := s.clients[bundle.DPU].List(ctx, ref(bundle.DPU, "apps", "Deployment", "kube-system", ""), "k8s-app=kube-dns")
	if err != nil {
		return dnsObservation{}, dnsReadError(err, "tenant DNS Deployment")
	}
	ready := len(objects) > 0
	for _, object := range objects {
		available, _, _ := Readiness(object)
		ready = ready && available
	}
	if !ready {
		return dnsPending("TENANT_DNS_NOT_READY", "tenant DNS Deployment is absent or not Ready"), nil
	}
	return dnsObservation{ready: true, code: "TENANT_DNS_READY", source: "tenant:Deployment k8s-app=kube-dns", message: "tenant DNS Deployment resource readiness; DNS reachability is not tested"}, nil
}

func dnsReadError(err error, subject string) error {
	if apierrors.IsNotFound(err) {
		return fail("DNS_RESOURCE_MISSING", subject+" is absent")
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return fail("DNS_ACCESS_DENIED", "cannot read "+subject)
	}
	return fail("DNS_RESOURCE_UNAVAILABLE", "cannot inspect "+subject)
}
func decodeDNS(o *unstructured.Unstructured, target any) error {
	if o == nil {
		return fmt.Errorf("DNS resource is absent")
	}
	data, err := json.Marshal(o.Object)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
func liveDNSObject(o *unstructured.Unstructured) bool {
	return o != nil && o.GetUID() != "" && o.GetDeletionTimestamp().IsZero()
}

func (s *run) hostedDNS(ctx context.Context, cluster *unstructured.Unstructured, vip string) (dnsObservation, error) {
	address, err := netip.ParseAddr(vip)
	if err != nil || !address.Is4() {
		return dnsPending("DNS_TOPOLOGY_UNSUPPORTED", "managed DNS requires the source-supported IPv4 keepalived VIP"), nil
	}
	// GetCoreDNSWorkloadLabels in the pinned inventory source. Names are hashed.
	selector := labels.SelectorFromSet(map[string]string{"app.kubernetes.io/name": "coredns", "dpu.nvidia.com/cluster": cluster.GetName(), "dpu.nvidia.com/cluster-namespace": cluster.GetNamespace()}).String()
	management := s.clients[bundle.Management]
	deployments, err := management.List(ctx, ref(bundle.Management, "apps", "Deployment", s.g.Namespace, ""), selector)
	if err != nil {
		return dnsObservation{}, dnsReadError(err, "management CoreDNS Deployments")
	}
	services, err := management.List(ctx, ref(bundle.Management, "", "Service", s.g.Namespace, ""), selector)
	if err != nil {
		return dnsObservation{}, dnsReadError(err, "management CoreDNS Services")
	}
	if len(deployments) == 0 || len(services) == 0 {
		return dnsPending("DNS_HOST_RESOURCES_MISSING", "management CoreDNS Deployment/Service for the bound tenant is absent"), nil
	}
	if len(deployments) != 1 || len(services) != 1 {
		return dnsPending("DNS_BINDING_AMBIGUOUS", "bound tenant labels must identify exactly one management CoreDNS Deployment and Service"), nil
	}
	deployment, service := deployments[0], services[0]
	ready, _, _ := Readiness(deployment)
	if !liveDNSObject(deployment) || !liveDNSObject(service) || !ready {
		return dnsPending("DNS_HOST_ROLLOUT_NOT_READY", "management CoreDNS must have a current fully available rollout"), nil
	}
	var dep appsv1.Deployment
	var svc corev1.Service
	if decodeDNS(deployment, &dep) != nil || decodeDNS(service, &svc) != nil {
		return dnsPending("DNS_RESOURCE_INVALID", "cannot decode management CoreDNS resources"), nil
	}
	if dep.Spec.Selector == nil || len(dep.Spec.Selector.MatchExpressions) > 0 || len(svc.Spec.Selector) == 0 || !jsonEqual(dep.Spec.Selector.MatchLabels, svc.Spec.Selector) {
		return dnsPending("DNS_SELECTOR_MISMATCH", "management DNS Service selector differs from its Deployment"), nil
	}
	if svc.Spec.Type != corev1.ServiceTypeNodePort || svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal {
		return dnsPending("DNS_TOPOLOGY_MISMATCH", "management DNS must expose cluster-routed NodePorts"), nil
	}
	nodePorts, ok := dnsServicePorts(svc.Spec.Ports, true)
	if !ok {
		return dnsPending("DNS_PORT_MISMATCH", "management DNS requires named UDP/TCP53 NodePorts targeting their DNS container ports"), nil
	}
	keepalivedNodes, observation, err := s.keepalivedDNS(ctx, cluster, vip, dep.Spec.Template.Spec.NodeSelector)
	if err != nil || !observation.ready {
		return observation, err
	}
	backingPorts := map[string]dnsPort{}
	for _, container := range dep.Spec.Template.Spec.Containers {
		for _, port := range container.Ports {
			if port.Name == "dns" || port.Name == "dns-tcp" {
				backingPorts[port.Name] = dnsPort{protocol: port.Protocol, port: port.ContainerPort}
			}
		}
	}
	if backingPorts["dns"].protocol != corev1.ProtocolUDP || backingPorts["dns-tcp"].protocol != corev1.ProtocolTCP || backingPorts["dns"].port < 1 || backingPorts["dns-tcp"].port < 1 {
		return dnsPending("DNS_PORT_MISMATCH", "management CoreDNS container DNS ports are missing or inconsistent"), nil
	}
	observation, err = s.dnsBackends(ctx, deployment, service, &dep, &svc, backingPorts, keepalivedNodes)
	if err != nil || !observation.ready {
		return observation, err
	}
	observation, err = s.tenantDNSPublication(ctx, vip, nodePorts)
	if err != nil || !observation.ready {
		return observation, err
	}
	return dnsObservation{ready: true, code: "MANAGED_DNS_RESOURCES_READY", source: "bound DPUCluster + management CoreDNS/keepalived + tenant kube-dns Service/EndpointSlice", message: "exact management-hosted DNS publication, current rollout and Ready owned backing pods verified; DNS reachability is not tested"}, nil
}

type dnsPort struct {
	protocol corev1.Protocol
	port     int32
}

func dnsServicePorts(ports []corev1.ServicePort, nodePort bool) (map[string]dnsPort, bool) {
	out := map[string]dnsPort{}
	for _, port := range ports {
		if port.Name != "dns" && port.Name != "dns-tcp" {
			continue
		}
		want := corev1.ProtocolUDP
		if port.Name == "dns-tcp" {
			want = corev1.ProtocolTCP
		}
		if _, duplicate := out[port.Name]; duplicate || port.Protocol != want || port.Port != 53 {
			return nil, false
		}
		if nodePort {
			if port.NodePort < 1 || port.NodePort > 65535 || port.TargetPort.Type != intstr.String || port.TargetPort.StrVal != port.Name {
				return nil, false
			}
			out[port.Name] = dnsPort{protocol: want, port: port.NodePort}
		} else {
			if port.TargetPort.Type != intstr.Int || port.TargetPort.IntVal != 53 {
				return nil, false
			}
			out[port.Name] = dnsPort{protocol: want, port: 53}
		}
	}
	return out, len(out) == 2
}
func dnsEndpointPorts(ports []discoveryv1.EndpointPort, expected map[string]dnsPort) bool {
	found := map[string]bool{}
	for _, port := range ports {
		if port.Name == nil {
			continue
		}
		want, needed := expected[*port.Name]
		if !needed {
			continue
		}
		if found[*port.Name] || port.Protocol == nil || *port.Protocol != want.protocol || port.Port == nil || *port.Port != want.port {
			return false
		}
		found[*port.Name] = true
	}
	return len(found) == len(expected)
}
func dnsOwner(o *unstructured.Unstructured, kind, name string, uid types.UID, controller bool) bool {
	for _, owner := range o.GetOwnerReferences() {
		if owner.Kind == kind && owner.Name == name && owner.UID == uid && (!controller || owner.Controller != nil && *owner.Controller) {
			return true
		}
	}
	return false
}

func (s *run) keepalivedDNS(ctx context.Context, cluster *unstructured.Unstructured, vip string, dnsNodeSelector map[string]string) (map[string]bool, dnsObservation, error) {
	c := s.clients[bundle.Management]
	object, err := c.Get(ctx, ref(bundle.Management, "apps", "DaemonSet", cluster.GetNamespace(), cluster.GetName()+"-keepalived"))
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, dnsPending("DNS_KEEPALIVED_NOT_READY", "bound keepalived DaemonSet is absent"), nil
		}
		return nil, dnsObservation{}, dnsReadError(err, "bound keepalived DaemonSet")
	}
	ready, _, _ := Readiness(object)
	if !liveDNSObject(object) || !ready || !dnsOwner(object, "DPUCluster", cluster.GetName(), cluster.GetUID(), false) {
		return nil, dnsPending("DNS_KEEPALIVED_NOT_READY", "keepalived must be current, Ready and owned by the bound DPUCluster UID"), nil
	}
	var ds appsv1.DaemonSet
	if decodeDNS(object, &ds) != nil {
		return nil, dnsPending("DNS_RESOURCE_INVALID", "cannot decode keepalived"), nil
	}
	expected, _, err := unstructured.NestedStringMap(cluster.Object, "spec", "clusterEndpoint", "keepalived", "nodeSelector")
	if err != nil || !labels.Equals(expected, ds.Spec.Template.Spec.NodeSelector) || !labels.Equals(expected, dnsNodeSelector) || !ds.Spec.Template.Spec.HostNetwork {
		return nil, dnsPending("DNS_KEEPALIVED_BINDING_MISMATCH", "CoreDNS and keepalived must follow the bound keepalived node selector"), nil
	}
	interfaceName := text(cluster, "spec", "clusterEndpoint", "keepalived", "interface")
	boundVIP, boundInterface := "", ""
	for _, container := range ds.Spec.Template.Spec.InitContainers {
		if container.Name == "create-conf" {
			for _, env := range container.Env {
				if env.Name == "VIRTUAL_IP" {
					boundVIP = env.Value
				}
				if env.Name == "INTERFACE" {
					boundInterface = env.Value
				}
			}
		}
	}
	if boundVIP != vip || interfaceName == "" || boundInterface != interfaceName {
		return nil, dnsPending("DNS_KEEPALIVED_BINDING_MISMATCH", "keepalived rendered VIP/interface differs from its DPUCluster"), nil
	}
	if ds.Spec.Selector == nil {
		return nil, dnsPending("DNS_SELECTOR_MISMATCH", "keepalived selector is missing"), nil
	}
	selector, err := metav1.LabelSelectorAsSelector(ds.Spec.Selector)
	if err != nil {
		return nil, dnsPending("DNS_SELECTOR_MISMATCH", "keepalived selector is invalid"), nil
	}
	pods, err := c.List(ctx, ref(bundle.Management, "", "Pod", object.GetNamespace(), ""), selector.String())
	if err != nil {
		return nil, dnsObservation{}, dnsReadError(err, "keepalived backing pods")
	}
	nodes := map[string]bool{}
	for _, pod := range pods {
		if !dnsOwner(pod, "DaemonSet", object.GetName(), object.GetUID(), true) {
			continue
		}
		ready, _, _ := Readiness(pod)
		if !ready {
			continue
		}
		name := text(pod, "spec", "nodeName")
		if name == "" || nodes[name] {
			return nil, dnsPending("DNS_KEEPALIVED_NOT_READY", "keepalived Ready pods do not have unique node membership"), nil
		}
		node, err := c.Get(ctx, ref(bundle.Management, "", "Node", "", name))
		if err != nil {
			return nil, dnsObservation{}, dnsReadError(err, "keepalived management Node")
		}
		nodeReady, _, _ := Readiness(node)
		if !nodeReady || !labels.SelectorFromSet(expected).Matches(labels.Set(node.GetLabels())) {
			return nil, dnsPending("DNS_KEEPALIVED_NOT_READY", "keepalived is not backed by Ready selected management Nodes"), nil
		}
		nodes[name] = true
	}
	if int64(len(nodes)) != number(object, "status", "desiredNumberScheduled") {
		return nil, dnsPending("DNS_KEEPALIVED_NOT_READY", "keepalived Ready owned pod membership differs from its rollout"), nil
	}
	return nodes, dnsObservation{ready: true}, nil
}

func (s *run) dnsBackends(ctx context.Context, deployment, service *unstructured.Unstructured, dep *appsv1.Deployment, svc *corev1.Service, ports map[string]dnsPort, keepalivedNodes map[string]bool) (dnsObservation, error) {
	c := s.clients[bundle.Management]
	pods, err := c.List(ctx, ref(bundle.Management, "", "Pod", service.GetNamespace(), ""), labels.SelectorFromSet(svc.Spec.Selector).String())
	if err != nil {
		return dnsObservation{}, dnsReadError(err, "management CoreDNS backing pods")
	}
	readyPods := map[types.UID]*corev1.Pod{}
	for _, object := range pods {
		ready, _, _ := Readiness(object)
		if !ready {
			continue
		}
		var pod corev1.Pod
		if decodeDNS(object, &pod) != nil || pod.UID == "" || !keepalivedNodes[pod.Spec.NodeName] {
			return dnsPending("DNS_BACKEND_BINDING_MISMATCH", "DNS pod is not on a Ready bound VIP management node"), nil
		}
		var rsOwner *metav1.OwnerReference
		for _, owner := range object.GetOwnerReferences() {
			if owner.Kind == "ReplicaSet" && owner.Controller != nil && *owner.Controller {
				copy := owner
				rsOwner = &copy
			}
		}
		if rsOwner == nil {
			return dnsPending("DNS_BACKEND_BINDING_MISMATCH", "DNS pod has no ReplicaSet controller identity"), nil
		}
		rs, err := c.Get(ctx, ref(bundle.Management, "apps", "ReplicaSet", pod.Namespace, rsOwner.Name))
		if err != nil {
			return dnsObservation{}, dnsReadError(err, "CoreDNS ReplicaSet")
		}
		if !liveDNSObject(rs) || rs.GetUID() != rsOwner.UID || !dnsOwner(rs, "Deployment", deployment.GetName(), deployment.GetUID(), true) {
			return dnsPending("DNS_BACKEND_BINDING_MISMATCH", "DNS pod ReplicaSet is not owned by the bound DNS Deployment UID"), nil
		}
		var replicaSet appsv1.ReplicaSet
		if decodeDNS(rs, &replicaSet) != nil || !dnsTemplateEqual(&replicaSet.Spec.Template, &dep.Spec.Template) {
			return dnsPending("DNS_HOST_ROLLOUT_NOT_READY", "Ready DNS backend belongs to a previous Deployment pod template"), nil
		}
		readyPods[pod.UID] = &pod
	}
	slices, err := c.List(ctx, ref(bundle.Management, "discovery.k8s.io", "EndpointSlice", service.GetNamespace(), ""), labels.SelectorFromSet(map[string]string{discoveryv1.LabelServiceName: service.GetName()}).String())
	if err != nil {
		return dnsObservation{}, dnsReadError(err, "management DNS EndpointSlices")
	}
	backed := map[types.UID]bool{}
	for _, object := range slices {
		var slice discoveryv1.EndpointSlice
		if !liveDNSObject(object) || decodeDNS(object, &slice) != nil || !dnsOwner(object, "Service", service.GetName(), service.GetUID(), true) || slice.AddressType != discoveryv1.AddressTypeIPv4 || !dnsEndpointPorts(slice.Ports, ports) {
			return dnsPending("DNS_BACKEND_BINDING_MISMATCH", "DNS EndpointSlice owner, address family or ports differ from its bound Service/Deployment"), nil
		}
		for _, endpoint := range slice.Endpoints {
			if endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready || endpoint.Conditions.Serving != nil && !*endpoint.Conditions.Serving || endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating {
				continue
			}
			target := endpoint.TargetRef
			if target == nil || target.Kind != "Pod" || target.Namespace != service.GetNamespace() {
				return dnsPending("DNS_BACKEND_BINDING_MISMATCH", "Ready DNS endpoint has no bound Pod identity"), nil
			}
			pod := readyPods[target.UID]
			if pod == nil || pod.Name != target.Name || len(endpoint.Addresses) != 1 || endpoint.Addresses[0] != pod.Status.PodIP {
				return dnsPending("DNS_BACKEND_BINDING_MISMATCH", "Ready DNS endpoint does not match an owned Ready DNS pod IP/UID"), nil
			}
			backed[pod.UID] = true
		}
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	if want < 1 || len(backed) != len(readyPods) || len(backed) != int(want) {
		return dnsPending("DNS_BACKENDS_NOT_READY", "management DNS requires the current replica count of Ready owned endpoint-backed pods"), nil
	}
	return dnsObservation{ready: true}, nil
}

func (s *run) tenantDNSPublication(ctx context.Context, vip string, nodePorts map[string]dnsPort) (dnsObservation, error) {
	c := s.clients[bundle.DPU]
	object, err := c.Get(ctx, ref(bundle.DPU, "", "Service", "kube-system", "kube-dns"))
	if err != nil {
		if apierrors.IsNotFound(err) {
			return dnsPending("TENANT_DNS_NOT_READY", "tenant kube-dns Service is absent"), nil
		}
		return dnsObservation{}, dnsReadError(err, "tenant kube-dns Service")
	}
	var svc corev1.Service
	if !liveDNSObject(object) || decodeDNS(object, &svc) != nil {
		return dnsPending("DNS_RESOURCE_INVALID", "tenant DNS Service identity is unavailable"), nil
	}
	address, err := netip.ParseAddr(svc.Spec.ClusterIP)
	_, portsOK := dnsServicePorts(svc.Spec.Ports, false)
	if err != nil || !address.Is4() || svc.Spec.Type != corev1.ServiceTypeClusterIP || len(svc.Spec.Selector) != 0 || !portsOK || len(svc.Spec.Ports) != 2 || svc.Labels["k8s-app"] != "kube-dns" || svc.Labels["kubernetes.io/name"] != "CoreDNS" {
		return dnsPending("DNS_PUBLICATION_MISMATCH", "tenant kube-dns must be the selectorless ClusterIP/UDP+TCP53 Service managed by Kamaji"), nil
	}
	slices, err := c.List(ctx, ref(bundle.DPU, "discovery.k8s.io", "EndpointSlice", "kube-system", ""), labels.SelectorFromSet(map[string]string{discoveryv1.LabelServiceName: "kube-dns"}).String())
	if err != nil {
		return dnsObservation{}, dnsReadError(err, "tenant DNS EndpointSlices")
	}
	if len(slices) != 1 {
		return dnsPending("DNS_PUBLICATION_MISMATCH", "tenant DNS requires exactly its single managed kube-dns EndpointSlice"), nil
	}
	var slice discoveryv1.EndpointSlice
	if !liveDNSObject(slices[0]) || decodeDNS(slices[0], &slice) != nil || slice.Name != "kube-dns" || slice.Labels[discoveryv1.LabelManagedBy] != "kamaji-cluster-manager" || slice.AddressType != discoveryv1.AddressTypeIPv4 || len(slice.Ports) != 2 || !dnsEndpointPorts(slice.Ports, nodePorts) || len(slice.Endpoints) != 1 {
		return dnsPending("DNS_PUBLICATION_MISMATCH", "tenant DNS slice provenance, ports or membership differs from the managed mapping"), nil
	}
	endpoint := slice.Endpoints[0]
	if len(endpoint.Addresses) != 1 || endpoint.Addresses[0] != vip || endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready || endpoint.Conditions.Serving != nil && !*endpoint.Conditions.Serving || endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating {
		return dnsPending("DNS_PUBLICATION_MISMATCH", "tenant DNS must publish the exact Ready keepalived VIP"), nil
	}
	return dnsObservation{ready: true}, nil
}

// The Deployment controller adds only pod-template-hash when deriving its
// ReplicaSet template. Follow its EqualIgnoreHash association rather than
// revision annotations, which can change during a rollback.
func dnsTemplateEqual(replicaSet, deployment *corev1.PodTemplateSpec) bool {
	left, right := replicaSet.DeepCopy(), deployment.DeepCopy()
	delete(left.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	delete(right.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	return apiequality.Semantic.DeepEqual(left, right)
}
