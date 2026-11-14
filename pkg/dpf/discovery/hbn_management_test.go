// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	extensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestDPFDeploymentPolicySchemaPathsMatchSelectedGAChart(t *testing.T) {
	// This is the exact DPF 26.8.0 GA chart member pinned by the managementprobe
	// fixture; its archive digest is recorded alongside that fixture's source.
	data, err := os.ReadFile(filepath.Join("..", "managementprobe", "testdata", "dpudeployments-v26.8.0.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd extensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("decode selected GA chart DPUDeployment CRD: %v", err)
	}
	var schema *extensionsv1.JSONSchemaProps
	for _, served := range crd.Spec.Versions {
		if served.Name == "v1alpha1" && served.Served && served.Schema != nil {
			schema = served.Schema.OpenAPIV3Schema
		}
	}
	if schema == nil {
		t.Fatal("selected GA chart does not serve the DPUDeployment v1alpha1 schema")
	}
	for _, required := range hbnManagementCRDs[1].fields {
		if !selectedChartSchemaPath(schema, required) {
			t.Errorf("HBN DPUDeployment capability path %q is absent from selected GA chart", required)
		}
	}
	for _, incorrect := range []string{"spec.dpuSets[].nameSuffix", "spec.dpuSetStrategy", "spec.nodeEffect"} {
		if selectedChartSchemaPath(schema, incorrect) {
			t.Errorf("incorrect root-level schema path %q unexpectedly exists", incorrect)
		}
	}
}

func selectedChartSchemaPath(root *extensionsv1.JSONSchemaProps, path string) bool {
	current := root
	for _, part := range strings.Split(path, ".") {
		arrayItem := strings.HasSuffix(part, "[]")
		name := strings.TrimSuffix(part, "[]")
		next, ok := current.Properties[name]
		if !ok {
			return false
		}
		if arrayItem {
			if next.Type != "array" || next.Items == nil || next.Items.Schema == nil {
				return false
			}
			next = *next.Items.Schema
		}
		current = &next
	}
	return true
}

func TestManagementHBNFreshInstallIsPlannedOnlyWithCatalogDigest(t *testing.T) {
	snapshot := freshAbsentHBNManagementSnapshot()
	inv := hbnManagementTestInventory()
	checks, projection := managementHBNChecks(&snapshot, &inv, "dpf-operator-system")
	for _, id := range []string{"apis", "operator", "operator-config", "enrollments", "kubernetes-version", "control-plane"} {
		check := managementCheckByID(t, checks, id)
		if check.State != "passed" {
			t.Errorf("%s state=%s reason=%s", id, check.State, check.ReasonCode)
		}
	}
	if projection.Namespace != "dpf-operator-system" || projection.Operator != nil {
		t.Fatalf("unexpected compact projection: %#v", projection)
	}
	inv.Software.Operator.ChartDigest = ""
	checks, _ = managementHBNChecks(&snapshot, &inv, "dpf-operator-system")
	if got := managementCheckByID(t, checks, "apis"); got.State != "unknown" || got.ReasonCode != "SELECTED_CHART_DIGEST_REQUIRED" {
		t.Fatalf("missing selected chart digest should block the install plan: %#v", got)
	}
}

func TestManagementHBNUnavailableSourcesRemainUnknown(t *testing.T) {
	snapshot := managementprobe.Snapshot{
		Namespace:       "dpf-operator-system",
		Deployments:     managementprobe.ResourceSet[managementprobe.Deployment]{State: managementprobe.CollectionUnavailable},
		OperatorConfigs: managementprobe.ResourceSet[managementprobe.OperatorConfig]{State: managementprobe.CollectionUnavailable},
		DPUClusters:     managementprobe.ResourceSet[managementprobe.DPUCluster]{State: managementprobe.CollectionUnavailable},
		DPUDevices:      managementprobe.ResourceSet[managementprobe.DPUDevice]{State: managementprobe.CollectionUnavailable},
		DPUs:            managementprobe.ResourceSet[managementprobe.DPU]{State: managementprobe.CollectionUnavailable},
		StorageClasses:  managementprobe.ResourceSet[managementprobe.StorageClass]{State: managementprobe.CollectionUnavailable},
		Issues:          []managementprobe.Issue{{Source: "dpus", Code: "API_ACCESS_DENIED", Message: "The configured Kubernetes identity is forbidden from reading this resource."}},
	}
	checks, projection := managementHBNChecks(&snapshot, nil, snapshot.Namespace)
	for _, id := range []string{"source-completeness", "apis", "operator", "operator-config", "enrollments"} {
		if got := managementCheckByID(t, checks, id); got.State != "unknown" {
			t.Errorf("%s state=%s, want unknown", id, got.State)
		}
	}
	if len(projection.Issues) != 1 || projection.Issues[0].Code != "API_ACCESS_DENIED" {
		t.Fatalf("safe source failure was not projected: %#v", projection.Issues)
	}
}

