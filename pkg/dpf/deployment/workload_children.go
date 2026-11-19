// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"reflect"
	"sort"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/discovery"
)

func (tenant *tenantClient) getWorkloadChild(ctx context.Context, parent *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if tenant == nil || parent == nil || ctx == nil {
		return nil, fmt.Errorf("bound management and tenant clients and workload parent are required")
	}
	if tenant.api == nil {
		return nil, fmt.Errorf("bound tenant API client is unavailable")
	}
	if err := tenant.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	group, version, resource, kind, err := workloadChildIdentity(parent)
	if err != nil {
		return nil, err
	}
	if len(validation.IsDNS1123Label(parent.GetNamespace())) != 0 || len(validation.IsDNS1123Subdomain(parent.GetName())) != 0 {
		return nil, fmt.Errorf("invalid retained tenant child identity")
	}
	apiVersion := group + "/" + version
	resources, err := discovery.ToServerResourcesInterfaceWithContext(tenant.api.Discovery()).ServerResourcesForGroupVersionWithContext(ctx, apiVersion)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("selected tenant does not serve the required workload child API")
	}
	if resources == nil {
		return nil, fmt.Errorf("selected tenant returned an empty workload child API description")
	}
	resourceFound := false
	for _, served := range resources.APIResources {
		if served.Kind != kind || served.Name != resource {
			continue
		}
		if !served.Namespaced || resourceFound {
			return nil, fmt.Errorf("selected tenant workload child API has ambiguous resource scope")
		}
		resourceFound = true
	}
	if !resourceFound {
		return nil, fmt.Errorf("selected tenant does not serve the required namespaced workload child kind")
	}
	path := "/apis/" + group + "/" + version + "/namespaces/" + parent.GetNamespace() + "/" + resource + "/" + parent.GetName()
	var child unstructured.Unstructured
	if err := tenant.api.Discovery().RESTClient().Get().AbsPath(path).Do(ctx).Into(&child); err != nil {
		return nil, err
	}
	if child.GroupVersionKind() != (schema.GroupVersionKind{Group: group, Version: version, Kind: kind}) {
		return nil, fmt.Errorf("selected tenant returned the wrong workload child API identity")
	}
	if err := tenant.VerifyIdentity(ctx); err != nil {
		return nil, err
	}
	return &child, nil
}

func workloadChildIdentity(parent *unstructured.Unstructured) (group, version, resource, kind string, err error) {
	if parent == nil {
		return "", "", "", "", fmt.Errorf("workload parent is required")
	}
	switch parent.GetKind() {
	case "DPUServiceIPAM":
		if parent.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" {
			return "", "", "", "", fmt.Errorf("unsupported retained DPUServiceIPAM API")
		}
		if _, found, nestedErr := unstructured.NestedMap(parent.Object, "spec", "ipv4Network"); nestedErr != nil || !found {
			return "", "", "", "", fmt.Errorf("retained DPUServiceIPAM does not use the pinned CIDRPool mapping")
		}
		return "nv-ipam.nvidia.com", "v1alpha1", "cidrpools", "CIDRPool", nil
	case "DPUServiceInterface":
		if parent.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" {
			return "", "", "", "", fmt.Errorf("unsupported retained DPUServiceInterface API")
		}
		return "svc.dpu.nvidia.com", "v1alpha1", "serviceinterfacesets", "ServiceInterfaceSet", nil
	default:
		return "", "", "", "", fmt.Errorf("workload resource has no controller-created tenant child")
	}
}

