// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const maxCallerPermissionChecks = 2048

// CheckCallerPermissions checks the authenticated identity's scoped
// permissions for the retained HBN management workflow. It does not evaluate
// controller service accounts, tenant-cluster credentials, host or switch
// access, or runtime readiness. A passing result describes the checked
// management-cluster authorization snapshot only.
func (c *APIClient) CheckCallerPermissions(ctx context.Context, request ManagementPreflightRequest) PreflightResult {
	result := PreflightResult{Checks: []PreflightCheck{}, Ready: false}
	if c == nil || c.access.Client == nil || c.dynamic == nil {
		result.Checks = append(result.Checks, blockedPreflight("management.identity", "MANAGEMENT_CLIENT_UNAVAILABLE", "The bound management API client is unavailable."))
		return result
	}
	if ctx == nil {
		result.Checks = append(result.Checks, blockedPreflight("management.identity", "PREFLIGHT_CONTEXT_MISSING", "A management permission-check context is required."))
		return result
	}
	plan, err := managementPreflightPlan(request)
	if err != nil {
		result.Checks = append(result.Checks, blockedPreflight("management.preflight-input", "PREFLIGHT_INPUT_INVALID", "The retained HBN bundle is not valid for caller-permission checks."))
		return result
	}
	if !c.matchesBinding(plan.intent.Management, plan.intent.KubernetesVersion) {
		result.Checks = append(result.Checks, blockedPreflight("management.identity", "MANAGEMENT_BUNDLE_BINDING_MISMATCH", "The authenticated management client does not match the generated bundle identity and Kubernetes version."))
		return result
	}
	if err := ctx.Err(); err != nil {
		result.Checks = append(result.Checks, blockedPreflight("management.identity", "PREFLIGHT_CANCELLED", "Management permission checks were canceled before identity verification."))
		return result
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		if ctx.Err() != nil {
			result.Checks = append(result.Checks, blockedPreflight("management.identity", "PREFLIGHT_CANCELLED", "Management permission checks were canceled before identity verification."))
			return result
		}
		result.Checks = append(result.Checks, blockedPreflight("management.identity", "MANAGEMENT_IDENTITY_UNVERIFIED", "The authenticated management identity could not be matched to the generated bundle."))
		return result
	}
	result.Checks = append(result.Checks, passedPreflight("management.identity", "The authenticated management identity matches the retained bundle."))

	checks := c.checkAllCallerPermissions(ctx, plan)
	result.Checks = append(result.Checks, checks...)
	result.Checks = normalizeCallerPermissionChecks(result.Checks)

	if err := ctx.Err(); err != nil {
		result.Checks = append(result.Checks, blockedPreflight("management.identity-final", "PREFLIGHT_CANCELLED", "Management permission checks were canceled before final identity revalidation."))
	} else if err := c.VerifyIdentity(ctx); err != nil {
		for index := range result.Checks {
			if result.Checks[index].State == "passed" {
				result.Checks[index] = blockedPreflight(result.Checks[index].ID, "MANAGEMENT_IDENTITY_CHANGED", "Management identity changed during permission checks; discard this observation and retry.")
			}
		}
		result.Checks = append(result.Checks, blockedPreflight("management.identity-final", "MANAGEMENT_IDENTITY_CHANGED", "Management identity changed during permission checks; discard this observation and retry."))
	}
	result.Ready = len(result.Checks) > 0
	for _, check := range result.Checks {
		if check.State != "passed" {
			result.Ready = false
			break
		}
	}
	return result
}