func TestManagementIssueProjectionKeepsGlobalAndProjectedObjectIssues(t *testing.T) {
	snapshot := freshAbsentHBNManagementSnapshot()
	snapshot.OperatorConfigs = managementprobe.ResourceSet[managementprobe.OperatorConfig]{
		State: managementprobe.CollectionComplete,
		Items: []managementprobe.OperatorConfig{{ObjectSummary: managementprobe.ObjectSummary{Name: "selected-config", Namespace: snapshot.Namespace}}},
	}
	snapshot.Issues = []managementprobe.Issue{
		{Source: "dpuclusters.provisioning.dpu.nvidia.com", Code: "API_COVERAGE_INCOMPLETE", Message: "cluster list incomplete"},
		{Source: "dpfoperatorconfigs.operator.dpu.nvidia.com", Namespace: snapshot.Namespace, Resource: "selected-config", Code: "MALFORMED_FIELD", Message: "selected object issue"},
		{Source: "dpfoperatorconfigs.operator.dpu.nvidia.com", Namespace: "unrelated", Resource: "other-config", Code: "MALFORMED_FIELD", Message: "unrelated object issue"},
		{Source: "dpus.provisioning.dpu.nvidia.com", Namespace: "unrelated", Resource: "other-dpu", Code: "MALFORMED_FIELD", Message: "unrelated enrollment issue"},
	}
	projection := projectHBNManagement(&snapshot, nil, snapshot.Namespace)
	if len(projection.Issues) != 2 {
		t.Fatalf("projected issue count=%d, want global source + selected object: %#v", len(projection.Issues), projection.Issues)
	}
	if projection.Issues[0].Code != "API_COVERAGE_INCOMPLETE" || projection.Issues[1].Resource != "selected-config" || projection.Issues[1].Namespace != snapshot.Namespace {
		t.Fatalf("issue projection lost source failure or retained wrong object: %#v", projection.Issues)
	}
}

func TestManagementHBNStaleReadyOperatorDoesNotPass(t *testing.T) {
	snapshot := freshAbsentHBNManagementSnapshot()
	snapshot.CRDs = nil // Existing operator evidence is partial, not a fresh empty installation.
	snapshot.Deployments.Items = []managementprobe.Deployment{{
		Name: "dpf-operator-controller-manager", UID: "operator-uid", Generation: 8, ObservedGeneration: 7,
		StatusFreshness: "stale", Replicas: int32Pointer(1), ReadyReplicas: 1, AvailableReplicas: 1,
		Images:     []string{"nvcr.io/nvidia/doca/dpf-system:v26.8.0"},
		Conditions: []managementprobe.Condition{{Type: "Available", Status: "True"}},
	}}
	checks, projection := managementHBNChecks(&snapshot, hbnManagementTestInventoryPointer(), snapshot.Namespace)
	if got := managementCheckByID(t, checks, "operator"); got.State != "failed" || got.ReasonCode != "DPF_OPERATOR_STATUS_STALE" {
		t.Fatalf("stale but Available operator should fail: %#v", got)
	}
	if projection.Operator == nil || projection.Operator.Ready != nil || projection.Operator.Image == "" {
		t.Fatalf("compact operator evidence missing: %#v", projection.Operator)
	}
}

