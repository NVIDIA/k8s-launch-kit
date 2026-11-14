// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bootstrap"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/bundle"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	dpfdiscovery "github.com/nvidia/k8s-launch-kit/pkg/dpf/discovery"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbn"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/hbnrender"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestManagementPreflightPlanRequiresPinnedBundleAndDependencies(t *testing.T) {
	generated, err := generation.Generate(preflightGenerationInputs(t))
	if err != nil {
		t.Fatalf("generate exact HBN bundle: %v", err)
	}
	plan, err := managementPreflightPlan(ManagementPreflightRequest{Bundle: generated})
	if err != nil {
		t.Fatal(err)
	}
	if plan.operatorNamespace != "dpf-operator-system" || len(plan.namespaces) != 2 {
		t.Fatalf("plan namespaces = %#v, operator=%q", plan.namespaces, plan.operatorNamespace)
	}
	if got := len(pinnedHBNAPIs()); got < 8 {
		t.Fatalf("fixed external API requirements = %d, want all pinned prerequisite APIs", got)
	}
	workloads := pinnedHBNWorkloads(plan.overrides, plan.requireNFDWorker)
	if len(workloads) != 7 || plan.requireNFDWorker {
		t.Fatalf("zero-trust Redfish HBN controller requirements = %d, requireNFDWorker=%v; want 7 and false", len(workloads), plan.requireNFDWorker)
	}
	if plan.overrides.CertManager != "cert-manager" || plan.overrides.ArgoCD != "dpf-operator-system" {
		t.Fatalf("default external namespaces = %#v", plan.overrides)
	}
	selectorByID := map[string]string{}
	for _, workload := range workloads {
		selectorByID[workload.id] = workload.selector
	}
	if _, found := selectorByID["nfd.worker"]; found || selectorByID["nfd.master"] == "" || selectorByID["cert-manager.cainjector"] != "app.kubernetes.io/name=cainjector,app.kubernetes.io/instance=cert-manager" {
		t.Fatalf("pinned workload selectors drifted: %#v", selectorByID)
	}
	_, err = managementPreflightPlan(ManagementPreflightRequest{Bundle: generated, Namespaces: ExternalNamespaceOverrides{CertManager: "bad/name"}})
	if err == nil {
		t.Fatal("invalid namespace override was accepted")
	}
	index := generated.Index()
	index.Release = "27.0.0"
	index.BundleDigest = ""
	other, err := bundle.New(index, generated.Files())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := managementPreflightPlan(ManagementPreflightRequest{Bundle: other}); err == nil {
		t.Fatal("bundle from an unpinned DPF release was accepted")
	}
}

func TestManagementPreflightPlanAcceptsActualGeneratedReleaseAndRejectsUnretainedArgoNamespace(t *testing.T) {
	generated, err := generation.Generate(preflightGenerationInputs(t))
	if err != nil {
		t.Fatalf("generate actual DPF bundle: %v", err)
	}
	if got := generated.Index().Release; got != "26.8.0" {
		t.Fatalf("generated release = %q, want canonical release 26.8.0", got)
	}
	if _, err := managementPreflightPlan(ManagementPreflightRequest{Bundle: generated}); err != nil {
		t.Fatalf("preflight rejected actual generated bundle: %v", err)
	}
	operatorStage, ok := generated.Stage(preflightOperatorStage)
	if !ok {
		t.Fatal("actual generated operator stage is missing")
	}
	projection := make([]*unstructured.Unstructured, 0, len(operatorStage.Projections()))
	for _, document := range operatorStage.Projections() {
		projection = append(projection, document.ObjectCopy())
	}
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		request := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		allowed := request.Spec.ResourceAttributes != nil && (request.Spec.ResourceAttributes.Verb == "escalate" || request.Spec.ResourceAttributes.Verb == "bind")
		return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: allowed}}, nil
	})
	api := &APIClient{access: dpfdiscovery.ManagementClient{Client: client}}
	rbacChecks := api.checkProjectedRBACPermissions(context.Background(), preflightPlan{projection: projection})
	if len(rbacChecks) == 0 {
		t.Fatal("actual rendered operator RBAC was not checked")
	}
	for _, check := range rbacChecks {
		if check.State != "passed" {
			t.Fatalf("actual rendered RBAC did not pass explicit escalate/bind checks: %#v", check)
		}
	}
	if _, err := managementPreflightPlan(ManagementPreflightRequest{Bundle: generated, Namespaces: ExternalNamespaceOverrides{ArgoCD: "argo-system"}}); err == nil {
		t.Fatal("unretained external Argo namespace override was accepted")
	}
}

