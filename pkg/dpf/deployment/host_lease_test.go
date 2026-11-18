// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestResolveHostWorkloadLeases(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	input := makeHostLeaseResolverFixture(t, fixture)
	before := cloneHostLeaseResolverFixture(input)

	leases, err := resolveHostWorkloadLeases(input.plan, input.reconciler, input.selected, input.activation, input.workload.BundleDigest, input.workload.Resources, input.snapshot)
	if err != nil {
		t.Fatalf("resolveHostWorkloadLeases(): %v", err)
	}
	if len(leases) != len(input.selected) {
		t.Fatalf("got %d leases for %d selected DPUs", len(leases), len(input.selected))
	}
	for i, lease := range leases {
		if lease.Serial == "" || lease.DPUUID == "" || lease.DPUNodeUID == "" || lease.NodeUID == "" || lease.PoolUID == "" {
			t.Fatalf("lease %d lacks a bound identity: %+v", i, lease)
		}
		prefix, err := netip.ParsePrefix(lease.Prefix)
		if err != nil || !prefix.Contains(netip.MustParseAddr(lease.Gateway)) || !prefix.Contains(netip.MustParseAddr(lease.ReservedHostAddress)) || lease.Gateway == lease.ReservedHostAddress {
			t.Fatalf("lease %d has invalid prefix/gateway/host mapping: %+v", i, lease)
		}
	}
	if !reflect.DeepEqual(input, before) {
		t.Fatal("resolver mutated retained or observed inputs")
	}

	for _, test := range []struct {
		name   string
		mutate func(*hostLeaseResolverFixture)
	}{
		{name: "changed cluster reference", mutate: func(f *hostLeaseResolverFixture) {
			dpu := f.activation.dpus[f.selected[0].dpuName]
			_ = unstructured.SetNestedField(dpu.Object, "other-tenant", "spec", "cluster", "name")
		}},
		{name: "replaced pool", mutate: func(f *hostLeaseResolverFixture) { f.snapshot.CIDRPool.SetUID("replacement") }},
		{name: "duplicate tenant node", mutate: func(f *hostLeaseResolverFixture) {
			duplicate := *f.snapshot.TenantNodes[0].DeepCopy()
			duplicate.SetName("duplicate-node")
			duplicate.SetUID("duplicate-node-uid")
			f.snapshot.TenantNodes = append(f.snapshot.TenantNodes, duplicate)
		}},
		{name: "overlapping pool allocations", mutate: func(f *hostLeaseResolverFixture) {
			rows, _, _ := unstructured.NestedSlice(f.snapshot.CIDRPool.Object, "status", "allocations")
			duplicate := runtime.DeepCopyJSONValue(rows[0]).(map[string]any)
			duplicate["nodeName"] = "other-node"
			rows = append(rows, duplicate)
			_ = unstructured.SetNestedSlice(f.snapshot.CIDRPool.Object, rows, "status", "allocations")
		}},
		{name: "wrong tenant allocation", mutate: func(f *hostLeaseResolverFixture) {
			rows, _, _ := unstructured.NestedSlice(f.snapshot.CIDRPool.Object, "status", "allocations")
			rows[0].(map[string]any)["nodeName"] = "not-selected"
			_ = unstructured.SetNestedSlice(f.snapshot.CIDRPool.Object, rows, "status", "allocations")
		}},
		{name: "wrong gateway", mutate: func(f *hostLeaseResolverFixture) {
			rows, _, _ := unstructured.NestedSlice(f.snapshot.CIDRPool.Object, "status", "allocations")
			rows[0].(map[string]any)["gateway"] = "192.0.2.254"
			_ = unstructured.SetNestedSlice(f.snapshot.CIDRPool.Object, rows, "status", "allocations")
		}},
		{name: "tenant Node association changed", mutate: func(f *hostLeaseResolverFixture) {
			labels := f.snapshot.TenantNodes[0].GetLabels()
			labels[hostLeaseDPUNodeNamespaceLabel] = "other-namespace"
			f.snapshot.TenantNodes[0].SetLabels(labels)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := cloneHostLeaseResolverFixture(input)
			test.mutate(&copy)
			if _, err := resolveHostWorkloadLeases(copy.plan, copy.reconciler, copy.selected, copy.activation, copy.workload.BundleDigest, copy.workload.Resources, copy.snapshot); err == nil {
				t.Fatal("resolver accepted an inconsistent identity or allocation")
			}
		})
	}
}

type hostLeaseResolverFixture struct {
	plan       *generation.Plan
	reconciler *Reconciler
	selected   []activationSelectedDevice
	activation activationObservation
	workload   workloadPreparationReceipt
	snapshot   hostWorkloadLeaseSnapshot
}

