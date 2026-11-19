// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package deployment

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nvidia/k8s-launch-kit/pkg/dpf/config"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/deployment/internal/controllercontract"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/generation"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/inventory"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/managementprobe"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/releases"
	"github.com/nvidia/k8s-launch-kit/pkg/dpf/validation"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/storage/driver"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type validationReadFake struct {
	get         func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error)
	list        func(context.Context, *unstructured.Unstructured, string) ([]unstructured.Unstructured, error)
	discover    func(context.Context, string) (*metav1.APIResourceList, error)
	operatorErr error
	identity    func(context.Context) error
}

func (f *validationReadFake) currentValidationReads() validationCurrentManagementReader { return f }

func (f *validationReadFake) Get(ctx context.Context, object *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if f.get != nil {
		return f.get(ctx, object)
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: object.GetKind()}, object.GetName())
}
func (f *validationReadFake) List(ctx context.Context, object *unstructured.Unstructured, selector string) ([]unstructured.Unstructured, error) {
	if f.list != nil {
		return f.list(ctx, object, selector)
	}
	return nil, nil
}
func (f *validationReadFake) discoverResourceList(ctx context.Context, version string) (*metav1.APIResourceList, error) {
	if f.discover != nil {
		return f.discover(ctx, version)
	}
	return nil, errors.New("unavailable")
}
func (f *validationReadFake) VerifyIdentity(ctx context.Context) error {
	if f.identity != nil {
		return f.identity(ctx)
	}
	return nil
}
func (*validationReadFake) observeTenantSources(context.Context, controllercontract.TenantReadinessExpected) (tenantSources, error) {
	return tenantSources{}, errors.New("unavailable")
}
func (*validationReadFake) listHBNRuntimeObjects(context.Context, string, string, string) ([]unstructured.Unstructured, error) {
	return nil, errors.New("unavailable")
}
func (*validationReadFake) getHBNRuntimeApplication(context.Context, string, string) (*unstructured.Unstructured, error) {
	return nil, errors.New("unavailable")
}
func (*validationReadFake) observeRegistrySnapshot(context.Context, string) (registrySnapshot, error) {
	return registrySnapshot{}, errors.New("unavailable")
}
func (*validationReadFake) observeValidationTenantStorage(context.Context, string) managementprobe.TenantStorageResult {
	return managementprobe.TenantStorageResult{State: "unknown", ReasonCode: "TENANT_STORAGE_CLIENT_UNAVAILABLE"}
}
func (f *validationReadFake) inspectValidationOperator(context.Context, *operatorDesired) error {
	return f.operatorErr
}

var _ validationManagementReader = (*validationReadFake)(nil)

