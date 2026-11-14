// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func TestCheckCallerPermissionsCoversRetainedManagementPlan(t *testing.T) {
	harness := newCallerPermissionHarness(t, nil)
	input := callerGenerationInputs(t, harness.server.URL)
	bundle, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatalf("generate caller-permission bundle: %v", err)
	}
	plan, err := managementPreflightPlan(ManagementPreflightRequest{Bundle: bundle})
	if err != nil {
		t.Fatalf("prepare exact retained plan: %v", err)
	}
	harness.projection = plan.projection
	client := callerPermissionClient(t, harness.server.URL)
	result := client.CheckCallerPermissions(context.Background(), ManagementPreflightRequest{Bundle: bundle})
	if !result.Ready {
		t.Fatalf("complete caller authorization unexpectedly blocked: %#v", failedCallerChecks(result))
	}
	if len(result.Checks) < 100 || len(harness.reviews) < 100 {
		t.Fatalf("full retained permission surface was not checked: checks=%d reviews=%d", len(result.Checks), len(harness.reviews))
	}
	seen := map[string]bool{}
	for _, check := range result.Checks {
		if check.ID == "" || check.ReasonCode == "" || check.Message == "" || seen[check.ID] {
			t.Fatalf("permission result is incomplete or duplicated: %#v", check)
		}
		seen[check.ID] = true
	}
	assertCallerReview(t, harness.reviews, func(review callerReview) bool {
		return review.Spec.ResourceAttributes != nil && review.Spec.ResourceAttributes.Verb == "list" && review.Spec.ResourceAttributes.Group == "provisioning.dpu.nvidia.com" && review.Spec.ResourceAttributes.Resource == "dpuclusters" && review.Spec.ResourceAttributes.Namespace == ""
	})
	assertCallerReview(t, harness.reviews, func(review callerReview) bool {
		return review.Spec.ResourceAttributes != nil && review.Spec.ResourceAttributes.Verb == "create" && review.Spec.ResourceAttributes.Group == "svc.dpu.nvidia.com" && review.Spec.ResourceAttributes.Resource == "dpudeployments"
	})
	assertCallerReview(t, harness.reviews, func(review callerReview) bool {
		return review.Spec.ResourceAttributes != nil && review.Spec.ResourceAttributes.Verb == "create" && review.Spec.ResourceAttributes.Resource == "pods" && review.Spec.ResourceAttributes.Subresource == "portforward" && review.Spec.ResourceAttributes.Namespace == "dpf-operator-system"
	})
	for _, groupVersion := range []string{"provisioning.dpu.nvidia.com/v1alpha1", "svc.dpu.nvidia.com/v1alpha1"} {
		for _, path := range harness.discoveryGets {
			if path == "/apis/"+groupVersion {
				t.Fatalf("caller permission planning depended on a not-yet-served DPF API: %s", path)
			}
		}
		assertCallerReview(t, harness.reviews, func(review callerReview) bool {
			attributes := review.Spec.NonResourceAttributes
			return attributes != nil && attributes.Verb == "get" && attributes.Path == "/apis/"+groupVersion
		})
	}
}

