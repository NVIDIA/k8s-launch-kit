// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"fmt"
	"net/netip"
	"reflect"
	"sort"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const (
	hostLeaseDPUNodeNameLabel      = "provisioning.dpu.nvidia.com/dpunode-name"
	hostLeaseDPUNodeNamespaceLabel = "provisioning.dpu.nvidia.com/dpunode-namespace"
)

// hostWorkloadLeaseSnapshot contains fresh tenant API reads. Its caller must
// bind them to the identity-verified tenant client and use a complete Node list.
type hostWorkloadLeaseSnapshot struct {
	DPUServiceIPAM *unstructured.Unstructured
	CIDRPool       *unstructured.Unstructured
	TenantNodes    []unstructured.Unstructured
}

// hostWorkloadLease is an allocation mapping, not evidence that the host has
// configured the reserved address.
type hostWorkloadLease struct {
	Serial              string
	DPUName             string
	DPUUID              types.UID
	DPUNodeName         string
	DPUNodeUID          types.UID
	TenantNodeName      string
	NodeUID             types.UID
	PoolName            string
	PoolUID             types.UID
	Prefix              string
	Gateway             string
	ReservedHostAddress string
}

// resolveHostWorkloadLeases joins already-verified activation identities to
// current tenant Nodes and CIDRPool allocations. It performs no API access and
// cannot establish host address configuration or network readiness.
func resolveHostWorkloadLeases(
	plan *generation.Plan,
	reconciler *Reconciler,
	selected []activationSelectedDevice,
	activation activationObservation,
	workloadDigest string, resources []workloadResourceReceipt,
	snapshot hostWorkloadLeaseSnapshot,
) ([]hostWorkloadLease, error) {
	if plan == nil || reconciler == nil || !activation.complete || len(selected) == 0 {
		return nil, fmt.Errorf("retained plan and complete selected activation are required")
	}
	if workloadDigest != plan.Digest() {
		return nil, fmt.Errorf("workload receipt does not identify the retained plan")
	}
	intent := plan.Intent()
	expectedTenant, _, err := tenantPlanInputs(plan)
	if err != nil {
		return nil, fmt.Errorf("retained tenant identity is invalid")
	}
	desiredIPAM, err := retainedWorkloadIPAM(plan)
	if err != nil {
		return nil, err
	}
	ipamReceipt, poolUID, err := hostLeaseWorkloadReceipt(resources, desiredIPAM)
	if err != nil {
		return nil, err
	}
	ipam := snapshot.DPUServiceIPAM
	if ipam == nil || ipam.GetUID() != ipamReceipt.UID || ipam.GetResourceVersion() == "" || ipam.GetDeletionTimestamp() != nil ||
		reconciler.Verify(desiredIPAM, ipam) != nil || !workloadResourceReadiness(ipam, workloadIPAMReadiness).Ready {
		return nil, fmt.Errorf("retained workload IPAM is missing, replaced, changed or not ready")
	}
	pool := snapshot.CIDRPool
	if pool == nil || pool.GetUID() != poolUID || pool.GetResourceVersion() == "" || pool.GetDeletionTimestamp() != nil {
		return nil, fmt.Errorf("tenant CIDRPool is missing, replaced or deleting")
	}
	if err := verifyWorkloadChild(ipam, pool, expectedTenant); err != nil {
		return nil, fmt.Errorf("tenant CIDRPool differs from retained workload IPAM: %w", err)
	}
	network, prefixSize, gatewayIndex, err := retainedWorkloadNetwork(ipam)
	if err != nil {
		return nil, err
	}
	reservations, err := hbn.HostReservations(intent.Networking, intent.Generation)
	if err != nil {
		return nil, fmt.Errorf("retained host-address reservation is invalid: %w", err)
	}
	allocations, err := hostLeaseAllocations(pool, network, prefixSize, gatewayIndex)
	if err != nil {
		return nil, err
	}
	selectedBySerial := make(map[string]activationSelectedDevice, len(selected))
	for _, item := range selected {
		serial := item.item.device.Serial
		if serial == "" {
			return nil, fmt.Errorf("selected activation contains an empty DPU serial")
		}
		if _, duplicate := selectedBySerial[serial]; duplicate {
			return nil, fmt.Errorf("selected activation contains duplicate DPU serial %q", serial)
		}
		selectedBySerial[serial] = item
	}
	if len(intent.Devices) == 0 || len(selectedBySerial) != len(intent.Devices) {
		return nil, fmt.Errorf("selected activation differs from the retained DPU cohort")
	}
	leases := make([]hostWorkloadLease, 0, len(intent.Devices))
	for _, expected := range intent.Devices {
		item, ok := selectedBySerial[expected.Serial]
		if !ok || !reflect.DeepEqual(item.item.device.SelectorLabels, expected.SelectorLabels) {
			return nil, fmt.Errorf("selected activation does not match retained serial %q", expected.Serial)
		}
		lease, err := hostLeaseForSelected(item, selected, activation, snapshot.TenantNodes, pool, allocations, reservations, expectedTenant, intent.Generation.HostAddressIndex)
		if err != nil {
			return nil, fmt.Errorf("selected serial %q: %w", expected.Serial, err)
		}
		leases = append(leases, lease)
	}
	sort.Slice(leases, func(i, j int) bool { return leases[i].Serial < leases[j].Serial })
	return leases, nil
}