func validationResourceFixture(t *testing.T) *validationObserver {
	t.Helper()
	retained, err := generateFixtureBundle(preflightGenerationInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	desired, err := prepareManagementDesired(retained)
	if err != nil {
		t.Fatal(err)
	}
	return &validationObserver{desired: desired, mode: validation.ModeResources}
}
func TestValidationRecorderClassifiesEvidenceWithoutExposingErrors(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		state  validation.State
		reason validation.Reason
	}{
		{"denied", apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "selected", errors.New("secret-canary")), validation.Unverified, validation.ReasonDenied},
		{"missing", apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "secret-canary"), validation.Failed, validation.ReasonMissing},
		{"helm missing", driver.ErrReleaseNotFound, validation.Failed, validation.ReasonMissing},
		{"timeout", context.DeadlineExceeded, validation.Unverified, validation.ReasonTimedOut},
		{"identity", errManagementIdentityChanged, validation.Unverified, validation.ReasonIdentityDrift},
		{"unknown", errors.New("secret-canary"), validation.Unverified, validation.ReasonUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := []validation.ProgressEvent{}
			r := newValidationRecorder([]validation.CheckSpec{validationSpec("selected", validation.AreaSystem, "selected", false)}, func(event validation.ProgressEvent) { events = append(events, event) })
			if err := r.run("selected", func() error { return tc.err }); err != tc.err {
				t.Fatal("check cause lost")
			}
			out := r.snapshot().Checks[0]
			if out.State != tc.state || out.Reason != tc.reason {
				t.Fatalf("unexpected classification: %+v", out)
			}
			if strings.Contains(out.Summary+out.Action, "secret-canary") {
				t.Fatal("raw error escaped")
			}
			if len(events) != 2 || events[0].Phase != validation.Running || events[1].Phase != validation.Completed {
				t.Fatal("check progress was not immediate")
			}
		})
	}
}
func TestValidationCoverageRetainsDeniedAndMissingDependencies(t *testing.T) {
	checks := []validation.CheckSpec{validationSpec("system/coverage", validation.AreaSystem, "system", true), validationSpec("system/a", validation.AreaSystem, "a", false), validationSpec("system/b", validation.AreaSystem, "b", false)}
	r := newValidationRecorder(checks, nil)
	r.record("system/a", validation.Passed, validation.ReasonNone, "ready", "")
	r.aggregate("system/coverage", "system/", nil)
	if r.outcomes["system/coverage"].State != validation.Unverified {
		t.Fatal("missing dependency passed")
	}
	r.recordError("system/b", apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "b", nil))
	r.aggregate("system/coverage", "system/", nil)
	if r.outcomes["system/coverage"].State != validation.Unverified {
		t.Fatal("denied evidence was turned into a health finding")
	}
	r.recordError("system/b", apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "b"))
	r.aggregate("system/coverage", "system/", nil)
	if r.outcomes["system/coverage"].State != validation.Failed {
		t.Fatal("authoritative missing child did not fail coverage")
	}
}
func TestValidationPlansEveryExpectedRoleAndDeviceWithoutLiveInventory(t *testing.T) {
	o := validationResourceFixture(t)
	checks := o.systemValidationChecks()
	roles := map[string]bool{}
	nodeRows := 0
	definitions := 0
	for _, check := range checks {
		if strings.HasPrefix(string(check.ID), "system/role/") && strings.HasSuffix(string(check.ID), "/service") {
			roles[string(check.ID)] = true
		}
		if strings.Contains(string(check.ID), "/device/") {
			nodeRows++
		}
		if strings.HasPrefix(string(check.ID), "definitions/tenant/") && check.ID != "definitions/tenant/coverage" {
			definitions++
		}
	}
	if len(roles) != 15 || definitions != 8 || nodeRows != 8*len(o.desired.plan.Intent().Devices) {
		t.Fatalf("expected graph reduced: roles=%d definitions=%d node rows=%d", len(roles), definitions, nodeRows)
	}
	// A reader with no create method can execute the resource domain. Every
	// expected role remains visible even when complete authorized reads are empty.
	r := newValidationRecorder(o.validationResourceChecks(), nil)
	o.fetchSystemChart = func(context.Context, string) ([]byte, error) { return nil, errors.New("offline") }
	if err := o.observeSystemResources(context.Background(), &validationReadFake{}, nil, r); err != nil {
		t.Fatal(err)
	}
	for id := range roles {
		if out := r.outcomes[validation.CheckID(id)]; out.State != validation.Failed || out.Reason != validation.ReasonMissing {
			t.Fatalf("missing role %s: %+v", id, out)
		}
	}
	if r.outcomes["system/coverage"].State == validation.Passed {
		t.Fatal("empty graph passed")
	}
}
func TestValidationCurrentConditionsRejectFutureStaleAndDuplicate(t *testing.T) {
	rules := testRules(t)
	fixture := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "svc.dpu.nvidia.com/v1alpha1", "kind": "DPUService", "metadata": map[string]any{"generation": int64(4)}, "status": map[string]any{"observedGeneration": int64(4), "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(4)}}}}}
	}
	if !rules.ValidationConditions(fixture(), "Ready").Ready {
		t.Fatal("current condition rejected")
	}
	for _, generation := range []int64{3, 5} {
		object := fixture()
		object.Object["status"].(map[string]any)["observedGeneration"] = generation
		if rules.ValidationConditions(object, "Ready").Ready {
			t.Fatal("noncurrent status accepted")
		}
	}
	duplicate := fixture()
	conditions := duplicate.Object["status"].(map[string]any)["conditions"].([]any)
	duplicate.Object["status"].(map[string]any)["conditions"] = append(conditions, conditions[0])
	if rules.ValidationConditions(duplicate, "Ready").Ready {
		t.Fatal("duplicate condition accepted")
	}
}
func TestValidationDefinitionRequiresRetainedSchemaStoredVersionAndDiscovery(t *testing.T) {
	o := validationResourceFixture(t)
	expected := o.desired.crds[0]
	base := managementFakeWithReadiness(t, o.desired.plan.Digest(), o.desired.manager, o.desired.crds)
	current := base.crds[expected.name].DeepCopy()
	versions, _, _ := unstructured.NestedSlice(current.Object, "spec", "versions")
	stored := ""
	for _, raw := range versions {
		version := raw.(map[string]any)
		if version["storage"] == true {
			stored = version["name"].(string)
		}
	}
	if err := unstructured.SetNestedStringSlice(current.Object, []string{stored}, "status", "storedVersions"); err != nil {
		t.Fatal(err)
	}
	reader := &validationReadFake{get: func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		return current.DeepCopy(), nil
	}, discover: base.discoverResourceList}
	if err := verifyValidationCRD(context.Background(), reader, expected); err != nil {
		t.Fatalf("complete definition rejected: %v", err)
	}
	changed := current.DeepCopy()
	unstructured.SetNestedStringSlice(changed.Object, []string{"old"}, "status", "storedVersions")
	reader.get = func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		return changed, nil
	}
	if verifyValidationCRD(context.Background(), reader, expected) == nil {
		t.Fatal("wrong stored version accepted")
	}
	reader.get = func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		return current.DeepCopy(), nil
	}
	reader.discover = func(context.Context, string) (*metav1.APIResourceList, error) {
		return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "apis"}, "", nil)
	}
	if err := verifyValidationCRD(context.Background(), reader, expected); !apierrors.IsForbidden(err) {
		t.Fatal("denied discovery lost its classification")
	}
}
func TestValidationSemanticFenceIgnoresResourceVersionButRejectsReplacement(t *testing.T) {
	before := validationObject("apps/v1", "Deployment", "selected", "controller")
	before.SetUID("uid")
	before.SetGeneration(3)
	before.SetResourceVersion("1")
	before.Object["spec"] = map[string]any{"replicas": int64(1)}
	after := before.DeepCopy()
	after.SetResourceVersion("2")
	if !sameValidationSemantic(before, after) {
		t.Fatal("resourceVersion churn changed semantic identity")
	}
	after.SetUID("replacement")
	if sameValidationSemantic(before, after) {
		t.Fatal("replacement accepted")
	}
}
func TestValidationApplicationInventoryRequiresIndependentRoleCardinality(t *testing.T) {
	app := validationObject("argoproj.io/v1alpha1", "Application", "operator", "app")
	app.Object["status"] = map[string]any{"resources": []any{}}
	if _, err := validationApplicationWorkload(app, "", "operator"); err != nil {
		t.Fatal("RBAC-only role required a workload")
	}
	if _, err := validationApplicationWorkload(app, "DaemonSet", "operator"); err == nil {
		t.Fatal("missing expected workload passed")
	}
	entry := map[string]any{"group": "apps", "version": "v1", "kind": "DaemonSet", "name": "controller", "namespace": "operator", "status": "Synced"}
	app.Object["status"].(map[string]any)["resources"] = []any{entry}
	if _, err := validationApplicationWorkload(app, "DaemonSet", "operator"); err != nil {
		t.Fatal(err)
	}
	app.Object["status"].(map[string]any)["resources"] = []any{entry, entry}
	if _, err := validationApplicationWorkload(app, "DaemonSet", "operator"); err == nil {
		t.Fatal("duplicate workload accepted")
	}
}
func TestValidationSystemServiceUsesExpectedRoleNotDiscoveredHealthySubset(t *testing.T) {
	o := validationResourceFixture(t)
	release, _ := releases.Resolve(o.desired.plan.Intent().Release)
	role := systemArtifactRole{Component: "multus"}
	values := systemArtifactValues(o.desired.plan.Intent(), release, role, false)
	object := validationObject("svc.dpu.nvidia.com/v1alpha1", "DPUService", o.desired.operatorNamespace, "system-multus")
	object.SetUID("service-uid")
	object.SetResourceVersion("1")
	object.SetGeneration(2)
	object.SetLabels(map[string]string{dpfComponentLabel: role.Component, "operator.dpu.nvidia.com/dpf-version": o.desired.operatorVersion, "applyset.kubernetes.io/part-of": validationApplySetID(role.Component, o.desired.operatorNamespace)})
	object.Object["spec"] = map[string]any{"helmChart": map[string]any{"source": map[string]any{"repoURL": release.SystemNetworking.ChartRepository, "chart": release.SystemNetworking.Chart, "version": release.SystemNetworking.ChartVersion}, "values": values}}
	conditions := []any{}
	for _, kind := range []string{"Ready", "ApplicationsReconciled", "ApplicationsReady"} {
		conditions = append(conditions, map[string]any{"type": kind, "status": "True", "observedGeneration": int64(2)})
	}
	object.Object["status"] = map[string]any{"observedGeneration": int64(2), "conditions": conditions}
	if _, err := o.selectValidationSystemService([]unstructured.Unstructured{*object}, role, values, release, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	other := object.DeepCopy()
	labels := other.GetLabels()
	labels[dpfComponentLabel] = "flannel"
	other.SetLabels(labels)
	if _, err := o.selectValidationSystemService([]unstructured.Unstructured{*other}, role, values, release, map[string]any{}); err == nil {
		t.Fatal("another healthy role satisfied missing role")
	}
	duplicate := object.DeepCopy()
	duplicate.SetName("duplicate")
	if _, err := o.selectValidationSystemService([]unstructured.Unstructured{*object, *duplicate}, role, values, release, map[string]any{}); err == nil {
		t.Fatal("ambiguous role passed")
	}
	before := object.DeepCopy()
	if _, err := o.selectValidationSystemService([]unstructured.Unstructured{*object}, role, values, release, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Object, object.Object) {
		t.Fatal("service verifier mutated current evidence")
	}
}
func TestValidationSystemPodRequiresCurrentOwnerImagesAndContainerHealth(t *testing.T) {
	controller := true
	template := corev1.PodSpec{Containers: []corev1.Container{{Name: "controller", Image: "registry.example/controller@sha256:locked"}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "operator", UID: "pod-uid", ResourceVersion: "1", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "controller", UID: "ds-uid", Controller: &controller}}}, Spec: *template.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "controller", ImageID: "runtime-image", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now())}}}}}}
	pod.Spec.NodeName = "node"
	if err := validationPodReady(pod, &template); err != nil {
		t.Fatal(err)
	}
	if !validationControllerOwner(pod.OwnerReferences, "DaemonSet", "controller", types.UID("ds-uid")) {
		t.Fatal("current owner rejected")
	}
	if validationControllerOwner(pod.OwnerReferences, "DaemonSet", "controller", types.UID("replaced")) {
		t.Fatal("replaced owner accepted")
	}
	changed := pod.DeepCopy()
	changed.Spec.Containers[0].Image = "unretained"
	if validationPodReady(changed, &template) == nil {
		t.Fatal("image drift accepted")
	}
	changed = pod.DeepCopy()
	changed.Status.ContainerStatuses[0].Ready = false
	if validationPodReady(changed, &template) == nil {
		t.Fatal("unready container accepted")
	}
	set := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Generation: 3}, Status: appsv1.DaemonSetStatus{ObservedGeneration: 3, DesiredNumberScheduled: 1, CurrentNumberScheduled: 1, UpdatedNumberScheduled: 1, NumberReady: 1, NumberAvailable: 1}}
	if validationDaemonSetReady(set, 1) != nil {
		t.Fatal("current rollout rejected")
	}
	set.Status.ObservedGeneration = 2
	if validationDaemonSetReady(set, 1) == nil {
		t.Fatal("stale rollout accepted")
	}
}

