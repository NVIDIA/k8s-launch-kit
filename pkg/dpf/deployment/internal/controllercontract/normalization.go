// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package controllercontract

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ComparisonCopies applies controller wire normalization only after caller schema validation.
func (r *Rules) ComparisonCopies(desired, current *unstructured.Unstructured) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	if !r.valid() || desired == nil || current == nil {
		return nil, nil, fmt.Errorf("selected rules and comparison objects are required")
	}
	want, got := desired.DeepCopy(), current.DeepCopy()
	if err := allowDPUClusterKubeconfig(want, got); err != nil {
		return nil, nil, err
	}
	defaultDPUFlavorAdmissionObjects(want)
	defaultDPUFlavorAdmissionObjects(got)
	return want, got, nil
}

// DPF 26.8's no-op DPUFlavor webhook still serializes these nonpointer
// value-struct sections as {}. Compare that wire representation only after
// schema validation/pruning checks; never overwrite a present value, including
// null. This is intentionally not general webhook or empty-object normalization.

func defaultDPUFlavorAdmissionObjects(object *unstructured.Unstructured) {
	if object == nil || object.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" || object.GetKind() != "DPUFlavor" {
		return
	}
	spec, ok := object.Object["spec"].(map[string]any)
	if !ok || spec == nil {
		return
	}
	for _, field := range []string{"grub", "sysctl", "ovs", "containerdConfig"} {
		if _, present := spec[field]; !present {
			spec[field] = map[string]any{}
		}
	}
}

func allowDPUClusterKubeconfig(desired, current *unstructured.Unstructured) error {
	if desired.GetAPIVersion() != "provisioning.dpu.nvidia.com/v1alpha1" || desired.GetKind() != "DPUCluster" {
		return nil
	}
	_, wantFound, err := unstructured.NestedString(desired.Object, "spec", "kubeconfig")
	if err != nil {
		return fmt.Errorf("read desired DPUCluster kubeconfig: %w", err)
	}
	got, gotFound, err := unstructured.NestedString(current.Object, "spec", "kubeconfig")
	if err != nil {
		return fmt.Errorf("read current DPUCluster kubeconfig: %w", err)
	}
	if !wantFound && gotFound {
		expected := desired.GetName() + "-admin-kubeconfig"
		if got != expected {
			return fmt.Errorf("DPUCluster kubeconfig differs from deterministic controller-owned Secret name")
		}
		unstructured.RemoveNestedField(current.Object, "spec", "kubeconfig")
	}
	return nil
}
