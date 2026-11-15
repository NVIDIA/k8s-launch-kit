// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func activationBoundaryScopeFixture(t *testing.T) (activationIntegrationFixture, enrollmentScopeSnapshot) {
	t.Helper()
	fixture := newActivationIntegrationFixture(t)
	deployment, err := retainedDeploymentObject(fixture.prepared.execution)
	if err != nil {
		t.Fatal(err)
	}
	marked, err := fixture.prepared.execution.reconciler.markedDesired(deployment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.client.Create(fixture.ctx, marked, false); err != nil {
		t.Fatal(err)
	}
	snapshot, err := collectEnrollmentScope(fixture.ctx, fixture.client, fixture.prepared.execution, fixture.prepared.selected, true)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, snapshot
}

func copyEnrollmentScope(snapshot enrollmentScopeSnapshot) enrollmentScopeSnapshot {
	copy := enrollmentScopeSnapshot{resources: make([]enrollmentScopeResource, len(snapshot.resources))}
	for i, resource := range snapshot.resources {
		copy.resources[i] = enrollmentScopeResource{resource: resource.resource, objects: deepCopyUnstructuredList(resource.objects)}
	}
	return copy
}

func TestActivationScopeAllowsOnlyItsRetainedParentAndSelectedChildren(t *testing.T) {
	fixture, baseline := activationBoundaryScopeFixture(t)
	execution, selected := fixture.prepared.execution, fixture.client.activationSelected
	if err := validateActivationScope(baseline, execution, selected, activationIntegrationDeploymentUID); err != nil {
		t.Fatalf("exact retained activation graph rejected: %v", err)
	}
	if err := inspectEnrollmentScope(fixture.ctx, fixture.client, execution, fixture.prepared.selected, true); err == nil {
		t.Fatal("initial enrollment policy allowed post-activation children")
	}
	if err := validateActivationScope(baseline, execution, selected, ""); err == nil {
		t.Fatal("precreation activation policy adopted an existing parent")
	}

	mutations := []struct {
		name string
		edit func(enrollmentScopeSnapshot)
	}{
		{"parent UID", func(s enrollmentScopeSnapshot) { s.objects("DPUDeployment")[0].SetUID(types.UID("replaced")) }},
		{"parent namespace", func(s enrollmentScopeSnapshot) { s.objects("DPUDeployment")[0].SetNamespace("other") }},
		{"device UID", func(s enrollmentScopeSnapshot) { s.objects("DPUDevice")[0].SetUID(types.UID("replaced")) }},
		{"node UID", func(s enrollmentScopeSnapshot) { s.objects("DPUNode")[0].SetUID(types.UID("replaced")) }},
		{"device readiness", func(s enrollmentScopeSnapshot) {
			setConditionStatus(&s.objects("DPUDevice")[0], "BMCCredentialsReady", "False", "CredentialsChanged", 1)
		}},
		{"invalid node details", func(s enrollmentScopeSnapshot) {
			appendNodeCondition(&s.objects("DPUNode")[0], "InvalidDPUDetails", "True")
		}},
		{"set namespace", func(s enrollmentScopeSnapshot) { s.objects("DPUSet")[0].SetNamespace("other") }},
		{"set owner UID", func(s enrollmentScopeSnapshot) {
			owners := s.objects("DPUSet")[0].GetOwnerReferences()
			owners[0].UID = types.UID("replaced")
			s.objects("DPUSet")[0].SetOwnerReferences(owners)
		}},
		{"set owner replaced", func(s enrollmentScopeSnapshot) { s.objects("DPUSet")[0].SetUID(types.UID("replaced")) }},
		{"set additional selector", func(s enrollmentScopeSnapshot) {
			_ = unstructured.SetNestedSlice(s.objects("DPUSet")[0].Object, []any{map[string]any{"key": "foreign", "operator": "Exists"}}, "spec", "dpuDeviceSelector", "matchExpressions")
		}},
		{"set tenant selector", func(s enrollmentScopeSnapshot) {
			_ = unstructured.SetNestedStringMap(s.objects("DPUSet")[0].Object, map[string]string{"other": "tenant"}, "spec", "dpuTemplate", "spec", "cluster", "selector", "matchLabels")
		}},
		{"DPU namespace", func(s enrollmentScopeSnapshot) { s.objects("DPU")[0].SetNamespace("other") }},
		{"DPU owner UID", func(s enrollmentScopeSnapshot) {
			owners := s.objects("DPU")[0].GetOwnerReferences()
			owners[0].UID = types.UID("replaced")
			s.objects("DPU")[0].SetOwnerReferences(owners)
		}},
		{"DPU serial", func(s enrollmentScopeSnapshot) {
			_ = unstructured.SetNestedField(s.objects("DPU")[0].Object, "other", "spec", "serialNumber")
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			snapshot := copyEnrollmentScope(baseline)
			test.edit(snapshot)
			if err := validateActivationScope(snapshot, execution, selected, activationIntegrationDeploymentUID); err == nil {
				t.Fatal("changed activation graph was accepted")
			}
		})
	}
	for _, kind := range []string{"DPUDiscovery", "DPUDevice", "DPUNode", "DPUDeployment", "DPUSet", "DPU"} {
		t.Run("unrelated "+kind, func(t *testing.T) {
			snapshot := copyEnrollmentScope(baseline)
			for i := range snapshot.resources {
				if snapshot.resources[i].resource.kind != kind {
					continue
				}
				extra := snapshot.resources[i].objects[0].DeepCopy()
				extra.SetName("unrelated")
				extra.SetUID(types.UID("unrelated"))
				snapshot.resources[i].objects = append(snapshot.resources[i].objects, *extra)
			}
			if err := validateActivationScope(snapshot, execution, selected, activationIntegrationDeploymentUID); err == nil {
				t.Fatal("unrelated all-namespace object was accepted")
			}
		})
	}
}

