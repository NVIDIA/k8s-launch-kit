// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"
	"maps"
	"reflect"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TenantReadinessExpected contains retained, non-secret inputs needed to check
// convergence of the DPF 26.8 Kamaji tenant resources.
type TenantReadinessExpected struct {
	Name, Namespace                      string
	Labels                               map[string]string
	MaxNodes                             int64
	KubernetesVersion                    string
	VIP, Interface, PodCIDR, ServiceCIDR string
	VirtualRouterID                      int64
	NodeSelector                         map[string]string
}

// TenantReadinessObserved contains fresh reads of the tenant and its managed
// companions. Keepalived must contain its DPF-owned ConfigMap and DaemonSet.
type TenantReadinessObserved struct {
	DPUCluster         *unstructured.Unstructured
	TenantControlPlane *unstructured.Unstructured
	Service            *unstructured.Unstructured
	Keepalived         []*unstructured.Unstructured
}

// TenantReadinessState classifies whether tenant convergence is complete,
// progressing, conflicting with retained intent, or malformed.
type TenantReadinessState string

const (
	TenantReadinessReady    TenantReadinessState = "ready"
	TenantReadinessPending  TenantReadinessState = "pending"
	TenantReadinessConflict TenantReadinessState = "conflict"
	TenantReadinessInvalid  TenantReadinessState = "invalid"
)

// TenantReadiness reports managed-resource convergence. Ready does not prove
// tenant API reachability, credentials, VIP ownership, or DPU enrollment.
type TenantReadiness struct {
	Ready      bool
	State      TenantReadinessState
	ReasonCode string
	Message    string
}

