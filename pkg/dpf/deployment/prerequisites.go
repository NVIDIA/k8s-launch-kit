// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	preflightNamespaceStage      = "management-namespaces"
	preflightOperatorStage       = "dpf-operator-helm"
	preflightOperatorConfigStage = "dpf-operator-config"
	maxPreflightNamespaces       = 32
	maxPreflightProjection       = 2048
	maxPreflightRBACObjects      = 128
	maxPreflightRBACRules        = 256
)

// ExternalNamespaceOverrides changes only where the pinned external
// prerequisites are observed. Empty fields use the DPF HBN profile defaults;
// no field disables a required dependency.
type ExternalNamespaceOverrides struct {
	CertManager string
	ArgoCD      string
	NodeFeature string
	Maintenance string
	Kamaji      string
}

// ManagementPreflightRequest binds prerequisite checks and caller RBAC checks
// to one immutable HBN generation bundle.
type ManagementPreflightRequest struct {
	Bundle     *bundle.Bundle
	Namespaces ExternalNamespaceOverrides
}

// PreflightCheck records one bounded prerequisite or authorization result.
type PreflightCheck struct {
	ID         string `json:"id" yaml:"id"`
	State      string `json:"state" yaml:"state"`
	ReasonCode string `json:"reasonCode,omitempty" yaml:"reasonCode,omitempty"`
	Message    string `json:"message" yaml:"message"`
}

// PreflightResult reports whether external management prerequisites and the
// exact namespace/operator install permissions are ready before any effects.
type PreflightResult struct {
	Ready  bool             `json:"ready" yaml:"ready"`
	Checks []PreflightCheck `json:"checks" yaml:"checks"`
}

type preflightNamespace struct {
	name string
}

type preflightAPIRequirement struct {
	id, crd, group, version, resource, kind string
	namespaced                              bool
}

type preflightWorkload struct {
	id, namespace, kind, selector string
}

type accessRequirement struct {
	verb, group, version, resource, namespace, resourceName string
}

type preflightPlan struct {
	operatorNamespace string
	namespaces        []preflightNamespace
	projection        []*unstructured.Unstructured
	overrides         ExternalNamespaceOverrides
	requireNFDWorker  bool
}

