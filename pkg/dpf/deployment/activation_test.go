// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestActivationSetIntentsRestrictSelectedDevicesAndTenant(t *testing.T) {
	devices := []generation.ResolvedDevice{resolvedActivationDevice("SERIAL-A", "serial-a"), resolvedActivationDevice("SERIAL-B", "serial-b")}
	deployment := activationDeploymentIntent(map[string]string{"tenant.example/id": "tenant-a"})
	selected := []activationSelectedDevice{
		{item: enrollmentBMC{device: devices[0]}, device: inventory.Device{SerialNumber: "SERIAL-A"}},
		{item: enrollmentBMC{device: devices[1]}, device: inventory.Device{SerialNumber: "SERIAL-B"}},
	}
	before := deployment.DeepCopy()
	sets, err := activationSetIntents(deployment, devices)
	if err != nil {
		t.Fatalf("activationSetIntents(): %v", err)
	}
	if len(sets) != len(devices) {
		t.Fatalf("got %d set intents, want %d", len(sets), len(devices))
	}
	for _, set := range sets {
		software, found, err := unstructured.NestedStringMap(set.Object, "spec", "dpuTemplate", "spec", "blueFieldSoftware")
		flavor, flavorFound, flavorErr := unstructured.NestedString(set.Object, "spec", "dpuTemplate", "spec", "dpuFlavor")
		if err != nil || !found || !reflect.DeepEqual(software, map[string]string{"name": "hbn-software"}) || flavorErr != nil || !flavorFound || flavor != "hbn-flavor" {
			t.Fatal("compiled consumed references differ from explicit retainedDD inputs")
		}
	}
	selected[0].dpuSet = sets["serial-a"]
	selected[1].dpuSet = sets["serial-b"]
	if err := validateActivationDeployment(deployment, selected, "dpf", TenantReadinessExpected{Namespace: "tenant", Labels: map[string]string{"tenant.example/id": "tenant-a"}}); err != nil {
		t.Fatalf("valid selected tenant selectors rejected: %v; set=%#v", err, selected[0].dpuSet.Object)
	}
	if !reflect.DeepEqual(before.Object, deployment.Object) {
		t.Fatal("building activation intents mutated the retained DPUDeployment")
	}

	bad := deployment.DeepCopy()
	settings, _, _ := unstructured.NestedSlice(bad.Object, "spec", "dpus", "dpuSets")
	setting := settings[0].(map[string]any)
	setting["dpuClusterSelector"] = map[string]any{"tenant.example/id": "other"}
	if err := unstructured.SetNestedSlice(bad.Object, settings, "spec", "dpus", "dpuSets"); err != nil {
		t.Fatal(err)
	}
	wrongTenantSets, err := activationSetIntents(bad, devices)
	if err != nil {
		t.Fatalf("build drifted tenant selector fixture: %v", err)
	}
	wrongTenantSelected := []activationSelectedDevice{
		{item: enrollmentBMC{device: devices[0]}, dpuSet: wrongTenantSets["serial-a"]},
		{item: enrollmentBMC{device: devices[1]}, dpuSet: wrongTenantSets["serial-b"]},
	}
	if err := validateActivationDeployment(bad, wrongTenantSelected, "dpf", TenantReadinessExpected{Namespace: "tenant", Labels: map[string]string{"tenant.example/id": "tenant-a"}}); err == nil {
		got, _, _ := unstructured.NestedStringMap(wrongTenantSelected[0].dpuSet.Object, "spec", "dpuTemplate", "spec", "cluster", "selector", "matchLabels")
		t.Fatalf("selector targeting a different tenant was accepted: %v", got)
	}

	bad = deployment.DeepCopy()
	settings, _, _ = unstructured.NestedSlice(bad.Object, "spec", "dpus", "dpuSets")
	setting = settings[1].(map[string]any)
	setting["dpuDeviceSelector"] = map[string]any{"matchLabels": map[string]any{"inventory.example/serial": "unselected"}}
	if err := unstructured.SetNestedSlice(bad.Object, settings, "spec", "dpus", "dpuSets"); err != nil {
		t.Fatal(err)
	}
	if _, err := activationSetIntents(bad, devices); err == nil {
		t.Fatal("selector for an unselected DPU was accepted")
	}

	bad = deployment.DeepCopy()
	settings, _, _ = unstructured.NestedSlice(bad.Object, "spec", "dpus", "dpuSets")
	settings[1] = settings[0]
	if err := unstructured.SetNestedSlice(bad.Object, settings, "spec", "dpus", "dpuSets"); err != nil {
		t.Fatal(err)
	}
	if _, err := activationSetIntents(bad, devices); err == nil {
		t.Fatal("duplicate DPUSet selector was accepted")
	}
}

