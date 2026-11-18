// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

func dpfObject(kind string, status map[string]any) *unstructured.Unstructured {
	apiVersion := "provisioning.dpu.nvidia.com/v1alpha1"
	if kind == "DPFOperatorConfig" {
		apiVersion = "operator.dpu.nvidia.com/v1alpha1"
	}
	if kind == "DPUDeployment" {
		apiVersion = "svc.dpu.nvidia.com/v1alpha1"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": "test-resource", "generation": int64(2)},
		"status":   status,
	}}
}

func conditionMap(conditionType, status, reason string, observedGeneration int64) map[string]any {
	condition := map[string]any{"type": conditionType, "status": status, "reason": reason}
	if observedGeneration > 0 {
		condition["observedGeneration"] = observedGeneration
	}
	return condition
}