func TestManagementHBNMismatchedOperatorVersionRequiresUpgrade(t *testing.T) {
	snapshot := freshAbsentHBNManagementSnapshot()
	snapshot.Deployments.Items = []managementprobe.Deployment{{
		Name: "release-manager", Generation: 2, ObservedGeneration: 2, StatusFreshness: "current",
		Replicas: int32Pointer(1), ReadyReplicas: 1, AvailableReplicas: 1,
		Images:     []string{"nvcr.io/nvidia/doca/dpf-system:v26.4.0"},
		Conditions: []managementprobe.Condition{{Type: "Available", Status: "True"}},
	}}
	checks, _ := managementHBNChecks(&snapshot, hbnManagementTestInventoryPointer(), snapshot.Namespace)
	if got := managementCheckByID(t, checks, "operator"); got.State != "failed" || got.ReasonCode != "DPF_OPERATOR_UPGRADE_REQUIRED" {
		t.Fatalf("old ready operator must require selected version: %#v", got)
	}
	_, projection := managementHBNChecks(&snapshot, hbnManagementTestInventoryPointer(), snapshot.Namespace)
	if projection.Operator == nil || projection.Operator.Name != "release-manager" || projection.Operator.Version != "v26.4.0" {
		t.Fatalf("repository-matched alternate deployment was not projected: %#v", projection.Operator)
	}
}

func TestManagementHBNUnrelatedDPUEnrollmentIsPreservedWithoutBlockingSelectedScope(t *testing.T) {
	snapshot := freshAbsentHBNManagementSnapshot()
	snapshot.DPUDevices = managementprobe.ResourceSet[managementprobe.DPUDevice]{State: managementprobe.CollectionComplete, Items: []managementprobe.DPUDevice{{ObjectSummary: managementprobe.ObjectSummary{Name: "foreign-device"}, SpecSerialNumber: stringPointer("FOREIGN-SERIAL")}}}
	snapshot.DPUs = managementprobe.ResourceSet[managementprobe.DPU]{State: managementprobe.CollectionComplete, Items: []managementprobe.DPU{}}
	checks, projection := managementHBNChecks(&snapshot, hbnManagementTestInventoryPointer(), snapshot.Namespace)
	if got := managementCheckByID(t, checks, "enrollments"); got.State != "passed" || got.ReasonCode != "DPU_SCOPE_UNCLAIMED" {
		t.Fatalf("known unrelated enrollment must not block selected scope: %#v", got)
	}
	if len(projection.Enrollments) != 0 {
		t.Fatalf("unrelated enrollment leaked into selected inventory: %#v", projection.Enrollments)
	}
}

func TestManagementHBNDoesNotProjectUnrelatedTenantRecordsWithoutResolvedTarget(t *testing.T) {
	snapshot := freshAbsentHBNManagementSnapshot()
	snapshot.DPUClusters = managementprobe.ResourceSet[managementprobe.DPUCluster]{State: managementprobe.CollectionComplete, Items: []managementprobe.DPUCluster{{
		ObjectSummary:   managementprobe.ObjectSummary{Name: "existing-tenant", Namespace: "existing-tenant", UID: "tenant-uid"},
		ResourceVersion: "42", Type: stringPointer("kamaji"), Phase: stringPointer("Ready"), VIP: stringPointer("192.0.2.20"),
		Interface: stringPointer("enp1s0"), VirtualRouterID: int64Pointer(126),
		LaunchKitTenantLabelKey: stringPointer("nvidia.kubernetes-launch-kit.dpf-tenant"), LaunchKitTenantLabelValue: stringPointer("existing-run-1"),
	}, {ObjectSummary: managementprobe.ObjectSummary{Name: "other-tenant", Namespace: "other-ns", UID: "other-uid"}, VIP: stringPointer("192.0.2.21")}}}
	_, projection := managementHBNChecks(&snapshot, hbnManagementTestInventoryPointer(), snapshot.Namespace)
	if got := tenantClusterCheck(&snapshot, nil); got.State != "unknown" || got.ReasonCode != "TENANT_TARGET_INPUTS_UNAVAILABLE" {
		t.Fatalf("existing tenant without resolved target must remain unresolved: %#v", got)
	}
	if len(projection.TenantClusters) != 0 {
		t.Fatalf("generic management projection leaked cross-namespace tenant records without a resolved target: %#v", projection.TenantClusters)
	}
}

