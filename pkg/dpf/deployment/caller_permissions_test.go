// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

const callerManagementAPIEndpoint = "https://example.com"

func TestCheckCallerPermissionsCoversRetainedManagementPlan(t *testing.T) {
	harness := newCallerPermissionHarness(t, nil)
	input := callerGenerationInputs(t, harness.server)
	bundle, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatalf("generate caller-permission bundle: %v", err)
	}
	plan, err := managementPreflightPlan(ManagementPreflightRequest{Bundle: bundle})
	if err != nil {
		t.Fatalf("prepare exact retained plan: %v", err)
	}
	harness.projection = plan.projection
	client := callerPermissionClient(t, harness.server)
	if err := client.VerifyIdentity(context.Background()); err != nil {
		t.Fatalf("secure fixture identity verification failed: %v", err)
	}
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
	for _, device := range plan.intent.Devices {
		assertCallerReview(t, harness.reviews, func(review callerReview) bool {
			attributes := review.Spec.ResourceAttributes
			return attributes != nil && attributes.Verb == "patch" && attributes.Group == "provisioning.dpu.nvidia.com" && attributes.Resource == "dpudevices" && attributes.Namespace == plan.operatorNamespace && attributes.Name == strings.ToLower(device.Serial)
		})
	}
	assertCallerReview(t, harness.reviews, func(review callerReview) bool {
		return review.Spec.ResourceAttributes != nil && review.Spec.ResourceAttributes.Verb == "create" && review.Spec.ResourceAttributes.Resource == "pods" && review.Spec.ResourceAttributes.Subresource == "portforward" && review.Spec.ResourceAttributes.Namespace == "dpf-operator-system"
	})
	for _, resource := range []string{"validatingwebhookconfigurations", "mutatingwebhookconfigurations"} {
		assertCallerReview(t, harness.reviews, func(review callerReview) bool {
			a := review.Spec.ResourceAttributes
			return a != nil && a.Verb == "list" && a.Group == "admissionregistration.k8s.io" && a.Resource == resource && a.Namespace == ""
		})
	}
	assertCallerReview(t, harness.reviews, func(review callerReview) bool {
		a := review.Spec.ResourceAttributes
		return a != nil && a.Verb == "list" && a.Resource == "services" && a.Namespace == "cert-manager"
	})
	assertCallerReview(t, harness.reviews, func(review callerReview) bool {
		a := review.Spec.ResourceAttributes
		return a != nil && a.Verb == "list" && a.Group == "apps" && a.Resource == "deployments" && a.Namespace == ""
	})
	for _, verb := range []string{"get", "create"} {
		assertCallerReview(t, harness.reviews, func(review callerReview) bool {
			attributes := review.Spec.ResourceAttributes
			return attributes != nil && attributes.Verb == verb && attributes.Resource == "configmaps" && attributes.Namespace == "dpf-operator-system"
		})
	}
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
	input := callerGenerationInputs(t, harness.server)
	bundle, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatalf("generate caller-permission bundle: %v", err)
	}
	plan, err := managementPreflightPlan(ManagementPreflightRequest{Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	harness.projection = plan.projection
	client := callerPermissionClient(t, harness.server)
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

func TestCheckCallerPermissionsRejectsSelectedDeviceSelectorPatchDenial(t *testing.T) {
	harness := newCallerPermissionHarness(t, func(review callerReview) bool {
		a := review.Spec.ResourceAttributes
		return a == nil || a.Verb != "patch" || a.Group != "provisioning.dpu.nvidia.com" || a.Resource != "dpudevices"
	})
	generated, err := generateFixtureBundle(callerGenerationInputs(t, harness.server))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := managementPreflightPlan(ManagementPreflightRequest{Bundle: generated})
	if err != nil {
		t.Fatal(err)
	}
	harness.projection = plan.projection
	result := callerPermissionClient(t, harness.server).CheckCallerPermissions(context.Background(), ManagementPreflightRequest{Bundle: generated})
	if result.Ready || !containsCallerReason(result, "CALLER_PERMISSION_MISSING") {
		t.Fatal("missing selected-device patch permission did not block readiness")
	}
	for _, device := range plan.intent.Devices {
		assertCallerReview(t, harness.reviews, func(review callerReview) bool {
			a := review.Spec.ResourceAttributes
			return a != nil && a.Verb == "patch" && a.Resource == "dpudevices" && a.Namespace == plan.operatorNamespace && a.Name == strings.ToLower(device.Serial)
		})
	}
}

func TestCheckCallerPermissionsStopsOnBindingMismatchOrCancellation(t *testing.T) {
	harness := newCallerPermissionHarness(t, nil)
	input := callerGenerationInputs(t, harness.server)
	bundle, err := generateFixtureBundle(input)
	if err != nil {
		t.Fatalf("generate caller-permission bundle: %v", err)
	}
	plan, err := managementPreflightPlan(ManagementPreflightRequest{Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	harness.projection = plan.projection
	client := callerPermissionClient(t, harness.server)
	client.expected.APIEndpoint = "https://different.example.test"
	result := client.CheckCallerPermissions(context.Background(), ManagementPreflightRequest{Bundle: bundle})
	if result.Ready || len(result.Checks) != 1 || result.Checks[0].ReasonCode != "MANAGEMENT_BUNDLE_BINDING_MISMATCH" {
		t.Fatalf("bundle binding mismatch reached permission reviews: %#v", failedCallerChecks(result))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result = callerPermissionClient(t, harness.server).CheckCallerPermissions(ctx, ManagementPreflightRequest{Bundle: bundle})
	if result.Ready || len(result.Checks) != 1 || result.Checks[0].ReasonCode != "PREFLIGHT_CANCELLED" {
		t.Fatalf("pre-canceled permission request was not stopped: %#v", failedCallerChecks(result))
	}
}

func TestCheckCallerPermissionsRejectsFinalIdentityDrift(t *testing.T) {
	harness := newCallerPermissionHarness(t, nil)
	input := callerGenerationInputs(t, harness.server)
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
	result := callerPermissionClient(t, harness.server).CheckCallerPermissions(context.Background(), ManagementPreflightRequest{Bundle: bundle})
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

func callerPermissionClient(t *testing.T, server *httptest.Server) *APIClient {
	t.Helper()
	listener := server.Listener.Addr().String()
	dialer := &net.Dialer{}
	config := &rest.Config{
		Host:          callerManagementAPIEndpoint,
		ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"},
		QPS:           1000, Burst: 1000,
		TLSClientConfig: rest.TLSClientConfig{CAData: callerManagementCAData(t, server)},
		// Keep every test on its own handler/listener while preserving the
		// canonical management identity used to cache the immutable plan. A
		// mismatched address fails locally instead of falling through to DNS.
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != "example.com:443" {
				return nil, fmt.Errorf("caller permission fixture refuses dial to %s/%s", network, address)
			}
			return dialer.DialContext(ctx, network, listener)
		},
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	return &APIClient{
		access:   discovery.ManagementClient{Client: client, REST: config, Namespace: "dpf-operator-system"},
		dynamic:  dynamicClient,
		expected: generation.ManagementIdentity{APIEndpoint: callerManagementAPIEndpoint, SystemNamespaceUID: "system-uid-1", TrustDigest: callerManagementTrustDigest(t, server)},
		version:  "v1.35.4",
	}
}

func callerManagementCAData(t *testing.T, server *httptest.Server) []byte {
	t.Helper()
	certificate := server.Certificate()
	if err := certificate.VerifyHostname("example.com"); err != nil {
		t.Fatalf("httptest certificate does not cover the retained fixture identity: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
}

func callerManagementTrustDigest(t *testing.T, server *httptest.Server) string {
	t.Helper()
	return fmt.Sprintf("sha256:%x", sha256.Sum256(callerManagementCAData(t, server)))
}

func TestCallerPermissionFixtureTransportRejectsNonFixtureEndpoint(t *testing.T) {
	harness := newCallerPermissionHarness(t, nil)
	client := callerPermissionClient(t, harness.server)
	config := rest.CopyConfig(client.access.REST)
	config.Host = "https://elsewhere.invalid/prefix"
	foreign, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Discovery().ServerVersion(); err == nil || !strings.Contains(err.Error(), "refuses dial") {
		t.Fatalf("fixture transport did not fail closed for a different host: %v", err)
	}
	if harness.requests != 0 {
		t.Fatalf("non-fixture endpoint reached handler %d times", harness.requests)
	}
}

type callerPermissionHarness struct {
	server                 *httptest.Server
	requests               int
	projection             []*unstructured.Unstructured
	reviews                []callerReview
	discoveryGets          []string
	allowed                func(callerReview) bool
	identityUIDChangeAtGet int
	identityUIDGets        int
}

func callerGenerationInputs(t *testing.T, server *httptest.Server) generation.Inputs {
	t.Helper()
	input := preflightGenerationInputs(t)
	input.Operator.KubernetesVersion = "v1.35.4"
	input.Management = generation.ManagementIdentity{APIEndpoint: callerManagementAPIEndpoint, SystemNamespaceUID: "system-uid-1", TrustDigest: callerManagementTrustDigest(t, server)}
	return input
}

func newCallerPermissionHarness(t *testing.T, allowed func(callerReview) bool) *callerPermissionHarness {
	t.Helper()
	harness := &callerPermissionHarness{allowed: allowed}
	harness.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		harness.requests++
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