func TestNFDWorkerRequirementUsesRetainedDPFOperatorConfiguration(t *testing.T) {
	operatorConfig := func(mode string, detectorDisabled, skipDPUNodeDiscovery bool) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "operator.dpu.nvidia.com/v1alpha1", "kind": "DPFOperatorConfig",
			"metadata": map[string]interface{}{"name": "dpfoperatorconfig", "namespace": "dpf-operator-system"},
			"spec": map[string]interface{}{
				"deploymentMode": mode,
				"dpuDetector":    map[string]interface{}{"disable": detectorDisabled},
				"provisioningController": map[string]interface{}{"installInterface": map[string]interface{}{"installViaRedfish": map[string]interface{}{
					"skipDPUNodeDiscovery": skipDPUNodeDiscovery,
				}}},
			},
		}}
	}
	for _, test := range []struct {
		name string
		obj  *unstructured.Unstructured
		want bool
	}{
		{name: "zero-trust redfish DPU discovery replaces host detector", obj: operatorConfig("zero-trust", true, false), want: false},
		{name: "host trusted mode keeps worker prerequisite", obj: operatorConfig("host-trusted", false, false), want: true},
		{name: "zero-trust detector enabled keeps worker prerequisite", obj: operatorConfig("zero-trust", false, false), want: true},
		{name: "Redfish DPU node discovery skipped keeps worker prerequisite", obj: operatorConfig("zero-trust", true, true), want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := nfdWorkerRequiredForOperatorConfig(test.obj, "dpf-operator-system")
			if err != nil || got != test.want {
				t.Fatalf("NFD worker requirement = %v, %v; want %v", got, err, test.want)
			}
		})
	}
	malformed := operatorConfig("zero-trust", true, false)
	delete(malformed.Object["spec"].(map[string]interface{}), "dpuDetector")
	if _, err := nfdWorkerRequiredForOperatorConfig(malformed, "dpf-operator-system"); err == nil {
		t.Fatal("missing detector setting was treated as a disabled detector")
	}
	if _, err := nfdWorkerRequiredForOperatorConfig(operatorConfig("unknown", true, false), "dpf-operator-system"); err == nil {
		t.Fatal("unknown deployment mode was treated as a supported NFD requirement")
	}
}

func TestPreflightCRDReadinessUsesServedEstablishedContract(t *testing.T) {
	requirement := preflightAPIRequirement{group: "example.test", version: "v1", resource: "widgets", kind: "Widget", namespaced: true}
	crd := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]interface{}{"name": "widgets.example.test", "generation": int64(4)},
		"spec":     map[string]interface{}{"group": "example.test", "scope": "Namespaced", "names": map[string]interface{}{"plural": "widgets", "kind": "Widget"}, "versions": []interface{}{map[string]interface{}{"name": "v1", "served": true, "storage": true}}},
		"status": map[string]interface{}{"conditions": []interface{}{
			map[string]interface{}{"type": "Established", "status": "True"},
			map[string]interface{}{"type": "NamesAccepted", "status": "True"},
		}},
	}}
	if !externalCRDReady(crd, requirement) {
		t.Fatal("served, established CRD without an invented observedGeneration was rejected")
	}
	for _, mutate := range []func(*unstructured.Unstructured){
		func(obj *unstructured.Unstructured) {
			obj.Object["status"].(map[string]interface{})["conditions"].([]interface{})[0].(map[string]interface{})["status"] = "False"
		},
		func(obj *unstructured.Unstructured) {
			obj.Object["spec"].(map[string]interface{})["versions"].([]interface{})[0].(map[string]interface{})["served"] = false
		},
		func(obj *unstructured.Unstructured) {
			conditions := obj.Object["status"].(map[string]interface{})["conditions"].([]interface{})
			obj.Object["status"].(map[string]interface{})["conditions"] = append(conditions, map[string]interface{}{"type": "Established", "status": "False"})
		},
		func(obj *unstructured.Unstructured) {
			obj.Object["metadata"].(map[string]interface{})["deletionTimestamp"] = "2026-11-14T00:00:00Z"
		},
	} {
		copy := crd.DeepCopy()
		mutate(copy)
		if externalCRDReady(copy, requirement) {
			t.Fatalf("malformed or incomplete CRD accepted: %#v", copy.Object)
		}
	}
}