func makeHostLeaseResolverFixture(t *testing.T, fixture activationIntegrationFixture) hostLeaseResolverFixture {
	t.Helper()
	plan := fixture.prepared.execution.plan
	expectedTenant, _, err := tenantPlanInputs(plan)
	if err != nil {
		t.Fatal(err)
	}
	desiredDeployment, err := retainedDeploymentObject(fixture.prepared.execution.verificationInputs())
	if err != nil {
		t.Fatal(err)
	}
	desiredDeployment.SetUID(activationIntegrationDeploymentUID)
	fixture.client.deployment = desiredDeployment.DeepCopy()
	fixture.client.seedActivationChildren(desiredDeployment)

	activation := activationObservation{sets: map[string]*unstructured.Unstructured{}, dpus: map[string]*unstructured.Unstructured{}, nodes: map[string]*unstructured.Unstructured{}, complete: true}
	for _, selected := range fixture.client.activationSelected {
		for i := range fixture.client.dpus {
			dpu := fixture.client.dpus[i].DeepCopy()
			if err := unstructured.SetNestedMap(dpu.Object, map[string]any{"name": expectedTenant.Name, "namespace": expectedTenant.Namespace}, "spec", "cluster"); err != nil {
				t.Fatal(err)
			}
			if dpu.GetName() == selected.dpuName {
				activation.dpus[selected.dpuName] = dpu
			}
		}
		for i := range fixture.client.sets {
			set := fixture.client.sets[i].DeepCopy()
			if set.GetName() == selected.dpuSet.GetName() {
				activation.sets[set.GetName()] = set
			}
		}
		node := fixture.client.base.resources["DPUNode/"+selected.dpuSet.GetNamespace()+"/"+selected.nodeName].DeepCopy()
		if node == nil {
			t.Fatalf("fixture DPUNode %q is missing", selected.nodeName)
		}
		_ = unstructured.SetNestedSlice(node.Object, []any{conditionMap("Ready", "True", "Success", 1)}, "status", "conditions")
		activation.nodes[selected.nodeName] = node
	}

	desiredIPAM, err := retainedWorkloadIPAM(plan)
	if err != nil {
		t.Fatal(err)
	}
	currentIPAM, err := fixture.client.base.Get(fixture.ctx, desiredIPAM)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := fixture.client.workloadChild(fixture.ctx, fixture.client.tenant, currentIPAM)
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(pool.Object, hostLeaseAllocationRows(t, plan, fixture.client.activationSelected), "status", "allocations"); err != nil {
		t.Fatal(err)
	}
	for _, resource := range fixture.workload.Resources {
		if resource.Kind == "DPUServiceIPAM" && resource.Name == currentIPAM.GetName() {
			pool.SetUID(resource.ChildUID)
			pool.SetResourceVersion("12")
		}
	}
	nodes := make([]unstructured.Unstructured, 0, len(fixture.client.activationSelected))
	for i, selected := range fixture.client.activationSelected {
		node := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Node",
			"metadata": map[string]any{"name": "tenant-node-" + string(rune('a'+i)), "uid": "tenant-node-uid-" + string(rune('a'+i)), "resourceVersion": "7",
				"labels": map[string]any{hostLeaseDPUNodeNameLabel: selected.nodeName, hostLeaseDPUNodeNamespaceLabel: selected.dpuSet.GetNamespace()}},
			"status": map[string]any{"conditions": []any{conditionMap("Ready", "True", "Success", 1)}},
		}}
		nodes = append(nodes, *node)
	}
	return hostLeaseResolverFixture{plan: plan, reconciler: fixture.prepared.execution.reconciler, selected: fixture.client.activationSelected,
		activation: activation, workload: fixture.workload, snapshot: hostWorkloadLeaseSnapshot{DPUServiceIPAM: currentIPAM, CIDRPool: pool, TenantNodes: nodes}}
}

func hostLeaseAllocationRows(t *testing.T, plan *generation.Plan, selected []activationSelectedDevice) []any {
	t.Helper()
	desired, err := retainedWorkloadIPAM(plan)
	if err != nil {
		t.Fatal(err)
	}
	pool, prefixSize, gatewayIndex, err := retainedWorkloadNetwork(desired)
	if err != nil {
		t.Fatal(err)
	}
	base := pool.Addr().As4()
	value := uint64(base[0])<<24 | uint64(base[1])<<16 | uint64(base[2])<<8 | uint64(base[3])
	block := uint64(1) << uint(32-prefixSize)
	rows := make([]any, 0, len(selected))
	for i := range selected {
		addressValue := value + uint64(i)*block
		prefix := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(addressValue >> 24), byte(addressValue >> 16), byte(addressValue >> 8), byte(addressValue)}), prefixSize)
		gateway, err := indexedIPv4Address(prefix, gatewayIndex)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, map[string]any{"nodeName": "tenant-node-" + string(rune('a'+i)), "prefix": prefix.String(), "gateway": gateway.String()})
	}
	return rows
}

func cloneHostLeaseResolverFixture(input hostLeaseResolverFixture) hostLeaseResolverFixture {
	copy := input
	copy.selected = append([]activationSelectedDevice(nil), input.selected...)
	copy.activation = activationObservation{sets: map[string]*unstructured.Unstructured{}, dpus: map[string]*unstructured.Unstructured{}, nodes: map[string]*unstructured.Unstructured{}, complete: input.activation.complete}
	for key, object := range input.activation.sets {
		copy.activation.sets[key] = object.DeepCopy()
	}
	for key, object := range input.activation.dpus {
		copy.activation.dpus[key] = object.DeepCopy()
	}
	for key, object := range input.activation.nodes {
		copy.activation.nodes[key] = object.DeepCopy()
	}
	copy.snapshot.DPUServiceIPAM = input.snapshot.DPUServiceIPAM.DeepCopy()
	copy.snapshot.CIDRPool = input.snapshot.CIDRPool.DeepCopy()
	copy.snapshot.TenantNodes = make([]unstructured.Unstructured, len(input.snapshot.TenantNodes))
	for i := range input.snapshot.TenantNodes {
		copy.snapshot.TenantNodes[i] = *input.snapshot.TenantNodes[i].DeepCopy()
	}
	copy.workload.Resources = append([]workloadResourceReceipt(nil), input.workload.Resources...)
	return copy
}