// CheckManagementPreflight reads the required external APIs/controllers and
// checks scoped SelfSubjectAccessReview permissions before namespace or Helm
// effects. Its dependency set comes from the pinned HBN profile, while the
// resources and namespaces for caller permissions come from the retained
// bundle's fixed stages and rendered operator projection.
func (c *APIClient) CheckManagementPreflight(ctx context.Context, request ManagementPreflightRequest) PreflightResult {
	result := PreflightResult{Checks: []PreflightCheck{}, Ready: false}
	if c == nil || c.access.Client == nil || c.dynamic == nil {
		result.Checks = append(result.Checks, blockedPreflight("management.identity", "MANAGEMENT_CLIENT_UNAVAILABLE", "The bound management API client is unavailable."))
		return result
	}
	plan, err := managementPreflightPlan(request)
	if err != nil {
		result.Checks = append(result.Checks, blockedPreflight("management.preflight-input", "PREFLIGHT_INPUT_INVALID", err.Error()))
		return result
	}
	if ctx == nil {
		result.Checks = append(result.Checks, blockedPreflight("management.identity", "PREFLIGHT_CONTEXT_MISSING", "A management preflight context is required."))
		return result
	}
	if err := c.VerifyIdentity(ctx); err != nil {
		result.Checks = append(result.Checks, blockedPreflight("management.identity", "MANAGEMENT_IDENTITY_UNVERIFIED", "The authenticated management identity does not match the generated bundle."))
		return result
	}
	result.Checks = append(result.Checks, passedPreflight("management.identity", "The authenticated management identity matches the generated bundle."))

	for _, dependency := range pinnedHBNAPIs() {
		result.Checks = append(result.Checks, c.checkPreflightAPI(ctx, dependency)...)
	}
	for _, workload := range pinnedHBNWorkloads(plan.overrides, plan.requireNFDWorker) {
		result.Checks = append(result.Checks, c.checkPreflightWorkload(ctx, workload))
	}
	result.Checks = append(result.Checks, c.checkCertManagerWebhookCA(ctx))
	result.Checks = append(result.Checks, c.checkArgoApplicationScope(ctx, plan.operatorNamespace, plan.overrides.ArgoCD))

	for _, namespace := range plan.namespaces {
		result.Checks = append(result.Checks, c.checkNamespacePermissions(ctx, namespace))
	}
	permissions, permissionErr := c.projectedOperatorPermissions(ctx, plan)
	if permissionErr != nil {
		result.Checks = append(result.Checks, blockedPreflight("caller.rbac.operator-projection", "OPERATOR_PROJECTION_UNAVAILABLE", "The retained operator projection could not be mapped to served APIs."))
	} else {
		for _, permission := range permissions {
			result.Checks = append(result.Checks, c.checkAccess(ctx, permission))
		}
	}
	for _, verb := range []string{"get", "list", "create", "update"} {
		result.Checks = append(result.Checks, c.checkAccess(ctx, accessRequirement{verb: verb, resource: "secrets", namespace: plan.operatorNamespace}))
	}
	result.Checks = append(result.Checks, c.checkProjectedRBACPermissions(ctx, plan)...)

	if err := ctx.Err(); err != nil {
		result.Checks = append(result.Checks, blockedPreflight("management.identity", "PREFLIGHT_CANCELLED", "Management preflight was cancelled before identity revalidation."))
	} else if err := c.VerifyIdentity(ctx); err != nil {
		for index := range result.Checks {
			if result.Checks[index].State == "passed" {
				result.Checks[index] = blockedPreflight(result.Checks[index].ID, "MANAGEMENT_IDENTITY_CHANGED", "Management identity changed during prerequisite inspection; discard this observation and retry.")
			}
		}
		result.Checks = append(result.Checks, blockedPreflight("management.identity-final", "MANAGEMENT_IDENTITY_CHANGED", "Management identity changed during prerequisite inspection; discard this observation and retry."))
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

func managementPreflightPlan(request ManagementPreflightRequest) (preflightPlan, error) {
	if request.Bundle == nil {
		return preflightPlan{}, fmt.Errorf("an immutable generated bundle is required")
	}
	index := request.Bundle.Index()
	if index.Target != bundle.Target || index.Phase != bundle.Phase || index.Profile != "hbn-bgp" || index.Release != "26.8.0" {
		return preflightPlan{}, fmt.Errorf("management preflight requires a generated DPF HBN-BGP bundle")
	}
	namespaceStage, ok := request.Bundle.Stage(preflightNamespaceStage)
	if !ok {
		return preflightPlan{}, fmt.Errorf("the generated namespace stage is missing")
	}
	operatorStage, ok := request.Bundle.Stage(preflightOperatorStage)
	if !ok || operatorStage.Stage().Namespace == "" {
		return preflightPlan{}, fmt.Errorf("the generated operator Helm stage is missing its namespace")
	}
	namespaces := make([]preflightNamespace, 0, len(namespaceStage.Documents()))
	seenNamespaces := map[string]bool{}
	for _, document := range namespaceStage.Documents() {
		object := document.ObjectCopy()
		if object == nil || object.GetKind() != "Namespace" || object.GetName() == "" || object.GetNamespace() != "" {
			return preflightPlan{}, fmt.Errorf("the generated namespace stage contains an invalid resource")
		}
		if seenNamespaces[object.GetName()] {
			return preflightPlan{}, fmt.Errorf("the generated namespace stage contains duplicate names")
		}
		seenNamespaces[object.GetName()] = true
		if len(namespaces) >= maxPreflightNamespaces {
			return preflightPlan{}, fmt.Errorf("the generated namespace stage exceeds its preflight bound")
		}
		namespaces = append(namespaces, preflightNamespace{name: object.GetName()})
	}
	operatorNamespace := operatorStage.Stage().Namespace
	requireNFDWorker, err := nfdWorkerRequiredFromBootstrap(request.Bundle, operatorNamespace)
	if err != nil {
		return preflightPlan{}, err
	}
	if request.Namespaces.ArgoCD != "" && request.Namespaces.ArgoCD != operatorNamespace {
		return preflightPlan{}, fmt.Errorf("the Argo CD namespace override requires matching retained and rendered HBN configuration")
	}
	if len(namespaces) == 0 || !seenNamespaces[operatorNamespace] {
		return preflightPlan{}, fmt.Errorf("the operator namespace is absent from the generated namespace stage")
	}
	projection := operatorStage.Projections()
	if len(projection) == 0 || len(projection) > maxPreflightProjection {
		return preflightPlan{}, fmt.Errorf("the rendered operator projection is missing or exceeds its preflight bound")
	}
	objects := make([]*unstructured.Unstructured, 0, len(projection))
	for _, document := range projection {
		object := document.ObjectCopy()
		if object == nil || object.GetAPIVersion() == "" || object.GetKind() == "" || object.GetName() == "" {
			return preflightPlan{}, fmt.Errorf("the rendered operator projection contains an invalid resource identity")
		}
		objects = append(objects, object)
	}
	defaults := ExternalNamespaceOverrides{CertManager: "cert-manager", ArgoCD: operatorNamespace, NodeFeature: operatorNamespace, Maintenance: operatorNamespace, Kamaji: operatorNamespace}
	if request.Namespaces.CertManager != "" {
		defaults.CertManager = request.Namespaces.CertManager
	}
	if request.Namespaces.ArgoCD != "" {
		defaults.ArgoCD = request.Namespaces.ArgoCD
	}
	if request.Namespaces.NodeFeature != "" {
		defaults.NodeFeature = request.Namespaces.NodeFeature
	}
	if request.Namespaces.Maintenance != "" {
		defaults.Maintenance = request.Namespaces.Maintenance
	}
	if request.Namespaces.Kamaji != "" {
		defaults.Kamaji = request.Namespaces.Kamaji
	}
	for _, namespace := range []string{defaults.CertManager, defaults.ArgoCD, defaults.NodeFeature, defaults.Maintenance, defaults.Kamaji} {
		if namespace == "" || len(validation.IsDNS1123Label(namespace)) != 0 {
			return preflightPlan{}, fmt.Errorf("external dependency namespaces must be valid Kubernetes namespaces")
		}
	}
	return preflightPlan{operatorNamespace: operatorNamespace, namespaces: namespaces, projection: objects, overrides: defaults, requireNFDWorker: requireNFDWorker}, nil
}

func nfdWorkerRequiredFromBootstrap(generated *bundle.Bundle, operatorNamespace string) (bool, error) {
	stage, found := generated.Stage(preflightOperatorConfigStage)
	if !found || stage.Stage().Destination != bundle.Management || stage.Stage().Namespace != operatorNamespace {
		return false, fmt.Errorf("the generated DPF operator configuration stage is missing or outside the operator namespace")
	}
	documents := stage.Documents()
	if len(documents) != 1 {
		return false, fmt.Errorf("the generated DPF operator configuration must contain exactly one resource")
	}
	object := documents[0].ObjectCopy()
	return nfdWorkerRequiredForOperatorConfig(object, operatorNamespace)
}

func nfdWorkerRequiredForOperatorConfig(object *unstructured.Unstructured, operatorNamespace string) (bool, error) {
	if object == nil || object.GetAPIVersion() != "operator.dpu.nvidia.com/v1alpha1" || object.GetKind() != "DPFOperatorConfig" || object.GetNamespace() != operatorNamespace {
		return false, fmt.Errorf("the retained DPF operator configuration has an unexpected identity")
	}
	mode, found, err := unstructured.NestedString(object.Object, "spec", "deploymentMode")
	if err != nil || !found || mode == "" || (mode != "zero-trust" && mode != "host-trusted") {
		return false, fmt.Errorf("the retained DPF operator deployment mode is missing or malformed")
	}
	detectorDisabled, disabledFound, disabledErr := unstructured.NestedBool(object.Object, "spec", "dpuDetector", "disable")
	redfish, redfishFound, redfishErr := unstructured.NestedMap(object.Object, "spec", "provisioningController", "installInterface", "installViaRedfish")
	skipDiscovery, skipFound, skipErr := unstructured.NestedBool(redfish, "skipDPUNodeDiscovery")
	if disabledErr != nil || !disabledFound || redfishErr != nil || !redfishFound || skipErr != nil || !skipFound {
		return false, fmt.Errorf("the retained DPF detector and Redfish discovery settings are missing or malformed")
	}
	// The zero-trust generated flow discovers DPUNodes from Redfish instead of
	// the NFD-backed DPU detector. NFD's API/master are still needed for the
	// operator chart's NodeFeatureRule; a worker DaemonSet on management nodes is
	// not required when this exact retained configuration is selected.
	return mode != "zero-trust" || !detectorDisabled || skipDiscovery, nil
}

func pinnedHBNAPIs() []preflightAPIRequirement {
	return []preflightAPIRequirement{
		{id: "cert-manager.certificates", crd: "certificates.cert-manager.io", group: "cert-manager.io", version: "v1", resource: "certificates", kind: "Certificate", namespaced: true},
		{id: "cert-manager.issuers", crd: "issuers.cert-manager.io", group: "cert-manager.io", version: "v1", resource: "issuers", kind: "Issuer", namespaced: true},
		{id: "cert-manager.cluster-issuers", crd: "clusterissuers.cert-manager.io", group: "cert-manager.io", version: "v1", resource: "clusterissuers", kind: "ClusterIssuer"},
		{id: "argo.applications", crd: "applications.argoproj.io", group: "argoproj.io", version: "v1alpha1", resource: "applications", kind: "Application", namespaced: true},
		{id: "argo.projects", crd: "appprojects.argoproj.io", group: "argoproj.io", version: "v1alpha1", resource: "appprojects", kind: "AppProject", namespaced: true},
		{id: "nfd.node-feature-rules", crd: "nodefeaturerules.nfd.k8s-sigs.io", group: "nfd.k8s-sigs.io", version: "v1alpha1", resource: "nodefeaturerules", kind: "NodeFeatureRule"},
		{id: "maintenance.node-maintenances", crd: "nodemaintenances.maintenance.nvidia.com", group: "maintenance.nvidia.com", version: "v1alpha1", resource: "nodemaintenances", kind: "NodeMaintenance", namespaced: true},
		{id: "kamaji.tenant-control-planes", crd: "tenantcontrolplanes.kamaji.clastix.io", group: "kamaji.clastix.io", version: "v1alpha1", resource: "tenantcontrolplanes", kind: "TenantControlPlane", namespaced: true},
	}
}

func pinnedHBNWorkloads(namespaces ExternalNamespaceOverrides, requireNFDWorker bool) []preflightWorkload {
	workloads := []preflightWorkload{
		{id: "cert-manager.controller", namespace: namespaces.CertManager, kind: "Deployment", selector: "app.kubernetes.io/name=cert-manager,app.kubernetes.io/component=controller"},
		{id: "cert-manager.cainjector", namespace: namespaces.CertManager, kind: "Deployment", selector: "app.kubernetes.io/name=cainjector,app.kubernetes.io/instance=cert-manager"},
		{id: "cert-manager.webhook", namespace: namespaces.CertManager, kind: "Deployment", selector: "app.kubernetes.io/name=webhook,app.kubernetes.io/instance=cert-manager"},
		{id: "argo.server", namespace: namespaces.ArgoCD, kind: "Deployment", selector: "app.kubernetes.io/name=argocd-server"},
		{id: "nfd.master", namespace: namespaces.NodeFeature, kind: "Deployment", selector: "app.kubernetes.io/name=node-feature-discovery,role=master"},
		{id: "maintenance.controller", namespace: namespaces.Maintenance, kind: "Deployment", selector: "app.kubernetes.io/name=maintenance-operator"},
		{id: "kamaji.controller", namespace: namespaces.Kamaji, kind: "Deployment", selector: "app.kubernetes.io/name=kamaji"},
	}
	if requireNFDWorker {
		workloads = append(workloads, preflightWorkload{id: "nfd.worker", namespace: namespaces.NodeFeature, kind: "DaemonSet", selector: "app.kubernetes.io/name=node-feature-discovery,role=worker"})
	}
	return workloads
}

func (c *APIClient) checkPreflightAPI(ctx context.Context, requirement preflightAPIRequirement) []PreflightCheck {
	checks := []PreflightCheck{}
	if c == nil || c.dynamic == nil {
		return []PreflightCheck{blockedPreflight("external.api."+requirement.id, "MANAGEMENT_CLIENT_UNAVAILABLE", "The bound management API client is unavailable.")}
	}
	crd, err := c.dynamic.Resource(schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}).Get(ctx, requirement.crd, metav1.GetOptions{})
	if err != nil {
		checks = append(checks, blockedPreflight("external.api."+requirement.id, apiFailureCode(err), "The required external API definition is unavailable or unreadable."))
		return checks
	}
	group, _, _ := unstructured.NestedString(crd.Object, "spec", "group")
	plural, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "plural")
	kind, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "kind")
	scope, _, _ := unstructured.NestedString(crd.Object, "spec", "scope")
	wantScope := "Cluster"
	if requirement.namespaced {
		wantScope = "Namespaced"
	}
	if group != requirement.group || plural != requirement.resource || kind != requirement.kind || scope != wantScope {
		checks = append(checks, blockedPreflight("external.api."+requirement.id, "EXTERNAL_API_IDENTITY_MISMATCH", "The installed API definition has an unexpected identity or scope."))
		return checks
	}
	if !externalCRDReady(crd, requirement) {
		checks = append(checks, blockedPreflight("external.api."+requirement.id, "EXTERNAL_API_NOT_ESTABLISHED", "The required API definition is not established, accepted, or served."))
		return checks
	}
	resourceList, err := c.discoverResourceList(ctx, requirement.group+"/"+requirement.version)
	if err != nil || resourceList == nil {
		checks = append(checks, blockedPreflight("external.api."+requirement.id, apiFailureCode(err), "The required external API version is not discoverable."))
		return checks
	}
	for _, resource := range resourceList.APIResources {
		if resource.Name == requirement.resource && resource.Kind == requirement.kind && resource.Namespaced == requirement.namespaced {
			checks = append(checks, passedPreflight("external.api."+requirement.id, "The required external API is established, served, and discoverable."))
			return checks
		}
	}
	checks = append(checks, blockedPreflight("external.api."+requirement.id, "EXTERNAL_API_NOT_SERVED", "The required external resource is not served with the expected scope."))
	return checks
}

