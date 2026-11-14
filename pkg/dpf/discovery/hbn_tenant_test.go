// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
	extensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

func TestTenantClusterCheckSelectsReadyCrossNamespaceTarget(t *testing.T) {
	target := resolvedTenantTarget(t, "existing-tenant", "existing-tenant", "192.0.2.20")
	cluster := observedTenantCluster(target, "cluster-uid", true)
	snapshot := tenantSnapshot(target, managementprobe.CollectionComplete, cluster)
	check := tenantClusterCheck(&snapshot, &target)
	if check.State != "passed" || check.ReasonCode != "TENANT_TARGET_MATCHES" {
		t.Fatalf("matching current cross-namespace tenant check=%#v", check)
	}
	if check.RequiredPhase != "deploy" {
		t.Fatalf("observed tenant readiness is required in phase %q, want deploy", check.RequiredPhase)
	}
}

func TestPinnedDPUClusterStatusWithoutObservedGenerationClassifiesTargetOnly(t *testing.T) {
	input, inv, target := hbnBootstrapNetworkFixture()
	crdData, err := os.ReadFile(filepath.Join("..", "bootstrap", "testdata", "dpuclusters.provisioning.dpu.nvidia.com.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd extensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(crdData, &crd); err != nil {
		t.Fatal(err)
	}
	labels := map[string]any{}
	for key, value := range target.Tenant.Labels {
		labels[key] = value
	}
	cluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUCluster",
		"metadata": map[string]any{"name": target.Tenant.Name, "namespace": target.Tenant.Namespace, "uid": "tenant-uid", "generation": int64(3), "resourceVersion": "8", "labels": labels},
		"spec": map[string]any{"type": "kamaji", "clusterEndpoint": map[string]any{"keepalived": map[string]any{
			"vip": target.Tenant.VIP, "interface": target.Tenant.Interface, "virtualRouterID": int64(target.Tenant.RouterID),
		}}},
		// DPF 26.8 writes phase and a metav1 Ready condition but no observedGeneration.
		"status": map[string]any{"phase": "Ready", "conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "HealthCheckPassed"}}},
	}}
	cluster.SetGroupVersionKind(schema.GroupVersionKind{Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1", Kind: "DPUCluster"})
	listKinds := map[schema.GroupVersionResource]string{{Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1", Resource: "dpuclusters"}: "DPUClusterList"}
	dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, cluster)
	collector := managementprobe.Collector{
		Kubernetes: kubernetesfake.NewSimpleClientset(), Dynamic: dynamic,
		Extensions: extensionsfake.NewClientset(&crd),
	}
	snapshot := collector.Collect(context.Background(), target.OperatorNamespace)
	if snapshot.DPUClusters.State != managementprobe.CollectionComplete || len(snapshot.DPUClusters.Items) != 1 {
		t.Fatalf("collected DPUCluster state/items = %s/%d", snapshot.DPUClusters.State, len(snapshot.DPUClusters.Items))
	}
	observed := snapshot.DPUClusters.Items[0]
	if observed.StatusFreshness != "unknown" || len(observed.Conditions) != 1 || observed.Conditions[0].ObservedGeneration != nil {
		t.Fatalf("pinned status shape was fabricated as generation-current: %#v", observed)
	}
	if got := tenantClusterCheck(&snapshot, &target); got.State != "passed" || got.ReasonCode != "TENANT_TARGET_MATCHES" || !strings.Contains(got.Message, "deployment must verify") {
		t.Fatalf("matching target classification=%#v", got)
	}
	checks, _ := hbnBootstrapChecks(input, target.OperatorNamespace, inv, &snapshot)
	if got := findCheck(checks, "dpf.hbn.management.tenant-network"); got.State != "unknown" || got.ReasonCode != "TENANT_EXISTING_NETWORK_UNVERIFIED" {
		t.Fatalf("identity classification incorrectly proved endpoint readiness: %#v", got)
	}
}

func TestCollectedConflictingTenantAliasesBlockSelectedTarget(t *testing.T) {
	target := resolvedTenantTarget(t, "tenant-a", "tenant-a", "192.0.2.20")
	crdData, err := os.ReadFile(filepath.Join("..", "bootstrap", "testdata", "dpuclusters.provisioning.dpu.nvidia.com.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd extensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(crdData, &crd); err != nil {
		t.Fatal(err)
	}
	cluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUCluster",
		"metadata": map[string]any{"name": target.Tenant.Name, "namespace": target.Tenant.Namespace, "uid": "tenant-uid", "generation": int64(3), "resourceVersion": "8", "labels": map[string]any{
			"nvidia.kubernetes-launch-kit.dpf-tenant": "other-tenant",
			"launch-kit.nvidia.com/dpf-tenant":        "tenant-a",
		}},
		"spec":   map[string]any{"type": "kamaji", "clusterEndpoint": map[string]any{"keepalived": map[string]any{"vip": "192.0.2.20", "interface": "eth0", "virtualRouterID": int64(126)}}},
		"status": map[string]any{"phase": "Ready", "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(3)}}},
	}}
	cluster.SetGroupVersionKind(schema.GroupVersionKind{Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1", Kind: "DPUCluster"})
	listKinds := map[schema.GroupVersionResource]string{{Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1", Resource: "dpuclusters"}: "DPUClusterList"}
	dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, cluster)
	collector := managementprobe.Collector{
		Kubernetes: kubernetesfake.NewSimpleClientset(), Dynamic: dynamic,
		Extensions: extensionsfake.NewClientset(&crd),
	}
	snapshot := collector.Collect(context.Background(), target.OperatorNamespace)
	if snapshot.DPUClusters.State != managementprobe.CollectionComplete || len(snapshot.DPUClusters.Items) != 1 {
		t.Fatalf("collected DPUCluster state/items = %s/%d", snapshot.DPUClusters.State, len(snapshot.DPUClusters.Items))
	}
	if !snapshot.DPUClusters.Items[0].TenantIdentityConflict {
		t.Fatal("collector did not preserve conflicting tenant alias fact")
	}
	foundIssue := false
	for _, issue := range snapshot.Issues {
		if issue.Code == "TENANT_LABEL_CONFLICT" {
			foundIssue = issue.Namespace == target.Tenant.Namespace && issue.Resource == target.Tenant.Name
		}
	}
	if !foundIssue {
		t.Fatalf("collector did not attach the conflict issue to its namespaced object: %#v", snapshot.Issues)
	}
	publicIssues := projectTenantIssues(&snapshot, tenantClusterProjection(&snapshot, &target))
	if len(publicIssues) != 1 || publicIssues[0].Namespace != target.Tenant.Namespace || publicIssues[0].Resource != target.Tenant.Name {
		t.Fatalf("selected conflict issue was not projected with object identity: %#v", publicIssues)
	}
	check := tenantClusterCheck(&snapshot, &target)
	if check.State != "failed" || check.ReasonCode != "TENANT_IDENTITY_CONFLICT" {
		t.Fatalf("collector conflict did not block target policy: %#v", check)
	}
}

func TestTenantClusterCheckRejectsDesiredVIPOwnedByDifferentCluster(t *testing.T) {
	target := resolvedTenantTarget(t, "new-tenant", "new-namespace", "192.0.2.20")
	otherTarget := resolvedTenantTarget(t, "existing-tenant", "existing-namespace", "192.0.2.20")
	cluster := observedTenantCluster(otherTarget, "existing-uid", true)
	snapshot := tenantSnapshot(target, managementprobe.CollectionComplete, cluster)
	check := tenantClusterCheck(&snapshot, &target)
	if check.State != "failed" || check.ReasonCode != "TENANT_VIP_CONFLICT" || !strings.Contains(check.Message, "existing-namespace/existing-tenant") {
		t.Fatalf("existing cross-namespace VIP conflict check=%#v", check)
	}

	cluster.VIP = stringPointer("::ffff:192.0.2.20")
	snapshot = tenantSnapshot(target, managementprobe.CollectionComplete, cluster)
	check = tenantClusterCheck(&snapshot, &target)
	if check.State != "failed" || check.ReasonCode != "TENANT_VIP_CONFLICT" {
		t.Fatalf("IPv4-mapped VIP representation was not recognized as a conflict: %#v", check)
	}
}

func TestTenantClusterCheckAllowsUnrelatedTenantAndDoesNotInferVRIDCollision(t *testing.T) {
	target := resolvedTenantTarget(t, "new-tenant", "new-namespace", "192.0.2.20")
	otherTarget := resolvedTenantTarget(t, "existing-tenant", "existing-namespace", "192.0.2.21")
	cluster := observedTenantCluster(otherTarget, "existing-uid", true)
	// Reusing a VRID and interface name in a different tenant is not a collision
	// proof without evidence that both endpoints share the same L2 scope.
	snapshot := tenantSnapshot(target, managementprobe.CollectionComplete, cluster)
	check := tenantClusterCheck(&snapshot, &target)
	if check.State != "passed" || check.ReasonCode != "TENANT_TARGET_ABSENT_FROM_DPUCLUSTERS" {
		t.Fatalf("unrelated tenant blocked selected target: %#v", check)
	}
}

func TestTenantClusterCheckDoesNotAdoptMatchingNameWithoutIdentity(t *testing.T) {
	target := resolvedTenantTarget(t, "tenant-a", "tenant-ns", "192.0.2.20")
	cluster := observedTenantCluster(target, "tenant-uid", true)
	cluster.LaunchKitTenantLabelKey, cluster.LaunchKitTenantLabelValue = nil, nil
	snapshot := tenantSnapshot(target, managementprobe.CollectionComplete, cluster)
	check := tenantClusterCheck(&snapshot, &target)
	if check.State != "unknown" || check.ReasonCode != "TENANT_TARGET_OWNERSHIP_UNKNOWN" {
		t.Fatalf("same-name unowned resource was adopted: %#v", check)
	}
	cluster = observedTenantCluster(target, "tenant-uid", true)
	wrongOwner := "different-tenant"
	cluster.LaunchKitTenantLabelValue = &wrongOwner
	snapshot = tenantSnapshot(target, managementprobe.CollectionComplete, cluster)
	check = tenantClusterCheck(&snapshot, &target)
	if check.State != "failed" || check.ReasonCode != "TENANT_TARGET_IDENTITY_MISMATCH" {
		t.Fatalf("mismatched tenant ownership marker was not rejected: %#v", check)
	}
}

func TestTenantClusterTargetClassificationDoesNotClaimReadiness(t *testing.T) {
	input, inv, target := hbnBootstrapNetworkFixture()
	for _, scenario := range []struct {
		name   string
		mutate func(*managementprobe.DPUCluster)
	}{
		{name: "stale non-ready status", mutate: func(cluster *managementprobe.DPUCluster) {
			phase := "NotReady"
			cluster.Phase = &phase
			cluster.StatusFreshness = "stale"
			cluster.Conditions = []managementprobe.Condition{{Type: "Ready", Status: "False", ObservedGeneration: int64Pointer(cluster.Generation - 1)}}
		}},
		{name: "missing status", mutate: func(cluster *managementprobe.DPUCluster) {
			cluster.Phase = nil
			cluster.StatusFreshness = "unknown"
			cluster.Conditions = nil
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			cluster := observedTenantCluster(target, "tenant-uid", true)
			scenario.mutate(&cluster)
			snapshot := tenantSnapshot(target, managementprobe.CollectionComplete, cluster)
			if got := tenantClusterCheck(&snapshot, &target); got.State != "passed" || got.ReasonCode != "TENANT_TARGET_MATCHES" || !strings.Contains(got.Message, "This is registry binding only") {
				t.Fatalf("target classification was conflated with controller readiness: %#v", got)
			}
			checks, _ := hbnBootstrapChecks(input, target.OperatorNamespace, inv, &snapshot)
			if got := findCheck(checks, "dpf.hbn.management.tenant-network"); got.State != "unknown" || got.ReasonCode != "TENANT_EXISTING_NETWORK_UNVERIFIED" {
				t.Fatalf("existing tenant received an endpoint readiness pass: %#v", got)
			}
		})
	}
}

func TestTenantClusterCheckNeedsVIPAndCompleteCollection(t *testing.T) {
	target := resolvedTenantTarget(t, "tenant-a", "tenant-ns", "")
	snapshot := tenantSnapshot(target, managementprobe.CollectionComplete)
	check := tenantClusterCheck(&snapshot, &target)
	if check.State != "unknown" || check.ReasonCode != "TENANT_VIP_UNRESOLVED" {
		t.Fatalf("missing desired VIP check=%#v", check)
	}

	target = resolvedTenantTarget(t, "tenant-a", "tenant-ns", "192.0.2.20")
	other := observedTenantCluster(resolvedTenantTarget(t, "tenant-b", "other-ns", "192.0.2.21"), "other-uid", true)
	snapshot = tenantSnapshot(target, managementprobe.CollectionIncomplete, other)
	check = tenantClusterCheck(&snapshot, &target)
	if check.State != "unknown" || check.ReasonCode != "TENANT_CLUSTERS_UNOBSERVED" {
		t.Fatalf("incomplete cross-namespace collection passed: %#v", check)
	}
	other.VIP = nil
	snapshot = tenantSnapshot(target, managementprobe.CollectionComplete, other)
	check = tenantClusterCheck(&snapshot, &target)
	if check.State != "unknown" || check.ReasonCode != "TENANT_ENDPOINT_UNOBSERVED" {
		t.Fatalf("complete collection with an unreported peer VIP passed: %#v", check)
	}
	malformedVIP := "not-an-ip"
	other.VIP = &malformedVIP
	snapshot = tenantSnapshot(target, managementprobe.CollectionComplete, other)
	check = tenantClusterCheck(&snapshot, &target)
	if check.State != "unknown" || check.ReasonCode != "TENANT_ENDPOINT_UNOBSERVED" {
		t.Fatalf("malformed peer VIP was treated as collision-free: %#v", check)
	}
}

func TestTenantClusterCheckReportsKnownVIPConflictEvenWhenCollectionIncomplete(t *testing.T) {
	target := resolvedTenantTarget(t, "tenant-a", "tenant-ns", "192.0.2.20")
	other := observedTenantCluster(resolvedTenantTarget(t, "tenant-b", "other-ns", "192.0.2.20"), "other-uid", true)
	snapshot := tenantSnapshot(target, managementprobe.CollectionIncomplete, other)
	check := tenantClusterCheck(&snapshot, &target)
	if check.State != "failed" || check.ReasonCode != "TENANT_VIP_CONFLICT" {
		t.Fatalf("observed conflict should remain actionable despite incomplete listing: %#v", check)
	}
}

func TestHBNBootstrapChecksDoesNotMutateConfigInventoryOrSnapshot(t *testing.T) {
	vip, iface := "192.0.2.20", "enp1s0"
	options := &config.Provisioning{Bootstrap: &config.BootstrapOptions{Tenant: config.TenantOptions{VIP: &vip, Interface: &iface}}}
	serial, endpoint := "DPU-A", "https://192.0.2.30"
	inv := &inventory.Inventory{Management: &inventory.Management{APIEndpoint: "https://192.0.2.10:6443"}, DPUs: []inventory.Device{{SerialNumber: serial, BMCEndpoint: endpoint}}}
	target := resolvedTenantTarget(t, "tenant-a", "tenant-ns", vip)
	snapshot := tenantSnapshot(target, managementprobe.CollectionComplete)
	beforeNamespace, beforeState := snapshot.Namespace, snapshot.DPUClusters.State
	checks, projected := hbnBootstrapChecks(options, "dpf-operator-system", inv, &snapshot)
	if len(checks) == 0 {
		t.Fatal("bootstrap checks were empty")
	}
	if len(projected) != 0 {
		t.Fatalf("empty complete tenant list produced public tenant rows: %#v", projected)
	}
	if *options.Bootstrap.Tenant.VIP != vip || *options.Bootstrap.Tenant.Interface != iface || inv.DPUs[0].SerialNumber != serial || inv.DPUs[0].BMCEndpoint != endpoint {
		t.Fatal("bootstrap checks mutated user config or inventory")
	}
	if snapshot.Namespace != beforeNamespace || snapshot.DPUClusters.State != beforeState {
		t.Fatal("bootstrap checks mutated the management snapshot")
	}
}

func TestTenantProjectionKeepsOnlySelectedConflictingAndUnobservableEndpoints(t *testing.T) {
	target := resolvedTenantTarget(t, "target-tenant", "target-ns", "192.0.2.50")
	selected := observedTenantCluster(target, "selected-uid", true)
	collision := observedTenantCluster(resolvedTenantTarget(t, "conflict", "other-ns", "192.0.2.50"), "conflict-uid", true)
	unrelated := observedTenantCluster(resolvedTenantTarget(t, "unrelated", "third-ns", "192.0.2.51"), "unrelated-uid", true)
	unknown := observedTenantCluster(resolvedTenantTarget(t, "unknown", "fourth-ns", "192.0.2.52"), "unknown-uid", true)
	unknown.VIP = nil
	collision.VIP = stringPointer("::ffff:192.0.2.50")
	snapshot := tenantSnapshot(target, managementprobe.CollectionComplete, unrelated, unknown, collision, selected)
	projection := tenantClusterProjection(&snapshot, &target)
	if len(projection) != 3 {
		t.Fatalf("tenant projection has %d rows, want selected + conflict + unknown: %#v", len(projection), projection)
	}
	byIdentity := make(map[string]inventory.HBNManagementCluster, len(projection))
	for _, cluster := range projection {
		byIdentity[cluster.Namespace+"/"+cluster.Name] = cluster
	}
	if _, ok := byIdentity["third-ns/unrelated"]; ok {
		t.Fatalf("unrelated tenant was persisted: %#v", projection)
	}
	got := byIdentity["target-ns/target-tenant"]
	if got.UID != "selected-uid" || got.ResourceVersion != "42" || got.Interface != target.Tenant.Interface || got.LaunchKitTenantLabelValue == "" {
		t.Fatalf("selected target lost identity or endpoint facts: %#v", got)
	}
	if got.VirtualRouterID == nil {
		t.Fatal("selected target projection lost VRID")
	}
	*got.VirtualRouterID = 1
	if selected.VirtualRouterID == nil || *selected.VirtualRouterID != int64(target.Tenant.RouterID) {
		t.Fatal("mutating public tenant projection changed the source snapshot")
	}
	got = byIdentity["other-ns/conflict"]
	if got.VIP != "::ffff:192.0.2.50" || got.UID != "" || got.Interface != "" || got.LaunchKitTenantLabelValue != "" || len(got.Owners) != 0 {
		t.Fatalf("known conflict projection retained unnecessary tenant fields: %#v", got)
	}
	got = byIdentity["fourth-ns/unknown"]
	if got.UID != "" || got.VIP != "" || got.Interface != "" || got.LaunchKitTenantLabelValue != "" || len(got.Owners) != 0 {
		t.Fatalf("unknown endpoint projection retained unrelated facts: %#v", got)
	}
	if projected := tenantClusterProjection(&snapshot, nil); len(projected) != 0 {
		t.Fatalf("missing target leaked cross-namespace tenants: %#v", projected)
	}
	wrongNamespace := snapshot
	wrongNamespace.Namespace = "wrong-operator-namespace"
	if projected := tenantClusterProjection(&wrongNamespace, &target); len(projected) != 0 {
		t.Fatalf("wrong-namespace snapshot leaked tenant facts: %#v", projected)
	}
}

func TestUnrelatedAliasConflictDoesNotBlockTenantOrLeakIssue(t *testing.T) {
	target := resolvedTenantTarget(t, "selected", "selected-ns", "192.0.2.20")
	selected := observedTenantCluster(target, "selected-uid", true)
	other := observedTenantCluster(resolvedTenantTarget(t, "other", "other-ns", "192.0.2.21"), "other-uid", true)
	other.TenantIdentityConflict = true
	snapshot := tenantSnapshot(target, managementprobe.CollectionComplete, selected, other)
	snapshot.Issues = []managementprobe.Issue{{Source: "dpuclusters.provisioning.dpu.nvidia.com", Namespace: "other-ns", Resource: "other", Code: "TENANT_LABEL_CONFLICT", Message: "conflicting aliases"}}
	if got := tenantClusterCheck(&snapshot, &target); got.State != "passed" {
		t.Fatalf("unrelated alias conflict blocked selected tenant: %#v", got)
	}
	projection := tenantClusterProjection(&snapshot, &target)
	if len(projection) != 1 || projection[0].Name != "selected" {
		t.Fatalf("unrelated tenant was retained in public projection: %#v", projection)
	}
	if issues := projectTenantIssues(&snapshot, projection); len(issues) != 0 {
		t.Fatalf("unrelated object issue leaked into public report: %#v", issues)
	}
}

func resolvedTenantTarget(t *testing.T, name, namespace, vip string) bootstrap.Inputs {
	t.Helper()
	nameValue, namespaceValue, vipValue, interfaceValue := name, namespace, vip, "enp1s0"
	routerID := 126
	options := &config.BootstrapOptions{Tenant: config.TenantOptions{Name: &nameValue, Namespace: &namespaceValue, VIP: &vipValue, Interface: &interfaceValue, RouterID: &routerID}}
	selected := []bootstrap.Device{{Serial: "DPU-A", BMCIP: "192.0.2.30", BMCPort: 443}}
	if vip == "" {
		options.Tenant.VIP = nil
	}
	resolved, issues, err := config.ResolveBootstrap(options, "dpf-operator-system", "https://192.0.2.10:6443", selected)
	if err != nil || vip != "" && len(issues) != 0 || vip == "" && len(issues) != 1 {
		t.Fatalf("ResolveBootstrap() error=%v issues=%#v", err, issues)
	}
	return resolved
}

func observedTenantCluster(target bootstrap.Inputs, uid string, ready bool) managementprobe.DPUCluster {
	phase, status := "Ready", "True"
	if !ready {
		phase, status = "Pending", "False"
	}
	var markerKey, markerValue *string
	for key, value := range target.Tenant.Labels {
		markerKey, markerValue = &key, &value
	}
	return managementprobe.DPUCluster{
		ObjectSummary: managementprobe.ObjectSummary{
			Name: target.Tenant.Name, Namespace: target.Tenant.Namespace, UID: uid,
			Generation: 3, StatusFreshness: "current",
			Conditions: []managementprobe.Condition{{Type: "Ready", Status: status, ObservedGeneration: int64Pointer(3)}},
		},
		ResourceVersion: "42", Phase: &phase, Type: stringPointer("kamaji"), VIP: stringPointer(target.Tenant.VIP),
		Interface: stringPointer(target.Tenant.Interface), VirtualRouterID: int64Pointer(int64(target.Tenant.RouterID)),
		LaunchKitTenantLabelKey: markerKey, LaunchKitTenantLabelValue: markerValue,
	}
}

func tenantSnapshot(target bootstrap.Inputs, state managementprobe.CollectionState, clusters ...managementprobe.DPUCluster) managementprobe.Snapshot {
	return managementprobe.Snapshot{
		Namespace:   target.OperatorNamespace,
		DPUClusters: managementprobe.ResourceSet[managementprobe.DPUCluster]{State: state, Items: clusters},
	}
}

func TestTenantClusterCheckRequiresKamajiType(t *testing.T) {
	target := resolvedTenantTarget(t, "tenant-a", "tenant-ns", "192.0.2.20")
	for _, observedType := range []*string{nil, stringPointer(""), stringPointer("kubeadm")} {
		cluster := observedTenantCluster(target, "tenant-uid", true)
		cluster.Type = observedType
		snapshot := tenantSnapshot(target, managementprobe.CollectionComplete, cluster)
		check := tenantClusterCheck(&snapshot, &target)
		wantState, wantReason := "unknown", "TENANT_TARGET_TYPE_UNKNOWN"
		if observedType != nil && *observedType != "" {
			wantState, wantReason = "failed", "TENANT_TARGET_TYPE_MISMATCH"
		}
		if check.State != wantState || check.ReasonCode != wantReason {
			t.Fatalf("type gate=%#v", check)
		}
	}
}