func TestProjectedCRDMappingSupportsPreInstallCallerRBAC(t *testing.T) {
	definition := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]interface{}{"name": "widgets.example.test"},
		"spec": map[string]interface{}{"group": "example.test", "scope": "Cluster", "names": map[string]interface{}{"plural": "widgets", "kind": "Widget"}, "versions": []interface{}{map[string]interface{}{"name": "v1", "served": true}}},
	}}
	mapping := projectedCRDDefinitions([]*unstructured.Unstructured{definition})
	if got := mapping["example.test/v1/Widget"]; got.resource != "widgets" || got.namespaced {
		t.Fatalf("projected CRD mapping = %#v", got)
	}
	widget := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "example.test/v1", "kind": "Widget", "metadata": map[string]interface{}{"name": "cluster-object"}}}
	projected := append([]*unstructured.Unstructured{definition}, widget)
	permissions, err := (&APIClient{}).projectedOperatorPermissions(context.Background(), preflightPlan{operatorNamespace: "dpf-operator-system", projection: projected})
	if err != nil {
		t.Fatalf("pre-install API mapping unexpectedly required live CRD discovery: %v", err)
	}
	found := false
	crdGet := false
	widgetGet := false
	for _, permission := range permissions {
		if permission.verb == "create" && permission.group == "example.test" && permission.resource == "widgets" && permission.namespace == "" {
			found = true
		}
		if permission.verb == "get" && permission.group == "example.test" && permission.resource == "widgets" && permission.namespace == "" {
			widgetGet = true
		}
		if permission.verb == "get" && permission.resource == "customresourcedefinitions" {
			crdGet = true
		}
	}
	if !found {
		t.Fatalf("missing cluster-scoped projected API permission: %#v", permissions)
	}
	if !widgetGet {
		t.Fatalf("Helm pre-install conflict-check GET permission is missing for a projected non-wait resource: %#v", permissions)
	}
	if !crdGet {
		t.Fatalf("Helm CRD readiness GET permission is missing: %#v", permissions)
	}
	conflicting := definition.DeepCopy()
	conflicting.Object["metadata"].(map[string]interface{})["name"] = "other-widgets.example.test"
	conflicting.Object["spec"].(map[string]interface{})["names"].(map[string]interface{})["plural"] = "otherwidgets"
	mapping = projectedCRDDefinitions([]*unstructured.Unstructured{definition, conflicting, definition.DeepCopy()})
	if _, found := mapping["example.test/v1/Widget"]; found {
		t.Fatal("contradictory projected API definitions were resolved last-wins")
	}
}