func externalCRDReady(crd *unstructured.Unstructured, requirement preflightAPIRequirement) bool {
	if crd == nil {
		return false
	}
	deleting, found, err := unstructured.NestedString(crd.Object, "metadata", "deletionTimestamp")
	if err != nil || found && deleting != "" {
		return false
	}
	versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if err != nil || !found {
		return false
	}
	served := false
	for _, value := range versions {
		version, ok := value.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := version["name"].(string)
		isServed, _ := version["served"].(bool)
		served = served || name == requirement.version && isServed
	}
	conditions, found, err := unstructured.NestedSlice(crd.Object, "status", "conditions")
	return served && err == nil && found && crdConditionTrue(conditions, "Established") && crdConditionTrue(conditions, "NamesAccepted")
}

func (c *APIClient) discoverResourceList(ctx context.Context, groupVersion string) (*metav1.APIResourceList, error) {
	if c == nil || c.access.Client == nil || ctx == nil {
		return nil, fmt.Errorf("management discovery client unavailable")
	}
	apiPath := "/apis/" + groupVersion
	if !strings.Contains(groupVersion, "/") {
		apiPath = "/api/" + groupVersion
	}
	data, err := c.access.Client.Discovery().RESTClient().Get().AbsPath(apiPath).Do(ctx).Raw()
	if err != nil {
		return nil, err
	}
	var resources metav1.APIResourceList
	if json.Unmarshal(data, &resources) != nil || resources.GroupVersion != groupVersion {
		return nil, fmt.Errorf("invalid discovery response")
	}
	return &resources, nil
}

