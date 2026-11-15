// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"
	"maps"
	"net/netip"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
)

// tenantClusterCheck binds the resolved target to the cross-namespace DPUCluster
// observations. A clear object-registry check does not prove network reservation.
func tenantClusterCheck(snapshot *managementprobe.Snapshot, target *bootstrap.Inputs) inventory.Check {
	check := inventory.Check{
		ID: "dpf.hbn.management.tenant-clusters", Subject: "selected DPUCluster",
		Vantage: "executor", RequiredPhase: "deploy", State: "unknown",
		ReasonCode: "TENANT_TARGET_UNRESOLVED",
		Message:    "The selected tenant identity and endpoint could not be verified against complete cross-namespace DPUCluster observations.",
	}
	if target == nil {
		check.ReasonCode = "TENANT_TARGET_INPUTS_UNAVAILABLE"
		check.Message = "Resolved tenant target inputs are unavailable; existing DPUClusters cannot be selected or compared."
		return check
	}
	check.Subject = target.Tenant.Namespace + "/" + target.Tenant.Name
	if target.Tenant.VIP == "" {
		check.ReasonCode = "TENANT_VIP_UNRESOLVED"
		check.Message = "A desired tenant VIP is required before checking observed DPUCluster endpoints."
		return check
	}
	desiredVIP, err := netip.ParseAddr(target.Tenant.VIP)
	if err != nil || !desiredVIP.Is4() || !desiredVIP.IsGlobalUnicast() {
		check.ReasonCode = "TENANT_VIP_UNRESOLVED"
		check.Message = "The desired tenant VIP is not a valid unicast IPv4 address."
		return check
	}
	if len(target.Tenant.NodeSelector) == 0 {
		check.ReasonCode = "TENANT_TARGET_SELECTOR_UNAVAILABLE"
		check.Message = "A nonempty selected-node selector is required to compare Keepalived placement."
		return check
	}
	if snapshot == nil {
		check.ReasonCode = "TENANT_CLUSTERS_UNOBSERVED"
		check.Message = "The cross-namespace DPUCluster list is unavailable or incomplete; tenant target and VIP conflicts remain unknown."
		return check
	}
	if snapshot.Namespace != target.OperatorNamespace {
		check.ReasonCode = "TENANT_OBSERVATION_NAMESPACE_MISMATCH"
		check.Message = "The explicit management observation namespace does not match the resolved DPF operator namespace."
		return check
	}
	if snapshot.DPUClusters.State == managementprobe.CollectionUnavailable {
		check.ReasonCode = "TENANT_CLUSTERS_UNOBSERVED"
		check.Message = "The cross-namespace DPUCluster list is unavailable; tenant target and VIP conflicts remain unknown."
		return check
	}

	var selected *managementprobe.DPUCluster
	unobservedEndpoint := false
	unobservedRouterID := false
	unobservedSelector := ""
	var overlappingRouterIDs []string
	for i := range snapshot.DPUClusters.Items {
		cluster := &snapshot.DPUClusters.Items[i]
		if cluster.Namespace == target.Tenant.Namespace && cluster.Name == target.Tenant.Name {
			if cluster.TenantIdentityConflict {
				check.State, check.ReasonCode = "failed", "TENANT_IDENTITY_CONFLICT"
				check.Message = "The selected DPUCluster has conflicting Launch Kit tenant identity labels; its target identity cannot be determined safely."
				return check
			}
			if selected != nil {
				check.State, check.ReasonCode = "failed", "TENANT_TARGET_AMBIGUOUS"
				check.Message = "More than one observed DPUCluster has the selected namespaced identity."
				return check
			}
			selected = cluster
			continue
		}
		var observedVIP netip.Addr
		if cluster.VIP != nil && *cluster.VIP != "" {
			parsed, err := netip.ParseAddr(*cluster.VIP)
			if err != nil || (!parsed.Is4() && !parsed.Is4In6()) || !parsed.IsGlobalUnicast() {
				unobservedEndpoint = true
				continue
			}
			observedVIP = parsed.Unmap()
			if observedVIP == desiredVIP {
				check.State, check.ReasonCode = "failed", "TENANT_VIP_CONFLICT"
				check.Message = fmt.Sprintf("Desired tenant VIP is already specified by DPUCluster %s/%s.", cluster.Namespace, cluster.Name)
				return check
			}
		}
		if cluster.KeepalivedPresent != nil && !*cluster.KeepalivedPresent {
			if observedVIP.IsValid() {
				unobservedEndpoint = true // Contradictory object fields are not a collision-free observation.
				continue
			}
			if cluster.Type != nil && (*cluster.Type == "kamaji" || *cluster.Type == "static") {
				continue
			}
			unobservedEndpoint = true
			continue
		}
		if cluster.KeepalivedPresent == nil || !observedVIP.IsValid() {
			unobservedEndpoint = true
			continue
		}
		if cluster.VirtualRouterID == nil || *cluster.VirtualRouterID < 1 || *cluster.VirtualRouterID > 255 {
			unobservedRouterID = true
		} else if *cluster.VirtualRouterID == int64(target.Tenant.RouterID) {
			if cluster.NodeSelector == nil {
				if unobservedSelector == "" {
					unobservedSelector = cluster.Namespace + "/" + cluster.Name
				}
			} else if selectorsMayOverlap(target.Tenant.NodeSelector, *cluster.NodeSelector) {
				overlappingRouterIDs = append(overlappingRouterIDs, cluster.Namespace+"/"+cluster.Name)
			}
		}
	}
	if len(overlappingRouterIDs) > 0 {
		sort.Strings(overlappingRouterIDs)
		check.State, check.ReasonCode = "failed", "TENANT_VRID_CONFLICT"
		check.Message = fmt.Sprintf("DPUCluster %s specifies the requested VRRP ID with a node selector that can overlap this tenant; choose a distinct VRID or disjoint node selectors.", overlappingRouterIDs[0])
		return check
	}
	if snapshot.DPUClusters.State != managementprobe.CollectionComplete && snapshot.DPUClusters.State != managementprobe.CollectionAbsent {
		check.ReasonCode = "TENANT_CLUSTERS_UNOBSERVED"
		check.Message = "The cross-namespace DPUCluster list is incomplete; known VIP and VRID conflicts are reported, but tenant selection cannot pass."
		return check
	}
	if unobservedEndpoint {
		check.ReasonCode = "TENANT_ENDPOINT_UNOBSERVED"
		check.Message = "At least one other existing DPUCluster has a missing or malformed Keepalived endpoint, so tenant VIP and VRID conflicts cannot be compared completely."
		return check
	}
	if unobservedSelector != "" {
		check.ReasonCode = "TENANT_VRID_SCOPE_UNVERIFIED"
		check.Message = fmt.Sprintf("DPUCluster %s specifies the requested VRRP ID but its node selector is missing or malformed; the placement overlap cannot be determined.", unobservedSelector)
		return check
	}
	if unobservedRouterID {
		check.ReasonCode = "TENANT_VRID_UNOBSERVED"
		check.Message = "An existing DPUCluster endpoint has no valid observed VRRP ID; conflicts with the selected tenant cannot be excluded."
		return check
	}
	if selected == nil {
		check.State, check.ReasonCode = "passed", "TENANT_TARGET_ABSENT_FROM_DPUCLUSTERS"
		check.Message = "No existing DPUCluster has the selected namespaced identity or specifies the desired VIP; address reservation and network reachability are not established."
		return check
	}
	if selected.Type == nil || *selected.Type == "" {
		check.ReasonCode, check.Message = "TENANT_TARGET_TYPE_UNKNOWN", "The selected DPUCluster type is not observed; a Kamaji target cannot be established."
		return check
	}
	if *selected.Type != "kamaji" {
		check.State, check.ReasonCode, check.Message = "failed", "TENANT_TARGET_TYPE_MISMATCH", "The selected DPUCluster uses a different control-plane type; the HBN bootstrap plan requires Kamaji."
		return check
	}
	if selected.LaunchKitTenantLabelKey == nil || selected.LaunchKitTenantLabelValue == nil {
		check.ReasonCode = "TENANT_TARGET_OWNERSHIP_UNKNOWN"
		check.Message = "The selected DPUCluster has no verifiable Launch Kit tenant identity label."
		return check
	}
	if len(target.Tenant.Labels) != 1 {
		check.ReasonCode = "TENANT_TARGET_IDENTITY_UNKNOWN"
		check.Message = "Resolved tenant identity labels are incomplete."
		return check
	}
	for key, value := range target.Tenant.Labels {
		if *selected.LaunchKitTenantLabelKey != key || *selected.LaunchKitTenantLabelValue != value {
			check.State, check.ReasonCode = "failed", "TENANT_TARGET_IDENTITY_MISMATCH"
			check.Message = "The selected DPUCluster Launch Kit tenant identity does not match the resolved target."
			return check
		}
	}
	if selected.KeepalivedPresent == nil || !*selected.KeepalivedPresent {
		check.ReasonCode = "TENANT_TARGET_ENDPOINT_UNOBSERVED"
		check.Message = "The selected DPUCluster Keepalived endpoint is absent or malformed."
		return check
	}
	if selected.VIP == nil || selected.Interface == nil || selected.VirtualRouterID == nil {
		check.ReasonCode = "TENANT_TARGET_ENDPOINT_UNOBSERVED"
		check.Message = "The selected DPUCluster does not report a complete keepalived endpoint."
		return check
	}
	if *selected.VIP != target.Tenant.VIP || *selected.Interface != target.Tenant.Interface || *selected.VirtualRouterID != int64(target.Tenant.RouterID) {
		check.State, check.ReasonCode = "failed", "TENANT_TARGET_ENDPOINT_MISMATCH"
		check.Message = "The selected DPUCluster keepalived endpoint differs from the resolved tenant VIP, interface, or VRID."
		return check
	}
	if selected.NodeSelector == nil {
		check.ReasonCode = "TENANT_TARGET_SELECTOR_UNOBSERVED"
		check.Message = "The selected DPUCluster Keepalived node selector is missing or malformed."
		return check
	}
	if !maps.Equal(*selected.NodeSelector, target.Tenant.NodeSelector) {
		check.State, check.ReasonCode = "failed", "TENANT_TARGET_SELECTOR_MISMATCH"
		check.Message = "The selected DPUCluster Keepalived node selector differs from the resolved tenant placement."
		return check
	}
	check.State, check.ReasonCode = "passed", "TENANT_TARGET_MATCHES"
	check.Message = "The selected DPUCluster identity, type, keepalived endpoint, and node selector match the target. This is registry binding only, not a Ready result; deployment must verify controller, tenant API, and endpoint readiness from fresh API observations."
	return check
}