// EvaluateTenantReadiness compares retained intent with fresh observations.
// It does not mutate inputs or invent generation fields absent from these APIs.
func EvaluateTenantReadiness(expected TenantReadinessExpected, observed TenantReadinessObserved) TenantReadiness {
	if err := validateTenantExpectation(expected); err != nil {
		return tenantResult("invalid", "TENANT_EXPECTATION_INVALID", "Retained tenant inputs are incomplete or invalid.", err)
	}
	for _, object := range []*unstructured.Unstructured{observed.DPUCluster, observed.TenantControlPlane, observed.Service} {
		if object == nil {
			return tenantResult("pending", "TENANT_RESOURCE_UNOBSERVED", "A required tenant resource has not been observed.", nil)
		}
		if object.GetDeletionTimestamp() != nil {
			return tenantResult("conflict", "TENANT_RESOURCE_DELETING", "A required tenant resource is being deleted.", nil)
		}
	}
	dc := observed.DPUCluster
	if dc.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" || dc.GetKind() != "DPUCluster" || dc.GetName() != expected.Name || dc.GetNamespace() != expected.Namespace || dc.GetUID() == "" {
		return tenantResult("conflict", "TENANT_IDENTITY_MISMATCH", "The observed DPUCluster identity differs from retained tenant intent.", nil)
	}
	if err := compareDPUClusterSpec(expected, dc); err != nil {
		return tenantResult("conflict", "TENANT_SPEC_MISMATCH", "The observed DPUCluster spec differs from retained tenant intent.", err)
	}
	dcReady := EvaluateReadiness(dc)
	if !dcReady.Ready {
		state := "pending"
		switch dcReady.ReasonCode {
		case "RESOURCE_TERMINAL_FAILURE", "RESOURCE_DELETING", "RESOURCE_PHASE_INVALID", "RESOURCE_STATUS_INVALID":
			state = "conflict"
		}
		return tenantResult(state, "TENANT_CLUSTER_"+dcReady.ReasonCode, "The DPUCluster controller has not reported current readiness.", nil)
	}
	tcp := observed.TenantControlPlane
	if tcp.GetUID() == "" {
		return tenantResult("invalid", "TENANT_CONTROL_PLANE_IDENTITY_INVALID", "The observed Kamaji control plane has no server identity.", nil)
	}
	if tcp.GetAPIVersion() != "kamaji.clastix.io/v1alpha1" || tcp.GetKind() != "TenantControlPlane" || tcp.GetName() != expected.Name || tcp.GetNamespace() != expected.Namespace {
		return tenantResult("conflict", "TENANT_CONTROL_PLANE_IDENTITY_MISMATCH", "The observed Kamaji control plane has an unexpected identity.", nil)
	}
	if !hasOwnerReference(tcp, dc) || !hasLabel(tcp, "dpu.nvidia.com/cluster", expected.Name) || !hasLabel(tcp, "tenant.clastix.io", expected.Name) {
		return tenantResult("conflict", "TENANT_CONTROL_PLANE_OWNERSHIP_MISMATCH", "The Kamaji control plane is not owned by the selected DPUCluster.", nil)
	}
	if err := compareTenantControlPlaneSpec(expected, tcp); err != nil {
		return tenantResult("conflict", "TENANT_CONTROL_PLANE_SPEC_MISMATCH", "The Kamaji control plane network or version spec differs from retained intent.", err)
	}
	versionState, found, err := unstructured.NestedString(tcp.Object, "status", "kubernetesResources", "version", "status")
	if err != nil {
		return tenantResult("invalid", "TENANT_CONTROL_PLANE_STATUS_INVALID", "Kamaji control plane status is malformed.", nil)
	}
	if !found || versionState == "" {
		return tenantResult("pending", "TENANT_CONTROL_PLANE_PROGRESSING", "Kamaji has not reported the selected Kubernetes version ready.", nil)
	}
	switch versionState {
	case "Ready":
	case "Unknown", "Provisioning", "CertificateAuthorityRotating", "Upgrading", "Migrating", "NotReady", "Sleeping", "WriteLimited":
		return tenantResult("pending", "TENANT_CONTROL_PLANE_PROGRESSING", "Kamaji has not reported the selected Kubernetes version ready.", nil)
	default:
		return tenantResult("invalid", "TENANT_CONTROL_PLANE_STATUS_INVALID", "Kamaji reported an unsupported control plane status.", nil)
	}
	runningVersion, runningVersionFound, runningVersionErr := unstructured.NestedString(tcp.Object, "status", "kubernetesResources", "version", "version")
	if runningVersionErr != nil || !runningVersionFound || runningVersion == "" {
		return tenantResult("invalid", "TENANT_CONTROL_PLANE_STATUS_INVALID", "Kamaji has not reported a running Kubernetes version.", nil)
	}
	if runningVersion != expected.KubernetesVersion {
		return tenantResult("conflict", "TENANT_CONTROL_PLANE_VERSION_MISMATCH", "The reported running tenant Kubernetes version differs from retained intent.", nil)
	}
	metadataGeneration, generationFound, generationErr := unstructured.NestedInt64(tcp.Object, "metadata", "generation")
	observedGeneration, statusGenerationFound, statusGenerationErr := unstructured.NestedInt64(tcp.Object, "status", "observedGeneration")
	if generationErr != nil || statusGenerationErr != nil || !generationFound || metadataGeneration < 1 || observedGeneration < 0 || observedGeneration > metadataGeneration {
		return tenantResult("invalid", "TENANT_CONTROL_PLANE_STATUS_INVALID", "Kamaji control plane generation status is malformed.", nil)
	}
	if !statusGenerationFound || observedGeneration < metadataGeneration {
		return tenantResult("pending", "TENANT_CONTROL_PLANE_STATUS_STALE", "Kamaji has not reconciled the current control plane spec.", nil)
	}
	if err := compareTenantService(expected, tcp, observed.Service); err != nil {
		return tenantResult("conflict", "TENANT_SERVICE_SPEC_MISMATCH", "The Kamaji API Service does not match the selected tenant endpoint.", err)
	}
	port, _, _ := unstructured.NestedInt64(tcp.Object, "spec", "networkProfile", "port")
	if readiness := evaluateKeepalived(expected, dc, observed.Keepalived, port); !readiness.Ready {
		return readiness
	}
	return TenantReadiness{Ready: true, State: TenantReadinessReady, ReasonCode: "TENANT_RESOURCES_CONVERGED", Message: "DPF and Kamaji tenant endpoint resources match retained network intent; tenant API access, credentials, keepalived image provenance, and VIP operation remain unverified."}
}

func validateTenantExpectation(e TenantReadinessExpected) error {
	if e.Name == "" || e.Namespace == "" || e.KubernetesVersion == "" || e.VIP == "" || e.Interface == "" || e.PodCIDR == "" || e.ServiceCIDR == "" || e.VirtualRouterID < 1 || e.VirtualRouterID > 255 || e.MaxNodes < 1 {
		return fmt.Errorf("required tenant identity, network, version, or capacity is missing")
	}
	return nil
}