func TestManagementHBNUnknownKubernetesRangeAndNilSnapshotStayUnknown(t *testing.T) {
	inv := hbnManagementTestInventory()
	inv.Software.SupportedKubernetes = nil
	checks, _ := managementHBNChecks(nil, &inv, "dpf-operator-system")
	if got := managementCheckByID(t, checks, "kubernetes-version"); got.State != "unknown" {
		t.Fatalf("missing selected support range state=%s, want unknown", got.State)
	}
	if got := managementCheckByID(t, checks, "source-completeness"); got.State != "unknown" {
		t.Fatalf("nil snapshot state=%s, want unknown", got.State)
	}
}

func TestManagementHBNRejectsSnapshotFromDifferentNamespace(t *testing.T) {
	snapshot := freshAbsentHBNManagementSnapshot()
	snapshot.Namespace = "other-namespace"
	checks, projection := managementHBNChecks(&snapshot, hbnManagementTestInventoryPointer(), "dpf-operator-system")
	if got := managementCheckByID(t, checks, "source-completeness"); got.State != "failed" || got.ReasonCode != "MANAGEMENT_NAMESPACE_MISMATCH" {
		t.Fatalf("wrong-namespace snapshot must fail: %#v", got)
	}
	if got := managementCheckByID(t, checks, "apis"); got.State != "unknown" {
		t.Fatalf("wrong-namespace CRD context must remain unknown to policy: %#v", got)
	}
	if projection.Namespace != "dpf-operator-system" {
		t.Fatalf("wrong-namespace facts leaked into projection: %#v", projection)
	}
}

func TestManagementHBNKubernetesMinorRangeIncludesAllPatchesAndRejectsPrereleases(t *testing.T) {
	inv := hbnManagementTestInventory()
	inv.Management.KubernetesVersion = "v1.36.7"
	if got := kubernetesRangeCheck(&inv); got.state != "passed" {
		t.Fatalf("maximum supported minor must include patch releases: %#v", got)
	}
	inv.Management.KubernetesVersion = "v1.36.0-rc.1"
	if got := kubernetesRangeCheck(&inv); got.state != "failed" || got.reason != "KUBERNETES_PRERELEASE_UNSUPPORTED" {
		t.Fatalf("pre-release Kubernetes version should fail without catalog support: %#v", got)
	}
}

func TestManagementHBNZeroReplicasAreNotReady(t *testing.T) {
	snapshot := freshAbsentHBNManagementSnapshot()
	snapshot.Deployments.Items = []managementprobe.Deployment{{
		Name: "dpf-operator-controller-manager", Generation: 1, ObservedGeneration: 1, StatusFreshness: "current",
		Replicas: int32Pointer(0), Images: []string{"nvcr.io/nvidia/doca/dpf-system:v26.8.0"},
		Conditions: []managementprobe.Condition{{Type: "Available", Status: "True"}},
	}}
	checks, projection := managementHBNChecks(&snapshot, hbnManagementTestInventoryPointer(), snapshot.Namespace)
	if got := managementCheckByID(t, checks, "operator"); got.State != "failed" || got.ReasonCode != "DPF_OPERATOR_NOT_READY" {
		t.Fatalf("zero operator replicas must not pass: %#v", got)
	}
	if projection.Operator == nil || projection.Operator.Ready == nil || *projection.Operator.Ready {
		t.Fatalf("zero replica operator projection should report not ready: %#v", projection.Operator)
	}
}