// tenantClusterProjection retains only records needed to explain the selected
// target, a known VIP collision, or an endpoint/selector that prevents a safe comparison.
// Target and VIP conflicts are still evaluated against the complete source snapshot.
func tenantClusterProjection(snapshot *managementprobe.Snapshot, target *bootstrap.Inputs) []inventory.HBNManagementCluster {
	result := []inventory.HBNManagementCluster{}
	if snapshot == nil || target == nil || snapshot.Namespace != target.OperatorNamespace ||
		snapshot.DPUClusters.State != managementprobe.CollectionComplete && snapshot.DPUClusters.State != managementprobe.CollectionIncomplete {
		return result
	}
	var desiredVIP netip.Addr
	if parsed, err := netip.ParseAddr(target.Tenant.VIP); err == nil && parsed.Is4() && parsed.IsGlobalUnicast() {
		desiredVIP = parsed
	}
	for _, cluster := range snapshot.DPUClusters.Items {
		selected := cluster.Namespace == target.Tenant.Namespace && cluster.Name == target.Tenant.Name
		if selected {
			result = append(result, inventory.HBNManagementCluster{
				Name: cluster.Name, Namespace: cluster.Namespace, UID: cluster.UID, ResourceVersion: cluster.ResourceVersion,
				Type: deref(cluster.Type), Phase: deref(cluster.Phase), Version: deref(cluster.Version), VIP: deref(cluster.VIP),
				Interface: deref(cluster.Interface), VirtualRouterID: copyInt64(cluster.VirtualRouterID),
				NodeSelector:            cloneSelector(cluster.NodeSelector),
				LaunchKitTenantLabelKey: deref(cluster.LaunchKitTenantLabelKey), LaunchKitTenantLabelValue: deref(cluster.LaunchKitTenantLabelValue),
				Owners: ownerSummaries(cluster.Owners),
			})
			continue
		}
		if !desiredVIP.IsValid() {
			continue
		}
		var observedVIP netip.Addr
		if cluster.VIP != nil && *cluster.VIP != "" {
			parsed, err := netip.ParseAddr(*cluster.VIP)
			if err == nil && (parsed.Is4() || parsed.Is4In6()) && parsed.IsGlobalUnicast() {
				observedVIP = parsed.Unmap()
				if observedVIP == desiredVIP {
					// The name and address identify a concrete collision. The rest
					// of this unrelated tenant's spec is not needed.
					result = append(result, inventory.HBNManagementCluster{Name: cluster.Name, Namespace: cluster.Namespace, VIP: *cluster.VIP})
					continue
				}
			} else {
				result = append(result, inventory.HBNManagementCluster{Name: cluster.Name, Namespace: cluster.Namespace, VirtualRouterID: copyInt64(cluster.VirtualRouterID), NodeSelector: cloneSelector(cluster.NodeSelector)})
				continue
			}
		}
		if cluster.KeepalivedPresent != nil && !*cluster.KeepalivedPresent && !observedVIP.IsValid() && cluster.Type != nil && (*cluster.Type == "kamaji" || *cluster.Type == "static") {
			continue
		}
		if cluster.KeepalivedPresent == nil || !observedVIP.IsValid() {
			result = append(result, inventory.HBNManagementCluster{Name: cluster.Name, Namespace: cluster.Namespace, VirtualRouterID: copyInt64(cluster.VirtualRouterID), NodeSelector: cloneSelector(cluster.NodeSelector)})
			continue
		}
		selectorMayConflict := cluster.NodeSelector == nil || selectorsMayOverlap(target.Tenant.NodeSelector, *cluster.NodeSelector)
		if cluster.KeepalivedPresent == nil || (cluster.KeepalivedPresent != nil && !*cluster.KeepalivedPresent) ||
			cluster.VirtualRouterID == nil || *cluster.VirtualRouterID < 1 || *cluster.VirtualRouterID > 255 ||
			(*cluster.VirtualRouterID == int64(target.Tenant.RouterID) && selectorMayConflict) {
			// Retain only the identity and VRRP observation needed to explain
			// an unresolved comparison; omit the unrelated tenant specification.
			result = append(result, inventory.HBNManagementCluster{Name: cluster.Name, Namespace: cluster.Namespace, VirtualRouterID: copyInt64(cluster.VirtualRouterID), NodeSelector: cloneSelector(cluster.NodeSelector)})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Namespace != result[j].Namespace {
			return strings.Compare(result[i].Namespace, result[j].Namespace) < 0
		}
		if result[i].Name != result[j].Name {
			return strings.Compare(result[i].Name, result[j].Name) < 0
		}
		return strings.Compare(result[i].UID, result[j].UID) < 0
	})
	return result
}