func retainedWorkloadIPAM(plan *generation.Plan) (*unstructured.Unstructured, error) {
	var desired *unstructured.Unstructured
	for _, stage := range plan.Stages() {
		if stage.Stage().ID != generation.StageWorkload {
			continue
		}
		for _, document := range stage.Documents() {
			object := document.ObjectCopy()
			if object == nil || object.GetKind() != "DPUServiceIPAM" || object.GetName() != "hbn-workload" {
				continue
			}
			if desired != nil {
				return nil, fmt.Errorf("retained HBN workload IPAM is ambiguous")
			}
			desired = object
		}
	}
	if desired == nil || desired.GetAPIVersion() != "svc.dpu.nvidia.com/v1alpha1" || desired.GetNamespace() == "" {
		return nil, fmt.Errorf("retained HBN workload IPAM is missing")
	}
	return desired, nil
}

func hostLeaseWorkloadReceipt(resources []workloadResourceReceipt, desired *unstructured.Unstructured) (workloadResourceReceipt, types.UID, error) {
	var receipt workloadResourceReceipt
	for _, resource := range resources {
		if resource.Kind != "DPUServiceIPAM" || resource.Namespace != desired.GetNamespace() || resource.Name != desired.GetName() {
			continue
		}
		if receipt.UID != "" || resource.UID == "" || resource.ChildUID == "" {
			return workloadResourceReceipt{}, "", fmt.Errorf("workload receipt has ambiguous or incomplete host IPAM identity")
		}
		receipt = resource
	}
	if receipt.UID == "" {
		return workloadResourceReceipt{}, "", fmt.Errorf("workload receipt does not bind the selected IPAM and CIDRPool UIDs")
	}
	return receipt, receipt.ChildUID, nil
}

func retainedWorkloadNetwork(ipam *unstructured.Unstructured) (netip.Prefix, int, uint64, error) {
	network, found, err := unstructured.NestedMap(ipam.Object, "spec", "ipv4Network")
	if err != nil || !found {
		return netip.Prefix{}, 0, 0, fmt.Errorf("retained workload IPAM network is malformed")
	}
	cidr, cidrOK := network["network"].(string)
	prefix, prefixOK := numericInt64(network["prefixSize"])
	gateway, gatewayOK := numericInt64(network["gatewayIndex"])
	pool, parseErr := netip.ParsePrefix(cidr)
	if !cidrOK || !prefixOK || !gatewayOK || parseErr != nil || !pool.Addr().Is4() ||
		pool != pool.Masked() || prefix < int64(pool.Bits()) || prefix > 30 || gateway < 1 || gateway > 0xffffffff {
		return netip.Prefix{}, 0, 0, fmt.Errorf("retained workload IPAM CIDR, prefix or gateway setting is invalid")
	}
	return pool, int(prefix), uint64(gateway), nil
}

type hostLeaseAllocation struct {
	prefix  netip.Prefix
	gateway netip.Addr
}