func crdConditionTrue(conditions []interface{}, conditionType string) bool {
	matched := false
	ready := false
	for _, raw := range conditions {
		condition, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		typeName, _ := condition["type"].(string)
		status, _ := condition["status"].(string)
		if typeName == conditionType {
			if matched || status != "True" {
				return false
			}
			matched = true
			ready = true
		}
	}
	return matched && ready
}

func (c *APIClient) checkPreflightWorkload(ctx context.Context, requirement preflightWorkload) PreflightCheck {
	if c == nil || c.access.Client == nil || ctx == nil {
		return blockedPreflight("external.controller."+requirement.id, "MANAGEMENT_CLIENT_UNAVAILABLE", "The bound management API client is unavailable.")
	}
	if _, err := labels.Parse(requirement.selector); err != nil {
		return blockedPreflight("external.controller."+requirement.id, "PREFLIGHT_SELECTOR_INVALID", "The pinned controller selector is invalid.")
	}
	listOptions := metav1.ListOptions{LabelSelector: requirement.selector, Limit: 2}
	var available bool
	switch requirement.kind {
	case "Deployment":
		list, err := c.access.Client.AppsV1().Deployments(requirement.namespace).List(ctx, listOptions)
		if err != nil {
			return blockedPreflight("external.controller."+requirement.id, apiFailureCode(err), "The required external controller could not be inspected.")
		}
		available = len(list.Items) == 1 && list.Continue == "" && list.Items[0].DeletionTimestamp == nil && deploymentAvailable(list.Items[0])
	case "StatefulSet":
		list, err := c.access.Client.AppsV1().StatefulSets(requirement.namespace).List(ctx, listOptions)
		if err != nil {
			return blockedPreflight("external.controller."+requirement.id, apiFailureCode(err), "The required external controller could not be inspected.")
		}
		available = len(list.Items) == 1 && list.Continue == "" && list.Items[0].DeletionTimestamp == nil && statefulSetAvailable(list.Items[0])
	case "DaemonSet":
		list, err := c.access.Client.AppsV1().DaemonSets(requirement.namespace).List(ctx, listOptions)
		if err != nil {
			return blockedPreflight("external.controller."+requirement.id, apiFailureCode(err), "The required external controller could not be inspected.")
		}
		available = len(list.Items) == 1 && list.Continue == "" && list.Items[0].DeletionTimestamp == nil && daemonSetAvailable(list.Items[0])
	default:
		return blockedPreflight("external.controller."+requirement.id, "PREFLIGHT_WORKLOAD_UNSUPPORTED", "The required external controller kind is unsupported.")
	}
	if !available {
		return blockedPreflight("external.controller."+requirement.id, "EXTERNAL_CONTROLLER_NOT_READY", "The required external controller is missing, ambiguous, stale, or not fully ready.")
	}
	return passedPreflight("external.controller."+requirement.id, "The required external controller has current ready replicas.")
}