func compareDPUClusterSpec(e TenantReadinessExpected, dc *unstructured.Unstructured) error {
	spec, found, err := unstructured.NestedMap(dc.Object, "spec")
	if err != nil || !found {
		return fmt.Errorf("DPUCluster spec is missing or malformed")
	}
	clusterType, _, err := unstructured.NestedString(spec, "type")
	if err != nil || clusterType != "kamaji" {
		return fmt.Errorf("DPUCluster type differs")
	}
	maxNodes, found, err := unstructured.NestedInt64(spec, "maxNodes")
	if err != nil || !found || maxNodes != e.MaxNodes {
		return fmt.Errorf("DPUCluster maxNodes differs")
	}
	vip, _, err := unstructured.NestedString(spec, "clusterEndpoint", "keepalived", "vip")
	if err != nil || vip != e.VIP {
		return fmt.Errorf("DPUCluster VIP differs")
	}
	iface, _, err := unstructured.NestedString(spec, "clusterEndpoint", "keepalived", "interface")
	if err != nil || iface != e.Interface {
		return fmt.Errorf("DPUCluster interface differs")
	}
	routerID, found, err := unstructured.NestedInt64(spec, "clusterEndpoint", "keepalived", "virtualRouterID")
	if err != nil || !found || routerID != e.VirtualRouterID {
		return fmt.Errorf("DPUCluster virtual router ID differs")
	}
	selector, found, err := unstructured.NestedStringMap(spec, "clusterEndpoint", "keepalived", "nodeSelector")
	if err != nil || (found && !maps.Equal(selector, e.NodeSelector)) || (!found && len(e.NodeSelector) != 0) {
		return fmt.Errorf("DPUCluster node selector differs")
	}
	if !reflect.DeepEqual(dc.GetLabels(), e.Labels) {
		return fmt.Errorf("DPUCluster identity labels differ")
	}
	kubeconfig, found, err := unstructured.NestedString(spec, "kubeconfig")
	if err != nil || (found && kubeconfig != "" && kubeconfig != e.Name+"-admin-kubeconfig") {
		return fmt.Errorf("DPUCluster kubeconfig name is not the controller-derived name")
	}
	return nil
}

func compareTenantControlPlaneSpec(e TenantReadinessExpected, tcp *unstructured.Unstructured) error {
	version, found, err := unstructured.NestedString(tcp.Object, "spec", "kubernetes", "version")
	if err != nil || !found || version != e.KubernetesVersion {
		return fmt.Errorf("kamaji Kubernetes version differs")
	}
	address, found, err := unstructured.NestedString(tcp.Object, "spec", "networkProfile", "address")
	if err != nil || !found || address != e.VIP {
		return fmt.Errorf("kamaji network address differs")
	}
	for _, field := range []struct{ name, value string }{{"serviceCidrs", e.ServiceCIDR}, {"podCidrs", e.PodCIDR}} {
		values, found, err := unstructured.NestedStringSlice(tcp.Object, "spec", "networkProfile", field.name)
		if err != nil || !found || len(values) != 1 || values[0] != field.value {
			return fmt.Errorf("kamaji %s differs", field.name)
		}
	}
	return nil
}

func compareTenantService(e TenantReadinessExpected, tcp, svc *unstructured.Unstructured) error {
	if svc.GetUID() == "" || svc.GetAPIVersion() != "v1" || svc.GetKind() != "Service" || svc.GetName() != e.Name || svc.GetNamespace() != e.Namespace {
		return fmt.Errorf("tenant API Service identity differs")
	}
	serviceType, _, err := unstructured.NestedString(svc.Object, "spec", "type")
	if err != nil || serviceType != "NodePort" {
		return fmt.Errorf("tenant API Service type differs")
	}
	selector, found, err := unstructured.NestedStringMap(svc.Object, "spec", "selector")
	if err != nil || !found || !reflect.DeepEqual(selector, map[string]string{"kamaji.clastix.io/name": e.Name}) {
		return fmt.Errorf("tenant API Service selector differs")
	}
	ports, found, err := unstructured.NestedSlice(svc.Object, "spec", "ports")
	if err != nil || !found || len(ports) != 1 {
		return fmt.Errorf("tenant API Service port is missing or ambiguous")
	}
	port, ok := ports[0].(map[string]any)
	if !ok {
		return fmt.Errorf("tenant API Service port is malformed")
	}
	name, _ := port["name"].(string)
	protocol, _ := port["protocol"].(string)
	servicePort, portOK, serviceErr := unstructured.NestedInt64(port, "port")
	nodePort, nodeOK, nodeErr := unstructured.NestedInt64(port, "nodePort")
	targetPort, targetOK, targetErr := unstructured.NestedInt64(port, "targetPort")
	tcpPort, tcpFound, tcpErr := unstructured.NestedInt64(tcp.Object, "spec", "networkProfile", "port")
	if serviceErr != nil || nodeErr != nil || targetErr != nil || name != "kube-apiserver" || protocol != "TCP" || !portOK || servicePort < 1 || servicePort > 65535 || !nodeOK || nodePort < 1 || nodePort > 65535 || !targetOK || targetPort != tcpPort || tcpErr != nil || !tcpFound || nodePort != tcpPort || servicePort != tcpPort {
		return fmt.Errorf("tenant API Service port does not match Kamaji allocated endpoint")
	}
	// DPF 26.8 expectedService does not set an owner reference. Exact name,
	// namespace, selector, and endpoint are the source-backed identity checks.
	return nil
}