func TestSelectedHostRebootPendingWaitsForDPFExternalGate(t *testing.T) {
	selected := []activationSelectedDevice{{
		nodeName: "dpu-node-serial-a", dpuName: "dpu-node-serial-a-serial-a",
		item: enrollmentBMC{device: resolvedActivationDevice("SERIAL-A", "serial-a")},
	}}
	node := dpuNodeIdentity("dpf", selected[0].nodeName)
	node.SetUID(types.UID("node-uid"))
	dpu := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPU",
		"metadata": map[string]any{"name": selected[0].dpuName, "namespace": "dpf"},
		"status":   map[string]any{"phase": "Creating"},
	}}
	observed := activationObservation{nodes: map[string]*unstructured.Unstructured{selected[0].nodeName: node}, dpus: map[string]*unstructured.Unstructured{selected[0].dpuName: dpu}}
	if pending, actionable, err := selectedHostRebootPending(observed, selected, []int{0}); err != nil || pending || actionable {
		t.Fatalf("no DPF request: got pending=%v actionable=%v err=%v", pending, actionable, err)
	}

	node.SetAnnotations(map[string]string{activationExternalRebootKey: "true"})
	if pending, actionable, err := selectedHostRebootPending(observed, selected, []int{0}); err != nil || !pending || actionable {
		t.Fatalf("initial annotation/status propagation should wait: pending=%v actionable=%v err=%v", pending, actionable, err)
	}

	_ = unstructured.SetNestedField(node.Object, "SystemLevelReset", "status", "rebootMethod")
	_ = unstructured.SetNestedField(dpu.Object, "Rebooting", "status", "phase")
	_ = unstructured.SetNestedField(dpu.Object, "Update Firmware", "status", "previousPhase")
	_ = unstructured.SetNestedMap(dpu.Object, map[string]any{"phase": "Pending", "reason": activationManualWaitReason}, "status", "rebootStatus")
	if pending, actionable, err := selectedHostRebootPending(observed, selected, []int{0}); err != nil || !pending || !actionable {
		t.Fatalf("exact external wait state should permit the selected host reboot: pending=%v actionable=%v err=%v", pending, actionable, err)
	}

	_ = unstructured.SetNestedField(node.Object, "PowerCycle", "status", "rebootMethod")
	if _, _, err := selectedHostRebootPending(observed, selected, []int{0}); err == nil {
		t.Fatal("unsupported power control was accepted")
	}
	node.SetAnnotations(map[string]string{activationExternalRebootKey: "false"})
	if _, _, err := selectedHostRebootPending(observed, selected, []int{0}); err == nil {
		t.Fatal("malformed external reboot annotation was accepted")
	}
}

func TestActivationMaintenanceAllowsControllerAnnotationPropagation(t *testing.T) {
	selected := activationSelectedDevice{
		nodeName: "dpu-node-serial-a",
		dpuName:  "dpu-node-serial-a-serial-a",
		item:     enrollmentBMC{device: resolvedActivationDevice("SERIAL-A", "serial-a")},
		dpuSet:   &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"namespace": "dpf"}}},
	}
	maintenance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUNodeMaintenance",
		"metadata": map[string]any{"name": selected.nodeName + "-hold", "namespace": "dpf", "labels": map[string]any{activationDPUNodeLabel: selected.nodeName}},
		"spec":     map[string]any{"dpuNodeName": selected.nodeName, "nodeEffect": map[string]any{"hold": true}, "requestor": []any{selected.dpuName}},
	}}
	if err := validateActivationMaintenanceRequestors(maintenance, selected, []string{selected.dpuName}, false); err != nil {
		t.Fatalf("maintenance object before its controller hold annotation was accepted as malformed: %v", err)
	}
}

