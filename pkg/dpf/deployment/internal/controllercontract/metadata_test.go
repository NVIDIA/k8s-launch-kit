// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package controllercontract

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestControllerMetadataPolicyIsFiniteAndDoesNotMutateInputs(t *testing.T) {
	deployment := activationDeploymentIntent(map[string]string{"tenant.example/id": "tenant-a"})
	software := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "BlueFieldSoftware", "metadata": map[string]any{"name": "hbn-software", "namespace": "dpf"}}}
	devices := []generation.ResolvedDevice{resolvedActivationDevice("SERIAL-A", "serial-a"), resolvedActivationDevice("SERIAL-B", "serial-b")}
	beforeDeployment, beforeSoftware := deployment.DeepCopy(), software.DeepCopy()
	beforeDevices := []generation.ResolvedDevice{resolvedActivationDevice("SERIAL-A", "serial-a"), resolvedActivationDevice("SERIAL-B", "serial-b")}
	policy, err := testRules(t).CompileMetadata(deployment, software, devices)
	if err != nil {
		t.Fatal(err)
	}
	want := map[softwareIdentity]map[string]struct{}{softwareIdentity{namespace: "dpf", name: "hbn-software"}: {"provisioning.dpu.nvidia.com/bfs-dpuset-hbn-a": {}, "provisioning.dpu.nvidia.com/bfs-dpuset-hbn-b": {}}}
	if !reflect.DeepEqual(policy.tokens, want) || !reflect.DeepEqual(deployment.Object, beforeDeployment.Object) || !reflect.DeepEqual(software.Object, beforeSoftware.Object) || !reflect.DeepEqual(devices, beforeDevices) {
		t.Fatal("policy differs from exact selected set names or mutated its inputs")
	}
	for _, tc := range []struct {
		name, namespace, software, kind string
		tokens                          []string
		accepted                        bool
	}{
		{name: "both selected sets", namespace: "dpf", software: "hbn-software", kind: "BlueFieldSoftware", tokens: []string{"provisioning.dpu.nvidia.com/bluefieldsoftware-protection", "dpu.nvidia.com/dpudeployment", "provisioning.dpu.nvidia.com/bfs-dpuset-hbn-a", "provisioning.dpu.nvidia.com/bfs-dpuset-hbn-b"}, accepted: true},
		{name: "other namespace", namespace: "other", software: "hbn-software", kind: "BlueFieldSoftware", tokens: []string{"provisioning.dpu.nvidia.com/bfs-dpuset-hbn-a"}, accepted: false},
		{name: "other software", namespace: "dpf", software: "other", kind: "BlueFieldSoftware", tokens: []string{"provisioning.dpu.nvidia.com/bfs-dpuset-hbn-a"}, accepted: false},
		{name: "other set", namespace: "dpf", software: "hbn-software", kind: "BlueFieldSoftware", tokens: []string{"provisioning.dpu.nvidia.com/bfs-dpuset-other-a"}, accepted: false},
		{name: "similar set prefix", namespace: "dpf", software: "hbn-software", kind: "BlueFieldSoftware", tokens: []string{"provisioning.dpu.nvidia.com/bfs-dpuset-hbn-a-extra"}, accepted: false},
		{name: "other token prefix", namespace: "dpf", software: "hbn-software", kind: "BlueFieldSoftware", tokens: []string{"foreign.example/bfs-dpuset-hbn-a"}, accepted: false},
		{name: "duplicate exact token", namespace: "dpf", software: "hbn-software", kind: "BlueFieldSoftware", tokens: []string{"provisioning.dpu.nvidia.com/bfs-dpuset-hbn-a", "provisioning.dpu.nvidia.com/bfs-dpuset-hbn-a"}, accepted: false},
		{name: "cross kind", namespace: "dpf", software: "hbn-software", kind: "DPUFlavor", tokens: []string{"provisioning.dpu.nvidia.com/bfs-dpuset-hbn-a"}, accepted: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := software.DeepCopy()
			object.SetNamespace(tc.namespace)
			object.SetName(tc.software)
			object.SetKind(tc.kind)
			object.SetFinalizers(tc.tokens)
			if err := testRules(t).ValidateMetadata(object, policy); (err == nil) != tc.accepted {
				t.Fatalf("metadata accepted=%v, want %v", err == nil, tc.accepted)
			}
		})
	}
	for _, tc := range []struct {
		name string
		edit func(*unstructured.Unstructured)
	}{
		{name: "wrong namespace", edit: func(o *unstructured.Unstructured) { o.SetNamespace("other") }},
		{name: "wrong software", edit: func(o *unstructured.Unstructured) { o.SetName("other") }},
		{name: "wrong kind", edit: func(o *unstructured.Unstructured) { o.SetKind("BFB") }},
	} {
		t.Run("compile "+tc.name, func(t *testing.T) {
			object := software.DeepCopy()
			tc.edit(object)
			if _, err := testRules(t).CompileMetadata(deployment, object, devices); err == nil {
				t.Fatal("unbound software policy compiled")
			}
		})
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

func TestMetadataProjectionDoesNotRetainCallerInputs(t *testing.T) {
	rules := testRules(t)
	deployment := activationDeploymentIntent(map[string]string{"tenant.example/id": "tenant-a"})
	software := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "BlueFieldSoftware", "metadata": map[string]any{"name": "hbn-software", "namespace": "dpf"}}}
	devices := []generation.ResolvedDevice{resolvedActivationDevice("SERIAL-A", "serial-a"), resolvedActivationDevice("SERIAL-B", "serial-b")}
	policy, err := rules.CompileMetadata(deployment, software, devices)
	if err != nil {
		t.Fatal(err)
	}
	deployment.SetName("changed")
	software.SetName("changed")
	devices[0].SelectorLabels["inventory.example/serial"] = "changed"
	current := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "BlueFieldSoftware", "metadata": map[string]any{"name": "hbn-software", "namespace": "dpf"}}}
	current.SetFinalizers([]string{"provisioning.dpu.nvidia.com/bfs-dpuset-hbn-a", "provisioning.dpu.nvidia.com/bfs-dpuset-hbn-b"})
	if err := rules.ValidateMetadata(current, policy); err != nil {
		t.Fatal("source mutation altered compiled tokens")
	}
	current.SetName("changed")
	if err := rules.ValidateMetadata(current, policy); err == nil {
		t.Fatal("source mutation broadened compiled identity")
	}
}

func TestGeneratedDPUNameUsesPinnedPrefixTruncation(t *testing.T) {
	rules := testRules(t)
	if got := rules.GeneratedDPUEName("node-a", "serial-a"); got != "node-a-serial-a" {
		t.Fatalf("generated name=%q", got)
	}
	if got := rules.GeneratedDPUEName(strings.Repeat("n", 70), "serial-a"); got != strings.Repeat("n", 54)+"-serial-a" {
		t.Fatalf("truncated generated name=%q", got)
	}
}