// checkBootstrapCallerPermissions is shared by the management bootstrap
// preflight and the complete retained-plan permission check. Later-stage
// permissions are deliberately excluded from bootstrap so an incomplete
// enrollment grant does not block the earlier operator install stage.
func (c *APIClient) checkBootstrapCallerPermissions(ctx context.Context, plan preflightPlan) []PreflightCheck {
	requirements, checks := c.bootstrapCallerAccessRequirements(ctx, plan)
	checks = append(checks, c.checkAccessRequirements(ctx, requirements)...)
	if ctx.Err() != nil {
		if !hasPreflightCancellation(checks) {
			checks = append(checks, blockedPreflight("caller.permission-cancelled", "PREFLIGHT_CANCELLED", "Management permission checks were canceled before RBAC privilege reviews completed."))
		}
		return checks
	}
	checks = append(checks, c.checkProjectedRBACPermissions(ctx, plan)...)
	return checks
}

func (c *APIClient) bootstrapCallerAccessRequirements(ctx context.Context, plan preflightPlan) (map[accessRequirement]struct{}, []PreflightCheck) {
	requirements := map[accessRequirement]struct{}{}
	checks := make([]PreflightCheck, 0)
	for _, namespace := range plan.namespaces {
		requirements[accessRequirement{verb: "get", version: "v1", resource: "namespaces", resourceName: namespace.name}] = struct{}{}
		requirements[accessRequirement{verb: "create", version: "v1", resource: "namespaces"}] = struct{}{}
	}
	permissions, err := c.projectedOperatorPermissions(ctx, plan)
	if err != nil {
		checks = append(checks, blockedPreflight("caller.rbac.operator-projection", "OPERATOR_PROJECTION_UNAVAILABLE", "The retained operator projection could not be mapped to APIs."))
	} else {
		for _, permission := range permissions {
			requirements[permission] = struct{}{}
		}
	}
	for _, verb := range []string{"get", "list", "create", "update"} {
		requirements[accessRequirement{verb: verb, version: "v1", resource: "secrets", namespace: plan.operatorNamespace}] = struct{}{}
	}
	addDiscoveryPaths(requirements, permissionDiscoveryPaths(plan, false))
	return requirements, checks
}

