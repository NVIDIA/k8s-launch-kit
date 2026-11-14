// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package managementprobe

import (
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	extensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

// Extracted from the selected DPF 26.8.0 GA chart archive, whose SHA-256 is
// 0252d473beaf473e93a09b23a39b7b78fff931c9a801f4e60ef93bface5e28c8.
// Archive member: dpf-operator/templates/crds/apiextensions.k8s.io_v1_customresourcedefinition_dpudeployments.svc.dpu.nvidia.com.yaml.
//
//go:embed testdata/dpudeployments-v26.8.0.yaml
var selectedChartDPUDeploymentCRD []byte

func TestCollectMissingCRDsAreAbsentAndNotQueried(t *testing.T) {
	kube := kubernetesfake.NewSimpleClientset()
	dynamic := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	collector := Collector{Kubernetes: kube, Dynamic: dynamic, Extensions: extensionsfake.NewClientset()}

	snapshot := collector.Collect(context.Background(), "dpf-operator-system")
	if snapshot.TenantStorage == nil || snapshot.TenantStorage.State == "passed" || snapshot.TenantStorage.ReasonCode == "" || snapshot.TenantStorage.Message == "" {
		t.Fatalf("absent tenant storage did not retain a blocking observation: %#v", snapshot.TenantStorage)
	}
	if snapshot.OperatorConfigs.State != CollectionAbsent || snapshot.DPUDevices.State != CollectionAbsent {
		t.Fatalf("missing CRDs should be represented as absent: operator=%s devices=%s", snapshot.OperatorConfigs.State, snapshot.DPUDevices.State)
	}
	if snapshot.Deployments.State != CollectionComplete || snapshot.StorageClasses.State != CollectionComplete {
		t.Fatalf("empty standard-resource collections should complete: deployments=%s storage=%s", snapshot.Deployments.State, snapshot.StorageClasses.State)
	}
}

func TestCollectForbiddenCRDReadIsUnavailableWithoutServerText(t *testing.T) {
	extensions := extensionsfake.NewClientset()
	extensions.PrependReactor("get", "customresourcedefinitions", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "customresourcedefinitions"}, "dpfoperatorconfigs.operator.dpu.nvidia.com", context.DeadlineExceeded)
	})
	collector := Collector{Kubernetes: kubernetesfake.NewSimpleClientset(), Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), Extensions: extensions}

	snapshot := collector.Collect(context.Background(), "dpf-operator-system")
	if snapshot.OperatorConfigs.State != CollectionUnavailable {
		t.Fatalf("forbidden CRD lookup state = %q, want unavailable", snapshot.OperatorConfigs.State)
	}
	for _, issue := range snapshot.Issues {
		if strings.Contains(issue.Message, context.DeadlineExceeded.Error()) {
			t.Fatalf("issue leaked server error text: %#v", issue)
		}
	}
}