func deploymentAvailable(deployment appsv1.Deployment) bool {
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	return desired > 0 && deployment.Generation > 0 && deployment.Status.ObservedGeneration == deployment.Generation && deployment.Status.UpdatedReplicas == desired && deployment.Status.ReadyReplicas == desired && deployment.Status.AvailableReplicas == desired
}

func statefulSetAvailable(set appsv1.StatefulSet) bool {
	desired := int32(1)
	if set.Spec.Replicas != nil {
		desired = *set.Spec.Replicas
	}
	return desired > 0 && set.Generation > 0 && set.Status.ObservedGeneration == set.Generation && set.Status.ReadyReplicas == desired && set.Status.UpdatedReplicas == desired && set.Status.CurrentReplicas == desired
}

func daemonSetAvailable(set appsv1.DaemonSet) bool {
	desired := set.Status.DesiredNumberScheduled
	return desired > 0 && set.Generation > 0 && set.Status.ObservedGeneration == set.Generation && set.Status.NumberReady == desired && set.Status.UpdatedNumberScheduled == desired && set.Status.NumberAvailable == desired && set.Status.NumberMisscheduled == 0
}

func (c *APIClient) checkArgoApplicationScope(ctx context.Context, operatorNamespace, argoNamespace string) PreflightCheck {
	if c == nil || c.access.Client == nil || ctx == nil {
		return blockedPreflight("external.controller.argo.application-scope", "MANAGEMENT_CLIENT_UNAVAILABLE", "The bound management API client is unavailable.")
	}
	if argoNamespace != operatorNamespace {
		return blockedPreflight("external.controller.argo.application-scope", "ARGO_OPERATOR_NAMESPACE_NOT_RECONCILED", "The HBN-generated Argo Application namespace is not the Argo CD control-plane namespace.")
	}
	const selector = "app.kubernetes.io/name=argocd-application-controller"
	sets, err := c.access.Client.AppsV1().StatefulSets(argoNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: 2})
	if err != nil {
		return blockedPreflight("external.controller.argo.application-scope", apiFailureCode(err), "Argo CD application namespace scope could not be verified.")
	}
	if len(sets.Items) != 1 || sets.Continue != "" || sets.Items[0].DeletionTimestamp != nil || !statefulSetAvailable(sets.Items[0]) {
		return blockedPreflight("external.controller.argo.application-scope", "ARGO_CONTROLLER_UNVERIFIED", "The Argo CD application controller is missing, ambiguous, stale, or not fully ready.")
	}
	if !argoControllerUsesInClusterNamespace(sets.Items[0], argoNamespace) {
		return blockedPreflight("external.controller.argo.application-scope", "ARGO_CONTROLLER_SCOPE_UNVERIFIED", "The Argo CD application controller has unsupported Kubernetes client overrides or namespace settings.")
	}
	// Argo CD always manages Applications in its control-plane namespace; the
	// application.namespaces setting only adds other source namespaces.
	return passedPreflight("external.controller.argo.application-scope", "Argo CD's ready control plane is in the generated Application namespace.")
}

func argoControllerUsesInClusterNamespace(set appsv1.StatefulSet, expectedNamespace string) bool {
	var controller *corev1.Container
	for index := range set.Spec.Template.Spec.Containers {
		container := &set.Spec.Template.Spec.Containers[index]
		if container.Name == "application-controller" {
			if controller != nil {
				return false
			}
			controller = container
		}
	}
	if controller == nil || len(controller.EnvFrom) > 0 {
		return false
	}
	for _, env := range controller.Env {
		if env.Name == "KUBECONFIG" && (env.Value != "" || env.ValueFrom != nil) {
			return false
		}
	}
	args := append(append([]string(nil), controller.Command...), controller.Args...)
	if len(args) > 0 && filepath.Base(args[0]) != "argocd-application-controller" {
		return false
	}
	for index := 0; index < len(args); index++ {
		argument := args[index]
		name, value, hasValue := strings.Cut(argument, "=")
		if name == "-n" || (strings.HasPrefix(argument, "-n") && len(argument) > 2 && !strings.HasPrefix(argument, "--")) {
			if name != "-n" {
				return false
			}
			if hasValue {
				return false
			}
			if index+1 >= len(args) || args[index+1] != expectedNamespace {
				return false
			}
			index++
			continue
		}
		switch name {
		case "--namespace":
			if !hasValue {
				if index+1 >= len(args) || args[index+1] != expectedNamespace {
					return false
				}
				index++
				continue
			}
			if value != expectedNamespace {
				return false
			}
		case "--kubeconfig", "--context", "--cluster", "--user", "--server", "--certificate-authority", "--client-certificate", "--client-key", "--token", "--username", "--password", "--proxy-url", "--tls-server-name", "--insecure-skip-tls-verify":
			return false
		}
		if strings.HasPrefix(argument, "-s") && len(argument) > 2 && !strings.HasPrefix(argument, "--") {
			return false
		}
		if argument == "-s" {
			return false
		}
	}
	return true
}

