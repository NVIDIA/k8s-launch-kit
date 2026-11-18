// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package controllercontract

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestDPUFlavorAdmissionDefaultsAreMissingOnlyAndExactScope(t *testing.T) {
	for _, value := range []any{nil, false, int64(0), []any{}, map[string]any{"present": false}} {
		spec := map[string]any{}
		for _, field := range []string{"grub", "sysctl", "ovs", "containerdConfig"} {
			spec[field] = value
		}
		object := flavorReconcileDesired(spec)
		before := object.DeepCopy()
		defaultDPUFlavorAdmissionObjects(object)
		if !reflect.DeepEqual(before.Object, object.Object) {
			t.Fatal("canonicalizer overwrote an explicitly present value")
		}
	}
	for _, change := range []func(*unstructured.Unstructured){
		func(o *unstructured.Unstructured) { o.SetKind("DPUFlavorTemplate") },
		func(o *unstructured.Unstructured) { o.SetAPIVersion("provisioning.dpu.nvidia.com/v1alpha2") },
		func(o *unstructured.Unstructured) { delete(o.Object, "spec") },
		func(o *unstructured.Unstructured) { o.Object["spec"] = map[string]any(nil) },
	} {
		object := flavorReconcileDesired(map[string]any{})
		change(object)
		before := object.DeepCopy()
		defaultDPUFlavorAdmissionObjects(object)
		if !reflect.DeepEqual(before.Object, object.Object) {
			t.Fatal("canonicalizer expanded beyond present exact flavor spec")
		}
	}
	defaultDPUFlavorAdmissionObjects(nil)
}

func flavorReconcileDesired(spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUFlavor",
		"metadata": map[string]any{"name": "hbn-flavor", "namespace": "dpf-system"}, "spec": spec,
	}}
}

func TestComparisonCopiesPreservePresenceAndDetachBothInputs(t *testing.T) {
	rules := testRules(t)
	want := flavorReconcileDesired(map[string]any{"grub": nil, "sysctl": map[string]any{"enabled": false}, "ovs": map[string]any{"count": int64(0)}})
	got := want.DeepCopy()
	beforeWant, beforeGot := want.DeepCopy(), got.DeepCopy()
	left, right, err := rules.ComparisonCopies(want, got)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, beforeWant) || !reflect.DeepEqual(got, beforeGot) {
		t.Fatal("projection mutated input")
	}
	for _, object := range []*unstructured.Unstructured{left, right} {
		spec := object.Object["spec"].(map[string]any)
		if value, ok := spec["grub"]; !ok || value != nil {
			t.Fatal("explicit null lost")
		}
		if !reflect.DeepEqual(spec["containerdConfig"], map[string]any{}) {
			t.Fatal("absent object was not normalized")
		}
		spec["sysctl"].(map[string]any)["enabled"] = true
	}
	if !reflect.DeepEqual(want, beforeWant) || !reflect.DeepEqual(got, beforeGot) {
		t.Fatal("returned projection aliases input")
	}
	cluster := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "provisioning.dpu.nvidia.com/v1alpha1", "kind": "DPUCluster", "metadata": map[string]any{"name": "tenant"}, "spec": map[string]any{}}}
	observed := cluster.DeepCopy()
	if err := unstructured.SetNestedField(observed.Object, "tenant-admin-kubeconfig", "spec", "kubeconfig"); err != nil {
		t.Fatal(err)
	}
	_, normalized, err := rules.ComparisonCopies(cluster, observed)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := normalized.Object["spec"].(map[string]any)["kubeconfig"]; ok {
		t.Fatal("controller name was not normalized")
	}
	if _, ok := observed.Object["spec"].(map[string]any)["kubeconfig"]; !ok {
		t.Fatal("observed name was removed from caller input")
	}
	if err := unstructured.SetNestedField(observed.Object, "foreign", "spec", "kubeconfig"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rules.ComparisonCopies(cluster, observed); err == nil {
		t.Fatal("foreign name normalized")
	}
}