func TestValidationSystemApplicationBindsCurrentServiceSourceAndTarget(t *testing.T) {
	o := validationResourceFixture(t)
	release, _ := releases.Resolve(o.desired.plan.Intent().Release)
	tenant := o.desired.plan.Intent().Bootstrap.Tenant
	role := systemArtifactRole{Component: "multus"}
	service := validationObject("svc.dpu.nvidia.com/v1alpha1", "DPUService", o.desired.operatorNamespace, "system-service")
	service.SetUID("selected-service")
	service.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-time.Minute)))
	serviceValues := systemArtifactValues(o.desired.plan.Intent(), release, role, false)
	service.Object["spec"] = map[string]any{"helmChart": map[string]any{"values": serviceValues}, "serviceDaemonSet": map[string]any{"labels": map[string]any{dpfComponentLabel: "multus", hbnServiceIDLabel: "current-id"}}}
	service.Object["status"] = map[string]any{"serviceID": "current-id"}
	values := runtime.DeepCopyJSON(serviceValues)
	values["global"] = map[string]any{"serviceDaemonSet": map[string]any{"labels": map[string]any{dpfComponentLabel: "multus", hbnServiceIDLabel: "current-id"}, "annotations": map[string]any{dpfDPUClusterNameLabel: tenant.Name, dpfDPUClusterNamespaceLabel: service.GetNamespace()}}}
	source := map[string]any{"repoURL": release.SystemNetworking.ChartRepository, "chart": release.SystemNetworking.Chart, "targetRevision": release.SystemNetworking.ChartVersion, "helm": map[string]any{"valuesObject": values}}
	destination := map[string]any{"name": tenant.Name, "namespace": service.GetNamespace()}
	fixture := func() *unstructured.Unstructured {
		app := validationObject("argoproj.io/v1alpha1", "Application", o.desired.operatorNamespace, dpfApplicationName(tenant.Name, service.GetName()))
		app.SetUID("app-uid")
		app.SetResourceVersion("1")
		app.SetFinalizers([]string{argoApplicationFinalizer})
		app.SetLabels(map[string]string{dpfComponentLabel: dpfServiceManagerComponent, dpfDPUServiceNameLabel: service.GetName(), dpfDPUServiceNamespaceLabel: service.GetNamespace(), dpfDPUClusterNameLabel: tenant.Name, dpfDPUClusterNamespaceLabel: tenant.Namespace})
		app.Object["spec"] = map[string]any{"source": runtime.DeepCopyJSON(source), "destination": runtime.DeepCopyJSON(destination)}
		app.Object["status"] = map[string]any{"sync": map[string]any{"status": "Synced", "comparedTo": map[string]any{"source": runtime.DeepCopyJSON(source), "destination": runtime.DeepCopyJSON(destination)}}, "health": map[string]any{"status": "Healthy"}, "reconciledAt": time.Now().Add(-time.Second).Format(time.RFC3339Nano)}
		return app
	}
	if err := o.verifyValidationSystemApplication(role, service, fixture(), release, systemArtifactExpectation{Values: serviceValues, Defaults: map[string]any{}}); err != nil {
		t.Fatalf("current bound application rejected: %#v", err)
	}
	cases := []struct {
		name   string
		change func(*unstructured.Unstructured)
	}{
		{"wrong selected tenant", func(app *unstructured.Unstructured) {
			labels := app.GetLabels()
			labels[dpfDPUClusterNameLabel] = "other"
			app.SetLabels(labels)
		}},
		{"wrong repo", func(app *unstructured.Unstructured) {
			unstructured.SetNestedField(app.Object, "https://other.example", "spec", "source", "repoURL")
		}},
		{"stale compared source", func(app *unstructured.Unstructured) {
			unstructured.SetNestedField(app.Object, "old", "status", "sync", "comparedTo", "source", "targetRevision")
		}},
		{"stale time", func(app *unstructured.Unstructured) {
			unstructured.SetNestedField(app.Object, time.Now().Add(-time.Hour).Format(time.RFC3339Nano), "status", "reconciledAt")
		}},
		{"future time", func(app *unstructured.Unstructured) {
			unstructured.SetNestedField(app.Object, time.Now().Add(time.Hour).Format(time.RFC3339Nano), "status", "reconciledAt")
		}},
		{"wrong service id", func(app *unstructured.Unstructured) {
			unstructured.SetNestedField(app.Object, "other-id", "spec", "source", "helm", "valuesObject", "global", "serviceDaemonSet", "labels", hbnServiceIDLabel)
		}},
		{"alternate sources", func(app *unstructured.Unstructured) {
			unstructured.SetNestedSlice(app.Object, []any{map[string]any{"repoURL": "https://other.example"}}, "spec", "sources")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := fixture()
			tc.change(app)
			if o.verifyValidationSystemApplication(role, service, app, release, systemArtifactExpectation{Values: serviceValues, Defaults: map[string]any{}}) == nil {
				t.Fatal("invalid system Application passed")
			}
		})
	}
	before := service.DeepCopy()
	if err := o.verifyValidationSystemApplication(role, service, fixture(), release, systemArtifactExpectation{Values: serviceValues, Defaults: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Object, service.Object) {
		t.Fatal("App verification mutated the source service")
	}
}