func TestExternalControllerReadinessRejectsZeroAndStale(t *testing.T) {
	ready := appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Generation: 3}, Spec: appsv1.DeploymentSpec{Replicas: int32Pointer(2)}, Status: appsv1.DeploymentStatus{ObservedGeneration: 3, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2}}
	if !deploymentAvailable(ready) {
		t.Fatal("current fully ready Deployment was rejected")
	}
	zeroGeneration := ready
	zeroGeneration.Generation = 0
	zeroGeneration.Status.ObservedGeneration = 0
	if deploymentAvailable(zeroGeneration) {
		t.Fatal("Deployment with missing generation was accepted")
	}
	futureStatus := ready
	futureStatus.Status.ObservedGeneration = 4
	if deploymentAvailable(futureStatus) {
		t.Fatal("Deployment status newer than its generation was accepted")
	}
	ready.Status.ObservedGeneration = 2
	if deploymentAvailable(ready) {
		t.Fatal("stale Deployment status was accepted")
	}
	ready.Status.ObservedGeneration = 3
	ready.Spec.Replicas = int32Pointer(0)
	if deploymentAvailable(ready) {
		t.Fatal("zero-replica controller was accepted")
	}
}

func TestExternalControllerStatefulSetAndDaemonSetRequireExactPositiveGeneration(t *testing.T) {
	stateful := appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32Pointer(1)},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 2, ReadyReplicas: 1, UpdatedReplicas: 1, CurrentReplicas: 1},
	}
	if !statefulSetAvailable(stateful) {
		t.Fatal("current fully ready StatefulSet was rejected")
	}
	stateful.Status.ObservedGeneration = 3
	if statefulSetAvailable(stateful) {
		t.Fatal("StatefulSet status newer than its generation was accepted")
	}
	stateful.Generation = 0
	stateful.Status.ObservedGeneration = 0
	if statefulSetAvailable(stateful) {
		t.Fatal("StatefulSet with missing generation was accepted")
	}

	daemon := appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Generation: 4},
		Status:     appsv1.DaemonSetStatus{ObservedGeneration: 4, DesiredNumberScheduled: 1, NumberReady: 1, UpdatedNumberScheduled: 1, NumberAvailable: 1},
	}
	if !daemonSetAvailable(daemon) {
		t.Fatal("current fully ready DaemonSet was rejected")
	}
	daemon.Status.ObservedGeneration = 5
	if daemonSetAvailable(daemon) {
		t.Fatal("DaemonSet status newer than its generation was accepted")
	}
	daemon.Generation = 0
	daemon.Status.ObservedGeneration = 0
	if daemonSetAvailable(daemon) {
		t.Fatal("DaemonSet with missing generation was accepted")
	}
}

func TestPreflightAuthorizationReviewIsScopedAndDoesNotExposeServerText(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		got := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		attributes := got.Spec.ResourceAttributes
		if attributes == nil || attributes.Verb != "get" || attributes.Resource != "namespaces" || attributes.Name != "dpf-operator-system" {
			t.Fatalf("authorization attributes = %#v", attributes)
		}
		return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	api := &APIClient{access: dpfdiscovery.ManagementClient{Client: client}}
	check := api.checkAccess(context.Background(), accessRequirement{verb: "get", version: "v1", resource: "namespaces", resourceName: "dpf-operator-system"})
	if check.State != "passed" {
		t.Fatalf("SSAR check = %#v", check)
	}
}

func TestProjectedRBACRequiresExplicitPrivilegesAndDoesNotSendRulesReviews(t *testing.T) {
	role := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]interface{}{"name": "operator-controller"},
		"rules":    []interface{}{map[string]interface{}{"apiGroups": []interface{}{"apps"}, "resources": []interface{}{"deployments"}, "verbs": []interface{}{"get"}}},
	}}
	binding := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]interface{}{"name": "operator-reader", "namespace": "tenant-system"},
		"roleRef":  map[string]interface{}{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "operator-controller"},
	}}
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: false}}, nil
	})
	client.PrependReactor("create", "selfsubjectrulesreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		t.Fatal("SelfSubjectRulesReview must not be used to prove RBAC escalation or bind authorization")
		return true, nil, nil
	})
	api := &APIClient{access: dpfdiscovery.ManagementClient{Client: client}}
	checks := api.checkProjectedRBACPermissions(context.Background(), preflightPlan{projection: []*unstructured.Unstructured{role, binding}})
	if len(checks) != 2 || checks[0].State != "blocked" || checks[0].ReasonCode != "RBAC_ROLE_PRIVILEGE_UNVERIFIABLE" || checks[1].State != "blocked" || checks[1].ReasonCode != "RBAC_BIND_PRIVILEGE_UNVERIFIABLE" {
		t.Fatalf("create permission without named escalate/bind was treated as sufficient authority: %#v", checks)
	}
}