func verifyWorkloadChild(parent, child *unstructured.Unstructured, tenant controllercontract.TenantReadinessExpected) error {
	if parent == nil || child == nil || parent.GetName() == "" || parent.GetNamespace() == "" ||
		child.GetName() != parent.GetName() || child.GetNamespace() != parent.GetNamespace() || child.GetUID() == "" || child.GetResourceVersion() == "" || child.GetDeletionTimestamp() != nil {
		return fmt.Errorf("tenant child identity or server metadata does not match its retained parent")
	}
	group, version, _, kind, err := workloadChildIdentity(parent)
	if err != nil || child.GroupVersionKind() != (schema.GroupVersionKind{Group: group, Version: version, Kind: kind}) {
		return fmt.Errorf("tenant child API identity does not match its pinned controller kind")
	}
	switch parent.GetKind() {
	case "DPUServiceInterface":
		return verifyServiceInterfaceSetChild(parent, child)
	case "DPUServiceIPAM":
		if err := verifyCIDRPoolChild(parent, child); err != nil {
			return err
		}
		if err := verifySelectedIPAMAllocation(parent, tenant); err != nil {
			return err
		}
	}
	return nil
}

// The non-VPC ServiceInterfaceSet controller stamps this exact mode after the
// management controller has copied the template. The stamp can still be absent
// when pre-node reconciliation first reports the child was created.
const workloadInterfaceModeAnnotation = "svc.dpu.nvidia.com/interface-mode"

func verifyServiceInterfaceSetChild(parent, child *unstructured.Unstructured) error {
	expectedSpec, found, specErr := unstructured.NestedMap(parent.Object, "spec", "template", "spec")
	actualSpec, actualFound, actualErr := unstructured.NestedMap(child.Object, "spec")
	if specErr != nil || actualErr != nil || !found || !actualFound || !reflect.DeepEqual(expectedSpec, actualSpec) {
		return reconcileError("WORKLOAD_INTERFACE_CHILD_SPEC_MISMATCH", "ServiceInterfaceSet spec differs from the retained interface template.", nil)
	}
	expectedLabels, _, err := unstructured.NestedStringMap(parent.Object, "spec", "template", "metadata", "labels")
	if err != nil {
		return reconcileError("WORKLOAD_INTERFACE_CHILD_METADATA_INVALID", "Retained interface labels are malformed.", nil)
	}
	actualLabels, _, err := unstructured.NestedStringMap(child.Object, "metadata", "labels")
	if err != nil {
		return reconcileError("WORKLOAD_INTERFACE_CHILD_METADATA_INVALID", "ServiceInterfaceSet labels are malformed.", nil)
	}
	expectedAnnotations, _, err := unstructured.NestedStringMap(parent.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		return reconcileError("WORKLOAD_INTERFACE_CHILD_METADATA_INVALID", "Retained interface annotations are malformed.", nil)
	}
	actualAnnotations, _, err := unstructured.NestedStringMap(child.Object, "metadata", "annotations")
	if err != nil {
		return reconcileError("WORKLOAD_INTERFACE_CHILD_METADATA_INVALID", "ServiceInterfaceSet annotations are malformed.", nil)
	}
	if mode, stamped := actualAnnotations[workloadInterfaceModeAnnotation]; stamped {
		if mode != "nsi" || !flatServiceInterfaceSpec(expectedSpec) {
			return reconcileError("WORKLOAD_INTERFACE_CHILD_MODE_MISMATCH", "ServiceInterfaceSet mode differs from the supported flat interface controller path.", nil)
		}
		if _, retained := expectedAnnotations[workloadInterfaceModeAnnotation]; !retained {
			// NestedStringMap returns an independent copy. Do not mutate the child or
			// discard an explicit retained annotation, even for the controller's key.
			delete(actualAnnotations, workloadInterfaceModeAnnotation)
		}
	}
	if !maps.Equal(expectedLabels, actualLabels) || !maps.Equal(expectedAnnotations, actualAnnotations) {
		return reconcileError("WORKLOAD_INTERFACE_CHILD_METADATA_MISMATCH", "ServiceInterfaceSet metadata differs from the retained interface template.", nil)
	}
	return nil
}