func TestManagementHBNOperatorConfigRequiresExactModeAndCurrentReadyCondition(t *testing.T) {
	snapshot := freshAbsentHBNManagementSnapshot()
	observed := int64(4)
	mode := "zero-trust"
	snapshot.OperatorConfigs = managementprobe.ResourceSet[managementprobe.OperatorConfig]{State: managementprobe.CollectionComplete, Items: []managementprobe.OperatorConfig{{
		ObjectSummary:  managementprobe.ObjectSummary{Name: "dpf-operator-config", Generation: 4, StatusFreshness: "current", Conditions: []managementprobe.Condition{{Type: "Ready", Status: "True", ObservedGeneration: &observed}}},
		DeploymentMode: &mode,
	}}}
	inv := hbnManagementTestInventoryPointer()
	if got := operatorConfigCheck(&snapshot, inv, snapshot.Namespace); got.state != "passed" {
		t.Fatalf("exact zero-trust current Ready config should pass: %#v", got)
	}
	stale := int64(3)
	snapshot.OperatorConfigs.Items[0].Conditions[0].ObservedGeneration = &stale
	if got := operatorConfigCheck(&snapshot, inv, snapshot.Namespace); got.state != "unknown" {
		t.Fatalf("stale Ready condition should remain unknown: %#v", got)
	}
	snapshot.OperatorConfigs.Items[0].Conditions[0].ObservedGeneration = &observed
	mode = "ZeroTrust"
	if got := operatorConfigCheck(&snapshot, inv, snapshot.Namespace); got.state != "failed" || got.reason != "DPF_DEPLOYMENT_MODE_MISMATCH" {
		t.Fatalf("non-enum mode spelling must not normalize into zero-trust: %#v", got)
	}
}

func freshAbsentHBNManagementSnapshot() managementprobe.Snapshot {
	snapshot := managementprobe.Snapshot{
		Namespace:       "dpf-operator-system",
		Deployments:     managementprobe.ResourceSet[managementprobe.Deployment]{State: managementprobe.CollectionComplete, Items: []managementprobe.Deployment{}},
		OperatorConfigs: managementprobe.ResourceSet[managementprobe.OperatorConfig]{State: managementprobe.CollectionAbsent, Items: []managementprobe.OperatorConfig{}},
		DPUClusters:     managementprobe.ResourceSet[managementprobe.DPUCluster]{State: managementprobe.CollectionAbsent, Items: []managementprobe.DPUCluster{}},
		DPUDevices:      managementprobe.ResourceSet[managementprobe.DPUDevice]{State: managementprobe.CollectionAbsent, Items: []managementprobe.DPUDevice{}},
		DPUs:            managementprobe.ResourceSet[managementprobe.DPU]{State: managementprobe.CollectionAbsent, Items: []managementprobe.DPU{}},
		StorageClasses:  managementprobe.ResourceSet[managementprobe.StorageClass]{State: managementprobe.CollectionComplete, Items: []managementprobe.StorageClass{}},
		CRDs:            make([]managementprobe.CRD, 0, len(hbnManagementCRDs)),
	}
	for _, crd := range hbnManagementCRDs {
		snapshot.CRDs = append(snapshot.CRDs, managementprobe.CRD{Name: crd.name, State: "absent"})
	}
	return snapshot
}

func hbnManagementTestInventory() inventory.Inventory {
	return inventory.Inventory{
		Software:   &releases.Release{SupportedKubernetes: &releases.VersionRange{Minimum: "1.33", Maximum: "1.36"}, Operator: releases.Operator{Chart: "dpf-operator", ChartDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Version: "v26.8.0", Image: "nvcr.io/nvidia/doca/dpf-system:v26.8.0"}},
		Management: &inventory.Management{KubernetesVersion: "v1.34.2", Nodes: []inventory.Node{{Name: "cp-a", ControlPlane: true, Ready: true}}},
		DPUs:       []inventory.Device{{SerialNumber: "SELECTED-SERIAL"}},
	}
}

func hbnManagementTestInventoryPointer() *inventory.Inventory {
	value := hbnManagementTestInventory()
	return &value
}

func managementCheckByID(t *testing.T, checks []inventory.Check, suffix string) inventory.Check {
	t.Helper()
	for _, check := range checks {
		if check.ID == "dpf.hbn.management."+suffix {
			return check
		}
	}
	t.Fatalf("missing management check %q", suffix)
	return inventory.Check{}
}

func int32Pointer(value int32) *int32    { return &value }
func int64Pointer(value int64) *int64    { return &value }
func stringPointer(value string) *string { return &value }