func TestCheckCallerPermissionsReportsActionableDenial(t *testing.T) {
	var deniedReview callerReview
	harness := newCallerPermissionHarness(t, func(review callerReview) bool {
		attributes := review.Spec.ResourceAttributes
		denied := attributes != nil && attributes.Verb == "create" && attributes.Resource == "pods" && attributes.Subresource == "portforward" && attributes.Namespace == "dpf-operator-system"
		if denied {
			deniedReview = review
		}
		return !denied
	})
	input := callerGenerationInputs(t, harness.server.URL)
	bundle, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatalf("generate caller-permission bundle: %v", err)
	}
	plan, err := managementPreflightPlan(ManagementPreflightRequest{Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	harness.projection = plan.projection
	client := callerPermissionClient(t, harness.server.URL)
	result := client.CheckCallerPermissions(context.Background(), ManagementPreflightRequest{Bundle: bundle})
	if result.Ready || deniedReview.Spec.ResourceAttributes == nil {
		t.Fatalf("port-forward denial did not block the retained workflow: ready=%v review=%#v", result.Ready, deniedReview)
	}
	found := false
	for _, check := range result.Checks {
		if check.ReasonCode == "CALLER_PERMISSION_MISSING" && strings.Contains(check.Message, "create pods/portforward") && strings.Contains(check.Message, "dpf-operator-system") {
			found = true
		}
	}
	if !found {
		t.Fatalf("denial did not name the missing operation and namespace: %#v", failedCallerChecks(result))
	}
}

func TestCheckCallerPermissionsStopsOnBindingMismatchOrCancellation(t *testing.T) {
	harness := newCallerPermissionHarness(t, nil)
	input := callerGenerationInputs(t, harness.server.URL)
	bundle, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatalf("generate caller-permission bundle: %v", err)
	}
	plan, err := managementPreflightPlan(ManagementPreflightRequest{Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	harness.projection = plan.projection
	client := callerPermissionClient(t, harness.server.URL)
	client.expected.APIEndpoint = "https://different.example.test"
	result := client.CheckCallerPermissions(context.Background(), ManagementPreflightRequest{Bundle: bundle})
	if result.Ready || len(result.Checks) != 1 || result.Checks[0].ReasonCode != "MANAGEMENT_BUNDLE_BINDING_MISMATCH" {
		t.Fatalf("bundle binding mismatch reached permission reviews: %#v", failedCallerChecks(result))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result = callerPermissionClient(t, harness.server.URL).CheckCallerPermissions(ctx, ManagementPreflightRequest{Bundle: bundle})
	if result.Ready || len(result.Checks) != 1 || result.Checks[0].ReasonCode != "PREFLIGHT_CANCELLED" {
		t.Fatalf("pre-canceled permission request was not stopped: %#v", failedCallerChecks(result))
	}
}

func TestCheckCallerPermissionsRejectsFinalIdentityDrift(t *testing.T) {
	harness := newCallerPermissionHarness(t, nil)
	input := callerGenerationInputs(t, harness.server.URL)
	bundle, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatalf("generate caller-permission bundle: %v", err)
	}
	plan, err := managementPreflightPlan(ManagementPreflightRequest{Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	harness.projection = plan.projection
	harness.identityUIDChangeAtGet = 2
	result := callerPermissionClient(t, harness.server.URL).CheckCallerPermissions(context.Background(), ManagementPreflightRequest{Bundle: bundle})
	if result.Ready || !containsCallerReason(result, "MANAGEMENT_IDENTITY_CHANGED") {
		t.Fatalf("final identity drift was not reported: %#v", failedCallerChecks(result))
	}
	for _, check := range result.Checks {
		if check.State == "passed" {
			t.Fatalf("passed check survived identity drift: %#v", check)
		}
	}
}

func TestProjectedRBACPreservesIncompleteAuthorizationReview(t *testing.T) {
	role := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": "operator-controller"},
		"rules":    []any{map[string]any{"apiGroups": []any{"apps"}, "resources": []any{"deployments"}, "verbs": []any{"get"}}},
	}}
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{EvaluationError: "private policy detail"}}, nil
	})
	checks := (&APIClient{access: discovery.ManagementClient{Client: client}}).checkProjectedRBACPermissions(context.Background(), preflightPlan{projection: []*unstructured.Unstructured{role}})
	if len(checks) != 1 || checks[0].State != "blocked" || checks[0].ReasonCode != "RBAC_REVIEW_INCOMPLETE" || strings.Contains(checks[0].Message, "private policy detail") {
		t.Fatalf("incomplete named escalate review was misreported or leaked: %#v", checks)
	}
}

func containsCallerReason(result PreflightResult, reason string) bool {
	for _, check := range result.Checks {
		if check.ReasonCode == reason {
			return true
		}
	}
	return false
}

type callerReview struct {
	Spec struct {
		ResourceAttributes    *callerResourceAttributes    `json:"resourceAttributes"`
		NonResourceAttributes *callerNonResourceAttributes `json:"nonResourceAttributes"`
	} `json:"spec"`
}

type callerResourceAttributes struct {
	Verb        string `json:"verb"`
	Group       string `json:"group"`
	Version     string `json:"version"`
	Resource    string `json:"resource"`
	Subresource string `json:"subresource"`
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
}

type callerNonResourceAttributes struct {
	Verb string `json:"verb"`
	Path string `json:"path"`
}

func callerPermissionClient(t *testing.T, endpoint string) *APIClient {
	t.Helper()
	config := &rest.Config{Host: endpoint, ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}, QPS: 1000, Burst: 1000, TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	return &APIClient{
		access:   discovery.ManagementClient{Client: client, REST: config, Namespace: "dpf-operator-system"},
		dynamic:  dynamicClient,
		expected: generation.ManagementIdentity{APIEndpoint: endpoint, SystemNamespaceUID: "system-uid-1", TrustDigest: "system-roots"},
		version:  "v1.35.4",
	}
}

type callerPermissionHarness struct {
	server                 *httptest.Server
	projection             []*unstructured.Unstructured
	reviews                []callerReview
	discoveryGets          []string
	allowed                func(callerReview) bool
	identityUIDChangeAtGet int
	identityUIDGets        int
}

func callerGenerationInputs(t *testing.T, endpoint string) generation.Inputs {
	t.Helper()
	input := preflightGenerationInputs(t)
	input.Operator.KubernetesVersion = "v1.35.4"
	input.Management = generation.ManagementIdentity{APIEndpoint: endpoint, SystemNamespaceUID: "system-uid-1", TrustDigest: "system-roots"}
	return input
}