func TestCollectTypedDPFObservationsPreserveContradictionsAndStaleness(t *testing.T) {
	crdObjects := []runtime.Object{
		crdFor(crds[4]), crdFor(crds[5]), crdFor(crds[6]),
	}
	extensions := extensionsfake.NewClientset(crdObjects...)
	device := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUDevice",
		"metadata": map[string]any{"name": "device-a", "namespace": "existing-enrollment", "uid": string(types.UID("device-uid")), "generation": int64(4)},
		"spec":     map[string]any{"serialNumber": "SERIAL-A", "bmcCredentialSecretName": "must-not-appear"},
		"status":   map[string]any{"serialNumber": "SERIAL-B", "psid": "PSID-A", "conditions": []any{map[string]any{"type": "Ready", "status": "False", "observedGeneration": int64(3)}}},
	}}
	device.SetGroupVersionKind(schema.GroupVersionKind{Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1", Kind: "DPUDevice"})
	dpu := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPU",
		"metadata": map[string]any{"name": "dpu-a", "namespace": "existing-enrollment", "ownerReferences": []any{map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUDevice", "name": "device-a", "uid": "device-uid", "controller": true}}},
		"spec":     map[string]any{"serialNumber": "SERIAL-A", "dpuDeviceName": "device-a", "dpuNodeName": "node-a"},
		"status":   map[string]any{"phase": "Config", "deploymentMode": "ZeroTrust", "dpuType": "BF3"},
	}}
	dpu.SetGroupVersionKind(schema.GroupVersionKind{Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1", Kind: "DPU"})
	dynamic := newDynamicClient(device, dpu)
	replicas := int32(2)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "dpf-operator", Namespace: "dpf-operator-system", Generation: 2}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1}}
	kube := kubernetesfake.NewSimpleClientset(deployment)
	collector := Collector{Kubernetes: kube, Dynamic: dynamic, Extensions: extensions}

	snapshot := collector.Collect(context.Background(), "dpf-operator-system")
	if snapshot.DPUDevices.State != CollectionComplete || len(snapshot.DPUDevices.Items) != 1 {
		t.Fatalf("DPUDevice observation = %#v", snapshot.DPUDevices)
	}
	got := snapshot.DPUDevices.Items[0]
	if got.Namespace != "existing-enrollment" || snapshot.DPUs.Items[0].Namespace != "existing-enrollment" {
		t.Fatal("cross-namespace enrollment identity was lost")
	}
	for _, action := range dynamic.Actions() {
		if action.GetResource().Resource == "dpudevices" || action.GetResource().Resource == "dpus" {
			if action.GetNamespace() != metav1.NamespaceAll {
				t.Fatal("enrollment read is not cluster-wide")
			}
		}
	}
	if got.SpecSerialNumber == nil || got.StatusSerialNumber == nil || *got.SpecSerialNumber == *got.StatusSerialNumber {
		t.Fatalf("serial conflict was not preserved: %#v", got)
	}
	if got.StatusFreshness != "stale" || snapshot.Deployments.Items[0].StatusFreshness != "stale" {
		t.Fatalf("stale status not represented: device=%s deployment=%s", got.StatusFreshness, snapshot.Deployments.Items[0].StatusFreshness)
	}
	if len(snapshot.DPUs.Items) != 1 || len(snapshot.DPUs.Items[0].Owners) != 1 || snapshot.DPUs.Items[0].Owners[0].Name != "device-a" {
		t.Fatalf("owner reference not preserved: %#v", snapshot.DPUs.Items)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(encoded)), "bmccredentialsecret") || strings.Contains(string(encoded), "must-not-appear") {
		t.Fatal("snapshot exposed a Secret reference")
	}
	foundConflict := false
	for _, issue := range snapshot.Issues {
		if issue.Code == "IDENTITY_CONFLICT" {
			foundConflict = true
		}
	}
	if !foundConflict {
		t.Fatalf("missing serial conflict issue: %#v", snapshot.Issues)
	}
}

func TestCollectDPUClustersAcrossNamespacesWithOwnershipAndEndpoint(t *testing.T) {
	cluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUCluster",
		"metadata": map[string]any{
			"name": "existing-tenant", "namespace": "existing-tenant", "uid": "tenant-uid", "resourceVersion": "42",
			"labels": map[string]any{"nvidia.kubernetes-launch-kit.dpf-tenant": "existing-run-1"},
		},
		"spec": map[string]any{"type": "kamaji", "clusterEndpoint": map[string]any{"keepalived": map[string]any{
			"vip": "192.0.2.20", "interface": "eth0", "virtualRouterID": int64(126),
		}}},
		"status": map[string]any{"phase": "Ready", "version": "v1.35.6"},
	}}
	cluster.SetGroupVersionKind(schema.GroupVersionKind{Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1", Kind: "DPUCluster"})
	dynamic := newDynamicClient(cluster)
	collector := Collector{Kubernetes: kubernetesfake.NewSimpleClientset(), Dynamic: dynamic, Extensions: extensionsfake.NewClientset(crdFor(crds[4]))}

	snapshot := collector.Collect(context.Background(), "dpf-operator-system")
	if snapshot.DPUClusters.State != CollectionComplete || len(snapshot.DPUClusters.Items) != 1 {
		t.Fatalf("cross-namespace DPUCluster observation = %#v", snapshot.DPUClusters)
	}
	got := snapshot.DPUClusters.Items[0]
	if got.Namespace != "existing-tenant" || got.Name != "existing-tenant" || got.UID != "tenant-uid" || got.ResourceVersion != "42" {
		t.Fatalf("namespaced DPUCluster identity was not retained: %#v", got)
	}
	if got.VIP == nil || *got.VIP != "192.0.2.20" || got.Interface == nil || *got.Interface != "eth0" || got.VirtualRouterID == nil || *got.VirtualRouterID != 126 {
		t.Fatalf("keepalived endpoint facts were not retained: %#v", got)
	}
	if got.LaunchKitTenantLabelKey == nil || *got.LaunchKitTenantLabelKey != "nvidia.kubernetes-launch-kit.dpf-tenant" || got.LaunchKitTenantLabelValue == nil || *got.LaunchKitTenantLabelValue != "existing-run-1" || got.Phase == nil || *got.Phase != "Ready" {
		t.Fatalf("tenant ownership/readiness facts were not retained: %#v", got)
	}
	if len(dynamic.Actions()) == 0 || dynamic.Actions()[0].GetNamespace() != metav1.NamespaceAll {
		t.Fatalf("DPUCluster list was not cluster-wide: %#v", dynamic.Actions())
	}
}

func TestCollectDPUClusterCrossNamespaceDeniedRemainsUnavailable(t *testing.T) {
	dynamic := newDynamicClient()
	dynamic.PrependReactor("list", "dpuclusters", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: crds[4].group, Resource: crds[4].plural}, "", context.Canceled)
	})
	collector := Collector{Kubernetes: kubernetesfake.NewSimpleClientset(), Dynamic: dynamic, Extensions: extensionsfake.NewClientset(crdFor(crds[4]))}
	snapshot := collector.Collect(context.Background(), "dpf-operator-system")
	if snapshot.DPUClusters.State != CollectionUnavailable {
		t.Fatalf("denied all-namespace DPUCluster read state=%s, want unavailable", snapshot.DPUClusters.State)
	}
	found := false
	for _, issue := range snapshot.Issues {
		if issue.Source == crds[4].name && issue.Code == "API_ACCESS_DENIED" {
			found = true
		}
		if strings.Contains(issue.Message, context.Canceled.Error()) {
			t.Fatalf("API error details leaked: %#v", issue)
		}
	}
	if !found {
		t.Fatalf("missing safe denied-read issue: %#v", snapshot.Issues)
	}
}