func (c *APIClient) checkAllCallerPermissions(ctx context.Context, plan preflightPlan) []PreflightCheck {
	requirements, checks := c.bootstrapCallerAccessRequirements(ctx, plan)
	operatorNamespace := plan.operatorNamespace
	for _, requirement := range []accessRequirement{
		{verb: "list", group: "operator.dpu.nvidia.com", version: "v1alpha1", resource: "dpfoperatorconfigs"},
		{verb: "get", group: "operator.dpu.nvidia.com", version: "v1alpha1", resource: "dpfoperatorconfigs", namespace: operatorNamespace},
		{verb: "create", group: "operator.dpu.nvidia.com", version: "v1alpha1", resource: "dpfoperatorconfigs", namespace: operatorNamespace},
	} {
		requirements[requirement] = struct{}{}
	}

	objects := make([]*unstructured.Unstructured, 0)
	for _, stage := range plan.stages {
		metadata := stage.Stage()
		if metadata.Destination != bundle.Management {
			checks = append(checks, blockedPreflight("caller.rbac.retained-stage", "RETAINED_STAGE_DESTINATION_UNSUPPORTED", "A retained stage targets a cluster outside the bound management API."))
			continue
		}
		if metadata.ID == generation.StageNamespaces || metadata.ID == generation.StageOperator || metadata.ID == generation.StageOperatorConfig {
			continue
		}
		for _, document := range stage.Documents() {
			object := document.ObjectCopy()
			if object == nil {
				return []PreflightCheck{blockedPreflight("caller.rbac.retained-stage", "RETAINED_RESOURCE_INVALID", "A retained management-stage resource could not be inspected.")}
			}
			objects = append(objects, object)
		}
	}
	permissions, err := retainedStageAccessRequirements(plan, objects)
	if err != nil {
		checks = append(checks, blockedPreflight("caller.rbac.retained-stage", "RETAINED_RESOURCE_MAPPING_UNAVAILABLE", "A retained management-stage API kind could not be mapped from the pinned DPF schemas."))
	} else {
		for _, permission := range permissions {
			requirements[permission] = struct{}{}
		}
	}
	for _, requirement := range enrollmentAccessRequirements(plan) {
		requirements[requirement] = struct{}{}
	}
	for _, requirement := range managementObservationAccessRequirements(plan) {
		requirements[requirement] = struct{}{}
	}
	if c.access.Namespace == "" {
		checks = append(checks, blockedPreflight("caller.permission.probe-namespace", "MANAGEMENT_NAMESPACE_UNAVAILABLE", "The configured management namespace is required for bounded connectivity observations."))
	} else {
		for _, requirement := range []accessRequirement{
			{verb: "get", version: "v1", resource: "namespaces", resourceName: c.access.Namespace},
			{verb: "get", version: "v1", resource: "configmaps", namespace: c.access.Namespace},
			{verb: "create", version: "v1", resource: "configmaps", namespace: c.access.Namespace},
			{verb: "delete", version: "v1", resource: "configmaps", namespace: c.access.Namespace},
			{verb: "get", version: "v1", resource: "pods", namespace: c.access.Namespace},
			{verb: "create", version: "v1", resource: "pods", namespace: c.access.Namespace},
			{verb: "delete", version: "v1", resource: "pods", namespace: c.access.Namespace},
			{verb: "get", version: "v1", resource: "pods", subresource: "log", namespace: c.access.Namespace},
			{verb: "get", version: "v1", resource: "nodes"},
		} {
			requirements[requirement] = struct{}{}
		}
	}
	addDiscoveryPaths(requirements, permissionDiscoveryPaths(plan, true))
	if len(requirements) > maxCallerPermissionChecks {
		checks = append(checks, blockedPreflight("caller.permission-bound", "CALLER_PERMISSION_CHECKS_UNBOUNDED", "The required management permission set exceeds the supported check bound."))
	} else {
		checks = append(checks, c.checkAccessRequirements(ctx, requirements)...)
	}
	if ctx.Err() != nil {
		if !hasPreflightCancellation(checks) {
			checks = append(checks, blockedPreflight("caller.permission-cancelled", "PREFLIGHT_CANCELLED", "Management permission checks were canceled before RBAC privilege reviews completed."))
		}
		return checks
	}
	rbacProjection := append(append([]*unstructured.Unstructured(nil), plan.projection...), objects...)
	checks = append(checks, c.checkProjectedRBACPermissions(ctx, preflightPlan{projection: rbacProjection})...)
	return checks
}

func hasPreflightCancellation(checks []PreflightCheck) bool {
	for _, check := range checks {
		if check.ReasonCode == "PREFLIGHT_CANCELLED" {
			return true
		}
	}
	return false
}

func retainedStageAccessRequirements(plan preflightPlan, objects []*unstructured.Unstructured) ([]accessRequirement, error) {
	definitions := projectedCRDDefinitions(plan.projection)
	requirements := map[accessRequirement]struct{}{}
	for _, object := range objects {
		if object == nil || object.GetAPIVersion() == "" || object.GetKind() == "" || object.GetName() == "" {
			return nil, fmt.Errorf("invalid retained object")
		}
		gvk := object.GroupVersionKind()
		definition, found := definitions[gvk.GroupVersion().String()+"/"+gvk.Kind]
		if !found || definition.resource == "" {
			return nil, fmt.Errorf("retained kind has no pinned CRD mapping")
		}
		if definition.namespaced && object.GetNamespace() == "" || !definition.namespaced && object.GetNamespace() != "" {
			return nil, fmt.Errorf("retained resource namespace differs from pinned CRD scope")
		}
		for _, verb := range []string{"get", "create"} {
			requirements[accessRequirement{verb: verb, group: gvk.Group, version: gvk.Version, resource: definition.resource, namespace: object.GetNamespace(), resourceName: accessName(verb, object.GetName())}] = struct{}{}
		}
	}
	// The DPUCluster list is an all-namespace collision check performed before
	// creating or reusing the selected tenant object.
	requirements[accessRequirement{verb: "list", group: "provisioning.dpu.nvidia.com", version: "v1alpha1", resource: "dpuclusters"}] = struct{}{}
	return sortedAccessRequirements(requirements), nil
}

