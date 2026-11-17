// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// checkTenantRuntimeReadPermissions checks the tenant identity's namespace-
// scoped list permissions needed to find candidate HBN DaemonSets and Pods.
// It performs authorization reviews only; the caller owns fresh source and
// tenant identity fences around any subsequent reads or effects.
func checkTenantRuntimeReadPermissions(ctx context.Context, tenant *tenantClient, namespace string) []PreflightCheck {
	const checkBase = "tenant.rbac.runtime"
	if err := validateTenantPermissionInputs(ctx, tenant, namespace); err != nil {
		return []PreflightCheck{blockedPreflight(checkBase, "TENANT_PERMISSION_INPUT_INVALID", "Tenant runtime authorization requires a bounded context, tenant client and valid selected namespace.")}
	}
	return checkTenantPermissions(ctx, tenant, []tenantPermissionRequirement{
		{id: checkBase + ".daemonsets.list", attributes: authorizationv1.ResourceAttributes{Verb: "list", Group: "apps", Version: "v1", Resource: "daemonsets", Namespace: namespace}},
		{id: checkBase + ".pods.list", attributes: authorizationv1.ResourceAttributes{Verb: "list", Version: "v1", Resource: "pods", Namespace: namespace}},
	})
}

// checkTenantPodExecPermissions checks only the named Pod read needed to
// revalidate its identity and the named pods/exec create permission. The
// caller remains responsible for validating the Pod UID/owner/node/image and
// tenant/source identity immediately before and after executing it.
func checkTenantPodExecPermissions(ctx context.Context, tenant *tenantClient, namespace, pod string) []PreflightCheck {
	const checkBase = "tenant.rbac.runtime"
	if err := validateTenantPermissionInputs(ctx, tenant, namespace); err != nil || len(validation.IsDNS1123Subdomain(pod)) != 0 {
		return []PreflightCheck{blockedPreflight(checkBase, "TENANT_PERMISSION_INPUT_INVALID", "Tenant Pod authorization requires a bounded context, tenant client and valid selected namespace and Pod name.")}
	}
	return checkTenantPermissions(ctx, tenant, []tenantPermissionRequirement{
		{id: checkBase + ".pod.get", attributes: authorizationv1.ResourceAttributes{Verb: "get", Version: "v1", Resource: "pods", Namespace: namespace, Name: pod}},
		{id: checkBase + ".pod.exec", attributes: authorizationv1.ResourceAttributes{Verb: "create", Version: "v1", Resource: "pods", Subresource: "exec", Namespace: namespace, Name: pod}},
	})
}

type tenantPermissionRequirement struct {
	id         string
	attributes authorizationv1.ResourceAttributes
}

func validateTenantPermissionInputs(ctx context.Context, tenant *tenantClient, namespace string) error {
	if ctx == nil || tenant == nil || tenant.api == nil || namespace == "" || len(validation.IsDNS1123Label(namespace)) != 0 {
		return fmt.Errorf("invalid tenant permission inputs")
	}
	if _, bounded := ctx.Deadline(); !bounded || ctx.Err() != nil {
		return fmt.Errorf("tenant permission context is not active and bounded")
	}
	return nil
}

func checkTenantPermissions(ctx context.Context, tenant *tenantClient, requirements []tenantPermissionRequirement) []PreflightCheck {
	checks := make([]PreflightCheck, 0, len(requirements))
	for _, requirement := range requirements {
		check := blockedPreflight(requirement.id, "TENANT_PERMISSION_REVIEW_UNAVAILABLE", "The tenant API did not return a complete authorization decision.")
		if ctx.Err() != nil {
			check = blockedPreflight(requirement.id, "TENANT_PERMISSION_REVIEW_CANCELED", "Tenant authorization review was canceled.")
			checks = append(checks, check)
			break
		}
		review, err := tenant.api.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &requirement.attributes},
		}, metav1.CreateOptions{})
		if ctx.Err() != nil {
			checks = append(checks, blockedPreflight(requirement.id, "TENANT_PERMISSION_REVIEW_CANCELED", "Tenant authorization review was canceled."))
			break
		}
		if err != nil {
			checks = append(checks, check)
			continue
		}
		if review == nil || review.Status.EvaluationError != "" || (review.Status.Allowed && review.Status.Denied) {
			checks = append(checks, check)
			continue
		}
		if !review.Status.Allowed {
			checks = append(checks, blockedPreflight(requirement.id, "TENANT_PERMISSION_DENIED", "The tenant identity is not authorized for the required runtime operation."))
			continue
		}
		checks = append(checks, passedPreflight(requirement.id, "The tenant identity is authorized for the required runtime operation."))
	}
	return checks
}