func selectorsMayOverlap(first, second map[string]string) bool {
	for key, value := range first {
		if other, exists := second[key]; exists && other != value {
			return false
		}
	}
	return true
}

func cloneSelector(selector *map[string]string) *map[string]string {
	if selector == nil {
		return nil
	}
	copy := maps.Clone(*selector)
	return &copy
}

func projectTenantIssues(snapshot *managementprobe.Snapshot, tenants []inventory.HBNManagementCluster) []inventory.HBNManagementIssue {
	if snapshot == nil || len(tenants) == 0 {
		return []inventory.HBNManagementIssue{}
	}
	retained := make(map[string]struct{}, len(tenants))
	for _, tenant := range tenants {
		retained[tenant.Namespace+"/"+tenant.Name] = struct{}{}
	}
	result := []inventory.HBNManagementIssue{}
	for _, issue := range snapshot.Issues {
		if issue.Source != "dpuclusters.provisioning.dpu.nvidia.com" || issue.Namespace == "" || issue.Resource == "" {
			continue
		}
		if _, ok := retained[issue.Namespace+"/"+issue.Resource]; ok {
			result = append(result, inventory.HBNManagementIssue{Source: issue.Source, Resource: issue.Resource, Namespace: issue.Namespace, Field: issue.Field, Code: issue.Code, Message: issue.Message})
		}
	}
	return result
}

func copyInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