func TestValidationResourceSnapshotPreservesIndependentRowsAndOmitsUnrequestedPhysical(t *testing.T) {
	o := validationResourceFixture(t)
	runtimeChecks, scope, err := validationRuntimeChecks(o.desired.plan)
	if err != nil {
		t.Fatal(err)
	}
	o.runtimeScope = scope
	o.checks = append(o.validationResourceChecks(), runtimeChecks...)
	o.checks = append(o.checks, o.physicalValidationChecks()...)
	selectedCRD := o.desired.crds[0].name
	fake := &validationReadFake{get: func(_ context.Context, wanted *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		if wanted.GetKind() == "CustomResourceDefinition" && wanted.GetName() == selectedCRD {
			return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "customresourcedefinitions"}, wanted.GetName(), errors.New("private-canary"))
		}
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: wanted.GetKind()}, wanted.GetName())
	}}
	o.newManagement = func(context.Context) (validationManagementReader, error) { return fake, nil }
	o.fetchSystemChart = func(context.Context, string) ([]byte, error) { return nil, errors.New("offline") }
	physicalCalls := 0
	o.physical = validationPhysicalReaders{bmc: func(context.Context, config.Target, time.Duration) (inventory.Device, map[string]inventory.Evidence, error) {
		physicalCalls++
		return inventory.Device{}, nil, nil
	}}
	snapshot, err := o.ObserveResources(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[validation.CheckID]validation.Outcome{}
	for _, out := range snapshot.Checks {
		byID[out.ID] = out
		if strings.HasPrefix(string(out.ID), "physical/") {
			t.Fatal("resource-only observation returned an unrequested physical row")
		}
		if strings.Contains(out.Summary+out.Action, "private-canary") {
			t.Fatal("transport error escaped")
		}
	}
	if physicalCalls != 0 {
		t.Fatal("resource-only observer contacted physical source")
	}
	if byID["operator/release"].State != validation.Passed {
		t.Fatal("independent operator release evidence lost")
	}
	if byID[validation.CheckID("definitions/management/"+selectedCRD)].Reason != validation.ReasonDenied {
		t.Fatal("denied definition became missing")
	}
	if byID["system/role/multus/tenant/service"].Reason != validation.ReasonMissing {
		t.Fatal("authoritative empty list did not preserve expected absent role")
	}
	active := 0
	for _, spec := range o.checks {
		if !spec.FullOnly {
			active++
		}
	}
	if len(snapshot.Checks) != active {
		t.Fatalf("incomplete snapshot: got %d expected %d", len(snapshot.Checks), active)
	}
	fake.identity = func(context.Context) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "kube-system", nil)
	}
	refreshed, err := o.ObserveResources(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range refreshed.Checks {
		if out.ID == "management/identity" && (out.State != validation.Unverified || out.Reason != validation.ReasonDenied) {
			t.Fatal("failed final identity refresh left an earlier pass")
		}
		if out.ID == "operator/release" && out.State != validation.Passed {
			t.Fatal("final unavailable refresh discarded independent release evidence")
		}
	}

}
func TestValidationWrongManagementIdentityStopsDependentReads(t *testing.T) {
	o := validationResourceFixture(t)
	o.checks = o.validationResourceChecks()
	opens := 0
	o.newManagement = func(context.Context) (validationManagementReader, error) {
		opens++
		return nil, errManagementIdentityChanged
	}
	fetches := 0
	o.fetchSystemChart = func(context.Context, string) ([]byte, error) { fetches++; return nil, nil }
	snapshot, err := o.ObserveResources(context.Background(), nil)
	var failure *validation.ObservationError
	if !errors.As(err, &failure) || failure.Category != validation.FatalIdentity {
		t.Fatalf("wrong cluster did not stop safely: %v", err)
	}
	if opens != 1 || fetches != 0 {
		t.Fatal("dependent work started after wrong management identity")
	}
	if len(snapshot.Checks) != len(o.checks) {
		t.Fatal("fatal observation discarded expected coverage")
	}
}