func TestActivationDPUNodeUsesPinnedExternalMethodObject(t *testing.T) {
	item := enrollmentBMC{device: resolvedActivationDevice("SERIAL-A", "serial-a")}
	selected := activationSelectedDevice{
		item: item, nodeName: "dpu-node-serial-a",
		dpuSet: &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"namespace": "dpf"}}},
	}
	node := dpuNodeIdentity("dpf", selected.nodeName)
	node.SetUID(types.UID("node-uid"))
	node.SetResourceVersion("8")
	_ = unstructured.SetNestedSlice(node.Object, []any{map[string]any{"name": "serial-a"}}, "spec", "dpus")
	_ = unstructured.SetNestedMap(node.Object, map[string]any{"external": map[string]any{}}, "spec", "nodeRebootMethod")
	if err := validateActivationDPUNode(node, selected, []activationSelectedDevice{selected}); err != nil {
		t.Fatalf("pinned external:{} method rejected: %v", err)
	}
	_ = unstructured.SetNestedMap(node.Object, map[string]any{"external": true}, "spec", "nodeRebootMethod")
	if err := validateActivationDPUNode(node, selected, []activationSelectedDevice{selected}); err == nil {
		t.Fatal("boolean external method was accepted instead of the pinned object shape")
	}
}

func TestActivationAnnotationPatchIsNarrowAndIdentityAware(t *testing.T) {
	falseValue := "false"
	patch, err := activationAnnotationPatch(stringPointer("true"), &falseValue)
	if err != nil {
		t.Fatalf("hold release patch: %v", err)
	}
	var operations []map[string]any
	if err := json.Unmarshal(patch, &operations); err != nil || len(operations) != 2 || operations[0]["op"] != "test" || operations[1]["op"] != "replace" || operations[1]["value"] != "false" {
		t.Fatalf("hold release patch does not test and replace the observed annotation: %s (%v)", patch, err)
	}
	patch, err = activationAnnotationPatch(stringPointer("true"), nil)
	if err != nil {
		t.Fatalf("external reboot acknowledgement patch: %v", err)
	}
	if err := json.Unmarshal(patch, &operations); err != nil || len(operations) != 2 || operations[1]["op"] != "remove" || operations[1]["path"] != "/metadata/annotations/provisioning.dpu.nvidia.com~1dpunode-external-reboot-required" {
		t.Fatalf("reboot acknowledgement patch is not a narrow annotation removal: %s (%v)", patch, err)
	}
	if _, err := activationAnnotationPatch(stringPointer("false"), stringPointer("true")); err == nil {
		t.Fatal("unsupported annotation transition was accepted")
	}
}

func resolvedActivationDevice(serial, selector string) generation.ResolvedDevice {
	return generation.ResolvedDevice{Serial: serial, SelectorLabels: map[string]string{"inventory.example/serial": selector}}
}

func activationDeploymentIntent(tenantSelector map[string]string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": "DPUDeployment",
		"metadata": map[string]any{"name": "hbn", "namespace": "dpf"},
		"spec": map[string]any{"dpus": map[string]any{"blueFieldSoftware": "hbn-software", "flavor": "hbn-flavor", "nodeEffect": map[string]any{"hold": true}, "dpuSets": []any{
			map[string]any{"nameSuffix": "a", "dpuDeviceSelector": map[string]any{"matchLabels": map[string]any{"inventory.example/serial": "serial-a"}}, "dpuClusterSelector": map[string]any{"tenant.example/id": tenantSelector["tenant.example/id"]}},
			map[string]any{"nameSuffix": "b", "dpuDeviceSelector": map[string]any{"matchLabels": map[string]any{"inventory.example/serial": "serial-b"}}, "dpuClusterSelector": map[string]any{"tenant.example/id": tenantSelector["tenant.example/id"]}},
		}}},
	}}
}