func TestCollectDPUClusterRepeatedContinuationRemainsIncomplete(t *testing.T) {
	dynamic := newDynamicClient()
	calls := 0
	dynamic.PrependReactor("list", "dpuclusters", func(action ktesting.Action) (bool, runtime.Object, error) {
		calls++
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1", Kind: "DPUClusterList"})
		list.SetContinue("repeated-token")
		return true, list, nil
	})
	collector := Collector{Kubernetes: kubernetesfake.NewSimpleClientset(), Dynamic: dynamic, Extensions: extensionsfake.NewClientset(crdFor(crds[4]))}
	snapshot := collector.Collect(context.Background(), "dpf-operator-system")
	if snapshot.DPUClusters.State != CollectionIncomplete || calls != 2 {
		t.Fatalf("repeated continuation state=%s calls=%d, want incomplete after 2 pages", snapshot.DPUClusters.State, calls)
	}
	if !hasIssue(snapshot.Issues, "API_PAGINATION_INVALID") {
		t.Fatalf("missing safe repeated-continuation issue: %#v", snapshot.Issues)
	}
}

func TestSelectedGAChartDPUClusterSchemaContainsObservedEndpointFields(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "bootstrap", "testdata", "dpuclusters.provisioning.dpu.nvidia.com.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd extensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("decode selected GA DPUCluster CRD: %v", err)
	}
	observed := observeCRD(&crd, crds[4], &[]Issue{})
	for _, required := range crds[4].fields {
		found := false
		for _, field := range observed.Fields {
			if field.Path == required && strings.Contains(strings.Join(field.PresentIn, ","), "v1alpha1") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("selected GA DPUCluster schema lacks required field %q", required)
		}
	}
}

func TestCollectMalformedDPFFieldAddsIssueButKeepsObservation(t *testing.T) {
	extensions := extensionsfake.NewClientset(crdFor(crds[5]))
	device := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUDevice",
		"metadata": map[string]any{"name": "device-a", "namespace": "dpf-operator-system"},
		"spec":     map[string]any{"serialNumber": int64(7)}, "status": map[string]any{},
	}}
	device.SetGroupVersionKind(schema.GroupVersionKind{Group: "provisioning.dpu.nvidia.com", Version: "v1alpha1", Kind: "DPUDevice"})
	dynamic := newDynamicClient(device)
	snapshot := (Collector{Kubernetes: kubernetesfake.NewSimpleClientset(), Dynamic: dynamic, Extensions: extensions}).Collect(context.Background(), "dpf-operator-system")
	if snapshot.DPUDevices.State != CollectionComplete || len(snapshot.DPUDevices.Items) != 1 {
		t.Fatalf("malformed field should not discard object: %#v", snapshot.DPUDevices)
	}
	for _, issue := range snapshot.Issues {
		if issue.Code == "API_RESPONSE_INVALID" && issue.Resource == "device-a" && issue.Field == "spec.serialNumber" {
			return
		}
	}
	t.Fatalf("missing malformed-field issue: %#v", snapshot.Issues)
}

func TestSchemaPathPresentTraversesArrayItems(t *testing.T) {
	root := &extensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]extensionsv1.JSONSchemaProps{
		"spec": {Type: "object", Properties: map[string]extensionsv1.JSONSchemaProps{
			"dpus": {Type: "array", Items: &extensionsv1.JSONSchemaPropsOrArray{Schema: &extensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]extensionsv1.JSONSchemaProps{"bfb": {Type: "string"}}}}},
		}},
	}}
	if !schemaPathPresent(root, []string{"spec", "dpus[]", "bfb"}) {
		t.Fatal("expected to find a property beneath array items")
	}
	if schemaPathPresent(root, []string{"spec", "dpus", "bfb"}) {
		t.Fatal("array item traversal must be explicit")
	}
}