func TestProjectedRBACUsesNamedEscalateAndBindReviews(t *testing.T) {
	role := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
		"metadata": map[string]interface{}{"name": "operator-reader", "namespace": "operator-system"},
		"rules":    []interface{}{map[string]interface{}{"apiGroups": []interface{}{"apps"}, "resources": []interface{}{"deployments"}, "verbs": []interface{}{"get"}}},
	}}
	binding := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]interface{}{"name": "operator-reader", "namespace": "operator-system"},
		"roleRef":  map[string]interface{}{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "operator-reader"},
	}}
	client := fake.NewSimpleClientset()
	var calls []authorizationv1.ResourceAttributes
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		request := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		if request.Spec.ResourceAttributes == nil {
			t.Fatal("missing explicit access-review attributes")
		}
		calls = append(calls, *request.Spec.ResourceAttributes.DeepCopy())
		return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	api := &APIClient{access: dpfdiscovery.ManagementClient{Client: client}}
	checks := api.checkProjectedRBACPermissions(context.Background(), preflightPlan{projection: []*unstructured.Unstructured{role, binding}})
	if len(checks) != 2 || checks[0].State != "passed" || checks[1].State != "passed" || len(calls) != 2 {
		t.Fatalf("explicit Role privileges not accepted: checks=%#v calls=%#v", checks, calls)
	}
	if calls[0].Verb != "escalate" || calls[0].Resource != "roles" || calls[0].Name != "operator-reader" || calls[0].Namespace != "operator-system" || calls[1].Verb != "bind" || calls[1].Resource != "roles" || calls[1].Name != "operator-reader" || calls[1].Namespace != "operator-system" {
		t.Fatalf("RBAC checks were not bound to the exact namespaced role: %#v", calls)
	}
}

func TestProjectedRBACEmptyRoleNeedsNoEscalate(t *testing.T) {
	role := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]interface{}{"name": "empty-role"},
		"rules":    []interface{}{},
	}}
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		t.Fatal("empty role should not require escalate")
		return true, nil, nil
	})
	api := &APIClient{access: dpfdiscovery.ManagementClient{Client: client}}
	checks := api.checkProjectedRBACPermissions(context.Background(), preflightPlan{projection: []*unstructured.Unstructured{role}})
	if len(checks) != 1 || checks[0].State != "passed" {
		t.Fatalf("empty cluster role result = %#v", checks)
	}
}

func TestArgoControlPlaneNamespaceNeedsNoAdditionalNamespaceSetting(t *testing.T) {
	controller := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "argocd-application-controller", Namespace: "dpf-operator-system", Generation: 1, Labels: map[string]string{"app.kubernetes.io/name": "argocd-application-controller"}},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32Pointer(1), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "argocd-application-controller"}}, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "application-controller"}}}}},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 1, ReadyReplicas: 1, UpdatedReplicas: 1, CurrentReplicas: 1},
	}
	client := fake.NewSimpleClientset(controller)
	api := &APIClient{access: dpfdiscovery.ManagementClient{Client: client}}
	check := api.checkArgoApplicationScope(context.Background(), "dpf-operator-system", "dpf-operator-system")
	if check.State != "passed" {
		t.Fatalf("same-namespace Argo control plane without an additional namespace list = %#v", check)
	}
	check = api.checkArgoApplicationScope(context.Background(), "dpf-operator-system", "other-argo-namespace")
	if check.State != "blocked" || check.ReasonCode != "ARGO_OPERATOR_NAMESPACE_NOT_RECONCILED" {
		t.Fatalf("unretained external Argo control plane namespace = %#v", check)
	}
}