func accessName(verb, name string) string {
	if verb == "get" {
		return name
	}
	// Kubernetes create authorization does not support a resource name.
	return ""
}

func managementObservationAccessRequirements(plan preflightPlan) []accessRequirement {
	requirements := map[accessRequirement]struct{}{}
	add := func(requirement accessRequirement) { requirements[requirement] = struct{}{} }
	for _, api := range pinnedHBNAPIs() {
		add(accessRequirement{verb: "get", group: "apiextensions.k8s.io", version: "v1", resource: "customresourcedefinitions", resourceName: api.crd})
	}
	for _, workload := range pinnedHBNWorkloads(plan.overrides, plan.requireNFDWorker) {
		resource := map[string]string{"Deployment": "deployments", "StatefulSet": "statefulsets", "DaemonSet": "daemonsets"}[workload.kind]
		add(accessRequirement{verb: "list", group: "apps", version: "v1", resource: resource, namespace: workload.namespace})
	}
	add(accessRequirement{verb: "list", group: "apps", version: "v1", resource: "statefulsets", namespace: plan.operatorNamespace})
	add(accessRequirement{verb: "get", group: "admissionregistration.k8s.io", version: "v1", resource: "validatingwebhookconfigurations", resourceName: "cert-manager-webhook"})
	add(accessRequirement{verb: "get", group: "admissionregistration.k8s.io", version: "v1", resource: "mutatingwebhookconfigurations", resourceName: "cert-manager-webhook"})

	// This is exactly the current collector read surface. Some names are
	// discovered from the StatefulSet, bound PVCs or Pod volume; authorization
	// is therefore checked at namespace scope for those dynamic objects.
	kamaji := plan.overrides.Kamaji
	add(accessRequirement{verb: "get", group: "kamaji.clastix.io", version: "v1alpha1", resource: "datastores", resourceName: "default"})
	add(accessRequirement{verb: "get", group: "apps", version: "v1", resource: "statefulsets", namespace: kamaji, resourceName: "kamaji-etcd"})
	add(accessRequirement{verb: "get", version: "v1", resource: "services", namespace: kamaji})
	add(accessRequirement{verb: "get", version: "v1", resource: "persistentvolumeclaims", namespace: kamaji})
	add(accessRequirement{verb: "get", version: "v1", resource: "persistentvolumes"})
	add(accessRequirement{verb: "get", version: "v1", resource: "pods", namespace: kamaji})
	add(accessRequirement{verb: "get", group: "storage.k8s.io", version: "v1", resource: "storageclasses", resourceName: "local-path"})
	add(accessRequirement{verb: "get", group: "apps", version: "v1", resource: "deployments", namespace: "local-path-provisioner", resourceName: "local-path-provisioner"})

	operatorNamespace, tenant := plan.operatorNamespace, plan.intent.Bootstrap.Tenant
	add(accessRequirement{verb: "get", version: "v1", resource: "namespaces", resourceName: "kube-system"})
	add(accessRequirement{verb: "get", group: "provisioning.dpu.nvidia.com", version: "v1alpha1", resource: "dpuclusters", namespace: tenant.Namespace, resourceName: tenant.Name})
	add(accessRequirement{verb: "get", group: "kamaji.clastix.io", version: "v1alpha1", resource: "tenantcontrolplanes", namespace: tenant.Namespace, resourceName: tenant.Name})
	add(accessRequirement{verb: "get", version: "v1", resource: "services", namespace: tenant.Namespace, resourceName: tenant.Name})
	add(accessRequirement{verb: "get", version: "v1", resource: "configmaps", namespace: tenant.Namespace, resourceName: tenant.Name + "-keepalived"})
	add(accessRequirement{verb: "get", group: "apps", version: "v1", resource: "daemonsets", namespace: tenant.Namespace, resourceName: tenant.Name + "-keepalived"})
	add(accessRequirement{verb: "get", version: "v1", resource: "secrets", namespace: tenant.Namespace, resourceName: tenant.Name + "-admin-kubeconfig"})
	add(accessRequirement{verb: "get", version: "v1", resource: "pods", namespace: operatorNamespace})
	add(accessRequirement{verb: "get", group: "coordination.k8s.io", version: "v1", resource: "leases", namespace: operatorNamespace, resourceName: softwareLeaderLeaseName})
	add(accessRequirement{verb: "get", version: "v1", resource: "configmaps", namespace: operatorNamespace})
	add(accessRequirement{verb: "get", version: "v1", resource: "persistentvolumeclaims", namespace: operatorNamespace})
	add(accessRequirement{verb: "list", version: "v1", resource: "nodes"})
	add(accessRequirement{verb: "list", group: "storage.k8s.io", version: "v1", resource: "storageclasses"})
	add(accessRequirement{verb: "get", version: "v1", resource: "pods", namespace: operatorNamespace, resourceName: softwareRegistryPodName})
	return sortedAccessRequirements(requirements)
}