func (c *APIClient) checkCertManagerWebhookCA(ctx context.Context) PreflightCheck {
	if c == nil || c.access.Client == nil || ctx == nil {
		return blockedPreflight("external.controller.cert-manager.webhook-ca", "MANAGEMENT_CLIENT_UNAVAILABLE", "The bound management API client is unavailable.")
	}
	validating, err := c.access.Client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, "cert-manager-webhook", metav1.GetOptions{})
	if err != nil || !webhookCABundlesPresentValidating(validating) {
		returnCode := "CERT_MANAGER_WEBHOOK_CA_NOT_READY"
		if err != nil {
			returnCode = apiFailureCode(err)
		}
		return blockedPreflight("external.controller.cert-manager.webhook-ca", returnCode, "cert-manager's validating webhook CA bundle is not ready.")
	}
	mutating, err := c.access.Client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, "cert-manager-webhook", metav1.GetOptions{})
	if err != nil || !webhookCABundlesPresentMutating(mutating) {
		returnCode := "CERT_MANAGER_WEBHOOK_CA_NOT_READY"
		if err != nil {
			returnCode = apiFailureCode(err)
		}
		return blockedPreflight("external.controller.cert-manager.webhook-ca", returnCode, "cert-manager's mutating webhook CA bundle is not ready.")
	}
	return passedPreflight("external.controller.cert-manager.webhook-ca", "cert-manager webhook CA bundles are populated.")
}

func webhookCABundlesPresentValidating(configuration *admissionregistrationv1.ValidatingWebhookConfiguration) bool {
	if configuration == nil || configuration.DeletionTimestamp != nil || len(configuration.Webhooks) == 0 {
		return false
	}
	for _, webhook := range configuration.Webhooks {
		if len(webhook.ClientConfig.CABundle) == 0 {
			return false
		}
	}
	return true
}

func webhookCABundlesPresentMutating(configuration *admissionregistrationv1.MutatingWebhookConfiguration) bool {
	if configuration == nil || configuration.DeletionTimestamp != nil || len(configuration.Webhooks) == 0 {
		return false
	}
	for _, webhook := range configuration.Webhooks {
		if len(webhook.ClientConfig.CABundle) == 0 {
			return false
		}
	}
	return true
}

func (c *APIClient) checkNamespacePermissions(ctx context.Context, namespace preflightNamespace) PreflightCheck {
	for _, verb := range []string{"get", "create"} {
		requirement := accessRequirement{verb: verb, version: "v1", resource: "namespaces"}
		if verb == "get" {
			requirement.resourceName = namespace.name
		}
		check := c.checkAccess(ctx, requirement)
		if check.State != "passed" {
			check.ID = "caller.rbac.namespace." + namespace.name + "." + verb
			check.Message = fmt.Sprintf("The configured identity lacks %s permission for the generated namespace %q.", verb, namespace.name)
			return check
		}
	}
	return passedPreflight("caller.rbac.namespace."+namespace.name, "The configured identity can verify or create the generated namespace.")
}

func (c *APIClient) projectedOperatorPermissions(ctx context.Context, plan preflightPlan) ([]accessRequirement, error) {
	requirements := map[accessRequirement]bool{}
	projectedDefinitions := projectedCRDDefinitions(plan.projection)
	for _, object := range plan.projection {
		groupVersion := object.GetAPIVersion()
		selected := (*metav1.APIResource)(nil)
		if definition, found := projectedDefinitions[groupVersion+"/"+object.GetKind()]; found {
			selected = &metav1.APIResource{Name: definition.resource, Kind: object.GetKind(), Namespaced: definition.namespaced}
		} else if object.GetAPIVersion() == "apiextensions.k8s.io/v1" && object.GetKind() == "CustomResourceDefinition" {
			selected = &metav1.APIResource{Name: "customresourcedefinitions", Kind: object.GetKind(), Namespaced: false}
		} else {
			resourceList, err := c.discoverResourceList(ctx, groupVersion)
			if err != nil || resourceList == nil {
				return nil, fmt.Errorf("projected API group/version is unavailable")
			}
			for index := range resourceList.APIResources {
				resource := &resourceList.APIResources[index]
				if resource.Kind != object.GetKind() || strings.Contains(resource.Name, "/") {
					continue
				}
				if selected != nil {
					return nil, fmt.Errorf("ambiguous API resource mapping")
				}
				selected = resource
			}
		}
		if selected == nil {
			return nil, fmt.Errorf("projected API kind is not served")
		}
		group, version := object.GroupVersionKind().Group, object.GroupVersionKind().Version
		namespace := object.GetNamespace()
		if selected.Namespaced {
			if namespace == "" {
				namespace = plan.operatorNamespace
			}
			if namespace != plan.operatorNamespace {
				return nil, fmt.Errorf("namespaced operator projection escapes the release namespace")
			}
		} else if namespace != "" {
			return nil, fmt.Errorf("cluster-scoped projection has a namespace")
		}
		requirements[accessRequirement{verb: "create", group: group, version: version, resource: selected.Name, namespace: namespace}] = true
		// Helm checks every rendered object for an existing release conflict
		// before creating anything, so the caller needs GET on the full
		// projection, not only on resources Helm later waits for.
		requirements[accessRequirement{verb: "get", group: group, version: version, resource: selected.Name, namespace: namespace}] = true
		if object.GetKind() == "Deployment" {
			requirements[accessRequirement{verb: "list", group: "apps", version: "v1", resource: "replicasets", namespace: namespace}] = true
		}
		if helmHookUsesWatch(object) {
			for _, verb := range []string{"get", "list", "watch"} {
				requirements[accessRequirement{verb: verb, group: group, version: version, resource: selected.Name, namespace: namespace}] = true
			}
		}
	}
	result := make([]accessRequirement, 0, len(requirements))
	for requirement := range requirements {
		result = append(result, requirement)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		return strings.Join([]string{a.namespace, a.group, a.version, a.resource, a.verb}, "\x00") < strings.Join([]string{b.namespace, b.group, b.version, b.resource, b.verb}, "\x00")
	})
	return result, nil
}

