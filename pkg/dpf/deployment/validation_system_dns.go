// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// The chart's host-side CoreDNS Service and the stable DPF Kamaji DNS
// reconciler form one selected functional path. No owner is invented across
// APIs: tenant DNS objects use the explicit service and managed-by labels.
func verifyValidationCoreDNSService(ctx context.Context, reader resourceReader, app *unstructured.Unstructured, expected systemArtifactExpectation) (*corev1.Service, error) {
	if len(expected.Services) != 1 || expected.Workload == nil {
		return nil, validationFailure(validation.Unverified, validation.ReasonIncomplete, "The locked CoreDNS functional Service projection is incomplete.")
	}
	for _, wanted := range expected.Services {
		if err := validationAppInventoryContains(app, wanted); err != nil {
			return nil, err
		}
		current, err := reader.Get(ctx, wanted.DeepCopy())
		if err != nil {
			return nil, err
		}
		if err := validationChartObjectIdentity(app, wanted, current); err != nil {
			return nil, err
		}
		var want, got corev1.Service
		if runtime.DefaultUnstructuredConverter.FromUnstructured(wanted.Object, &want) != nil || runtime.DefaultUnstructuredConverter.FromUnstructured(current.Object, &got) != nil {
			return nil, fmt.Errorf("CoreDNS Service decode failed")
		}
		if got.Spec.Type != corev1.ServiceTypeNodePort || got.Spec.ClusterIP == corev1.ClusterIPNone {
			return nil, validationFailure(validation.Failed, validation.ReasonHealth, "CoreDNS requires a non-headless NodePort Service.")
		}
		if got.Spec.ClusterIP == "" {
			return nil, validationFailure(validation.Pending, validation.ReasonProgressing, "CoreDNS NodePort Service has no current allocated ClusterIP.")
		}
		address, addressErr := netip.ParseAddr(got.Spec.ClusterIP)
		if addressErr != nil || !address.Is4() {
			return nil, validationFailure(validation.Failed, validation.ReasonHealth, "CoreDNS Service has an unsupported or invalid allocated address.")
		}
		if len(got.Spec.Ports) != len(want.Spec.Ports) || len(got.Spec.Selector) == 0 {
			return nil, validationFailure(validation.Failed, validation.ReasonHealth, "CoreDNS Service ports or selector differ from the locked chart.")
		}
		templateLabels, _, _ := unstructured.NestedStringMap(expected.Workload.Object, "spec", "template", "metadata", "labels")
		for key, value := range got.Spec.Selector {
			if templateLabels[key] != value {
				return nil, validationFailure(validation.Failed, validation.ReasonHealth, "CoreDNS Service selector does not select the verified workload.")
			}
		}
		for i := range want.Spec.Ports {
			if got.Spec.Ports[i].NodePort == 0 {
				return nil, validationFailure(validation.Pending, validation.ReasonProgressing, "CoreDNS Service is waiting for an allocated NodePort.")
			}
			if got.Spec.Ports[i].NodePort < 0 || got.Spec.Ports[i].NodePort > 65535 {
				return nil, validationFailure(validation.Failed, validation.ReasonHealth, "CoreDNS Service has an invalid allocated NodePort.")
			}
			got.Spec.Ports[i].NodePort = 0
		}
		for _, spec := range []*corev1.ServiceSpec{&want.Spec, &got.Spec} {
			spec.ClusterIP = ""
			spec.ClusterIPs = nil
			spec.IPFamilies = nil
			spec.IPFamilyPolicy = nil
			if spec.SessionAffinity == "" {
				spec.SessionAffinity = corev1.ServiceAffinityNone
			}
			if spec.ExternalTrafficPolicy == "" {
				spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
			}
			if spec.InternalTrafficPolicy == nil {
				spec.InternalTrafficPolicy = ptr.To(corev1.ServiceInternalTrafficPolicyCluster)
			}
		}
		if !jsonEqual(want.Spec, got.Spec) {
			return nil, validationFailure(validation.Failed, validation.ReasonHealth, "CoreDNS functional Service differs from its independent chart ports, targets or selector.")
		}
		// Return the unmodified current allocated NodePorts to bind tenant endpoints.
		var result corev1.Service
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(current.Object, &result); err != nil {
			return nil, err
		}
		return &result, nil
	}
	return nil, fmt.Errorf("missing CoreDNS service projection")
}
func validationDNSNodePorts(service *corev1.Service) (int32, int32, error) {
	udp, tcp := int32(0), int32(0)
	for _, port := range service.Spec.Ports {
		switch port.Name {
		case "dns":
			if port.Protocol != corev1.ProtocolUDP || port.Port != 53 || port.TargetPort != intstr.FromString("dns") {
				return 0, 0, fmt.Errorf("invalid UDP DNS service port")
			}
			udp = port.NodePort
		case "dns-tcp":
			if port.Protocol != corev1.ProtocolTCP || port.Port != 53 || port.TargetPort != intstr.FromString("dns-tcp") {
				return 0, 0, fmt.Errorf("invalid TCP DNS service port")
			}
			tcp = port.NodePort
		}
	}
	if udp <= 0 || tcp <= 0 {
		return 0, 0, fmt.Errorf("DNS node ports are unavailable")
	}
	return udp, tcp, nil
}
func (o *validationObserver) observeValidationTenantDNS(ctx context.Context, management resourceReader, tenant *tenantClient, hostDNS *corev1.Service, r *validationRecorder) error {
	if tenant == nil {
		return nil
	}
	reader := validationTenantResources{tenant: tenant}
	var service *corev1.Service
	err := r.run("tenant/dns/service", func() error {
		current, err := reader.Get(ctx, validationObject("v1", "Service", metav1.NamespaceSystem, "kube-dns"))
		if err != nil {
			return err
		}
		var typed corev1.Service
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(current.Object, &typed); err != nil {
			return err
		}
		if err := validationTenantDNSService(&typed, o.desired.plan.Intent().Bootstrap.Tenant.ServiceCIDR); err != nil {
			return err
		}
		service = &typed
		return nil
	})
	if validationIdentityChanged(err) {
		return err
	}
	if hostDNS == nil || service == nil {
		r.pendingDependents("tenant/dns/endpoints", "system/dns/management-service")
		r.pendingDependents("tenant/dns/endpoints", "tenant/dns/service")
		return nil
	}
	err = r.run("tenant/dns/endpoints", func() error {
		slices, err := reader.List(ctx, validationObject("discovery.k8s.io/v1", "EndpointSlice", metav1.NamespaceSystem, ""), discoveryv1.LabelServiceName+"=kube-dns")
		if err != nil {
			return err
		}
		if err := validationTenantDNSEndpoints(slices, hostDNS, o.desired.plan.Intent().Bootstrap.Tenant.VIP); err != nil {
			return err
		}
		refreshed, err := management.Get(ctx, validationObject("v1", "Service", hostDNS.Namespace, hostDNS.Name))
		if err != nil {
			return err
		}
		var after corev1.Service
		if runtime.DefaultUnstructuredConverter.FromUnstructured(refreshed.Object, &after) != nil {
			return fmt.Errorf("cannot decode final host DNS Service")
		}
		if err := validationDNSAllocationStable(hostDNS, &after); err != nil {
			return err
		}
		finalService, err := reader.Get(ctx, validationObject("v1", "Service", metav1.NamespaceSystem, "kube-dns"))
		if err != nil {
			return err
		}
		var final corev1.Service
		if runtime.DefaultUnstructuredConverter.FromUnstructured(finalService.Object, &final) != nil {
			return fmt.Errorf("cannot decode final tenant DNS Service")
		}
		if err := validationDNSAllocationStable(service, &final); err != nil {
			return err
		}

		return nil
	})
	if validationIdentityChanged(err) {
		return err
	}
	return nil
}
func validationTenantDNSService(service *corev1.Service, cidr string) error {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is4() {
		return validationFailure(validation.Unverified, validation.ReasonUnsupported, "Selected DNS service range is unsupported.")
	}
	ip := prefix.Masked().Addr()
	for range 10 {
		ip = ip.Next()
	}
	if service.Name != "kube-dns" || service.Namespace != metav1.NamespaceSystem || service.UID == "" || service.ResourceVersion == "" || service.DeletionTimestamp != nil || service.Labels["k8s-app"] != "kube-dns" || service.Labels["kubernetes.io/name"] != "CoreDNS" || len(service.Spec.Selector) != 0 || service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.ClusterIP != ip.String() {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Tenant kube-dns identity, selector or selected service address differs from the supported contract.")
	}
	want := []corev1.ServicePort{{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt32(53)}, {Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(53)}}
	if !reflect.DeepEqual(service.Spec.Ports, want) {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Tenant kube-dns must expose the selected UDP and TCP DNS ports.")
	}
	return nil
}
func validationTenantDNSEndpoints(objects []unstructured.Unstructured, host *corev1.Service, vip string) error {
	udp, tcp, err := validationDNSNodePorts(host)
	if err != nil {
		return validationFailure(validation.Unverified, validation.ReasonDependency, "Selected management DNS NodePorts could not be verified.")
	}
	if len(objects) != 1 {
		return validationFailure(validation.Failed, validation.ReasonIncomplete, "Tenant kube-dns requires exactly one complete service-bound EndpointSlice.")
	}
	var current discoveryv1.EndpointSlice
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(objects[0].Object, &current); err != nil {
		return err
	}
	if current.Name != "kube-dns" || current.Namespace != metav1.NamespaceSystem || current.UID == "" || current.ResourceVersion == "" || current.DeletionTimestamp != nil || current.AddressType != discoveryv1.AddressTypeIPv4 || current.Labels[discoveryv1.LabelServiceName] != "kube-dns" || current.Labels[discoveryv1.LabelManagedBy] != "kamaji-cluster-manager" {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Tenant DNS EndpointSlice identity or manager binding differs.")
	}
	wantPorts := []discoveryv1.EndpointPort{{Name: ptr.To("dns"), Port: ptr.To(udp), Protocol: ptr.To(corev1.ProtocolUDP)}, {Name: ptr.To("dns-tcp"), Port: ptr.To(tcp), Protocol: ptr.To(corev1.ProtocolTCP)}}
	wantEndpoints := []discoveryv1.Endpoint{{Addresses: []string{vip}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}}
	if !jsonEqual(current.Ports, wantPorts) || !jsonEqual(current.Endpoints, wantEndpoints) {
		return validationFailure(validation.Failed, validation.ReasonHealth, "Tenant DNS endpoints do not map the selected VIP to the correct UDP/TCP management NodePorts.")
	}
	return nil
}

func validationDNSAllocationStable(before, after *corev1.Service) error {
	if before == nil || after == nil || before.UID != after.UID || before.Name != after.Name || before.Namespace != after.Namespace || after.DeletionTimestamp != nil || before.Annotations["argocd.argoproj.io/tracking-id"] != after.Annotations["argocd.argoproj.io/tracking-id"] || !jsonEqual(before.Spec, after.Spec) {
		return fmt.Errorf("selected DNS Service allocation changed during endpoint inspection: %w", errValidationResourceChanged)
	}
	return nil
}