func enrollmentAccessRequirements(plan preflightPlan) []accessRequirement {
	operatorNamespace, tenant := plan.operatorNamespace, plan.intent.Bootstrap.Tenant
	requirements := []accessRequirement{
		{verb: "get", version: "v1", resource: "secrets", namespace: operatorNamespace, resourceName: enrollmentSecretName},
		{verb: "create", version: "v1", resource: "secrets", namespace: operatorNamespace},
		{verb: "create", version: "v1", resource: "pods", subresource: "portforward", namespace: operatorNamespace, resourceName: softwareRegistryPodName},
	}
	for _, resource := range []string{"dpudiscoveries", "dpudevices", "dpunodes", "dpus", "dpusets", "dpudeployments"} {
		group := "provisioning.dpu.nvidia.com"
		if resource == "dpudeployments" {
			group = "svc.dpu.nvidia.com"
		}
		requirements = append(requirements, accessRequirement{verb: "list", group: group, version: "v1alpha1", resource: resource})
	}
	for _, device := range plan.intent.Devices {
		name := strings.ToLower(device.Serial)
		requirements = append(requirements,
			accessRequirement{verb: "get", group: "provisioning.dpu.nvidia.com", version: "v1alpha1", resource: "dpudevices", namespace: operatorNamespace, resourceName: name},
			accessRequirement{verb: "get", group: "provisioning.dpu.nvidia.com", version: "v1alpha1", resource: "dpunodes", namespace: operatorNamespace, resourceName: "dpu-node-" + name},
			accessRequirement{verb: "get", version: "v1", resource: "secrets", namespace: tenant.Namespace, resourceName: tenant.Name + "-admin-kubeconfig"},
		)
	}
	return requirements
}

func addDiscoveryPaths(requirements map[accessRequirement]struct{}, paths []string) {
	for _, path := range paths {
		requirements[accessRequirement{verb: "get", nonResourceURL: path}] = struct{}{}
	}
}