type projectedRBACRole struct {
	kind, name, namespace string
	rules                 []rbacv1.PolicyRule
}

func (c *APIClient) checkProjectedRBACPermissions(ctx context.Context, plan preflightPlan) []PreflightCheck {
	roleObjects, bindingObjects := 0, 0
	roleChecks := []PreflightCheck{}
	for _, object := range plan.projection {
		if object == nil {
			continue
		}
		if object.GetKind() == "RoleBinding" || object.GetKind() == "ClusterRoleBinding" {
			bindingObjects++
			continue
		}
		if object.GetKind() != "Role" && object.GetKind() != "ClusterRole" {
			continue
		}
		roleObjects++
		role, ok := decodeProjectedRole(object)
		if !ok {
			return []PreflightCheck{blockedPreflight("caller.rbac.role-rules", "RBAC_RULES_UNVERIFIABLE", "A rendered Role or ClusterRole has malformed or unsupported policy rules.")}
		}
		check := passedPreflight("caller.rbac.escalate."+role.kind+"."+role.name, "The rendered RBAC role grants no permissions and needs no escalate authorization.")
		if len(role.rules) > 0 {
			resource := "roles"
			if role.kind == "ClusterRole" {
				resource = "clusterroles"
			}
			check = c.checkAccess(ctx, accessRequirement{verb: "escalate", group: "rbac.authorization.k8s.io", resource: resource, namespace: role.namespace, resourceName: role.name})
			check.ID = "caller.rbac.escalate." + role.kind + "." + role.name
			if check.State != "passed" {
				check.ReasonCode = "RBAC_ROLE_PRIVILEGE_UNVERIFIABLE"
				check.Message = "The configured identity lacks explicit escalate permission for the rendered role; implicit permission coverage is not established by this preflight."
			} else {
				check.Message = "The configured identity has explicit escalate permission for the rendered RBAC role."
			}
		}
		roleChecks = append(roleChecks, check)
	}
	if roleObjects > maxPreflightRBACObjects || bindingObjects > maxPreflightRBACObjects {
		return []PreflightCheck{blockedPreflight("caller.rbac.roles", "RBAC_PROJECTION_EXCEEDS_BOUND", "The rendered RBAC projection exceeds the preflight bound.")}
	}
	checks := roleChecks
	for _, object := range plan.projection {
		if object == nil || (object.GetKind() != "RoleBinding" && object.GetKind() != "ClusterRoleBinding") {
			continue
		}
		check := c.checkProjectedRoleBinding(ctx, object)
		checks = append(checks, check)
	}
	return checks
}

func decodeProjectedRole(object *unstructured.Unstructured) (projectedRBACRole, bool) {
	if object == nil || object.GetName() == "" || (object.GetKind() != "Role" && object.GetKind() != "ClusterRole") {
		return projectedRBACRole{}, false
	}
	var rules []rbacv1.PolicyRule
	if object.GetKind() == "Role" {
		role := &rbacv1.Role{}
		if runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, role) != nil {
			return projectedRBACRole{}, false
		}
		rules = role.Rules
		if object.GetNamespace() == "" {
			return projectedRBACRole{}, false
		}
	} else {
		role := &rbacv1.ClusterRole{}
		if runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, role) != nil || role.AggregationRule != nil {
			return projectedRBACRole{}, false
		}
		rules = role.Rules
		if object.GetNamespace() != "" {
			return projectedRBACRole{}, false
		}
	}
	if len(rules) > maxPreflightRBACRules {
		return projectedRBACRole{}, false
	}
	return projectedRBACRole{kind: object.GetKind(), name: object.GetName(), namespace: object.GetNamespace(), rules: rules}, true
}