func hostLeaseAllocations(pool *unstructured.Unstructured, expected netip.Prefix, prefixSize int, gatewayIndex uint64) (map[string]hostLeaseAllocation, error) {
	cidr, found, cidrErr := unstructured.NestedString(pool.Object, "spec", "cidr")
	perNode, perNodeFound, perNodeErr := unstructured.NestedFieldNoCopy(pool.Object, "spec", "perNodeNetworkPrefix")
	actualPrefix, parseErr := netip.ParsePrefix(cidr)
	parsedPerNode, perNodeOK := numericInt64(perNode)
	if cidrErr != nil || perNodeErr != nil || !found || !perNodeFound || parseErr != nil || actualPrefix != expected || !perNodeOK || parsedPerNode != int64(prefixSize) {
		return nil, fmt.Errorf("tenant CIDRPool spec differs from the retained workload network")
	}
	rows, found, err := unstructured.NestedSlice(pool.Object, "status", "allocations")
	if err != nil || !found || len(rows) == 0 {
		return nil, fmt.Errorf("tenant CIDRPool has no current allocation status")
	}
	allocations := make(map[string]hostLeaseAllocation, len(rows))
	prefixes := make([]netip.Prefix, 0, len(rows))
	for _, raw := range rows {
		row, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tenant CIDRPool allocation is malformed")
		}
		name, nameOK := row["nodeName"].(string)
		prefixText, prefixOK := row["prefix"].(string)
		gatewayText, gatewayOK := row["gateway"].(string)
		prefix, prefixErr := netip.ParsePrefix(prefixText)
		gateway, gatewayErr := netip.ParseAddr(gatewayText)
		if !nameOK || name == "" || !prefixOK || !gatewayOK || prefixErr != nil || gatewayErr != nil || !prefix.Addr().Is4() ||
			prefix != prefix.Masked() || prefix.Bits() != prefixSize || !expected.Contains(prefix.Addr()) || !expected.Contains(prefixLastAddress(prefix)) ||
			!gateway.Is4() || !prefix.Contains(gateway) {
			return nil, fmt.Errorf("tenant CIDRPool contains an invalid or out-of-pool allocation")
		}
		if _, duplicate := allocations[name]; duplicate {
			return nil, fmt.Errorf("tenant CIDRPool has duplicate allocations for Node %q", name)
		}
		wantGateway, err := indexedIPv4Address(prefix, gatewayIndex)
		if err != nil || gateway != wantGateway {
			return nil, fmt.Errorf("tenant CIDRPool gateway differs from retained workload intent")
		}
		allocations[name] = hostLeaseAllocation{prefix: prefix, gateway: gateway}
		prefixes = append(prefixes, prefix)
	}
	sort.Slice(prefixes, func(i, j int) bool { return prefixes[i].Addr().Less(prefixes[j].Addr()) })
	for i := 1; i < len(prefixes); i++ {
		if !prefixLastAddress(prefixes[i-1]).Less(prefixes[i].Addr()) {
			return nil, fmt.Errorf("tenant CIDRPool allocations overlap")
		}
	}
	return allocations, nil
}