func permissionDiscoveryPaths(plan preflightPlan, includeStages bool) []string {
	groups := map[string]struct{}{"/version": {}, "/apis": {}, "/api/v1": {}}
	add := func(object *unstructured.Unstructured) {
		if object == nil || object.GroupVersionKind().Version == "" {
			return
		}
		gvk := object.GroupVersionKind()
		if gvk.Group == "" {
			groups["/api/"+gvk.Version] = struct{}{}
		} else {
			groups["/apis/"+gvk.GroupVersion().String()] = struct{}{}
		}
	}
	for _, object := range plan.projection {
		add(object)
	}
	for _, api := range pinnedHBNAPIs() {
		groups["/apis/"+api.group+"/"+api.version] = struct{}{}
	}
	if includeStages {
		for _, stage := range plan.stages {
			if stage.Stage().Destination != bundle.Management {
				continue
			}
			for _, document := range stage.Documents() {
				add(document.ObjectCopy())
			}
		}
	}
	result := make([]string, 0, len(groups))
	for path := range groups {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func (c *APIClient) checkAccessRequirements(ctx context.Context, requirements map[accessRequirement]struct{}) []PreflightCheck {
	ordered := sortedAccessRequirements(requirements)
	if len(ordered) > maxCallerPermissionChecks {
		return []PreflightCheck{blockedPreflight("caller.permission-bound", "CALLER_PERMISSION_CHECKS_UNBOUNDED", "The required management permission set exceeds the supported check bound.")}
	}
	checks := make([]PreflightCheck, 0, len(ordered))
	for _, requirement := range ordered {
		if err := ctx.Err(); err != nil {
			checks = append(checks, blockedPreflight("caller.permission-cancelled", "PREFLIGHT_CANCELLED", "Management permission checks were canceled before all scoped reviews completed."))
			return checks
		}
		requirement.checkID = callerPermissionCheckID(requirement)
		checks = append(checks, c.checkAccess(ctx, requirement))
	}
	return checks
}

func callerPermissionCheckID(requirement accessRequirement) string {
	if requirement.nonResourceURL != "" {
		return "caller.permission.nonresource:" + requirement.verb + ":" + requirement.nonResourceURL
	}
	namespace := requirement.namespace
	if namespace == "" {
		namespace = "_cluster"
	}
	groupVersion := requirement.group + "/" + requirement.version
	if requirement.group == "" {
		groupVersion = "core/" + requirement.version
	}
	name := requirement.resourceName
	if name == "" {
		name = "_any"
	}
	resource := requirement.resource
	if requirement.subresource != "" {
		resource += "/" + requirement.subresource
	}
	return strings.Join([]string{"caller.permission.resource", requirement.verb, namespace, groupVersion, resource, name}, ":")
}

func sortedAccessRequirements(requirements map[accessRequirement]struct{}) []accessRequirement {
	result := make([]accessRequirement, 0, len(requirements))
	for requirement := range requirements {
		result = append(result, requirement)
	}
	sort.Slice(result, func(i, j int) bool {
		return accessRequirementKey(result[i]) < accessRequirementKey(result[j])
	})
	return result
}

func accessRequirementKey(requirement accessRequirement) string {
	return strings.Join([]string{requirement.verb, requirement.namespace, requirement.group, requirement.version,
		requirement.resource, requirement.subresource, requirement.resourceName, requirement.nonResourceURL}, "\x00")
}

func normalizeCallerPermissionChecks(checks []PreflightCheck) []PreflightCheck {
	seen := map[string]bool{}
	for index := range checks {
		check := &checks[index]
		if check.ID == "" || seen[check.ID] {
			check.ID = fmt.Sprintf("caller.permission-result-invalid.%d", index)
			check.State = "blocked"
			check.ReasonCode = "CALLER_PERMISSION_RESULT_INVALID"
			check.Message = "The management permission result set contains a missing or duplicate check identity."
			seen[check.ID] = true
			continue
		}
		seen[check.ID] = true
		if check.ReasonCode == "" {
			if check.State == "passed" {
				check.ReasonCode = "CALLER_PERMISSION_VERIFIED"
			} else {
				check.ReasonCode = "CALLER_PERMISSION_UNVERIFIED"
			}
		}
		if check.Message == "" {
			check.Message = "The management permission result is incomplete."
			if check.State == "passed" {
				check.Message = "The configured identity has the required scoped permission."
			}
		}
	}
	return checks
}