func TestArgoApplicationControllerNamespaceAndConnectionOverrides(t *testing.T) {
	base := corev1.Container{Name: "application-controller", Args: []string{"/usr/local/bin/argocd-application-controller", "--metrics-port=8082"}}
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Container)
		wantOK bool
	}{
		{name: "default in-cluster config", wantOK: true},
		{name: "explicit same namespace", mutate: func(container *corev1.Container) {
			container.Args = append(container.Args, "--namespace=dpf-operator-system")
		}, wantOK: true},
		{name: "long namespace override", mutate: func(container *corev1.Container) { container.Args = append(container.Args, "--namespace=other") }},
		{name: "short namespace override", mutate: func(container *corev1.Container) { container.Args = append(container.Args, "-n", "other") }},
		{name: "attached short namespace override", mutate: func(container *corev1.Container) { container.Args = append(container.Args, "-nother") }},
		{name: "equals short namespace override", mutate: func(container *corev1.Container) { container.Args = append(container.Args, "-n=other") }},
		{name: "attached server override", mutate: func(container *corev1.Container) { container.Args = append(container.Args, "-shttps://other.example") }},
		{name: "equals server override", mutate: func(container *corev1.Container) { container.Args = append(container.Args, "-s=https://other.example") }},
		{name: "shell wrapped controller command", mutate: func(container *corev1.Container) {
			container.Command = []string{"sh", "-c", "argocd-application-controller --namespace=other"}
			container.Args = nil
		}},
		{name: "external kubeconfig", mutate: func(container *corev1.Container) {
			container.Args = append(container.Args, "--kubeconfig=/etc/other/config")
		}},
		{name: "KUBECONFIG env", mutate: func(container *corev1.Container) {
			container.Env = []corev1.EnvVar{{Name: "KUBECONFIG", Value: "/etc/other/config"}}
		}},
		{name: "environment import", mutate: func(container *corev1.Container) { container.EnvFrom = []corev1.EnvFromSource{{Prefix: "ARGO_"}} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			container := *base.DeepCopy()
			if test.mutate != nil {
				test.mutate(&container)
			}
			set := appsv1.StatefulSet{Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{container}}}}}
			if got := argoControllerUsesInClusterNamespace(set, "dpf-operator-system"); got != test.wantOK {
				t.Fatalf("argoControllerUsesInClusterNamespace() = %v, want %v", got, test.wantOK)
			}
		})
	}
}

func TestCertManagerWebhookCARequiresBothConfigurations(t *testing.T) {
	validating := &admissionregistrationv1.ValidatingWebhookConfiguration{Webhooks: []admissionregistrationv1.ValidatingWebhook{{ClientConfig: admissionregistrationv1.WebhookClientConfig{CABundle: []byte("ca")}}}}
	mutating := &admissionregistrationv1.MutatingWebhookConfiguration{Webhooks: []admissionregistrationv1.MutatingWebhook{{ClientConfig: admissionregistrationv1.WebhookClientConfig{CABundle: []byte("ca")}}}}
	if !webhookCABundlesPresentValidating(validating) || !webhookCABundlesPresentMutating(mutating) {
		t.Fatal("populated cert-manager webhook CA bundles were rejected")
	}
	validating.Webhooks[0].ClientConfig.CABundle = nil
	mutating.Webhooks = nil
	if webhookCABundlesPresentValidating(validating) || webhookCABundlesPresentMutating(mutating) {
		t.Fatal("empty webhook CA data was accepted")
	}
}