func newCallerPermissionHarness(t *testing.T, allowed func(callerReview) bool) *callerPermissionHarness {
	t.Helper()
	harness := &callerPermissionHarness{allowed: allowed}
	harness.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/version":
			writeCallerJSON(t, writer, map[string]string{"gitVersion": "v1.35.4"})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/namespaces/kube-system":
			harness.identityUIDGets++
			uid := "system-uid-1"
			if harness.identityUIDChangeAtGet > 0 && harness.identityUIDGets >= harness.identityUIDChangeAtGet {
				uid = "system-uid-2"
			}
			writeCallerJSON(t, writer, map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "uid": uid}})
		case request.Method == http.MethodGet && (strings.HasPrefix(request.URL.Path, "/api/") || strings.HasPrefix(request.URL.Path, "/apis/")):
			harness.discoveryGets = append(harness.discoveryGets, request.URL.Path)
			writeCallerJSON(t, writer, callerAPIResources(request.URL.Path, harness.projection))
		case request.Method == http.MethodPost && request.URL.Path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
			var review callerReview
			if err := json.NewDecoder(request.Body).Decode(&review); err != nil {
				t.Errorf("decode authorization review: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			isAllowed := true
			harness.reviews = append(harness.reviews, review)
			if harness.allowed != nil {
				isAllowed = harness.allowed(review)
			}
			writeCallerJSON(t, writer, map[string]any{"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview", "status": map[string]any{"allowed": isAllowed}})
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(harness.server.Close)
	return harness
}

func callerAPIResources(path string, projection []*unstructured.Unstructured) map[string]any {
	groupVersion := strings.TrimPrefix(path, "/apis/")
	if strings.HasPrefix(path, "/api/") {
		groupVersion = strings.TrimPrefix(path, "/api/")
	}
	definitions := projectedCRDDefinitions(projection)
	resources := map[string]metav1.APIResource{}
	for _, object := range projection {
		if object == nil || object.GetAPIVersion() != groupVersion {
			continue
		}
		resource, found := definitions[groupVersion+"/"+object.GetKind()]
		name := resource.resource
		if !found {
			name = callerPlural(object.GetKind())
		}
		resources[name] = metav1.APIResource{Name: name, Kind: object.GetKind(), Namespaced: object.GetNamespace() != ""}
	}
	apiResources := make([]metav1.APIResource, 0, len(resources))
	for _, resource := range resources {
		apiResources = append(apiResources, resource)
	}
	return map[string]any{"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": groupVersion, "resources": apiResources}
}

func callerPlural(kind string) string {
	switch kind {
	case "APIService":
		return "apiservices"
	case "Certificate", "certificates":
		return "certificates"
	case "ClusterIssuer":
		return "clusterissuers"
	case "Issuer":
		return "issuers"
	case "Application":
		return "applications"
	case "AppProject":
		return "appprojects"
	case "NodeFeatureRule":
		return "nodefeaturerules"
	case "NodeMaintenance":
		return "nodemaintenances"
	case "MutatingWebhookConfiguration":
		return "mutatingwebhookconfigurations"
	case "ValidatingWebhookConfiguration":
		return "validatingwebhookconfigurations"
	case "StorageClass":
		return "storageclasses"
	case "CSIDriver":
		return "csidrivers"
	case "PriorityClass":
		return "priorityclasses"
	case "Namespace":
		return "namespaces"
	case "ConfigMap":
		return "configmaps"
	case "Secret":
		return "secrets"
	case "Service":
		return "services"
	case "ServiceAccount":
		return "serviceaccounts"
	case "Pod":
		return "pods"
	case "Node":
		return "nodes"
	case "Role":
		return "roles"
	case "RoleBinding":
		return "rolebindings"
	case "ClusterRole":
		return "clusterroles"
	case "ClusterRoleBinding":
		return "clusterrolebindings"
	case "CustomResourceDefinition":
		return "customresourcedefinitions"
	case "Deployment":
		return "deployments"
	case "StatefulSet":
		return "statefulsets"
	case "DaemonSet":
		return "daemonsets"
	case "ReplicaSet":
		return "replicasets"
	default:
		return strings.ToLower(kind) + "s"
	}
}

func writeCallerJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode API response: %v", err)
	}
}

func assertCallerReview(t *testing.T, reviews []callerReview, predicate func(callerReview) bool) {
	t.Helper()
	for _, review := range reviews {
		if predicate(review) {
			return
		}
	}
	t.Fatalf("required scoped authorization review was not issued")
}

func failedCallerChecks(result PreflightResult) []PreflightCheck {
	checks := make([]PreflightCheck, 0)
	for _, check := range result.Checks {
		if check.State != "passed" {
			checks = append(checks, check)
		}
	}
	return checks
}