func TestActivationScopeAllowsNodeRebootTransitionsAndPartialChildren(t *testing.T) {
	fixture, baseline := activationBoundaryScopeFixture(t)
	for _, status := range []string{"False", "Unknown"} {
		snapshot := copyEnrollmentScope(baseline)
		setNodeCondition(&snapshot.objects("DPUNode")[0], "Ready", status)
		appendNodeCondition(&snapshot.objects("DPUNode")[0], "DPUNodeRebootInProgress", "True")
		if err := validateActivationScope(snapshot, fixture.prepared.execution, fixture.client.activationSelected, activationIntegrationDeploymentUID); err != nil {
			t.Fatalf("node transition %s blocked exact activation scope: %v", status, err)
		}
	}
	snapshot := copyEnrollmentScope(baseline)
	for i := range snapshot.resources {
		if snapshot.resources[i].resource.kind == "DPU" || snapshot.resources[i].resource.kind == "DPUSet" {
			snapshot.resources[i].objects = []unstructured.Unstructured{}
		}
	}
	if err := validateActivationScope(snapshot, fixture.prepared.execution, fixture.client.activationSelected, activationIntegrationDeploymentUID); err != nil {
		t.Fatalf("in-progress child creation rejected: %v", err)
	}
}

func TestActivationBoundaryRefreshesSoftwareIdentityAndReceiptWithoutRestreaming(t *testing.T) {
	fixture := newActivationIntegrationFixture(t)
	client, execution := fixture.client, fixture.prepared.execution
	requests := client.streamRequests.Load()
	if err := verifyActivationSoftware(fixture.ctx, client, execution, fixture.workload.Software); err != nil {
		t.Fatalf("valid software receipt rejected: %v", err)
	}
	if client.streamRequests.Load() != requests {
		t.Fatal("activation restreamed software bytes")
	}
	for _, mutation := range []string{"UID", "generation", "digest", "missing component", "missing verification time", "registry release"} {
		t.Run(mutation, func(t *testing.T) {
			receipt := fixture.workload.Software
			receipt.Components = append([]verifiedSoftwareComponent(nil), receipt.Components...)
			desired, err := retainedBlueFieldSoftware(execution)
			if err != nil {
				t.Fatal(err)
			}
			key := tenantStageKey(desired)
			saved := client.base.resources[key].DeepCopy()
			image := client.managerImage
			t.Cleanup(func() { client.base.resources[key] = saved; client.managerImage = image })
			switch mutation {
			case "UID":
				client.base.resources[key].SetUID(types.UID("replaced"))
			case "generation":
				client.base.resources[key].SetGeneration(receipt.Generation + 1)
			case "digest":
				receipt.Components[0].SHA256 = strings.Repeat("0", 64)
			case "missing component":
				receipt.Components = receipt.Components[:0]
			case "missing verification time":
				receipt.VerifiedAt = time.Time{}
			case "registry release":
				client.managerImage = "unrelated"
			}
			if err := verifyActivationSoftware(fixture.ctx, client, execution, receipt); err == nil {
				t.Fatal("changed software binding was accepted")
			}
		})
	}
}

func TestCollectEnrollmentScopeRejectsDuplicateChildIdentities(t *testing.T) {
	execution, client, selected := enrollmentScopeFixture(t)
	child := enrollmentScopeObject("provisioning.dpu.nvidia.com/v1alpha1", "DPUSet", "duplicate", execution.operatorNamespace)
	client.listed["DPUSet"] = []unstructured.Unstructured{*child.DeepCopy(), *child.DeepCopy()}
	_, err := collectEnrollmentScope(tenantStageContext(t), client, execution, selected, true)
	if err == nil || !strings.Contains(err.Error(), "duplicate identities") {
		t.Fatalf("duplicate child identities accepted: %v", err)
	}
}
