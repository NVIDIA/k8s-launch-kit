// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

// checkSoftwarePermissions checks effect permissions before staging a download.
// Registry observation reads are exercised directly at the stage boundary;
// these reviews do not establish registry readiness or grant lasting access.
func (c *APIClient) checkSoftwarePermissions(ctx context.Context, desired *unstructured.Unstructured, registryNamespace string) []PreflightCheck {
	if c == nil || c.access.Client == nil || ctx == nil || desired == nil ||
		desired.GetAPIVersion() != blueFieldSoftwareAPIVersion || desired.GetKind() != "BlueFieldSoftware" ||
		desired.GetName() == "" || len(validation.IsDNS1123Subdomain(desired.GetName())) != 0 ||
		desired.GetNamespace() == "" || len(validation.IsDNS1123Label(desired.GetNamespace())) != 0 ||
		registryNamespace == "" || len(validation.IsDNS1123Label(registryNamespace)) != 0 {
		return []PreflightCheck{blockedPreflight("caller.rbac.software", "SOFTWARE_PERMISSION_INPUT_INVALID", "Software authorization requires the retained resource, registry namespace and management client.")}
	}
	if err := ctx.Err(); err != nil {
		return []PreflightCheck{blockedPreflight("caller.rbac.software", "PREFLIGHT_CANCELLED", "Software authorization was cancelled before inspection.")}
	}
	requirements := []accessRequirement{
		{verb: "get", group: "provisioning.dpu.nvidia.com", version: "v1alpha1", resource: "bluefieldsoftwares", namespace: desired.GetNamespace(), resourceName: desired.GetName()},
		// Kubernetes cannot authorize an ordinary resource create by name.
		{verb: "create", group: "provisioning.dpu.nvidia.com", version: "v1alpha1", resource: "bluefieldsoftwares", namespace: desired.GetNamespace()},
		{verb: "create", version: "v1", resource: "pods", subresource: "portforward", namespace: registryNamespace, resourceName: softwareRegistryPodName},
	}
	checks := make([]PreflightCheck, 0, len(requirements))
	for _, requirement := range requirements {
		checks = append(checks, c.checkAccess(ctx, requirement))
	}
	return checks
}