// Match the physical/PF/service forms generated by the flat HBN profile. VPC
// interfaces use a separate sticky mode/feature-gate policy and are not covered.
func flatServiceInterfaceSpec(spec map[string]any) bool {
	kind, found, err := unstructured.NestedString(spec, "template", "spec", "interfaceType")
	if err != nil || !found {
		return false
	}
	switch kind {
	case "physical":
		// The pinned GetVirtualNetworkName has no physical branch.
		return true
	case "pf":
		network, _, err := unstructured.NestedString(spec, "template", "spec", "pf", "virtualNetwork")
		return err == nil && network == ""
	case "service":
		network, _, err := unstructured.NestedString(spec, "template", "spec", "service", "virtualNetwork")
		return err == nil && network == ""
	default:
		return false
	}
}

// Only locally produced, finite categories may enrich a public stage message.
// The error type's Message and its causes are deliberately not copied.
func workloadChildVerificationReason(err error) string {
	var verification *ReconciliationError
	if !errors.As(err, &verification) || verification == nil {
		return ""
	}
	switch verification.ReasonCode {
	case "WORKLOAD_INTERFACE_CHILD_METADATA_INVALID", "WORKLOAD_INTERFACE_CHILD_METADATA_MISMATCH", "WORKLOAD_INTERFACE_CHILD_SPEC_MISMATCH", "WORKLOAD_INTERFACE_CHILD_MODE_MISMATCH":
		return verification.ReasonCode
	default:
		return ""
	}
}

func workloadChildVerificationMessage(message string, err error) string {
	if reason := workloadChildVerificationReason(err); reason != "" {
		return message + " Verification category: " + reason + "."
	}
	return message
}

func verifyCIDRPoolChild(parent, child *unstructured.Unstructured) error {
	network, found, err := unstructured.NestedMap(parent.Object, "spec", "ipv4Network")
	poolSpec, poolFound, poolErr := unstructured.NestedMap(child.Object, "spec")
	if err != nil || poolErr != nil || !found || !poolFound {
		return fmt.Errorf("retained IPAM or tenant CIDRPool spec is malformed")
	}
	networkCIDR, ok := network["network"].(string)
	prefix, prefixOK := numericInt64(network["prefixSize"])
	poolCIDR, poolCIDROK := poolSpec["cidr"].(string)
	poolPrefix, poolPrefixOK := numericInt64(poolSpec["perNodeNetworkPrefix"])
	if !ok || !prefixOK || !poolCIDROK || !poolPrefixOK || poolCIDR != networkCIDR || poolPrefix != prefix {
		return fmt.Errorf("tenant CIDRPool network or prefix differs from retained IPAM")
	}
	if err := compareOptionalNumber(network, poolSpec, "gatewayIndex"); err != nil {
		return err
	}
	nodeSelector, selectorFound, selectorErr := unstructured.NestedMap(parent.Object, "spec", "nodeSelector")
	if selectorErr != nil {
		return fmt.Errorf("retained IPAM node selector is malformed")
	}
	childSelector, childSelectorFound := poolSpec["nodeSelector"]
	if selectorFound != childSelectorFound || selectorFound && !reflect.DeepEqual(nodeSelector, childSelector) {
		return fmt.Errorf("tenant CIDRPool node selector differs from retained IPAM")
	}
	defaultGateway, _, defaultErr := unstructured.NestedBool(network, "defaultGateway")
	if defaultErr != nil {
		return fmt.Errorf("retained IPAM default-gateway setting is malformed")
	}
	childDefaultGateway := false
	if raw, found := poolSpec["defaultGateway"]; found {
		var ok bool
		childDefaultGateway, ok = raw.(bool)
		if !ok {
			return fmt.Errorf("tenant CIDRPool default-gateway setting is malformed")
		}
	}
	if defaultGateway != childDefaultGateway {
		return fmt.Errorf("tenant CIDRPool default-gateway setting differs from retained IPAM")
	}
	if err := verifyCIDRPoolRoutes(network, poolSpec); err != nil {
		return err
	}
	if err := verifyCIDRPoolStaticAllocations(network, poolSpec); err != nil {
		return err
	}
	if err := verifyCIDRPoolExclusions(network, poolSpec); err != nil {
		return err
	}
	wantedLabels := map[string]string{}
	if labels, found, err := unstructured.NestedStringMap(parent.Object, "spec", "labels"); err != nil {
		return fmt.Errorf("retained IPAM pool labels are malformed")
	} else if found {
		for key, value := range labels {
			wantedLabels[key] = value
		}
	}
	wantedLabels["dpu.nvidia.com/dpuserviceipam-name"] = parent.GetName()
	wantedLabels["dpu.nvidia.com/dpuserviceipam-namespace"] = parent.GetNamespace()
	if !reflect.DeepEqual(wantedLabels, child.GetLabels()) {
		return fmt.Errorf("tenant CIDRPool labels do not identify this retained IPAM")
	}
	return nil
}