func TestCertManagerWebhookCARejectsDeletingConfigurations(t *testing.T) {
	validating := &admissionregistrationv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &metav1.Time{}}, Webhooks: []admissionregistrationv1.ValidatingWebhook{{ClientConfig: admissionregistrationv1.WebhookClientConfig{CABundle: []byte("ca")}}}}
	mutating := &admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &metav1.Time{}}, Webhooks: []admissionregistrationv1.MutatingWebhook{{ClientConfig: admissionregistrationv1.WebhookClientConfig{CABundle: []byte("ca")}}}}
	if webhookCABundlesPresentValidating(validating) || webhookCABundlesPresentMutating(mutating) {
		t.Fatal("deleting cert-manager webhook configuration was accepted")
	}
}

func TestPreflightFailureClassificationHandlesWrappedCancellation(t *testing.T) {
	if got := apiFailureCode(fmt.Errorf("discovery: %w", context.Canceled)); got != "PREFLIGHT_CANCELLED" {
		t.Fatalf("wrapped cancellation code = %q", got)
	}
}

func int32Pointer(value int32) *int32 { return &value }

func preflightGenerationInputs(t *testing.T) generation.Inputs {
	t.Helper()
	hbnArchive, err := os.ReadFile(filepath.Join("..", "generation", "testdata", "doca-hbn-3.5.0.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	operatorArchive, err := os.ReadFile(filepath.Join("..", "bootstrap", "testdata", "operator", "dpf-operator-v26.8.0.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := hbn.Resolve(hbn.Options{})
	if err != nil {
		t.Fatal(err)
	}
	generationDefaults, err := hbn.ResolveGeneration(hbn.GenerationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assignments, err := profile.AllocateASNs([]string{"SERIAL-A"})
	if err != nil {
		t.Fatal(err)
	}
	devices := []bootstrap.Device{{Serial: "SERIAL-A", BMCIP: "192.0.2.20", BMCPort: 443}}
	apiIP, apiPort, vip, tenantInterface := "192.0.2.10", 6443, "192.0.2.11", "bond0"
	bootstrapInputs, issues, err := config.ResolveBootstrap(&config.BootstrapOptions{
		ManagementAPIIP: &apiIP, ManagementAPIPort: &apiPort,
		Tenant: config.TenantOptions{VIP: &vip, Interface: &tenantInterface},
	}, "dpf-operator-system", "https://api.example.test/prefix", devices)
	if err != nil || len(issues) != 0 {
		t.Fatalf("resolve test bootstrap inputs = %#v, %v", issues, err)
	}
	return generation.Inputs{
		Operator:        bootstrap.OperatorRenderInputs{ReleaseVersion: "26.8.0", ReleaseName: "dpf-operator", KubernetesVersion: "1.35.4", Values: map[string]any{}, Bootstrap: bootstrapInputs},
		OperatorArchive: operatorArchive,
		Workload: hbnrender.Inputs{
			Namespace: "dpf-operator-system", Generation: generationDefaults, Profile: profile,
			Software:           config.Software{OSISO: config.Artifact{URL: "https://artifacts.example/os.iso", SHA256: strings.Repeat("a", 64)}, PLDM: map[string]config.Artifact{"MT_0000001774": {URL: "https://artifacts.example/fw.pldm", SHA256: strings.Repeat("b", 64)}}},
			Devices:            []hbnrender.Device{{Serial: "SERIAL-A", PSID: "MT_0000001774", SelectorLabels: map[string]string{"inventory.example/serial-hash": "serial-a"}, ASN: assignments["SERIAL-A"]}},
			DPUClusterSelector: bootstrapInputs.Tenant.Labels,
			FlavorSpec:         map[string]any{"dpuMode": "dpu"},
			HBNResources:       map[string]any{"memory": "6Gi", "nvidia.com/bf_sf": "4"},
		},
		HBNChartArchive: hbnArchive,
		SourceConfig:    []byte("dpf:\n  useCase: hbn-bgp\n"),
		Management:      generation.ManagementIdentity{APIEndpoint: "https://api.example.test/prefix", SystemNamespaceUID: "system-uid-1", TrustDigest: "sha256:" + strings.Repeat("c", 64)},
	}
}