func TestActivationScopeRejectsChangedConsumedReferences(t *testing.T) {
	fixture, baseline := activationBoundaryScopeFixture(t)
	if err := validateActivationScope(baseline, fixture.prepared.execution, fixture.client.activationSelected, activationIntegrationDeploymentUID); err != nil {
		t.Fatalf("invalid baseline: %v", err)
	}
	for _, tc := range []struct {
		name, kind string
		path       []string
	}{
		{name: "set software", kind: "DPUSet", path: []string{"spec", "dpuTemplate", "spec", "blueFieldSoftware", "name"}},
		{name: "set flavor", kind: "DPUSet", path: []string{"spec", "dpuTemplate", "spec", "dpuFlavor"}},
		{name: "DPU software", kind: "DPU", path: []string{"spec", "blueFieldSoftware"}},
		{name: "DPU flavor", kind: "DPU", path: []string{"spec", "dpuFlavor"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := copyEnrollmentScope(baseline)
			objects := snapshot.objects(tc.kind)
			if len(objects) == 0 {
				t.Fatal("missing consumed child")
			}
			if err := unstructured.SetNestedField(objects[0].Object, "foreign-reference", tc.path...); err != nil {
				t.Fatal(err)
			}
			if err := validateActivationScope(snapshot, fixture.prepared.execution, fixture.client.activationSelected, activationIntegrationDeploymentUID); err == nil {
				t.Fatal("foreign consumed reference passed complete activation scope")
			}
		})
	}
}

func TestActivationSetIntentsRejectsUnsupportedReferenceModes(t *testing.T) {
	devices := []generation.ResolvedDevice{resolvedActivationDevice("SERIAL-A", "serial-a"), resolvedActivationDevice("SERIAL-B", "serial-b")}
	baseline := activationDeploymentIntent(map[string]string{"tenant.example/id": "tenant-a"})
	for _, tc := range []struct {
		name, field string
		value       any
		remove      bool
	}{
		{name: "missing software", field: "blueFieldSoftware", remove: true},
		{name: "empty software", field: "blueFieldSoftware", value: ""},
		{name: "object software", field: "blueFieldSoftware", value: map[string]any{"name": "hbn-software"}},
		{name: "invalid software name", field: "blueFieldSoftware", value: "foreign/name"},
		{name: "missing flavor", field: "flavor", remove: true},
		{name: "empty flavor", field: "flavor", value: ""},
		{name: "object flavor", field: "flavor", value: map[string]any{"name": "hbn-flavor"}},
		{name: "BFB mode", field: "bfb", value: "foreign"},
		{name: "null BFB mode", field: "bfb", value: nil},
		{name: "flavor template mode", field: "flavorTemplate", value: map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deployment := baseline.DeepCopy()
			if tc.remove {
				unstructured.RemoveNestedField(deployment.Object, "spec", "dpus", tc.field)
			} else if err := unstructured.SetNestedField(deployment.Object, tc.value, "spec", "dpus", tc.field); err != nil {
				t.Fatal(err)
			}
			if _, err := activationSetIntents(deployment, devices); err == nil {
				t.Fatal("unsupported retained reference mode compiled")
			}
		})
	}
	if _, err := activationSetIntents(nil, devices); err == nil {
		t.Fatal("nil deployment compiled")
	}
	if _, err := activationSetIntents(baseline, nil); err == nil {
		t.Fatal("empty selected cohort compiled")
	}
}