// verifyCIDRPoolExclusions follows the pinned DPUServiceIPAM controller's
// single-cluster mapping: with no per-cluster quota or static allocation, the
// selected cluster receives the whole CIDR and the controller copies the
// merged user exclusions into CIDRPool.spec.exclusions. HBN host reservations
// are singleton, non-adjacent IPv4 ranges, so this comparison does not need to
// duplicate the controller's general-purpose IP allocator.
func verifyCIDRPoolExclusions(network, poolSpec map[string]any) error {
	if quota, found := network["subnetsPerDPUCluster"]; found && quota != nil {
		return fmt.Errorf("retained IPAM uses unsupported per-cluster subnet allocation")
	}
	if legacy, found := network["exclusions"]; found && legacy != nil {
		if values, ok := legacy.([]any); !ok || len(values) != 0 {
			return fmt.Errorf("retained IPAM uses unsupported legacy exclusions")
		}
	}
	if allocations, found := network["allocations"]; found && allocations != nil {
		if values, ok := allocations.(map[string]any); !ok || len(values) != 0 {
			return fmt.Errorf("retained IPAM uses unsupported static allocations")
		}
	}

	wanted, err := canonicalWorkloadAddressRanges(network["excludeRanges"])
	if err != nil {
		return fmt.Errorf("retained IPAM host reservations are malformed")
	}
	got, err := canonicalWorkloadAddressRanges(poolSpec["exclusions"])
	if err != nil {
		return fmt.Errorf("tenant CIDRPool exclusions are malformed")
	}
	if !reflect.DeepEqual(wanted, got) {
		return fmt.Errorf("tenant CIDRPool exclusions differ from retained IPAM host reservations")
	}
	return nil
}