func evaluateKeepalived(e TenantReadinessExpected, dc *unstructured.Unstructured, objects []*unstructured.Unstructured, port int64) TenantReadiness {
	if len(objects) != 2 {
		return tenantResult("pending", "TENANT_KEEPALIVED_UNAVAILABLE", "DPF has not observed both owned keepalived resources.", nil)
	}
	var configMap, daemonSet *unstructured.Unstructured
	for _, object := range objects {
		if object == nil {
			return tenantResult("pending", "TENANT_KEEPALIVED_UNAVAILABLE", "DPF has not observed both owned keepalived resources.", nil)
		}
		if object.GetDeletionTimestamp() != nil {
			return tenantResult("conflict", "TENANT_KEEPALIVED_DELETING", "A keepalived resource is being deleted.", nil)
		}
		if object.GetUID() == "" || object.GetNamespace() != e.Namespace || object.GetName() != e.Name+"-keepalived" || !hasOwnerReference(object, dc) {
			return tenantResult("conflict", "TENANT_KEEPALIVED_OWNERSHIP_MISMATCH", "A keepalived resource is not owned by the selected DPUCluster.", nil)
		}
		switch object.GetKind() {
		case "ConfigMap":
			if object.GetAPIVersion() != "v1" || configMap != nil {
				return tenantResult("conflict", "TENANT_KEEPALIVED_IDENTITY_MISMATCH", "Keepalived ConfigMap API or identity is ambiguous.", nil)
			}
			configMap = object
		case "DaemonSet":
			if object.GetAPIVersion() != "apps/v1" || daemonSet != nil {
				return tenantResult("conflict", "TENANT_KEEPALIVED_IDENTITY_MISMATCH", "Keepalived DaemonSet API or identity is ambiguous.", nil)
			}
			daemonSet = object
		default:
			return tenantResult("conflict", "TENANT_KEEPALIVED_IDENTITY_MISMATCH", "An unexpected keepalived resource kind was observed.", nil)
		}
	}
	if configMap == nil || daemonSet == nil {
		return tenantResult("pending", "TENANT_KEEPALIVED_UNAVAILABLE", "DPF has not observed both owned keepalived resources.", nil)
	}
	template, found, err := unstructured.NestedString(configMap.Object, "data", "keepalived.conf.tmpl")
	if err != nil || !found || !sameKeepalivedTemplate(template) {
		return tenantResult("conflict", "TENANT_KEEPALIVED_SPEC_MISMATCH", "The keepalived configuration template differs from the pinned controller template.", nil)
	}
	selector, found, err := unstructured.NestedStringMap(daemonSet.Object, "spec", "selector", "matchLabels")
	if err != nil || !found || !reflect.DeepEqual(selector, map[string]string{"app.kubernetes.io/name": e.Name + "-keepalived"}) {
		return tenantResult("conflict", "TENANT_KEEPALIVED_SPEC_MISMATCH", "The keepalived DaemonSet selector differs from the pinned controller template.", nil)
	}
	nodeSelector, found, err := unstructured.NestedStringMap(daemonSet.Object, "spec", "template", "spec", "nodeSelector")
	if err != nil || (found && !maps.Equal(nodeSelector, e.NodeSelector)) || (!found && len(e.NodeSelector) != 0) {
		return tenantResult("conflict", "TENANT_KEEPALIVED_SPEC_MISMATCH", "The keepalived node selector differs from retained intent.", nil)
	}
	readiness := daemonSetReadiness(daemonSet)
	if !readiness.Ready {
		return readiness
	}
	if err := compareKeepalivedNetwork(daemonSet, e, port); err != nil {
		return tenantResult("conflict", "TENANT_KEEPALIVED_SPEC_MISMATCH", "The keepalived endpoint settings differ from retained intent.", err)
	}
	return TenantReadiness{Ready: true}
}