func TestValidationConstructorFreezesExplicitAccessWithoutNetwork(t *testing.T) {
	api := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	inputs := preflightGenerationInputs(t)
	inputs.Management = api.expected
	inputs.Operator.KubernetesVersion = "v1.35.6"
	inputs.SourceConfig = validationConstructorSource(t)
	retained, err := generateFixtureBundle(inputs)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := generation.LoadPlan(retained)
	if err != nil {
		t.Fatal(err)
	}
	input, err := config.Decode(inputs.SourceConfig, "source.yaml")
	if err != nil {
		t.Fatal(err)
	}
	request := api.request
	request.Input = input
	compiled, observer, err := NewValidationObserver(retained, plan, request, []config.AccessIssue{{InputRef: "/discovery/dpus/0", Role: "dpus", Message: "unavailable"}}, validation.ModeResources)
	if err != nil {
		t.Fatal(err)
	}
	if api.calls.Load() != 0 || api.writes.Load() != 0 {
		t.Fatal("constructor contacted the API")
	}
	if len(compiled.Checks()) == 0 {
		t.Fatal("unavailable credentials reduced expected checks")
	}
	native := observer.(*validationObserver)
	request.Input.Site.Discovery.DPUs[0].BMCEndpoint = "https://192.0.2.99"
	if native.request.Input.Site.Discovery.DPUs[0].BMCEndpoint != "https://192.0.2.20" {
		t.Fatal("constructor retained mutable request scope")
	}
	if err := os.WriteFile(api.request.Kubeconfig, []byte("now malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := native.newManagement(context.Background()); err != nil {
		t.Fatalf("observer reread changed kubeconfig instead of frozen access: %v", err)
	}
	if api.writes.Load() != 0 {
		t.Fatal("identity connection sent an API write")
	}
	api.uid.Store("replacement-cluster")
	if _, err := native.newManagement(context.Background()); !validationIdentityChanged(err) {
		t.Fatal("frozen credentials incorrectly cached management identity")
	}
}
func TestValidationConstructorRetainsUnavailableAccessAsObservation(t *testing.T) {
	api := newAPIFixture(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	bytes, err := os.ReadFile(api.request.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	inputs := preflightGenerationInputs(t)
	inputs.Management = api.expected
	inputs.Operator.KubernetesVersion = "v1.35.6"
	inputs.SourceConfig = validationConstructorSource(t)
	retained, err := generateFixtureBundle(inputs)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := generation.LoadPlan(retained)
	if err != nil {
		t.Fatal(err)
	}
	input, err := config.Decode(inputs.SourceConfig, "source.yaml")
	if err != nil {
		t.Fatal(err)
	}
	request := api.request
	request.Input = input
	request.Kubeconfig = filepath.Join(t.TempDir(), "not-created-yet")
	_, observer, err := NewValidationObserver(retained, plan, request, nil, validation.ModeResources)
	if err != nil {
		t.Fatalf("missing local access discarded the plan: %v", err)
	}
	if err := os.WriteFile(request.Kubeconfig, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	native := observer.(*validationObserver)
	if _, err := native.newManagement(context.Background()); err == nil {
		t.Fatal("observer silently replaced its unavailable frozen access")
	}
	if api.calls.Load() != 0 {
		t.Fatal("unavailable source triggered a network request")
	}
	snapshot, err := native.ObserveResources(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range snapshot.Checks {
		if out.ID == "management/identity" && out.State != validation.Unverified {
			t.Fatal("unavailable access became an asserted health failure")
		}
	}
}

func TestValidationSystemAttachmentsRequireExactConfigAndAppProvenance(t *testing.T) {
	app := validationObject("argoproj.io/v1alpha1", "Application", "operator", "selected-system-app")
	expected := validationObject("k8s.cni.cncf.io/v1", "NetworkAttachmentDefinition", "projected", "mybrhbn")
	expected.Object["spec"] = map[string]any{"config": "{\"cniVersion\":\"0.3.1\",\"type\":\"ovs\",\"bridge\":\"br-hbn\"}"}
	current := expected.DeepCopy()
	current.SetNamespace("operator")
	current.SetUID("attachment-uid")
	current.SetResourceVersion("1")
	current.SetAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": "selected-system-app:k8s.cni.cncf.io/NetworkAttachmentDefinition:operator/mybrhbn"})
	resource := map[string]any{"group": "k8s.cni.cncf.io", "version": "v1", "kind": "NetworkAttachmentDefinition", "namespace": "operator", "name": "mybrhbn", "status": "Synced"}
	app.Object["status"] = map[string]any{"resources": []any{resource}}
	if err := validationAttachmentInventory(app, "mybrhbn", "operator"); err != nil {
		t.Fatal(err)
	}
	if err := validationAttachmentMatches(app, expected, current, "operator"); err != nil {
		t.Fatal(err)
	}
	wrong := current.DeepCopy()
	wrong.Object["spec"].(map[string]any)["config"] = "{}"
	if validationAttachmentMatches(app, expected, wrong, "operator") == nil {
		t.Fatal("unrelated CNI config passed")
	}
	wrong = current.DeepCopy()
	wrong.SetAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": "another-app:k8s.cni.cncf.io/NetworkAttachmentDefinition:operator/mybrhbn"})
	if validationAttachmentMatches(app, expected, wrong, "operator") == nil {
		t.Fatal("wrong source App passed")
	}
	app.Object["status"].(map[string]any)["resources"] = []any{resource, resource}
	if validationAttachmentInventory(app, "mybrhbn", "operator") == nil {
		t.Fatal("duplicate attachment inventory passed")
	}
	app.Object["status"].(map[string]any)["resources"] = []any{}
	if validationAttachmentInventory(app, "mybrhbn", "operator") == nil {
		t.Fatal("missing required attachment inventory passed")
	}
}

func TestValidationPendingDependencyDoesNotDisableWait(t *testing.T) {
	checks := []validation.CheckSpec{validationSpec("system/coverage", validation.AreaSystem, "system", true), validationSpec("system/source", validation.AreaSystem, "source", false), validationSpec("system/child", validation.AreaSystem, "child", false)}
	r := newValidationRecorder(checks, nil)
	r.record("system/source", validation.Pending, validation.ReasonProgressing, "controller progressing", "")
	r.pendingDependents("system/", "system/source")
	r.aggregate("system/coverage", "system/", nil)
	if r.outcomes["system/coverage"].State != validation.Pending || r.outcomes["system/child"].State != validation.Pending {
		t.Fatal("ordinary convergence became a permanent wait blocker")
	}
	r = newValidationRecorder(checks, nil)
	r.record("system/source", validation.Unverified, validation.ReasonDenied, "denied", "")
	r.pendingDependents("system/", "system/source")
	r.blockRemaining()
	if r.outcomes["system/child"].State != validation.Unverified {
		t.Fatal("permission failure became progressing")
	}
}

func validationConstructorSource(t *testing.T) []byte {
	t.Helper()
	source := []byte("discovery:\n  dpus:\n    - bmcEndpoint: https://192.0.2.20\n      credentialRef: bmc\ncredentials:\n  bmc: {username: administrator, passwordRef: {file: missing}}\n")
	saved := &inventory.Inventory{FormatVersion: inventory.FormatVersion, ScopeDigest: "constructor-fixture-scope", DPUs: []inventory.Device{{ID: "dpu-source-a", RecordKey: "dpu-record-a", InputRef: "/discovery/dpus/0", SerialNumber: "SERIAL-A", BMCEndpoint: "https://192.0.2.20", ObservationState: "observed"}}}
	encoded, err := yaml.Marshal(map[string]*inventory.Inventory{"inventory": saved})
	if err != nil {
		t.Fatal(err)
	}
	return append(source, encoded...)
}

func TestValidationNamespaceRejectsTerminatingScopeWithSurvivingResources(t *testing.T) {
	desired := validationObject("v1", "Namespace", "", "selected")
	current := desired.DeepCopy()
	current.SetUID("namespace-uid")
	current.SetResourceVersion("1")
	current.Object["status"] = map[string]any{"phase": "Active"}
	reader := &validationReadFake{get: func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		return current.DeepCopy(), nil
	}}
	if err := verifyValidationNamespace(context.Background(), reader, desired); err != nil {
		t.Fatal(err)
	}
	stamp := metav1.Now()
	current.SetDeletionTimestamp(&stamp)
	if verifyValidationNamespace(context.Background(), reader, desired) == nil {
		t.Fatal("terminating Namespace passed because remaining resources might be ready")
	}
	current.SetDeletionTimestamp(nil)
	current.Object["status"].(map[string]any)["phase"] = "Terminating"
	if verifyValidationNamespace(context.Background(), reader, desired) == nil {
		t.Fatal("non-Active Namespace passed")
	}
}

func TestValidationTenantStoragePreservesMissingDeniedAndHealth(t *testing.T) {
	for _, tc := range []struct {
		state, code string
		want        validation.State
		reason      validation.Reason
	}{
		{"failed", "TENANT_STORAGE_RESOURCE_MISSING", validation.Failed, validation.ReasonMissing},
		{"failed", "TENANT_ETCD_WORKLOAD_UNHEALTHY", validation.Failed, validation.ReasonHealth},
		{"unknown", "TENANT_STORAGE_ACCESS_DENIED", validation.Unverified, validation.ReasonDenied},
		{"unknown", "TENANT_STORAGE_CLIENT_UNAVAILABLE", validation.Unverified, validation.ReasonUnavailable},
		{"unknown", "TENANT_STORAGE_CHANGED_DURING_CHECK", validation.Unverified, validation.ReasonIncomplete},
	} {
		t.Run(tc.code, func(t *testing.T) {
			r := newValidationRecorder([]validation.CheckSpec{validationSpec("tenant/storage", validation.AreaTenant, "storage", false)}, nil)
			r.run("tenant/storage", func() error {
				return validationTenantStorageResult(managementprobe.TenantStorageResult{State: tc.state, ReasonCode: tc.code, Message: "private-server-canary"})
			})
			got := r.outcomes["tenant/storage"]
			if got.State != tc.want || got.Reason != tc.reason || strings.Contains(got.Summary, "private-server") {
				t.Fatalf("incorrect classification: %+v", got)
			}
		})
	}
	if err := validationTenantStorageResult(managementprobe.TenantStorageResult{State: "passed"}); err != nil {
		t.Fatal(err)
	}
}