func canonicalWorkloadAddressRanges(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	ranges, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("address ranges are not a list")
	}
	result := make([]string, 0, len(ranges))
	for _, raw := range ranges {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("address range is not an object")
		}
		start, startOK := item["startIP"].(string)
		end, endOK := item["endIP"].(string)
		startAddr, startErr := netip.ParseAddr(start)
		endAddr, endErr := netip.ParseAddr(end)
		if !startOK || !endOK || startErr != nil || endErr != nil || !startAddr.Is4() || !endAddr.Is4() || startAddr.Compare(endAddr) != 0 {
			return nil, fmt.Errorf("HBN host reservation is not a canonical singleton IPv4 range")
		}
		result = append(result, startAddr.String()+"/"+endAddr.String())
	}
	sort.Strings(result)
	for i := 1; i < len(result); i++ {
		if result[i] == result[i-1] {
			return nil, fmt.Errorf("duplicate host reservation")
		}
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

func verifySelectedIPAMAllocation(parent *unstructured.Unstructured, tenant controllercontract.TenantReadinessExpected) error {
	if tenant.Name == "" || tenant.Namespace == "" {
		return fmt.Errorf("selected DPUCluster identity is unavailable")
	}
	allocations, found, err := unstructured.NestedSlice(parent.Object, "status", "dpuClusterAllocations")
	if err != nil || !found || len(allocations) != 1 {
		return fmt.Errorf("IPAM does not report one allocation for the selected DPUCluster")
	}
	allocation, ok := allocations[0].(map[string]any)
	cluster, clusterOK := allocation["dpuCluster"].(string)
	if !ok || !clusterOK || cluster != tenant.Namespace+"/"+tenant.Name {
		return fmt.Errorf("IPAM allocation does not identify the selected DPUCluster")
	}
	if _, ok := allocation["ipRanges"].([]any); !ok {
		return fmt.Errorf("IPAM allocation ranges are malformed")
	}
	return nil
}

func verifyCIDRPoolRoutes(network, poolSpec map[string]any) error {
	routes, found, err := unstructured.NestedSlice(network, "routes")
	if err != nil {
		return fmt.Errorf("retained IPAM routes are malformed")
	}
	wanted := make([]any, 0, len(routes))
	if found {
		for _, raw := range routes {
			route, ok := raw.(map[string]any)
			dst, dstOK := route["dst"].(string)
			if !ok || !dstOK {
				return fmt.Errorf("retained IPAM route is incomplete")
			}
			wanted = append(wanted, map[string]any{"dst": dst})
		}
	}
	actual, actualFound, actualErr := unstructured.NestedSlice(poolSpec, "routes")
	if len(wanted) == 0 {
		wanted = nil
	}
	if len(actual) == 0 {
		actual = nil
	}
	if actualErr != nil || len(wanted) > 0 && !actualFound || len(actual) > 0 && !found || !reflect.DeepEqual(wanted, actual) {
		return fmt.Errorf("tenant CIDRPool routes differ from retained IPAM")
	}
	return nil
}

func verifyCIDRPoolStaticAllocations(network, poolSpec map[string]any) error {
	allocations, found, err := unstructured.NestedStringMap(network, "allocations")
	if err != nil {
		return fmt.Errorf("retained IPAM static allocations are malformed")
	}
	wanted := make([]string, 0, len(allocations))
	if found {
		for node, prefix := range allocations {
			wanted = append(wanted, node+"\x00"+prefix)
		}
	}
	actual, actualFound, actualErr := unstructured.NestedSlice(poolSpec, "staticAllocations")
	if actualErr != nil {
		return fmt.Errorf("tenant CIDRPool static allocations are malformed")
	}
	got := make([]string, 0, len(actual))
	if actualFound {
		for _, raw := range actual {
			allocation, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("tenant CIDRPool static allocation is malformed")
			}
			node, nodeOK := allocation["nodeName"].(string)
			prefix, prefixOK := allocation["prefix"].(string)
			if !nodeOK || !prefixOK {
				return fmt.Errorf("tenant CIDRPool static allocation is incomplete")
			}
			got = append(got, node+"\x00"+prefix)
		}
	}
	sort.Strings(wanted)
	sort.Strings(got)
	if !reflect.DeepEqual(wanted, got) {
		return fmt.Errorf("tenant CIDRPool static allocations differ from retained IPAM")
	}
	return nil
}

func compareOptionalNumber(source, target map[string]any, key string) error {
	want, wantPresent := source[key]
	got, gotPresent := target[key]
	if wantPresent != gotPresent {
		return fmt.Errorf("tenant CIDRPool %s differs from retained IPAM", key)
	}
	if wantPresent {
		wantNumber, wantOK := numericInt64(want)
		gotNumber, gotOK := numericInt64(got)
		if !wantOK || !gotOK || wantNumber != gotNumber {
			return fmt.Errorf("tenant CIDRPool %s differs from retained IPAM", key)
		}
	}
	return nil
}

func numericInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int32:
		return int64(typed), true
	case int:
		return int64(typed), true
	case float64:
		return int64(typed), typed == float64(int64(typed))
	default:
		return 0, false
	}
}