func daemonSetReadiness(ds *unstructured.Unstructured) TenantReadiness {
	generation, found, err := unstructured.NestedInt64(ds.Object, "metadata", "generation")
	if err != nil || !found || generation < 1 {
		return tenantResult("invalid", "TENANT_KEEPALIVED_STATUS_INVALID", "Keepalived DaemonSet metadata generation is malformed.", nil)
	}
	observed, found, err := unstructured.NestedInt64(ds.Object, "status", "observedGeneration")
	if err != nil || (found && observed > generation) {
		return tenantResult("invalid", "TENANT_KEEPALIVED_STATUS_INVALID", "Keepalived DaemonSet observed generation is malformed.", nil)
	}
	if !found || observed < generation {
		return tenantResult("pending", "TENANT_KEEPALIVED_STATUS_STALE", "The DaemonSet controller has not observed the current keepalived spec.", nil)
	}
	desired, found, err := unstructured.NestedInt64(ds.Object, "status", "desiredNumberScheduled")
	if err != nil || (found && desired < 0) {
		return tenantResult("invalid", "TENANT_KEEPALIVED_STATUS_INVALID", "Keepalived DaemonSet desired count is malformed.", nil)
	}
	if !found || desired == 0 {
		return tenantResult("pending", "TENANT_KEEPALIVED_NOT_SCHEDULED", "Keepalived is not scheduled on any eligible management node.", nil)
	}
	updated, found, err := unstructured.NestedInt64(ds.Object, "status", "updatedNumberScheduled")
	if err != nil || (found && updated < 0) {
		return tenantResult("invalid", "TENANT_KEEPALIVED_STATUS_INVALID", "Keepalived DaemonSet updated count is malformed.", nil)
	}
	if !found || updated < desired {
		return tenantResult("pending", "TENANT_KEEPALIVED_ROLLOUT_PENDING", "Keepalived has not rolled out to every eligible management node.", nil)
	}
	ready, found, err := unstructured.NestedInt64(ds.Object, "status", "numberReady")
	if err != nil || (found && ready < 0) {
		return tenantResult("invalid", "TENANT_KEEPALIVED_STATUS_INVALID", "Keepalived DaemonSet ready count is malformed.", nil)
	}
	if !found || ready < desired {
		return tenantResult("pending", "TENANT_KEEPALIVED_NOT_READY", "Keepalived DaemonSet pods are not all ready.", nil)
	}
	if ready > updated || updated > desired {
		return tenantResult("invalid", "TENANT_KEEPALIVED_STATUS_INVALID", "Keepalived DaemonSet counts are inconsistent.", nil)
	}
	misscheduled, found, err := unstructured.NestedInt64(ds.Object, "status", "numberMisscheduled")
	if err != nil || (found && misscheduled < 0) {
		return tenantResult("invalid", "TENANT_KEEPALIVED_STATUS_INVALID", "Keepalived DaemonSet misscheduled count is malformed.", nil)
	}
	if !found || misscheduled != 0 {
		return tenantResult("pending", "TENANT_KEEPALIVED_MISSCHEDULED", "Keepalived placement has not converged on only eligible management nodes.", nil)
	}
	return TenantReadiness{Ready: true, State: TenantReadinessReady, ReasonCode: "TENANT_KEEPALIVED_READY"}
}

func hasOwnerReference(object, owner *unstructured.Unstructured) bool {
	refs, found, err := unstructured.NestedSlice(object.Object, "metadata", "ownerReferences")
	if err != nil || !found {
		return false
	}
	var matchingRefs int
	var hasSelectedOwner bool
	for _, raw := range refs {
		ref, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		kind, _ := ref["kind"].(string)
		if kind != "DPUCluster" {
			continue
		}
		matchingRefs++
		uid, _ := ref["uid"].(string)
		name, _ := ref["name"].(string)
		apiVersion, _ := ref["apiVersion"].(string)
		hasSelectedOwner = uid == string(owner.GetUID()) && name == owner.GetName() && apiVersion == "provisioning.dpu.nvidia.com/v1alpha1"
	}
	return matchingRefs == 1 && hasSelectedOwner
}

func hasLabel(object *unstructured.Unstructured, key, want string) bool {
	return object.GetLabels()[key] == want
}

func tenantResult(state string, code, message string, err error) TenantReadiness {
	readinessState := TenantReadinessState(state)
	if err != nil {
		message += " " + err.Error()
	}
	return TenantReadiness{State: readinessState, ReasonCode: code, Message: message}
}