func TestSelectedGAChartDPUDeploymentSchemaPaths(t *testing.T) {
	var obj extensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(selectedChartDPUDeploymentCRD, &obj); err != nil {
		t.Fatalf("decode pinned DPF 26.8.0 chart CRD fixture: %v", err)
	}
	issues := []Issue{}
	observed := observeCRD(&obj, crds[1], &issues)
	if observed.State != "observed" || observed.Name != crds[1].name {
		t.Fatalf("fixture CRD identity was not observed: state=%s name=%s issues=%#v", observed.State, observed.Name, issues)
	}
	for _, required := range crds[1].fields {
		found := false
		for _, field := range observed.Fields {
			if field.Path == required && strings.Contains(strings.Join(field.PresentIn, ","), "v1alpha1") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("selected GA chart DPUDeployment CRD does not contain required schema path %q", required)
		}
	}
	for _, incorrectRootPath := range []string{"spec.dpuSets[].nameSuffix", "spec.dpuSetStrategy", "spec.nodeEffect"} {
		for _, field := range observed.Fields {
			if field.Path == incorrectRootPath && len(field.PresentIn) > 0 {
				t.Errorf("incorrect root-level path %q unexpectedly exists in the selected chart", incorrectRootPath)
			}
		}
	}
}

func TestCRDGenerationTrackingDisabledIsUnknownNotStale(t *testing.T) {
	spec := crds[4]
	obj := crdFor(spec)
	obj.Generation = 5
	obj.Status.ObservedGeneration = 0
	issues := []Issue{}
	got := observeCRD(obj, spec, &issues)
	if got.StatusFreshness != "unknown" {
		t.Fatalf("unreported observedGeneration freshness=%q, want unknown", got.StatusFreshness)
	}
	for _, issue := range issues {
		if issue.Code == "STATUS_STALE" {
			t.Fatalf("zero observedGeneration was incorrectly marked stale: %#v", issues)
		}
	}

	obj.Status.ObservedGeneration = 4
	issues = nil
	got = observeCRD(obj, spec, &issues)
	if got.StatusFreshness != "stale" {
		t.Fatalf("positive lag freshness=%q, want stale", got.StatusFreshness)
	}
	foundStale := false
	for _, issue := range issues {
		foundStale = foundStale || issue.Code == "STATUS_STALE"
	}
	if !foundStale {
		t.Fatalf("positive generation lag should produce stale issue: %#v", issues)
	}
}

func TestTypedSnapshotYAMLUsesStableLowerCamelCaseAndFlattensSummary(t *testing.T) {
	value := Snapshot{Namespace: "dpf-operator-system", DPUDevices: ResourceSet[DPUDevice]{State: CollectionComplete, Items: []DPUDevice{{ObjectSummary: ObjectSummary{Name: "device-a", StatusFreshness: "current"}, SpecSerialNumber: stringPointer("SERIAL-A")}}}}
	encoded, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{"namespace:", "dpuDevices:", "specSerialNumber:", "statusFreshness:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("YAML output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "ObjectSummary:") || strings.Contains(text, "SpecSerialNumber:") {
		t.Fatalf("YAML output did not use public lowerCamelCase shape:\n%s", text)
	}
}

func crdFor(spec crdSpec) *extensionsv1.CustomResourceDefinition {
	return &extensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: spec.name, Generation: 1},
		Spec: extensionsv1.CustomResourceDefinitionSpec{
			Group: spec.group, Scope: extensionsv1.NamespaceScoped,
			Names:    extensionsv1.CustomResourceDefinitionNames{Plural: spec.plural, Kind: spec.kind},
			Versions: []extensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true, Schema: &extensionsv1.CustomResourceValidation{OpenAPIV3Schema: &extensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]extensionsv1.JSONSchemaProps{"spec": {Type: "object"}, "status": {Type: "object"}}}}}},
		},
		Status: extensionsv1.CustomResourceDefinitionStatus{ObservedGeneration: 1},
	}
}

func newDynamicClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	listKinds := map[schema.GroupVersionResource]string{}
	for _, spec := range crds {
		version := schema.GroupVersion{Group: spec.group, Version: "v1alpha1"}
		listKinds[version.WithResource(spec.plural)] = spec.kind + "List"
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objects...)
}

func stringPointer(value string) *string { return &value }

func hasIssue(issues []Issue, code string) bool {
	for _, issue := range issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}