func TestActivationScopeRejectsMissingMalformedAndAlternativeConsumedReferences(t *testing.T) {
	fixture, baseline := activationBoundaryScopeFixture(t)
	for _, reference := range []struct {
		name, kind string
		path       []string
	}{
		{name: "set software", kind: "DPUSet", path: []string{"spec", "dpuTemplate", "spec", "blueFieldSoftware"}},
		{name: "set flavor", kind: "DPUSet", path: []string{"spec", "dpuTemplate", "spec", "dpuFlavor"}},
		{name: "DPU software", kind: "DPU", path: []string{"spec", "blueFieldSoftware"}},
		{name: "DPU flavor", kind: "DPU", path: []string{"spec", "dpuFlavor"}},
	} {
		for _, mode := range []string{"missing", "malformed", "empty"} {
			t.Run(reference.name+" "+mode, func(t *testing.T) {
				snapshot := copyEnrollmentScope(baseline)
				objects := snapshot.objects(reference.kind)
				if len(objects) == 0 {
					t.Fatal("missing child")
				}
				if mode == "missing" {
					unstructured.RemoveNestedField(objects[0].Object, reference.path...)
				} else {
					var value any = int64(1)
					if mode == "empty" {
						value = ""
					}
					if err := unstructured.SetNestedField(objects[0].Object, value, reference.path...); err != nil {
						t.Fatal(err)
					}
				}
				if err := validateActivationScope(snapshot, fixture.prepared.execution, fixture.client.activationSelected, activationIntegrationDeploymentUID); err == nil {
					t.Fatal("unsupported consumed reference passed complete scope")
				}
			})
		}
	}
	for _, tc := range []struct {
		kind string
		path []string
	}{
		{kind: "DPUSet", path: []string{"spec", "dpuTemplate", "spec", "bfb"}},
		{kind: "DPUSet", path: []string{"spec", "dpuTemplate", "spec", "dpuFlavorTemplate"}},
		{kind: "DPU", path: []string{"spec", "bfb"}},
	} {
		t.Run(tc.kind+" "+strings.Join(tc.path, "/"), func(t *testing.T) {
			snapshot := copyEnrollmentScope(baseline)
			objects := snapshot.objects(tc.kind)
			if len(objects) == 0 {
				t.Fatal("missing child")
			}
			if err := unstructured.SetNestedField(objects[0].Object, "foreign", tc.path...); err != nil {
				t.Fatal(err)
			}
			if err := validateActivationScope(snapshot, fixture.prepared.execution, fixture.client.activationSelected, activationIntegrationDeploymentUID); err == nil {
				t.Fatal("alternative consumed mode passed complete scope")
			}
		})
	}
}

type activationConsumedReferenceClient struct {
	*activationIntegrationFake
	kind    string
	path    []string
	mutated bool
}

func (c *activationConsumedReferenceClient) Create(ctx context.Context, desired *unstructured.Unstructured, dry bool) (*unstructured.Unstructured, error) {
	created, err := c.activationIntegrationFake.Create(ctx, desired, dry)
	if err != nil || desired.GetKind() != "DPUDeployment" {
		return created, err
	}
	objects := c.dpus
	if c.kind == "DPUSet" {
		objects = c.sets
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf("fixture consumed child is missing")
	}
	if err := unstructured.SetNestedField(objects[0].Object, "foreign-consumed-reference", c.path...); err != nil {
		return nil, err
	}
	c.mutated = true
	return created, nil
}

func TestRunActivationRejectsForeignConsumedReferencesBeforeHoldOrReboot(t *testing.T) {
	execution := prepareSoftwareStageExecution(t, []string{"SERIAL-A"})
	for _, tc := range []struct {
		name, kind string
		path       []string
	}{
		{name: "set software", kind: "DPUSet", path: []string{"spec", "dpuTemplate", "spec", "blueFieldSoftware", "name"}},
		{name: "set flavor", kind: "DPUSet", path: []string{"spec", "dpuTemplate", "spec", "dpuFlavor"}},
		{name: "DPU software", kind: "DPU", path: []string{"spec", "blueFieldSoftware"}},
		{name: "DPU flavor", kind: "DPU", path: []string{"spec", "dpuFlavor"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newActivationIntegrationFixtureFromExecution(t, execution, false)
			ctx, cancel := context.WithTimeout(fixture.ctx, 10*time.Second)
			defer cancel()
			client := &activationConsumedReferenceClient{activationIntegrationFake: fixture.client, kind: tc.kind, path: tc.path}
			// Tenant binding must identify the actual wrapper used by this stage.
			fixture.controlPlane.tenant.Client.source = client
			callbacks := 0
			rebooter := RebooterFunc(func(context.Context, ActivationRequest) (ActivationObservation, error) {
				callbacks++
				return ActivationObservation{}, fmt.Errorf("unexpected reboot")
			})
			_, err := runActivationStage(ctx, client, fixture.prepared, fixture.controlPlane, fixture.workload, fixture.inventory, fixture.request, fixture.journal, time.Millisecond, time.Second, rebooter)
			if err == nil || !strings.Contains(err.Error(), "ACTIVATION_SCOPE_CHANGED") || !client.mutated || len(client.patches) != 0 || client.guardCalls != 0 || callbacks != 0 {
				t.Fatalf("foreign consumed reference reached effect: error=%v mutated=%v patches=%v claims=%d reboots=%d", err, client.mutated, client.patches, client.guardCalls, callbacks)
			}
		})
	}
}