func (c *APIClient) checkProjectedRoleBinding(ctx context.Context, binding *unstructured.Unstructured) PreflightCheck {
	name := binding.GetName()
	ref, found, err := unstructured.NestedMap(binding.Object, "roleRef")
	if err != nil || !found {
		return blockedPreflight("caller.rbac.bind."+name, "RBAC_ROLE_REFERENCE_INVALID", "A rendered RoleBinding has an invalid role reference.")
	}
	apiGroup, _ := ref["apiGroup"].(string)
	kind, _ := ref["kind"].(string)
	roleName, _ := ref["name"].(string)
	if apiGroup != "rbac.authorization.k8s.io" || roleName == "" || (kind != "Role" && kind != "ClusterRole") || binding.GetKind() == "ClusterRoleBinding" && kind != "ClusterRole" {
		return blockedPreflight("caller.rbac.bind."+name, "RBAC_ROLE_REFERENCE_INVALID", "A rendered RoleBinding references an unsupported RBAC role.")
	}
	resource := "roles"
	if kind == "ClusterRole" {
		resource = "clusterroles"
	}
	bindNamespace := binding.GetNamespace()
	if binding.GetKind() == "ClusterRoleBinding" {
		bindNamespace = ""
	}
	check := c.checkAccess(ctx, accessRequirement{verb: "bind", group: "rbac.authorization.k8s.io", resource: resource, namespace: bindNamespace, resourceName: roleName})
	check.ID = "caller.rbac.bind." + name
	if check.State != "passed" {
		check.ReasonCode = "RBAC_BIND_PRIVILEGE_UNVERIFIABLE"
		check.Message = "The configured identity lacks explicit bind permission for the referenced role; implicit permission coverage is not established by this preflight."
	} else {
		check.Message = "The configured identity has explicit bind permission for the referenced role."
	}
	return check
}

func helmHookUsesWatch(object *unstructured.Unstructured) bool {
	if object == nil {
		return false
	}
	if object.GetKind() != "Job" && object.GetKind() != "Pod" {
		return false
	}
	return object.GetAnnotations()["helm.sh/hook"] != ""
}

func (c *APIClient) checkAccess(ctx context.Context, requirement accessRequirement) PreflightCheck {
	attributes := &authorizationv1.ResourceAttributes{
		Verb: requirement.verb, Group: requirement.group, Version: requirement.version,
		Resource: requirement.resource, Namespace: requirement.namespace, Name: requirement.resourceName,
	}
	review, err := c.access.Client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: attributes}}, metav1.CreateOptions{})
	identity := strings.Join([]string{requirement.namespace, requirement.group, requirement.resource, requirement.resourceName, requirement.verb}, ".")
	if err != nil {
		return blockedPreflight("caller.rbac."+identity, apiFailureCode(err), "The configured identity's scoped authorization could not be verified.")
	}
	if review == nil || review.Status.EvaluationError != "" {
		return blockedPreflight("caller.rbac."+identity, "RBAC_REVIEW_INCOMPLETE", "The API did not return a complete authorization decision.")
	}
	if !review.Status.Allowed {
		return blockedPreflight("caller.rbac."+identity, "CALLER_PERMISSION_MISSING", fmt.Sprintf("The configured identity cannot %s %s in namespace %q.", requirement.verb, resourceDescription(requirement.group, requirement.resource), requirement.namespace))
	}
	return passedPreflight("caller.rbac."+identity, "The configured identity has the required scoped permission.")
}

func resourceDescription(group, resource string) string {
	if group == "" {
		return resource
	}
	return resource + "." + group
}

func apiFailureCode(err error) string {
	switch {
	case err == nil:
		return "API_RESOURCE_UNAVAILABLE"
	case apierrors.IsForbidden(err):
		return "API_ACCESS_DENIED"
	case apierrors.IsUnauthorized(err):
		return "API_AUTHENTICATION_FAILED"
	case apierrors.IsNotFound(err):
		return "API_RESOURCE_NOT_FOUND"
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return "API_TIMEOUT"
	case errors.Is(err, context.Canceled):
		return "PREFLIGHT_CANCELLED"
	case errors.Is(err, context.DeadlineExceeded):
		return "API_TIMEOUT"
	default:
		return "API_INSPECTION_FAILED"
	}
}

type projectedCRDDefinition struct {
	resource   string
	namespaced bool
}

func projectedCRDDefinitions(objects []*unstructured.Unstructured) map[string]projectedCRDDefinition {
	definitions := map[string]projectedCRDDefinition{}
	conflicts := map[string]bool{}
	for _, object := range objects {
		if object == nil || object.GetAPIVersion() != "apiextensions.k8s.io/v1" || object.GetKind() != "CustomResourceDefinition" {
			continue
		}
		group, _, groupErr := unstructured.NestedString(object.Object, "spec", "group")
		plural, _, pluralErr := unstructured.NestedString(object.Object, "spec", "names", "plural")
		scope, _, scopeErr := unstructured.NestedString(object.Object, "spec", "scope")
		versions, found, versionsErr := unstructured.NestedSlice(object.Object, "spec", "versions")
		kinds, _, kindErr := unstructured.NestedString(object.Object, "spec", "names", "kind")
		if groupErr != nil || pluralErr != nil || scopeErr != nil || versionsErr != nil || kindErr != nil || !found || group == "" || plural == "" || kinds == "" || (scope != "Namespaced" && scope != "Cluster") {
			continue
		}
		for _, item := range versions {
			version, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := version["name"].(string)
			served, _ := version["served"].(bool)
			if name != "" && served {
				key := group + "/" + name + "/" + kinds
				if conflicts[key] {
					continue
				}
				if _, duplicate := definitions[key]; duplicate {
					delete(definitions, key)
					conflicts[key] = true
					continue
				}
				definitions[key] = projectedCRDDefinition{resource: plural, namespaced: scope == "Namespaced"}
			}
		}
	}
	return definitions
}

func passedPreflight(id, message string) PreflightCheck {
	return PreflightCheck{ID: id, State: "passed", Message: message}
}

func blockedPreflight(id, reason, message string) PreflightCheck {
	return PreflightCheck{ID: id, State: "blocked", ReasonCode: reason, Message: message}
}