func hostLeaseForSelected(item activationSelectedDevice, cohort []activationSelectedDevice, activation activationObservation,
	tenantNodes []unstructured.Unstructured, pool *unstructured.Unstructured, allocations map[string]hostLeaseAllocation, reservations []hbn.AddressRange, expectedTenant TenantReadinessExpected, hostIndex int,
) (hostWorkloadLease, error) {
	if item.dpuSet == nil || hostIndex <= 0 {
		return hostWorkloadLease{}, fmt.Errorf("selected DPUSet or retained host address index is invalid")
	}
	dpu := activation.dpus[item.dpuName]
	set := activation.sets[item.dpuSet.GetName()]
	node := activation.nodes[item.nodeName]
	if dpu == nil || set == nil || node == nil || dpu.GetUID() == "" || set.GetUID() == "" || node.GetUID() != item.receipt.DPUNodeUID || node.GetDeletionTimestamp() != nil {
		return hostWorkloadLease{}, fmt.Errorf("activation no longer contains the selected DPU, DPUSet and DPUNode identities")
	}
	if !activationDPUMatches(dpu, item) || !activationDPUSetMatches(set, item) || !activationOwnerMatches(dpu, "provisioning.dpu.nvidia.com/v1alpha1", "DPUSet", set.GetName(), set.GetUID()) {
		return hostWorkloadLease{}, fmt.Errorf("selected DPU ownership or device identity differs from activation")
	}
	clusterName, clusterNameFound, clusterNameErr := unstructured.NestedString(dpu.Object, "spec", "cluster", "name")
	clusterNamespace, clusterNamespaceFound, clusterNamespaceErr := unstructured.NestedString(dpu.Object, "spec", "cluster", "namespace")
	if clusterNameErr != nil || clusterNamespaceErr != nil || !clusterNameFound || !clusterNamespaceFound || clusterName != expectedTenant.Name || clusterNamespace != expectedTenant.Namespace {
		return hostWorkloadLease{}, fmt.Errorf("selected DPU cluster reference differs from retained tenant identity")
	}
	if err := validateActivationDPUNode(node, item, cohort); err != nil || !enrolledNodeReadiness(node).Ready {
		return hostWorkloadLease{}, fmt.Errorf("selected DPUNode identity or readiness differs from activation")
	}
	tenantNode, err := uniqueTenantNode(tenantNodes, item.nodeName, node.GetNamespace())
	if err != nil {
		return hostWorkloadLease{}, err
	}
	allocation, found := allocations[tenantNode.GetName()]
	if !found {
		return hostWorkloadLease{}, fmt.Errorf("CIDRPool has no allocation for selected tenant Node %q", tenantNode.GetName())
	}
	address, err := indexedIPv4Address(allocation.prefix, uint64(hostIndex))
	if err != nil || address == allocation.gateway || !hostAddressReserved(address, reservations) {
		return hostWorkloadLease{}, fmt.Errorf("selected tenant Node host address is invalid or not reserved")
	}
	return hostWorkloadLease{Serial: item.item.device.Serial, DPUName: dpu.GetName(), DPUUID: dpu.GetUID(), DPUNodeName: node.GetName(), DPUNodeUID: node.GetUID(),
		TenantNodeName: tenantNode.GetName(), NodeUID: tenantNode.GetUID(), PoolName: pool.GetName(), PoolUID: pool.GetUID(),
		Prefix: allocation.prefix.String(), Gateway: allocation.gateway.String(), ReservedHostAddress: address.String()}, nil
}

func uniqueTenantNode(nodes []unstructured.Unstructured, dpuNodeName, dpuNodeNamespace string) (*unstructured.Unstructured, error) {
	var match *unstructured.Unstructured
	for i := range nodes {
		node := &nodes[i]
		if node.GetAPIVersion() != "v1" || node.GetKind() != "Node" || node.GetNamespace() != "" || node.GetName() == "" {
			return nil, fmt.Errorf("tenant Node observation has an invalid identity")
		}
		labels := node.GetLabels()
		if labels[hostLeaseDPUNodeNameLabel] != dpuNodeName || labels[hostLeaseDPUNodeNamespaceLabel] != dpuNodeNamespace {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("DPUNode identity matches multiple tenant Nodes")
		}
		match = node
	}
	if match == nil || match.GetUID() == "" || match.GetResourceVersion() == "" || match.GetDeletionTimestamp() != nil || !EvaluateReadiness(match).Ready {
		return nil, fmt.Errorf("selected DPUNode has no current ready tenant Node")
	}
	return match, nil
}

func hostAddressReserved(address netip.Addr, reservations []hbn.AddressRange) bool {
	for _, reservation := range reservations {
		if reservation.StartIP == address.String() && reservation.EndIP == address.String() {
			return true
		}
	}
	return false
}

func indexedIPv4Address(prefix netip.Prefix, index uint64) (netip.Addr, error) {
	if !prefix.IsValid() || !prefix.Addr().Is4() || prefix.Bits() > 30 {
		return netip.Addr{}, fmt.Errorf("allocation prefix has no usable IPv4 host space")
	}
	capacity := uint64(1) << uint(32-prefix.Bits())
	if index == 0 || index >= capacity-1 {
		return netip.Addr{}, fmt.Errorf("address index is outside usable IPv4 host space")
	}
	base := prefix.Addr().As4()
	value := uint64(base[0])<<24 | uint64(base[1])<<16 | uint64(base[2])<<8 | uint64(base[3])
	value += index
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}), nil
}

func prefixLastAddress(prefix netip.Prefix) netip.Addr {
	base := prefix.Addr().As4()
	value := uint64(base[0])<<24 | uint64(base[1])<<16 | uint64(base[2])<<8 | uint64(base[3])
	value += (uint64(1) << uint(32-prefix.Bits())) - 1
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)})
}