func TestManagementHBNOutputOmitsSuccessfulAPIDumpAndNamesSchemaFailure(t *testing.T) {
	snapshot := freshAbsentHBNManagementSnapshot()
	inv := hbnManagementTestInventory()
	_, projection := managementHBNChecks(&snapshot, &inv, snapshot.Namespace)
	encoded, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"apis"`) || strings.Contains(string(encoded), `"state"`) {
		t.Fatalf("management output contains generic API success metadata: %s", encoded)
	}
	for i, contract := range hbnManagementCRDs {
		snapshot.CRDs[i] = managementprobe.CRD{Name: contract.name, State: "observed", Group: contract.group, Plural: contract.plural, Kind: contract.kind, Scope: "Namespaced"}
	}
	// Even an identity mismatch must identify the resource, rather than asking
	// users to interpret a dump of every API's internal observation metadata.

	for i, contract := range hbnManagementCRDs {
		snapshot.CRDs[i].Conditions = []managementprobe.Condition{{Type: "Established", Status: "True"}, {Type: "NamesAccepted", Status: "True"}}
		snapshot.CRDs[i].Versions = []managementprobe.CRDVersion{{Name: "v1alpha1", Served: true, Storage: true}}
		for _, path := range contract.fields {
			snapshot.CRDs[i].Fields = append(snapshot.CRDs[i].Fields, managementprobe.SchemaField{Path: path, PresentIn: []string{"v1alpha1"}})
		}
	}
	snapshot.CRDs[1].Fields = snapshot.CRDs[1].Fields[1:]
	missing := apiCapabilityCheck(&snapshot, &inv)
	if missing.reason != "DPF_API_SCHEMA_MISMATCH" || !strings.Contains(missing.message, "spec.dpus.bfb") || !strings.Contains(missing.message, hbnManagementCRDs[1].name) {
		t.Fatalf("schema failure must name resource and missing field: %#v", missing)
	}
	snapshot.CRDs[0].Kind = "Unexpected"
	check := apiCapabilityCheck(&snapshot, &inv)
	if check.state != "failed" || check.subject != hbnManagementCRDs[0].name {
		t.Fatalf("API identity failure does not identify its resource: %#v", check)
	}
}

func TestHBNManagementStorageUsesMeasuredResult(t *testing.T) {
	for _, test := range []struct {
		name          string
		observation   *managementprobe.TenantStorageResult
		state, reason string
	}{
		{name: "not collected", state: "unknown", reason: "STORAGE_HEALTH_UNMEASURED"},
		{name: "healthy", observation: &managementprobe.TenantStorageResult{State: "passed", ReasonCode: "TENANT_STORAGE_READY", Message: "Current backing storage is ready."}, state: "passed", reason: "TENANT_STORAGE_READY"},
		{name: "denied", observation: &managementprobe.TenantStorageResult{State: "unknown", ReasonCode: "API_ACCESS_DENIED", Message: "Storage observation was denied."}, state: "unknown", reason: "API_ACCESS_DENIED"},
		{name: "mismatch", observation: &managementprobe.TenantStorageResult{State: "failed", ReasonCode: "STORAGE_BINDING_MISMATCH", Message: "The PV claim binding differs."}, state: "failed", reason: "STORAGE_BINDING_MISMATCH"},
		{name: "invalid state", observation: &managementprobe.TenantStorageResult{State: "ready", ReasonCode: "CLAIM", Message: "Unsupported result."}, state: "unknown", reason: "STORAGE_RESULT_INVALID"},
		{name: "missing explanation", observation: &managementprobe.TenantStorageResult{State: "passed", ReasonCode: "CLAIM"}, state: "unknown", reason: "STORAGE_RESULT_INVALID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := freshAbsentHBNManagementSnapshot()
			snapshot.TenantStorage = test.observation
			checks, _ := managementHBNChecks(&snapshot, nil, snapshot.Namespace)
			got := managementCheckByID(t, checks, "storage-health")
			if got.State != test.state || got.ReasonCode != test.reason || got.RequiredPhase != "deploy" {
				t.Fatalf("storage result = %#v; want %s/%s deployment gate", got, test.state, test.reason)
			}
			checks, _ = managementHBNChecks(&snapshot, nil, "different-namespace")
			if got := managementCheckByID(t, checks, "storage-health"); got.State == "passed" {
				t.Fatal("storage collected from another namespace was accepted")
			}
		})
	}
}
